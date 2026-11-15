package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	binPath string
	fakeSbx string
)

// TestMain re-executes the test binary as the fake sbx when it is invoked
// under the name "sbx", and builds the real sbx-acp binary once otherwise.
func TestMain(m *testing.M) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "sbx" {
		os.Exit(runFakeSbx(os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "sbx-acp-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := func() int {
		binPath = filepath.Join(dir, "sbx-acp")
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building sbx-acp: %v\n%s", err, out)
			return 1
		}
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fakeSbx = filepath.Join(dir, "bin", "sbx")
		if err := os.MkdirAll(filepath.Dir(fakeSbx), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Symlink(self, fakeSbx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}()
	os.RemoveAll(dir)
	os.Exit(code)
}

// runFakeSbx implements `sbx exec [-i] [-e K=V]... SANDBOX CMD...`,
// `sbx create ... --name NAME ...`, and `sbx rm --force NAME`. Each sandbox is a
// directory under FAKE_SBX_ROOT holding its staged kits, and create copies
// FAKE_SBX_TEMPLATE there. Every argv is appended to FAKE_SBX_LOG as one JSON
// array per line.
func runFakeSbx(args []string) int {
	if f, err := os.OpenFile(os.Getenv("FAKE_SBX_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		line, _ := json.Marshal(args)
		fmt.Fprintf(f, "%s\n", line)
		f.Close()
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake sbx: no command")
		return 2
	}
	switch args[0] {
	case "exec":
		return fakeExec(args[1:])
	case "create":
		return fakeCreate(args[1:])
	case "rm":
		return fakeRm(args[1:])
	}
	fmt.Fprintf(os.Stderr, "fake sbx: %q is not implemented\n", args[0])
	return 2
}

func fakeSandboxDir(name string) (string, bool) {
	dir := filepath.Join(os.Getenv("FAKE_SBX_ROOT"), name)
	_, err := os.Stat(dir)
	return dir, err == nil
}

func fakeCreate(args []string) int {
	name := ""
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			name = args[i+1]
		}
	}
	switch os.Getenv("FAKE_SBX_FAULT") {
	case "create-fails":
		fmt.Fprintln(os.Stderr, "error: the daemon refused the create")
		return 1
	case "create-sleeps":
		time.Sleep(10 * time.Second)
	}
	dir, exists := fakeSandboxDir(name)
	if name == "" || exists {
		fmt.Fprintf(os.Stderr, "error: sandbox '%s' already exists\n", name)
		return 1
	}
	if err := os.CopyFS(dir, os.DirFS(os.Getenv("FAKE_SBX_TEMPLATE"))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("\n  ✓ Created sandbox %s\n", name)
	return 0
}

func fakeRm(args []string) int {
	if len(args) != 2 || args[0] != "--force" {
		fmt.Fprintf(os.Stderr, "fake sbx: usage: sbx rm --force NAME, got %q\n", args)
		return 2
	}
	dir, exists := fakeSandboxDir(args[1])
	switch {
	case os.Getenv("FAKE_SBX_FAULT") == "rm-fails":
		fmt.Fprintln(os.Stderr, "error: the daemon refused the removal")
		return 1
	case !exists:
		fmt.Fprintf(os.Stderr, "error: sandbox '%s' not found\n", args[1])
		return 1
	}
	os.RemoveAll(dir)
	fmt.Printf("Removed sandbox %s\n", args[1])
	return 0
}

func fakeExec(args []string) int {
	interactive := false
	var overlay []string
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "-i":
			interactive = true
			args = args[1:]
		case args[0] == "-e" && len(args) > 1 && strings.Contains(args[1], "="):
			overlay = append(overlay, args[1])
			args = args[2:]
		default:
			fmt.Fprintf(os.Stderr, "fake sbx: unknown flag or usage %q\n", args[0])
			return 2
		}
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "fake sbx: usage: sbx exec [flags] SANDBOX COMMAND [ARG...]")
		return 2
	}
	sandbox, argv := args[0], args[1:]
	root, exists := fakeSandboxDir(sandbox)
	if !exists {
		fmt.Fprintf(os.Stderr, "sandbox %q not found\n", sandbox)
		return 125
	}
	if len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], kitRoot) {
		return fakeDiscovery(argv[2], root)
	}
	if os.Getenv("FAKE_SBX_FAULT") == "die-by-kill" {
		self, _ := os.FindProcess(os.Getpid())
		_ = self.Signal(syscall.SIGKILL)
		time.Sleep(10 * time.Second)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), overlay...)
	if interactive {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

func fakeDiscovery(script, root string) int {
	if os.Getenv("FAKE_SBX_FAULT") == "no-sh" {
		fmt.Fprintln(os.Stderr, "exec: sh: not found")
		return 127
	}
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(script, kitRoot, root)).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out = bytes.ReplaceAll(out, []byte(root), []byte(kitRoot))
	if n, ok := strings.CutPrefix(os.Getenv("FAKE_SBX_FAULT"), "truncate:"); ok {
		cut, _ := strconv.Atoi(n)
		out = out[:len(out)-cut]
	}
	os.Stdout.Write(out)
	return 0
}

