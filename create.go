package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const lookupTimeout = 15 * time.Second

var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// createPlan is everything sbx create is told.
type createPlan struct {
	Name  string
	Agent string
	Kits  []string
	Paths []string
}

func (p createPlan) argv() []string {
	args := []string{"create", "--no-progress", "--name", p.Name}
	for _, k := range p.Kits {
		args = append(args, "--kit", k)
	}
	args = append(args, p.Agent)
	return append(args, p.Paths...)
}

// planCreate returns a usage error when the arguments cannot become a plan.
func planCreate(agent string, kits, paths []string, cwd, home string, rnd io.Reader) (createPlan, error) {
	a := strings.ToLower(agent)
	if !agentName.MatchString(a) {
		return createPlan{}, fmt.Errorf("agent %q: is not an agent name; use a built-in agent such as claude, codex, or opencode", agent)
	}
	for _, p := range paths {
		if p == "" || strings.HasPrefix(p, "-") {
			return createPlan{}, fmt.Errorf("agent %q: PATH %q must not be empty or start with -", a, p)
		}
	}
	if len(paths) == 0 {
		ws, err := defaultWorkspace(cwd, home)
		if err != nil {
			return createPlan{}, fmt.Errorf("agent %q: %v", a, err)
		}
		paths = []string{ws}
	}
	name, err := sandboxName(a, rnd)
	if err != nil {
		return createPlan{}, fmt.Errorf("agent %q: cannot choose a sandbox name: %v", a, err)
	}
	return createPlan{Name: name, Agent: a, Kits: kits, Paths: paths}, nil
}

// defaultWorkspace is the working directory, because an editor starts the agent
// server in the project and sbx mounts it at the same path, so the cwd the
// client sends exists inside the sandbox. An app launched from a desktop starts
// in / or the home directory, and mounting either would expose far more.
func defaultWorkspace(cwd, home string) (string, error) {
	ws := filepath.Clean(cwd)
	if real := resolved(ws); filepath.Dir(real) == real || home != "" && real == resolved(home) {
		return "", fmt.Errorf("refusing to mount %q as the workspace; start sbx-acp in the project directory or pass a PATH", ws)
	}
	return ws, nil
}

// resolved follows symlinks, so a home directory that is reached through one
// still compares equal to the working directory the kernel reports.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// sandboxName fits sbx's 63-character limit.
func sandboxName(agent string, rnd io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", err
	}
	const maxAgent = 63 - len("acp--") - 2*len(b)
	if len(agent) > maxAgent {
		agent = agent[:maxAgent]
	}
	return "acp-" + agent + "-" + hex.EncodeToString(b[:]), nil
}

func runCreate(args []string, stderr io.Writer, hub catalog) int {
	fs := flag.NewFlagSet("sbx-acp create", flag.ContinueOnError)
	var kits []string
	fs.Func("kit", "ACP mixin reference for sbx create --kit, repeatable; skips the Docker Hub lookup", func(s string) error {
		kits = append(kits, s)
		return nil
	})
	rm := fs.Bool("rm", false, "remove the sandbox when the session ends")
	sbxName := fs.String("sbx", "sbx", "name or path of the sbx executable")
	check := fs.Bool("check", false, "look up the mixin, print the sbx create command on stderr, and create nothing")
	if code, ok := parseFlags(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "sbx-acp: expected an AGENT name, then optional PATH arguments")
		printUsage(fs, stderr)
		return exitUsage
	}
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	plan, err := planCreate(fs.Arg(0), kits, fs.Args()[1:], cwd, home, rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "sbx-acp: %v\n", err)
		return exitUsage
	}
	say := agentReporter(stderr, plan.Agent).say
	sbxPath, err := locateSbx(*sbxName)
	if err != nil {
		say("%v", err)
		return exitSbxFailure
	}
	in := watchInterrupts()
	defer in.stop()

	source, hint := "mixins from --kit", ""
	if len(plan.Kits) == 0 {
		ctx, cancel := context.WithTimeout(in.ctx, lookupTimeout)
		published, err := hub.Published(ctx, mixinNamespace, plan.Agent)
		cancel()
		if code := in.received(); code != 0 {
			return code
		}
		if err != nil {
			say("cannot read the kit catalog on Docker Hub (%s); pass --kit REF to name the ACP mixin yourself", oneLine(err))
			return exitCatalog
		}
		ref, warnings, err := ACPMixin(plan.Agent, published)
		for _, w := range warnings {
			say("warning: %s", w)
		}
		if err != nil {
			say("%v", err)
			return exitNoLaunch
		}
		if ref == "" {
			source, hint = "no ACP mixin found on Docker Hub; the agent's own kit must declare ACP", "; pass --kit REF to add an ACP mixin"
		} else {
			source, plan.Kits = fmt.Sprintf("ACP mixin %s, found on Docker Hub", ref), []string{ref}
		}
	}
	if *check {
		say("%s", source)
		say("would run: %s", shellLine(append([]string{sbxPath}, plan.argv()...)))
		return 0
	}

	sandboxSay := sandboxReporter(stderr, plan.Name).say
	say("creating sandbox %q", plan.Name)
	code, err := createSandbox(in, sbxPath, plan.argv(), stderr)
	switch {
	case errors.Is(err, errInterrupted):
		return code
	case err != nil:
		sandboxSay("cannot start sbx: %v", err)
		return exitSbxFailure
	case code != 0 && in.received() != 0:
		return in.received()
	case code != 0:
		// A failed create does not prove this process made a sandbox of that
		// name, so removing it could delete someone else's.
		sandboxSay("sbx create failed (exit %d); if the sandbox exists, remove it with: sbx rm %s", code, plan.Name)
		return exitSbxFailure
	}

	code, started := attachTo(in, sbxPath, plan.Name, plan.Agent, false, stderr)
	if started && !*rm {
		return code
	}
	if err := removeSandbox(sbxPath, plan.Name, stderr); err != nil {
		sandboxSay("warning: could not remove it; run: sbx rm %s", plan.Name)
	} else if !started {
		sandboxSay("removed, because it never started an ACP agent%s", hint)
	}
	return code
}
