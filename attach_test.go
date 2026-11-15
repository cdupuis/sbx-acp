package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func withFake(flags ...string) []string {
	return append(append([]string{"attach", "--sbx", fakeSbx}, flags...), "box")
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
	if !strings.Contains(r.stderr, "sbx-acp attach") || !strings.Contains(r.stderr, "--check") {
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
		wantStderr(t, r, "Usage:", "attach", "create")
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
		{"unknown flag", setup{}, []string{"attach", "--nope", "box"}, 2, []string{"nope", "Usage:"}, 0, true},
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
		{"bare sandbox name", setup{kits: []string{"echo"}}, []string{"box"}, 2, []string{`unknown command "box"`, "run: sbx-acp attach box", "Usage:"}, 0, true},
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
	r := startAndSignal(t, setup{kits: []string{"echo"}}, 2, withFake()...)
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
