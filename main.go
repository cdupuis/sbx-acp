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
	exitSbxFailure = 125
)

const usageLine = "usage: sbx-acp [--agent NAME] [--sbx PATH] [--check] SANDBOX\n"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sbx-acp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "", "ACP agent to start when the sandbox declares more than one")
	sbxName := fs.String("sbx", "sbx", "name or path of the sbx executable")
	check := fs.Bool("check", false, "resolve the launch, print it on stderr, and start nothing")

	usage := func() {
		fmt.Fprint(stderr, usageLine)
		fs.SetOutput(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0
		}
		fmt.Fprintf(stderr, "sbx-acp: %v\n", err)
		usage()
		return exitUsage
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" || strings.HasPrefix(fs.Arg(0), "-") {
		fmt.Fprintln(stderr, "sbx-acp: expected exactly one SANDBOX name that does not start with -")
		usage()
		return exitUsage
	}
	sandbox := fs.Arg(0)

	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: %s\n", sandbox, fmt.Sprintf(format, a...))
		return code
	}

	sbxPath, err := locateSbx(*sbxName)
	if err != nil {
		return fail(exitSbxFailure, "%v", err)
	}
	kits, err := readKits(sbxPath, sandbox)
	if err != nil {
		return fail(exitSbxFailure, "%v", err)
	}
	if len(kits) == 0 {
		return fail(exitNoLaunch, "was not composed from Kits, so it has no staged descriptors under %s", kitRoot)
	}
	launch, warnings, err := Resolve(kits, *agent)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: warning: %s\n", sandbox, w)
	}
	if err != nil {
		return fail(exitNoLaunch, "%v", err)
	}
	if *check {
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: agent %s, declared by %s, ACP protocol version %d\n",
			sandbox, launch.Agent, launch.DeclaredBy, launch.ProtocolVersion)
		fmt.Fprintf(stderr, "sbx-acp: sandbox %q: would run: %s\n", sandbox, shellLine(append([]string{sbxPath}, execArgv(sandbox, launch)...)))
		return 0
	}
	code, err := stream(sbxPath, sandbox, launch, stderr)
	if err != nil {
		return fail(exitSbxFailure, "cannot start sbx: %v", err)
	}
	return code
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
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			plain = false
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
