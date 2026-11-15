package main

import (
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type attachOptions struct {
	agent string
	sbx   string
	check bool
}

func attachCmd(stderr io.Writer) *cobra.Command {
	var o attachOptions
	cmd := &cobra.Command{
		Use:   "attach [flags] SANDBOX",
		Short: "Start the ACP agent of an existing sandbox",
		Example: "  sbx-acp attach my-sandbox\n" +
			"  sbx-acp attach my-sandbox --agent claude --check",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
				return errors.New("expected exactly one SANDBOX name that does not start with -")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return status(runAttach(o, args[0], stderr))
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.agent, "agent", "", "ACP agent `NAME` to start when the sandbox declares more than one")
	f.StringVar(&o.sbx, "sbx", "sbx", "name or `PATH` of the sbx executable")
	f.BoolVar(&o.check, "check", false, "resolve the launch, print it on stderr, and start nothing")
	return cmd
}

func runAttach(o attachOptions, sandbox string, stderr io.Writer) int {
	sbxPath, err := locateSbx(o.sbx)
	if err != nil {
		sandboxReporter(stderr, sandbox).say("%v", err)
		return exitSbxFailure
	}
	in := watchInterrupts()
	defer in.stop()
	code, _ := attachTo(in, sbxPath, sandbox, o.agent, o.check, stderr)
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
