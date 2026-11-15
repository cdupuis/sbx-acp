package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeHub serves the Docker Hub repository list, the token service, and the
// registry from one test server, and records every request.
type fakeHub struct {
	list      string
	listCode  int
	manifests map[string]string

	mu       sync.Mutex
	requests []string
}

func (h *fakeHub) start(t *testing.T) (catalog, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.URL.RequestURI())
		h.mu.Unlock()
		q := r.URL.Query()
		repo, isManifest := strings.CutPrefix(r.URL.Path, "/v2/sbx/")
		repo, isManifest = strings.CutSuffix(repo, "/manifests/latest")
		switch {
		case r.URL.Path == "/v2/namespaces/sbx/repositories" && q.Get("name") == "acp" && q.Get("page_size") == "100":
			if h.listCode != 0 {
				w.WriteHeader(h.listCode)
			}
			w.Write([]byte(h.list))
		case r.URL.Path == "/token" && q.Get("service") == "registry.docker.io" && strings.HasPrefix(q.Get("scope"), "repository:sbx/"):
			w.Write([]byte(`{"token":"tok-` + q.Get("scope") + `"}`))
		case isManifest && r.Header.Get("Authorization") == "Bearer tok-repository:sbx/"+repo+":pull" &&
			strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json"):
			body, ok := h.manifests[repo]
			if !ok {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Write([]byte(body))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := dockerHub
	c.hub, c.auth, c.registry = srv.URL, srv.URL, srv.URL
	return c, srv
}

func (h *fakeHub) fetched() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var repos []string
	for _, r := range h.requests {
		if repo, ok := strings.CutPrefix(r, "/v2/sbx/"); ok {
			repos = append(repos, strings.TrimSuffix(repo, "/manifests/latest"))
		}
	}
	return repos
}

func readHubFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hub", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listing is the real sbx namespace listing plus v3 kit repositories named extra.
func listing(t *testing.T, extra ...string) string {
	t.Helper()
	var l map[string]any
	if err := json.Unmarshal([]byte(readHubFixture(t, "repositories.json")), &l); err != nil {
		t.Fatal(err)
	}
	results := l["results"].([]any)
	for _, name := range extra {
		results = append(results, map[string]any{"name": name, "namespace": "sbx", "media_types": []string{kitMediaType}})
	}
	l["results"] = results
	b, _ := json.Marshal(l)
	return string(b)
}

// manifest is a real index fixture with each old string in the raw file replaced.
func manifest(t *testing.T, repo string, oldNew ...string) string {
	t.Helper()
	return strings.NewReplacer(oldNew...).Replace(readHubFixture(t, repo+".manifest.json"))
}

func realManifests(t *testing.T) map[string]string {
	return map[string]string{"claude-acp": manifest(t, "claude-acp"), "codex-acp": manifest(t, "codex-acp")}
}

