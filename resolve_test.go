package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fixtures(t *testing.T, names ...string) []StagedKit {
	t.Helper()
	var kits []StagedKit
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", n+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		kits = append(kits, StagedKit{Stem: n, Descriptor: b})
	}
	return kits
}

func TestResolveLaunches(t *testing.T) {
	cases := []struct {
		name  string
		kits  []string
		agent string
		want  Launch
	}{
		{
			name: "real claude workload plus claude-acp mixin",
			kits: []string{"claude", "claude-acp"},
			want: Launch{Agent: "claude", Argv: []string{"claude-agent-acp"}, ProtocolVersion: 1, DeclaredBy: "claude-acp"},
		},
		{
			name: "string command runs through sh -c",
			kits: []string{"shstring"},
			want: Launch{Agent: "shstring", Argv: []string{"sh", "-c", `printf "from-sh:%s" "$((6*7))"`}, ProtocolVersion: 1, DeclaredBy: "shstring"},
		},
		{
			name: "env is carried and empty values are kept",
			kits: []string{"envagent"},
			want: Launch{
				Agent:           "envagent",
				Argv:            []string{"sh", "-c", `printf "%s|%s" "$GREETING" "$EMPTY"`},
				Env:             map[string]string{"GREETING": "hello", "EMPTY": ""},
				ProtocolVersion: 1,
				DeclaredBy:      "envagent",
			},
		},
		{
			name: "identical restatements collapse across spellings of agent, env, and default version",
			kits: []string{"echo", "echo-restated", "echo-qualified", "echo-explicit-v1"},
			want: Launch{Agent: "echo", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "echo, echo-explicit-v1, echo-qualified, echo-restated"},
		},
		{
			name: "declared protocol version is reported, not enforced",
			kits: []string{"proto2"},
			want: Launch{Agent: "proto2", Argv: []string{"cat"}, ProtocolVersion: 2, DeclaredBy: "proto2"},
		},
		{
			name: "entry inside a capability group counts",
			kits: []string{"grouped"},
			want: Launch{Agent: "grouped", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "grouped"},
		},
		{
			name: "entry for an agent nobody provides is not applicable",
			kits: []string{"orphan", "echo"},
			want: Launch{Agent: "echo", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "echo"},
		},
		{
			name:  "--agent picks one of two",
			kits:  []string{"echo", "second"},
			agent: "second",
			want:  Launch{Agent: "second", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "second"},
		},
		{
			name:  "--agent accepts the namespace-qualified spelling",
			kits:  []string{"echo", "second"},
			agent: "com.docker.kit/second",
			want:  Launch{Agent: "second", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "second"},
		},
		{
			name:  "--agent selects a working agent beside a broken one",
			kits:  []string{"echo", "argagent"},
			agent: "echo",
			want:  Launch{Agent: "echo", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "echo"},
		},
		{
			name: "an undecodable kit does not stop an unrelated agent",
			kits: []string{"broken", "echo"},
			want: Launch{Agent: "echo", Argv: []string{"cat"}, ProtocolVersion: 1, DeclaredBy: "echo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Resolve(fixtures(t, tc.kits...), tc.agent)
			if err != nil {
				t.Fatalf("Resolve returned an error: %v", err)
			}
			if got.Agent != tc.want.Agent || got.ProtocolVersion != tc.want.ProtocolVersion || got.DeclaredBy != tc.want.DeclaredBy ||
				!slices.Equal(got.Argv, tc.want.Argv) || !maps.Equal(got.Env, tc.want.Env) {
				t.Errorf("launch = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveRefusals(t *testing.T) {
	cases := []struct {
		name  string
		kits  []string
		agent string
		want  []string
	}{
		{"real claude kit alone declares nothing", []string{"claude"}, "", []string{"does not declare com.docker.sandbox/acp@1"}},
		{"real claude-acp mixin alone names a provider that is absent", []string{"claude-acp"}, "", []string{"claude-acp", "claude", "no kit", "not applicable"}},
		{"declared agent that nothing provides is named when it is the reason", []string{"orphan"}, "", []string{"orphan", "ghosty", "no kit"}},
		{"--agent for an agent that nothing provides", []string{"orphan", "echo"}, "ghosty", []string{"orphan", "ghosty", "not applicable"}},
		{"two agents without --agent", []string{"echo", "second"}, "", []string{"echo", "second", "--agent"}},
		{"unknown --agent lists the candidates", []string{"echo", "second"}, "nope", []string{"nope", "echo", "second"}},
		{"broken agent still counts toward ambiguity", []string{"echo", "argagent"}, "", []string{"echo", "argagent", "--agent"}},
		{"different configs for one agent conflict", []string{"echo", "echo-other"}, "", []string{"echo", "echo-other", "different", "conflict"}},
		{"kit argument in argv is unresolved", []string{"argagent"}, "", []string{"argagent", "kit.args.model", "create-time"}},
		{"kit env reference in env value is unresolved", []string{"envarg"}, "", []string{"envarg", "kit.env.HOME"}},
		{"blank argv[0]", []string{"blankargv"}, "", []string{"blankargv", "command must name a command"}},
		{"empty argv", []string{"emptyargv"}, "", []string{"emptyargv", "command must name a command"}},
		{"versioned agent name names the kit that held it", []string{"versioned"}, "", []string{"does not declare", "versioned"}},
		{"undecodable kit is named in the no-agent error", []string{"broken"}, "", []string{"does not declare", "broken"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Resolve(fixtures(t, tc.kits...), tc.agent)
			if err == nil {
				t.Fatalf("Resolve returned %+v, want an error", got)
			}
			for _, frag := range tc.want {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("error %q does not contain %q", err, frag)
				}
			}
		})
	}
}

func TestResolveWarnsAboutSkippedKits(t *testing.T) {
	_, warnings, err := Resolve(fixtures(t, "broken", "echo"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "broken") || !strings.Contains(warnings[0], "bogusField") {
		t.Errorf("warnings = %q, want one naming kit broken and its decode error", warnings)
	}
}
