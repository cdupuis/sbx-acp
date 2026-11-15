package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

const echoKit = "docker.io/example/echo-acp:1"

func createWith(flags ...string) []string {
	return append([]string{"create", "--sbx", fakeSbx}, flags...)
}

var sandboxNamePattern = regexp.MustCompile(`^acp-echo-[0-9a-f]{8}$`)

// createdName returns the --name the fake sbx create received.
func createdName(t *testing.T, r result) string {
	t.Helper()
	if len(r.calls) == 0 || len(r.calls[0]) < 4 || r.calls[0][0] != "create" || r.calls[0][2] != "--name" {
		t.Fatalf("first sbx call = %q, want create --no-progress --name NAME ...\nstderr: %s", r.calls, r.stderr)
	}
	return r.calls[0][3]
}

// realTempDir is a temporary directory under the path the kernel reports for
// it. On macOS the default one is reached through the /var symlink.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateStreamsTheAgentInANewSandbox(t *testing.T) {
	wd := realTempDir(t)
	r := invoke(t, setup{createKits: []string{"echo"}, stdin: []byte("ping"), dir: wd}, createWith("--kit", echoKit, "echo")...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("ping"))
	name := createdName(t, r)
	if !sandboxNamePattern.MatchString(name) {
		t.Errorf("sandbox name %q does not match %s", name, sandboxNamePattern)
	}
	if len(r.calls) != 3 {
		t.Fatalf("sbx was called %d times, want create, read, stream and no rm: %q", len(r.calls), r.calls)
	}
	wantArgv(t, r.calls[0], "create", "--no-progress", "--name", name, "--kit", echoKit, "echo", wd)
	if r.calls[1][1] != name || r.calls[1][2] != "sh" {
		t.Errorf("second sbx call = %q, want the kit read in sandbox %s", r.calls[1], name)
	}
	wantArgv(t, r.calls[2], "exec", "-i", name, "cat")
	wantStderr(t, r, `sbx-acp: agent "echo": creating sandbox "`+name+`"`, "Created sandbox "+name)
}

func TestCreateCheckPrintsAFreshPlanAndCallsNothing(t *testing.T) {
	plan := regexp.MustCompile(`would run: \S+ create --no-progress --name (acp-claude-[0-9a-f]{8}) --kit ` + regexp.QuoteMeta(echoKit) + ` claude /\S+\n`)
	var names []string
	for range 2 {
		r := invoke(t, setup{dir: t.TempDir()}, createWith("--check", "--kit", echoKit, "Claude")...)
		wantCode(t, r, 0)
		wantStdout(t, r, nil)
		if len(r.calls) != 0 {
			t.Errorf("--check called sbx: %q", r.calls)
		}
		wantStderr(t, r, `sbx-acp: agent "claude": mixins from --kit`)
		m := plan.FindStringSubmatch(r.stderr)
		if m == nil {
			t.Fatalf("stderr %q has no would-run line matching %s", r.stderr, plan)
		}
		names = append(names, m[1])
	}
	if names[0] == names[1] {
		t.Errorf("two runs chose the same sandbox name %q", names[0])
	}
}

func TestCreateWithRmRemovesTheSandboxAfterTheSession(t *testing.T) {
	r := invoke(t, setup{createKits: []string{"echo"}, stdin: []byte("ping"), dir: t.TempDir()}, createWith("--rm", "--kit", echoKit, "echo")...)
	wantCode(t, r, 0)
	wantStdout(t, r, []byte("ping"))
	name := createdName(t, r)
	if len(r.calls) != 4 {
		t.Fatalf("sbx was called %d times, want create, read, stream, rm: %q", len(r.calls), r.calls)
	}
	wantArgv(t, r.calls[3], "rm", "--force", name)
}

