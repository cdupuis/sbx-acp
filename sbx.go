package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
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
func readKits(in *interrupts, sbxPath, sandbox string) ([]StagedKit, error) {
	cmd := exec.Command(sbxPath, "exec", sandbox, "sh", "-c", discoverScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code, err := in.run(cmd)
	detail := ""
	if s := strings.TrimSpace(stderr.String()); s != "" {
		detail = ": " + s
	}
	switch {
	case err != nil:
		return nil, fmt.Errorf("sbx exec failed (%v)%s", err, detail)
	case code == 125:
		return nil, fmt.Errorf("sbx could not run a command in the sandbox (exit 125)%s", detail)
	case code == 127:
		return nil, fmt.Errorf("the sandbox image needs sh and cat to read its kit descriptors (exit 127)%s", detail)
	case code != 0:
		return nil, fmt.Errorf("sbx exec failed (exit status %d)%s", code, detail)
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

const checkExecutableScript = `case "$1" in
  */*) [ -f "$1" ] && [ -x "$1" ] && exit 0; exit 127 ;;
esac
search=${2-$PATH}
while :; do
  dir=${search%%:*}
  [ -n "$dir" ] || dir=.
  [ -f "$dir/$1" ] && [ -x "$dir/$1" ] && exit 0
  case "$search" in
    *:*) search=${search#*:} ;;
    *) exit 127 ;;
  esac
done`

func checkLaunch(in *interrupts, sbxPath, sandbox string, l Launch, stderr io.Writer) (int, error) {
	args := []string{"exec", sandbox, "sh", "-c", checkExecutableScript, "sbx-acp", l.Argv[0]}
	if path, ok := l.Env["PATH"]; ok {
		args = append(args, path)
	}
	cmd := exec.Command(sbxPath, args...)
	cmd.Stderr = stderr
	return in.run(cmd)
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
func stream(in *interrupts, sbxPath, sandbox string, l Launch, stderr io.Writer) (code int, err error) {
	cmd := exec.Command(sbxPath, execArgv(sandbox, l)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	return in.run(cmd)
}

// createSandbox leaves stdin disconnected, and sends sbx create's stdout to
// stderr because sbx create prints its progress there.
func createSandbox(in *interrupts, sbxPath string, argv []string, stderr io.Writer) (code int, err error) {
	cmd := exec.Command(sbxPath, argv...)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	return in.run(cmd)
}

// removeSandbox gets no signal forwarding: an interrupt during removal must
// not leave the sandbox half removed.
func removeSandbox(sbxPath, name string, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sbxPath, "rm", "--force", name)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

// errInterrupted means a signal arrived before the child could start, so it
// was not started.
var errInterrupted = errors.New("interrupted before sbx started")

// interrupts owns SIGINT and SIGTERM for the whole run. Registering once,
// before the first child, means a signal between two children is held for
// the next one instead of killing this process and skipping removal.
type interrupts struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan os.Signal
	mu     sync.Mutex
	got    os.Signal
	child  *os.Process
}

func watchInterrupts() *interrupts {
	in := &interrupts{ch: make(chan os.Signal, 1)}
	in.ctx, in.cancel = context.WithCancel(context.Background())
	signal.Notify(in.ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range in.ch {
			in.mu.Lock()
			if in.got == nil {
				in.got = s
				in.cancel()
			}
			if in.child != nil && in.child.Signal(s) != nil {
				_ = in.child.Kill()
			}
			in.mu.Unlock()
		}
	}()
	return in
}

func (in *interrupts) stop() {
	signal.Stop(in.ch)
	close(in.ch)
	in.cancel()
}

// received returns 128+n for the first signal, or 0 when none arrived.
func (in *interrupts) received() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return signalCode(in.got)
}

func signalCode(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return 0
}

// run starts cmd, forwards signals to it while it runs, and returns its exit code.
func (in *interrupts) run(cmd *exec.Cmd) (code int, err error) {
	in.mu.Lock()
	if in.got != nil {
		defer in.mu.Unlock()
		return signalCode(in.got), errInterrupted
	}
	if err := cmd.Start(); err != nil {
		in.mu.Unlock()
		return 0, err
	}
	in.child = cmd.Process
	in.mu.Unlock()
	code = exitCode(cmd.Wait())
	in.mu.Lock()
	in.child = nil
	in.mu.Unlock()
	return code, nil
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
