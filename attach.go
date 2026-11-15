package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

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
		sandboxReporter(stderr, sandbox).say("%v", err)
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
	r := sandboxReporter(stderr, sandbox)
	kits, err := readKits(sbxPath, sandbox)
	if err != nil {
		r.say("%v", err)
		return exitSbxFailure, false
	}
	if len(kits) == 0 {
		r.say("was not composed from Kits, so it has no staged descriptors under %s", kitRoot)
		return exitNoLaunch, false
	}
	launch, warnings, err := Resolve(kits, agent)
	for _, w := range warnings {
		r.say("warning: %s", w)
	}
	if err != nil {
		r.say("%v", err)
		return exitNoLaunch, false
	}
	if check {
		r.say("agent %s, declared by %s, ACP protocol version %d", launch.Agent, launch.DeclaredBy, launch.ProtocolVersion)
		r.say("would run: %s", shellLine(append([]string{sbxPath}, execArgv(sandbox, launch)...)))
		return 0, true
	}
	code, err = stream(in, sbxPath, sandbox, launch, stderr)
	switch {
	case errors.Is(err, errInterrupted):
		return code, false
	case err != nil:
		r.say("cannot start sbx: %v", err)
		return exitSbxFailure, false
	}
	return code, true
}
