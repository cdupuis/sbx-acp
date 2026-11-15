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
	"runtime"
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
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
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

func withFake(flags ...string) []string {
	return append(append([]string{"attach", "--sbx", fakeSbx}, flags...), "box")
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

func TestStreamIsByteIdentical(t *testing.T) {
	payload := []byte("{\"jsonrpc\":\"2.0\"}\n\x00\xff\r\n\x00\x00{\"id\":2}")
	payload = append(payload, bytes.Repeat([]byte("0123456789abcdef\n\x00"), 20000)...)
	r := invoke(t, setup{kits: []string{"echo"}, stdin: payload}, withFake()...)
	wantCode(t, r, 0)
	wantStdout(t, r, payload)
	if len(r.calls) != 2 {
		t.Fatalf("sbx was called %d times, want 2 (one read, one stream): %q", len(r.calls), r.calls)
	}
	if r.calls[0][1] != "box" || r.calls[0][2] != "sh" {
		t.Errorf("first sbx call = %q, want exec box sh ...", r.calls[0])
	}
	for _, a := range r.calls[0] {
		if a == "-i" || a == "-t" {
			t.Errorf("the read must not forward stdin or allocate a TTY: %q", r.calls[0])
		}
	}
	wantArgv(t, r.calls[1], "exec", "-i", "box", "cat")
}

func TestStreamExitCodePassesThrough(t *testing.T) {
	r := invoke(t, setup{kits: []string{"exiter"}, stdin: []byte("hello")}, withFake()...)
	wantCode(t, r, 7)
	wantStdout(t, r, []byte("hello"))
	wantArgv(t, r.calls[1], "exec", "-i", "box", "sh", "-c", "cat; exit 7")
}

func TestDeclaredEnvReachesTheAgent(t *testing.T) {
	r := invoke(t, setup{kits: []string{"envagent"}}, withFake()...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("hello|"))
	wantArgv(t, r.calls[1], "exec", "-i", "-e", "EMPTY=", "-e", "GREETING=hello", "box",
		"sh", "-c", `printf "%s|%s" "$GREETING" "$EMPTY"`)
}

func TestStringCommandRunsThroughSh(t *testing.T) {
	r := invoke(t, setup{kits: []string{"shstring"}}, withFake()...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("from-sh:42"))
	wantArgv(t, r.calls[1], "exec", "-i", "box", "sh", "-c", `printf "from-sh:%s" "$((6*7))"`)
}

func TestAgentFlagSelectsOneOfTwo(t *testing.T) {
	r := invoke(t, setup{kits: []string{"echo", "second"}, stdin: []byte("ping")}, withFake("--agent", "second")...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("ping"))
}

func TestDefaultSbxIsFoundOnPath(t *testing.T) {
	r := invoke(t, setup{kits: []string{"echo"}, stdin: []byte("ping"), path: true}, "attach", "box")
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("ping"))
}

func TestUndecodableKitWarnsAndDoesNotStopTheAgent(t *testing.T) {
	r := invoke(t, setup{kits: []string{"broken", "echo"}, stdin: []byte("ping")}, withFake()...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("ping"))
	if !strings.Contains(r.stderr, "warning") || !strings.Contains(r.stderr, "broken") {
		t.Errorf("stderr = %q, want a warning naming kit broken", r.stderr)
	}
}

func TestCheckReportsOnStderrAndStartsNothing(t *testing.T) {
	r := invoke(t, setup{kits: []string{"claude", "claude-acp"}, stdin: []byte("must not be read")}, withFake("--check")...)
	wantCode(t, r, 0)
	wantStdout(t, r, nil)
	for _, frag := range []string{`sandbox "box"`, "claude", "claude-acp", "protocol version 1", "claude-agent-acp"} {
		if !strings.Contains(r.stderr, frag) {
			t.Errorf("stderr %q does not contain %q", r.stderr, frag)
		}
	}
	if len(r.calls) != 1 {
		t.Errorf("--check made %d sbx calls, want only the read: %q", len(r.calls), r.calls)
	}
}

func TestHelpWritesNothingToStdout(t *testing.T) {
	r := invoke(t, setup{}, "attach", "--help")
	wantCode(t, r, 0)
	wantStdout(t, r, nil)
	if !strings.Contains(r.stderr, "usage: sbx-acp") || !strings.Contains(r.stderr, "--check") {
		t.Errorf("stderr = %q, want the usage text", r.stderr)
	}
}

func TestUsageWithoutACommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{{nil, 2}, {[]string{"--help"}, 0}, {[]string{"-h"}, 0}, {[]string{"help"}, 0}} {
		r := invoke(t, setup{}, tc.args...)
		wantCode(t, r, tc.code)
		wantStdout(t, r, nil)
		if r.stderr != usageText {
			t.Errorf("sbx-acp %q: stderr = %q, want the usage", tc.args, r.stderr)
		}
	}
}