func TestCreatePassesPathsVerbatim(t *testing.T) {
	r := invoke(t, setup{createKits: []string{"echo"}, dir: t.TempDir()}, createWith("--kit", echoKit, "echo", "/src/app:/work:ro", "rel/dir")...)
	wantCode(t, r, 0)
	wantArgv(t, r.calls[0], "create", "--no-progress", "--name", createdName(t, r), "--kit", echoKit, "echo", "/src/app:/work:ro", "rel/dir")
}

func TestCreateUsageErrors(t *testing.T) {
	home := realTempDir(t)
	homeLink := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, homeLink); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		setup  setup
		args   []string
		stderr string
	}{
		{"working directory is the filesystem root", setup{dir: "/"}, createWith("--kit", echoKit, "echo"),
			`sbx-acp: agent "echo": refusing to mount "/" as the workspace; start sbx-acp in the project directory or pass a PATH`},
		{"working directory is home", setup{dir: home, env: []string{"HOME=" + home}}, createWith("--kit", echoKit, "echo"),
			`sbx-acp: agent "echo": refusing to mount "` + home + `" as the workspace`},
		{"working directory is home through a symlink", setup{dir: home, env: []string{"HOME=" + homeLink}}, createWith("--kit", echoKit, "echo"),
			`sbx-acp: agent "echo": refusing to mount "` + home + `" as the workspace`},
		{"kit reference as the agent", setup{}, createWith("--kit", echoKit, "sbx/claude:latest"),
			`sbx-acp: agent "sbx/claude:latest": is not an agent name; use a built-in agent such as claude, codex, or opencode`},
		{"PATH that starts with a dash", setup{}, createWith("--kit", echoKit, "echo", "-v"), `"-v"`},
		{"no agent", setup{}, createWith("--kit", echoKit), "AGENT"},
		{"unknown flag", setup{}, createWith("--nope", "echo"), "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invoke(t, tc.setup, tc.args...)
			wantCode(t, r, 2)
			wantStdout(t, r, nil)
			wantStderr(t, r, tc.stderr)
			if len(r.calls) != 0 {
				t.Errorf("a usage error called sbx: %q", r.calls)
			}
		})
	}
}

func TestCreateFailureNamesTheSandboxAndRemovesNothing(t *testing.T) {
	r := invoke(t, setup{createKits: []string{"echo"}, fault: "create-fails", dir: t.TempDir()}, createWith("--rm", "--kit", echoKit, "echo")...)
	wantCode(t, r, 125)
	wantStdout(t, r, nil)
	name := createdName(t, r)
	wantStderr(t, r, "error: the daemon refused the create",
		`sbx-acp: sandbox "`+name+`": sbx create failed (exit 1); if the sandbox exists, remove it with: sbx rm `+name)
	if len(r.calls) != 1 {
		t.Errorf("after a failed create sbx was called again: %q", r.calls)
	}
}

func TestCreateRemovesASandboxThatNeverStartedAnAgent(t *testing.T) {
	cases := []struct {
		name   string
		setup  setup
		agent  string
		code   int
		stderr []string
	}{
		{"no ACP launch", setup{createKits: []string{"claude"}}, "claude", 3,
			[]string{"has no ACP agent named claude", `": removed, because it never started an ACP agent` + "\n"}},
		{"unreadable kits", setup{createKits: []string{"echo"}, fault: "no-sh"}, "echo", 125,
			[]string{"sh and cat", `": removed, because it never started an ACP agent` + "\n"}},
		{"removal fails", setup{createKits: []string{"claude"}, fault: "rm-fails"}, "claude", 3,
			[]string{"has no ACP agent named claude", "error: the daemon refused the removal", `": warning: could not remove it; run: sbx rm acp-claude-`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup.dir = t.TempDir()
			r := invoke(t, tc.setup, createWith("--kit", echoKit, tc.agent)...)
			wantCode(t, r, tc.code)
			wantStdout(t, r, nil)
			wantStderr(t, r, tc.stderr...)
			if len(r.calls) != 3 {
				t.Fatalf("sbx was called %d times, want create, read, rm: %q", len(r.calls), r.calls)
			}
			wantArgv(t, r.calls[2], "rm", "--force", createdName(t, r))
		})
	}
}

