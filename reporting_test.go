package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type reportWriter func([]byte) (int, error)

func (w reportWriter) Write(p []byte) (int, error) { return w(p) }

func TestAttachCheckSignalDuringReporting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal forwarding is exercised on Unix only")
	}
	for _, trigger := range []string{"warning:", "agent echo", "would run:"} {
		t.Run(trigger, func(t *testing.T) {
			ss := newSession(t, setup{kits: []string{"broken", "echo"}}, withFake("--check")...)
			for _, env := range ss.cmd.Env {
				if strings.HasPrefix(env, "FAKE_SBX_") {
					key, value, _ := strings.Cut(env, "=")
					t.Setenv(key, value)
				}
			}
			in := watchInterrupts()
			defer in.stop()
			var output bytes.Buffer
			canceled := false
			writer := reportWriter(func(p []byte) (int, error) {
				if !canceled && strings.Contains(string(p), trigger) {
					canceled = true
					self, err := os.FindProcess(os.Getpid())
					if err != nil {
						t.Fatal(err)
					}
					if err := self.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
					select {
					case <-in.ctx.Done():
					case <-time.After(10 * time.Second):
						t.Fatal("signal was not handled")
					}
					if code := in.received(); code != 143 {
						t.Fatalf("handled signal status = %d, want 143", code)
					}
				}
				return output.Write(p)
			})
			code, _ := attachTo(in, fakeSbx, "box", "", true, writer)
			if !canceled {
				t.Fatal("reporting trigger was not exercised")
			}
			if code != 143 {
				t.Errorf("canceled check exited %d, want 143", code)
			}
			if trigger != "would run:" && strings.Contains(output.String(), "would run:") {
				t.Errorf("canceled check reported a launch: %s", output.String())
			}
		})
	}
}
