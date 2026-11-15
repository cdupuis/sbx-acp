package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	exitUsage      = 2
	exitNoLaunch   = 3
	exitCatalog    = 4
	exitSbxFailure = 125
)

const usageText = `usage: sbx-acp attach [--agent NAME] [--sbx PATH] [--check] SANDBOX
       sbx-acp create [--kit REF]... [--rm] [--sbx PATH] [--check] AGENT [PATH...]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "attach":
		return runAttach(args[1:], stderr)
	case "create":
		return runCreate(args[1:], stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stderr, usageText)
		return 0
	}
	fmt.Fprintf(stderr, "sbx-acp: unknown command %q; to attach to a sandbox, run: sbx-acp attach %s\n", args[0], shellQuote(args[0]))
	fmt.Fprint(stderr, usageText)
	return exitUsage
}

// parseFlags prints the usage and returns the exit code when it fails.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case err == nil:
		return 0, true
	case errors.Is(err, flag.ErrHelp):
		code = 0
	default:
		code = exitUsage
		fmt.Fprintf(stderr, "sbx-acp: %v\n", err)
	}
	printUsage(fs, stderr)
	return code, false
}

func printUsage(fs *flag.FlagSet, stderr io.Writer) {
	fmt.Fprint(stderr, usageText)
	fs.SetOutput(stderr)
	fs.PrintDefaults()
}

func runAttach(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sbx-acp attach", flag.ContinueOnError)
	agent := fs.String("agent", "", "ACP agent to start when the sandbox declares more than one")
	sbxName := fs.String("sbx", "sbx", "name or path of the sbx executable")
	check := fs.Bool("check", false, "resolve the launch, print it on stderr, and start nothing")
	if code, ok := parseFlags(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" || strings.HasPrefix(fs.Arg(0), "-") {
		fmt.Fprintln(stderr, "sbx-acp: expected exactly one SANDBOX name that does not start with -")
		printUsage(fs, stderr)
		return exitUsage
	}
	sandbox := fs.Arg(0)
	sbxPath, err := locateSbx(*sbxName)
	if err != nil {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: %v\n", sandbox, err)
		return exitSbxFailure
	}
	in := watchInterrupts()
	defer in.stop()
	code, _ := attachTo(in, sbxPath, sandbox, *agent, *check, stderr)
	return code
}

// attachTo reads the sandbox's staged kits and starts its ACP agent on stdio.
// started is true once the agent's stream was started or --check printed.
func attachTo(in *interrupts, sbxPath, sandbox, agent string, check bool, stderr io.Writer) (code int, started bool) {
	fail := func(code int, format string, a ...any) (int, bool) {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: %s\n", sandbox, fmt.Sprintf(format, a...))
		return code, false
	}
	kits, err := readKits(sbxPath, sandbox)
	if err != nil {
		return fail(exitSbxFailure, "%v", err)
	}
	if len(kits) == 0 {
		return fail(exitNoLaunch, "was not composed from Kits, so it has no staged descriptors under %s", kitRoot)
	}
	launch, warnings, err := Resolve(kits, agent)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: warning: %s\n", sandbox, w)
	}
	if err != nil {
		return fail(exitNoLaunch, "%v", err)
	}
	if check {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: agent %s, declared by %s, ACP protocol version %d\n",
			sandbox, launch.Agent, launch.DeclaredBy, launch.ProtocolVersion)
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: would run: %s\n", sandbox, shellLine(append([]string{sbxPath}, execArgv(sandbox, launch)...)))
		return 0, true
	}
	code, err = stream(in, sbxPath, sandbox, launch, stderr)
	switch {
	case errors.Is(err, errInterrupted):
		return code, false
	case err != nil:
		return fail(exitSbxFailure, "cannot start sbx: %v", err)
	}
	return code, true
}

func shellLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	plain := s != ""
	for _, r := range s {
		safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)
		plain = plain && safe
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