type result struct {
	stdout []byte
	stderr string
	code   int
	calls  [][]string
}

type setup struct {
	kits       []string
	createKits []string
	fault      string
	stdin      []byte
	path       bool
	dir        string
	env        []string
}

type session struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	logPath        string
}

// newSession prepares the built sbx-acp with an existing sandbox "box" staging
// the fixtures in s.kits, and makes every sandbox it creates stage s.createKits.
// Unless s.path is set, callers pass --sbx with the fake.
func newSession(t *testing.T, s setup, args ...string) *session {
	t.Helper()
	tmp := t.TempDir()
	root, template := filepath.Join(tmp, "sandboxes"), filepath.Join(tmp, "template")
	stageFixtures(t, filepath.Join(root, "box"), s.kits)
	stageFixtures(t, template, s.createKits)
	ss := &session{logPath: filepath.Join(tmp, "calls.jsonl")}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ss.cmd = exec.CommandContext(ctx, binPath, args...)
	ss.cmd.Dir = s.dir
	ss.cmd.Env = append(os.Environ(),
		"FAKE_SBX_LOG="+ss.logPath, "FAKE_SBX_ROOT="+root, "FAKE_SBX_TEMPLATE="+template, "FAKE_SBX_FAULT="+s.fault)
	if s.path {
		ss.cmd.Env = append(ss.cmd.Env, "PATH="+filepath.Dir(fakeSbx)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	ss.cmd.Env = append(ss.cmd.Env, s.env...)
	if s.stdin != nil {
		ss.cmd.Stdin = bytes.NewReader(s.stdin)
	}
	ss.cmd.Stdout, ss.cmd.Stderr = &ss.stdout, &ss.stderr
	return ss
}

// stageFixtures makes dir a kit root holding testdata/NAME.yaml as NAME/kit.yaml.
func stageFixtures(t *testing.T, dir string, names []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "kit.yaml"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (ss *session) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(ss.logPath)
	if err != nil {
		return nil
	}
	var calls [][]string
	for _, line := range strings.Split(string(b), "\n") {
		// The fake creates the log before it writes its line, so a poll can
		// find the file empty.
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("bad call log line %q: %v", line, err)
		}
		calls = append(calls, argv)
	}
	return calls
}

func (ss *session) wait(t *testing.T) result {
	t.Helper()
	err := ss.cmd.Wait()
	res := result{stdout: ss.stdout.Bytes(), stderr: ss.stderr.String(), calls: ss.calls(t)}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("running sbx-acp: %v", err)
	}
	return res
}

func invoke(t *testing.T, s setup, args ...string) result {
	t.Helper()
	ss := newSession(t, s, args...)
	if err := ss.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return ss.wait(t)
}

func wantCode(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", r.code, code, r.stderr)
	}
}

func wantStdout(t *testing.T, r result, want []byte) {
	t.Helper()
	if !bytes.Equal(r.stdout, want) {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", r.stdout, want, r.stderr)
	}
}

func wantArgv(t *testing.T, call []string, want ...string) {
	t.Helper()
	if strings.Join(call, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("sbx argv = %q, want %q", call, want)
	}
}

func wantStderr(t *testing.T, r result, frags ...string) {
	t.Helper()
	for _, frag := range frags {
		if !strings.Contains(r.stderr, frag) {
			t.Errorf("stderr %q does not contain %q", r.stderr, frag)
		}
	}
}

// startAndSignal starts sbx-acp, waits until the fake sbx has logged calls
// calls, and sends SIGTERM. Stdin stays open so the agent cannot finish first.
func startAndSignal(t *testing.T, s setup, calls int, args ...string) result {
	t.Helper()
	ss := newSession(t, s, args...)
	stdin, err := ss.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	// Buffers would make Wait block on pipes the orphaned agent still holds, and
	// closing stdin to release them lets the agent exit 0 before the signal lands.
	ss.cmd.Stdout, ss.cmd.Stderr = nil, nil
	if err := ss.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(ss.calls(t)) < calls; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("sbx-acp never reached sbx call %d: %q", calls, ss.calls(t))
		}
	}
	if err := ss.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	return ss.wait(t)
}
