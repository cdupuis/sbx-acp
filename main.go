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