func TestCreateSignalMidStreamStillRemovesWithRm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal forwarding is exercised on Unix only")
	}
	r := startAndSignal(t, setup{createKits: []string{"echo"}, dir: t.TempDir()}, 3, createWith("--rm", "--kit", echoKit, "echo")...)
	wantCode(t, r, 128+int(syscall.SIGTERM))
	if len(r.calls) != 4 {
		t.Fatalf("sbx was called %d times, want create, read, stream, rm: %q", len(r.calls), r.calls)
	}
	wantArgv(t, r.calls[3], "rm", "--force", createdName(t, r))
}

func TestCreateSignalDuringCreateIsForwardedAndRemovesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal forwarding is exercised on Unix only")
	}
	r := startAndSignal(t, setup{createKits: []string{"echo"}, fault: "create-sleeps", dir: t.TempDir()}, 1, createWith("--rm", "--kit", echoKit, "echo")...)
	wantCode(t, r, 128+int(syscall.SIGTERM))
	if len(r.calls) != 1 {
		t.Errorf("after a signal during create sbx was called again: %q", r.calls)
	}
}

// useHub points the create command at h for the rest of the test.
func useHub(t *testing.T, h *fakeHub) *httptest.Server {
	t.Helper()
	c, srv := h.start(t)
	saved := dockerHub
	dockerHub = c
	t.Cleanup(func() { dockerHub = saved })
	return srv
}

func TestCreateCheckUsesTheLookup(t *testing.T) {
	useHub(t, &fakeHub{list: listing(t), manifests: realManifests(t)})
	var stderr bytes.Buffer
	code := run([]string{"create", "--sbx", fakeSbx, "--check", "claude"}, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	for _, frag := range []string{
		`sbx-acp: agent "claude": ACP mixin sbx/claude-acp:latest, found on Docker Hub` + "\n",
		" create --no-progress --name acp-claude-", " --kit sbx/claude-acp:latest claude ",
	} {
		if !strings.Contains(stderr.String(), frag) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), frag)
		}
	}
}

func TestCreateExitsFourWhenTheCatalogIsDown(t *testing.T) {
	srv := useHub(t, &fakeHub{})
	srv.Close()
	var stderr bytes.Buffer
	code := run([]string{"create", "--sbx", fakeSbx, "--check", "claude"}, &stderr)
	if code != 4 {
		t.Fatalf("exit code = %d, want 4\nstderr: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), `sbx-acp: agent "claude": cannot read the kit catalog on Docker Hub (`) ||
		!strings.HasSuffix(stderr.String(), "); pass --kit REF to name the ACP mixin yourself\n") {
		t.Errorf("stderr = %q, want the catalog sentence", stderr.String())
	}
}

func TestCreateWithoutAMixinSuggestsKitWhenNoAgentStarts(t *testing.T) {
	h := &fakeHub{list: listing(t), manifests: realManifests(t)}
	useHub(t, h)
	tmp := t.TempDir()
	root, template, log := filepath.Join(tmp, "sandboxes"), filepath.Join(tmp, "template"), filepath.Join(tmp, "calls.jsonl")
	stageFixtures(t, template, []string{"claude"})
	t.Setenv("FAKE_SBX_ROOT", root)
	t.Setenv("FAKE_SBX_TEMPLATE", template)
	t.Setenv("FAKE_SBX_LOG", log)
	t.Setenv("FAKE_SBX_FAULT", "")
	var stderr bytes.Buffer
	code := run([]string{"create", "--sbx", fakeSbx, "opencode", tmp}, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `": removed, because it never started an ACP agent; pass --kit REF to add an ACP mixin`+"\n") {
		t.Errorf("stderr %q lacks the removal sentence with the --kit hint", stderr.String())
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), `["rm","--force","acp-opencode-`) {
		t.Errorf("sbx calls %s include no rm of the created sandbox", b)
	}
}