func TestFailures(t *testing.T) {
	cases := []struct {
		name      string
		setup     setup
		args      []string
		code      int
		stderr    []string
		sbxCalls  int
		noSandbox bool
	}{
		{"no arguments", setup{}, []string{"attach"}, 2, []string{"SANDBOX"}, 0, true},
		{"two sandboxes", setup{}, []string{"attach", "a", "b"}, 2, []string{"SANDBOX"}, 0, true},
		{"sandbox that starts with a dash", setup{}, []string{"attach", "--", "-box"}, 2, []string{"SANDBOX"}, 0, true},
		{"unknown flag", setup{}, []string{"attach", "--nope", "box"}, 2, []string{"nope", "usage"}, 0, true},
		{"sbx executable missing", setup{}, []string{"attach", "--sbx", "/nonexistent/sbx", "box"}, 125, []string{"--sbx"}, 0, false},
		{"unknown sandbox", setup{kits: []string{"echo"}}, []string{"attach", "--sbx", fakeSbx, "ghost"}, 125, []string{`sandbox "ghost"`, "could not run a command"}, 1, false},
		{"image without sh", setup{kits: []string{"echo"}, fault: "no-sh"}, withFake(), 125, []string{"sh and cat"}, 1, false},
		{"discovery stream cut inside the last descriptor", setup{kits: []string{"echo"}, fault: "truncate:2"}, withFake(), 125, []string{"ended early"}, 1, false},
		{"discovery stream cut inside the end marker", setup{kits: []string{"echo"}, fault: "truncate:1"}, withFake(), 125, []string{"ended early"}, 1, false},
		{"sandbox without staged kits", setup{}, withFake(), 3, []string{"not composed from Kits"}, 1, false},
		{"sandbox without the capability", setup{kits: []string{"claude"}}, withFake(), 3, []string{"does not declare com.docker.sandbox/acp@1"}, 1, false},
		{"agent not provided", setup{kits: []string{"claude-acp"}}, withFake(), 3, []string{"claude-acp", "no kit", "provides"}, 1, false},
		{"conflicting launches", setup{kits: []string{"echo", "echo-other"}}, withFake(), 3, []string{"echo-other", "different ACP launches"}, 1, false},
		{"unresolved kit argument", setup{kits: []string{"argagent"}}, withFake(), 3, []string{"argagent", "kit.args.model"}, 1, false},
		{"two agents without --agent", setup{kits: []string{"echo", "second"}}, withFake(), 3, []string{"echo", "second", "--agent"}, 1, false},
		{"--agent names nothing", setup{kits: []string{"echo", "second"}}, withFake("--agent", "nope"), 3, []string{"nope", "echo", "second"}, 1, false},
		{"bare sandbox name", setup{kits: []string{"echo"}}, []string{"box"}, 2, []string{`unknown command "box"`, "run: sbx-acp attach box", "usage"}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invoke(t, tc.setup, tc.args...)
			wantCode(t, r, tc.code)
			wantStdout(t, r, nil)
			if len(r.calls) != tc.sbxCalls {
				t.Errorf("sbx was called %d times, want %d: %q", len(r.calls), tc.sbxCalls, r.calls)
			}
			for _, frag := range tc.stderr {
				if !strings.Contains(r.stderr, frag) {
					t.Errorf("stderr %q does not contain %q", r.stderr, frag)
				}
			}
			prefix := "sbx-acp: sandbox "
			if tc.noSandbox {
				prefix = "sbx-acp: "
			}
			if !strings.HasPrefix(r.stderr, prefix) {
				t.Errorf("stderr %q does not start with %q", r.stderr, prefix)
			}
		})
	}
}

func TestSbxKilledBySignalExitsWith128PlusSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exit codes for signals are a Unix convention")
	}
	r := invoke(t, setup{kits: []string{"echo"}, fault: "die-by-kill"}, withFake()...)
	wantCode(t, r, 128+int(syscall.SIGKILL))
	wantStdout(t, r, nil)
}

func TestSignalIsForwardedToSbx(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal forwarding is exercised on Unix only")
	}
	ss := newSession(t, setup{kits: []string{"echo"}}, withFake()...)
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
	for deadline := time.Now().Add(10 * time.Second); len(ss.calls(t)) < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("sbx-acp never started the stream")
		}
	}
	if err := ss.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r := ss.wait(t)
	wantCode(t, r, 128+int(syscall.SIGTERM))
}

func TestRealAdapterAnswersInitialize(t *testing.T) {
	if _, err := exec.LookPath("claude-agent-acp"); err != nil {
		t.Skip("claude-agent-acp is not on PATH")
	}
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}` + "\n"
	r := invoke(t, setup{kits: []string{"claude", "claude-acp"}, stdin: []byte(req)}, withFake()...)
	wantCode(t, r, 0)
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion *int `json:"protocolVersion"`
		} `json:"result"`
	}
	line, _, _ := bytes.Cut(r.stdout, []byte("\n"))
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("first stdout line %q is not JSON: %v\nstderr: %s", line, err, r.stderr)
	}
	if resp.ID != 1 || resp.Result.ProtocolVersion == nil {
		t.Errorf("response %s lacks id 1 or protocolVersion", line)
	}
	wantArgv(t, r.calls[1], "exec", "-i", "box", "claude-agent-acp")
}