func TestLookup(t *testing.T) {
	cases := []struct {
		name     string
		agent    string
		extra    []string
		edit     func(map[string]string)
		ref      string
		fetched  []string
		warnings []string
	}{
		{name: "claude gets its adapter mixin and the older-format repository is not fetched", agent: "claude",
			ref: "sbx/claude-acp:latest", fetched: []string{"claude-acp"}},
		{name: "codex gets its adapter mixin", agent: "codex",
			ref: "sbx/codex-acp:latest", fetched: []string{"codex-acp"}},
		{name: "opencode declares ACP itself and gets none", agent: "opencode"},
		{name: "a repository without a kit descriptor is skipped", agent: "claude", extra: []string{"claude-acp-old"},
			edit: func(m map[string]string) { m["claude-acp-old"] = `{"schemaVersion":2,"manifests":[]}` },
			ref:  "sbx/claude-acp:latest", fetched: []string{"claude-acp", "claude-acp-old"}},
		{name: "a workload kit is not selected", agent: "claude", extra: []string{"claude-acp-workload"},
			edit: func(m map[string]string) {
				m["claude-acp-workload"] = manifest(t, "claude-acp", `\"kind\":\"mixin\"`, `\"kind\":\"workload\"`)
			},
			ref: "sbx/claude-acp:latest", fetched: []string{"claude-acp", "claude-acp-workload"}},
		{name: "an entry for another agent is not selected", agent: "claude", extra: []string{"claude-acp-other"},
			edit: func(m map[string]string) {
				m["claude-acp-other"] = manifest(t, "claude-acp", `\"agent\":\"claude\"`, `\"agent\":\"codex\"`)
			},
			ref: "sbx/claude-acp:latest", fetched: []string{"claude-acp", "claude-acp-other"}},
		{name: "a mixin whose entry cannot be launched is not selected", agent: "claude", extra: []string{"claude-acp-empty"},
			edit: func(m map[string]string) {
				m["claude-acp-empty"] = manifest(t, "claude-acp", `\"command\":[\"claude-agent-acp\"]`, `\"command\":[\"\"]`)
			},
			ref: "sbx/claude-acp:latest", fetched: []string{"claude-acp", "claude-acp-empty"}},
		{name: "an unreadable manifest is a warning", agent: "codex",
			edit:     func(m map[string]string) { delete(m, "codex-acp") },
			fetched:  []string{"codex-acp"},
			warnings: []string{"kit sbx/codex-acp was skipped", "500"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHub{list: listing(t, tc.extra...), manifests: realManifests(t)}
			if tc.edit != nil {
				tc.edit(h.manifests)
			}
			c, _ := h.start(t)
			published, err := c.Published(context.Background(), mixinNamespace, tc.agent)
			if err != nil {
				t.Fatalf("Published: %v", err)
			}
			ref, warnings, err := ACPMixin(tc.agent, published)
			if err != nil {
				t.Fatalf("ACPMixin: %v", err)
			}
			if ref != tc.ref {
				t.Errorf("ref = %q, want %q", ref, tc.ref)
			}
			if got := h.fetched(); strings.Join(got, ",") != strings.Join(tc.fetched, ",") {
				t.Errorf("fetched manifests of %q, want %q", got, tc.fetched)
			}
			if len(warnings) != min(len(tc.warnings), 1) {
				t.Errorf("warnings = %q, want %d", warnings, min(len(tc.warnings), 1))
			}
			for _, frag := range tc.warnings {
				if len(warnings) == 0 || !strings.Contains(warnings[0], frag) {
					t.Errorf("warnings %q do not contain %q", warnings, frag)
				}
			}
			for _, r := range h.requests {
				if !strings.HasPrefix(r, "/v2/namespaces/sbx/repositories?") && !strings.HasPrefix(r, "/v2/sbx/") &&
					!strings.Contains(r, "scope=repository%3Asbx%2F") {
					t.Errorf("request %q leaves the sbx namespace", r)
				}
			}
		})
	}
}

func TestLookupTwoMixinsIsAnError(t *testing.T) {
	h := &fakeHub{list: listing(t, "claude-acp-twin"), manifests: realManifests(t)}
	h.manifests["claude-acp-twin"] = h.manifests["claude-acp"]
	c, _ := h.start(t)
	published, err := c.Published(context.Background(), mixinNamespace, "claude")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ACPMixin("claude", published)
	want := "mixins sbx/claude-acp and sbx/claude-acp-twin both declare an ACP launch for it; choose one with --kit REF"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestLookupListFailures(t *testing.T) {
	cases := []struct {
		name string
		hub  *fakeHub
		want string
	}{
		{"server error", &fakeHub{list: "{}", listCode: http.StatusInternalServerError}, "500"},
		{"a second page", &fakeHub{list: `{"count":101,"next":"https://hub.docker.com/v2/namespaces/sbx/repositories?page=2","results":[]}`}, "more than one page"},
		{"undecodable body", &fakeHub{list: `<html>`}, "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := tc.hub.start(t)
			_, err := c.Published(context.Background(), mixinNamespace, "claude")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
