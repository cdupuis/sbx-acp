package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const (
	exitUsage      = 2
	exitNoLaunch   = 3
	exitCatalog    = 4
	exitSbxFailure = 125
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, dockerHub()))
}

// exitStatus carries the exit code a command chose after printing its own
// sentences, so run can tell it apart from a usage error.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

func status(code int) error {
	if code == 0 {
		return nil
	}
	return exitStatus(code)
}

func run(args []string, stderr io.Writer, hub catalog) int {
	root := rootCmd(stderr, hub)
	// A nil slice makes cobra read os.Args, which under go test are the test
	// binary's flags.
	root.SetArgs(append([]string{}, args...))
	cmd, err := root.ExecuteC()
	var st exitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &st):
		return int(st)
	}
	fmt.Fprintf(stderr, "sbx-acp: %v\n", err)
	fmt.Fprint(stderr, cmd.UsageString())
	return exitUsage
}

func rootCmd(stderr io.Writer, hub catalog) *cobra.Command {
	root := &cobra.Command{
		Use:   "sbx-acp",
		Short: "Start the ACP agent of a Docker sandbox on stdio",
		Long: "sbx-acp starts the Agent Client Protocol agent of a Docker sandbox on stdio.\n" +
			"Only the agent's bytes reach stdout. Errors, --check, and help go to stderr.",
		// Runnable so that no arguments exits 2 and the old bare form reaches
		// Args: a root that cannot run returns ErrHelp first, which exits 0.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q; to attach to a sandbox, run: sbx-acp attach %s", args[0], shellQuote(args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = cmd.Help()
			return exitStatus(exitUsage)
		},
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		CompletionOptions:     cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	// Help, usage, and the hidden __complete command write to Out, which is
	// stdout unless set, and stdout belongs to the agent.
	root.SetOut(stderr)
	root.SetErr(stderr)
	root.AddCommand(attachCmd(stderr), createCmd(stderr, hub))
	return root
}

type reporter struct {
	w       io.Writer
	subject string
}

func sandboxReporter(w io.Writer, name string) reporter {
	return reporter{w, fmt.Sprintf("sandbox %q", name)}
}

func agentReporter(w io.Writer, agent string) reporter {
	return reporter{w, fmt.Sprintf("agent %q", agent)}
}

func (r reporter) say(format string, a ...any) {
	fmt.Fprintf(r.w, "sbx-acp: %s: %s\n", r.subject, fmt.Sprintf(format, a...))
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
