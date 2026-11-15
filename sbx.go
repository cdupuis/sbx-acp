package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

const kitRoot = "/usr/share/sandbox/kit"

// The script frames each descriptor as NUL path NUL bytes and ends with NUL NUL.
// NUL is safe as a delimiter because the kit spec forbids it in descriptor strings.
const discoverScript = `for f in ` + kitRoot + `/*/kit.yaml; do [ -f "$f" ] || continue; printf '\000%s\000' "$f"; cat "$f" || exit 1; done; printf '\000\000'`

func locateSbx(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("cannot find the sbx executable %q; put it on PATH or pass --sbx PATH", name)
	}
	return path, nil
}

// readKits leaves stdin disconnected: the client's first request is already
// waiting there and belongs to the agent.
func readKits(sbxPath, sandbox string) ([]StagedKit, error) {
	cmd := exec.Command(sbxPath, "exec", sandbox, "sh", "-c", discoverScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	detail := ""
	if s := strings.TrimSpace(stderr.String()); s != "" {
		detail = ": " + s
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 125:
		return nil, fmt.Errorf("sbx could not run a command in the sandbox (exit 125)%s", detail)
	case errors.As(err, &exit) && exit.ExitCode() == 127:
		return nil, fmt.Errorf("the sandbox image needs sh and cat to read its kit descriptors (exit 127)%s", detail)
	case err != nil:
		return nil, fmt.Errorf("sbx exec failed (%v)%s", err, detail)
	}
	return parseStaged(stdout.Bytes())
}

var errTruncated = errors.New("the kit descriptor stream ended early, so the read is incomplete")

func parseStaged(b []byte) ([]StagedKit, error) {
	var kits []StagedKit
	for {
		if len(b) < 2 || b[0] != 0 {
			return nil, errTruncated
		}
		if b[1] == 0 {
			if len(b) != 2 {
				return nil, errors.New("the kit descriptor stream has bytes after its end marker")
			}
			return kits, nil
		}
		path, rest, ok := bytes.Cut(b[1:], []byte{0})
		if !ok {
			return nil, errTruncated
		}
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, errTruncated
		}
		stem, ok := strings.CutPrefix(string(path), kitRoot+"/")
		stem, ok2 := strings.CutSuffix(stem, "/kit.yaml")
		if !ok || !ok2 || stem == "" || strings.Contains(stem, "/") {
			return nil, fmt.Errorf("the kit descriptor stream names an unexpected path %q", path)
		}
		kits = append(kits, StagedKit{Stem: stem, Descriptor: rest[:end]})
		b = rest[end:]
	}
}

// execArgv is the argument list after the sbx path, for both --check and the stream.
func execArgv(sandbox string, l Launch) []string {
	args := []string{"exec", "-i"}
	for _, k := range slices.Sorted(maps.Keys(l.Env)) {
		args = append(args, "-e", k+"="+l.Env[k])
	}
	args = append(args, sandbox)
	return append(args, l.Argv...)
}

// stream hands this process's stdin and stdout to sbx directly. They are
// *os.File, so os/exec passes the descriptors through and no copy loop can
// reorder or drop protocol bytes.
func stream(sbxPath, sandbox string, l Launch, stderr io.Writer) (code int, err error) {
	cmd := exec.Command(sbxPath, execArgv(sandbox, l)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if cmd.Process.Signal(s) != nil {
				cmd.Process.Kill()
			}
		}
	}()
	return exitCode(cmd.Wait()), nil
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		if err != nil {
			return 1
		}
		return 0
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if c := exit.ExitCode(); c >= 0 {
		return c
	}
	return 1
}
