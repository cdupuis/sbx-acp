package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDiscoverySignalStopsBeforeTheStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal forwarding is exercised on Unix only")
	}
	for _, tc := range []struct {
		name   string
		args   []string
		create bool
	}{
		{"attach", withFake(), false},
		{"attach check", withFake("--check"), false},
		{"create", createWith("--kit", echoKit, "echo"), true},
		{"create rm", createWith("--rm", "--kit", echoKit, "echo"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ss := newSession(t, setup{
				kits: []string{"echo"}, createKits: []string{"echo"},
				fault: "discovery-waits", dir: t.TempDir(),
				env: []string{"FAKE_SBX_DISCOVERY_ADDR=" + listener.Addr().String()},
			}, tc.args...)
			if err := ss.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = ss.cmd.Process.Kill()
					_ = ss.cmd.Wait()
				}
			})
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var ready [1]byte
			if _, err := io.ReadFull(conn, ready[:]); err != nil || ready[0] != 'R' {
				t.Fatalf("discovery readiness = %q, %v", ready, err)
			}
			if err := ss.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, ready[:]); err != nil || ready[0] != 'T' {
				t.Errorf("discovery did not receive SIGTERM: %q, %v", ready, err)
			}
			_ = conn.Close()
			r := ss.wait(t)
			waited = true
			wantCode(t, r, 128+int(syscall.SIGTERM))
			wantStdout(t, r, nil)
			if strings.Contains(r.stderr, "would run:") {
				t.Errorf("interrupted discovery reported a launch: %s", r.stderr)
			}
			removed := false
			for _, call := range r.calls {
				if call[0] == "rm" {
					removed = true
					wantArgv(t, call, "rm", "--force", createdName(t, r))
				}
				for _, arg := range call {
					if arg == "-i" {
						t.Errorf("interrupted discovery started the stream: %q", call)
					}
				}
			}
			if removed != tc.create {
				t.Errorf("removed sandbox = %t, want %t", removed, tc.create)
			}
		})
	}
}

func TestCreateRemovesAnUnavailableExecutable(t *testing.T) {
	for _, executable := range []string{"missing", "nonexecutable"} {
		t.Run(executable, func(t *testing.T) {
			dir := t.TempDir()
			command := filepath.Join(dir, "agent")
			if executable == "nonexecutable" {
				if runtime.GOOS == "windows" {
					t.Skip("Unix executable permissions are required")
				}
				if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ss := newSession(t, setup{createKits: []string{"echo"}, dir: dir}, createWith("--kit", echoKit, "echo")...)
			setCreateLaunch(t, ss, []string{command}, nil)
			if err := ss.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			r := ss.wait(t)
			if r.code == 0 {
				t.Error("unavailable executable exited 0")
			}
			wantStdout(t, r, nil)
			for _, call := range r.calls {
				if call[0] == "rm" {
					wantArgv(t, call, "rm", "--force", createdName(t, r))
					return
				}
			}
			t.Errorf("unavailable executable left its sandbox: %q", r.calls)
		})
	}
}

func TestCreatePreservesAnAgentsFailureStatus(t *testing.T) {
	for _, code := range []int{125, 126, 127} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			ss := newSession(t, setup{createKits: []string{"echo"}, dir: t.TempDir()}, createWith("--kit", echoKit, "echo")...)
			setCreateLaunch(t, ss, []string{"sh", "-c", fmt.Sprintf("exit %d", code)}, nil)
			if err := ss.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			r := ss.wait(t)
			wantCode(t, r, code)
			wantStdout(t, r, nil)
			for _, call := range r.calls {
				if call[0] == "rm" {
					t.Errorf("removed a sandbox after its agent ran and exited %d: %q", code, r.calls)
				}
			}
		})
	}
}

func TestCreateCanLaunchWithAPathThatHasNoShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an executable Unix symlink is required")
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	command := filepath.Join(dir, "agent")
	if err := os.Symlink(cat, command); err != nil {
		t.Fatal(err)
	}
	ss := newSession(t, setup{createKits: []string{"echo"}, dir: dir, stdin: []byte("hello")}, createWith("--rm", "--kit", echoKit, "echo")...)
	setCreateLaunch(t, ss, []string{command}, map[string]string{"PATH": dir})
	if err := ss.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := ss.wait(t)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("hello"))
}

func setCreateLaunch(t *testing.T, ss *session, command []string, env map[string]string) {
	t.Helper()
	argv, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := fmt.Sprintf("schemaVersion: \"3\"\nkind: workload\nprovides: [\"echo@1.0.0\"]\ncapabilities:\n  - type: com.docker.sandbox/acp@1\n    optional: true\n    config:\n      agent: echo\n      command: %s\n      env: %s\n", argv, environment)
	for _, env := range ss.cmd.Env {
		if dir, ok := strings.CutPrefix(env, "FAKE_SBX_TEMPLATE="); ok {
			if err := os.WriteFile(filepath.Join(dir, "echo", "kit.yaml"), []byte(descriptor), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("fake sbx has no template directory")
}
