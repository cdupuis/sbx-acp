package main

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/docker/sandbox-kit-spec/v3/spec"
)

// StagedKit is one published descriptor read from /usr/share/sandbox/kit/<Stem>/kit.yaml.
type StagedKit struct {
	Stem       string
	Descriptor []byte
}

// Launch is a resolved acp@1 entry. Argv is complete and nonempty.
type Launch struct {
	Agent           string
	Argv            []string
	Env             map[string]string
	ProtocolVersion int
	DeclaredBy      string
}

type entry struct {
	kit      string
	argv     []string
	env      map[string]string
	protocol int
	// problem is empty for a launchable entry. Otherwise it is a clause that
	// completes "the ACP launch for agent X in kit Y ...".
	problem string
}

func (e entry) sameLaunch(o entry) bool {
	return slices.Equal(e.argv, o.argv) && maps.Equal(e.env, o.env) && e.protocol == o.protocol
}

type unreadKit struct{ stem, why string }

// table is everything Resolve knows about the sandbox. Every contract rule is a
// read of it: provides answers "can this agent run here", byAgent answers
// "what do the kits say about it".
type table struct {
	provides map[string]bool
	byAgent  map[string][]entry
	unread   []unreadKit
}

// Resolve applies the acp@1 contract to the staged descriptors of one sandbox.
// agent is the --agent value, or "" to require exactly one candidate.
// warnings name kits and entries that were skipped because they did not decode.
func Resolve(kits []StagedKit, agent string) (launch Launch, warnings []string, err error) {
	t := buildTable(kits)
	for _, u := range t.unread {
		warnings = append(warnings, fmt.Sprintf("kit %s was skipped: %s", u.stem, u.why))
	}
	key, err := t.choose(agent)
	if err != nil {
		return Launch{}, warnings, err
	}
	launch, err = t.launch(key)
	return launch, warnings, err
}

func buildTable(kits []StagedKit) table {
	t := table{provides: map[string]bool{}, byAgent: map[string][]entry{}}
	sorted := slices.Clone(kits)
	slices.SortStableFunc(sorted, func(a, b StagedKit) int { return strings.Compare(a.Stem, b.Stem) })
	for _, k := range sorted {
		d, err := spec.Decode(k.Descriptor)
		if err != nil {
			t.unread = append(t.unread, unreadKit{k.Stem, oneLine(err)})
			continue
		}
		for _, p := range d.Provides {
			if pr, err := spec.ParseProvide(p); err == nil {
				t.provides[pr.Name] = true
			}
		}
		for _, c := range spec.DeclaredCapabilities(d.Capabilities) {
			if c.Type != spec.CapabilityACP {
				continue
			}
			key, e := readEntry(k.Stem, c)
			if key == "" {
				t.unread = append(t.unread, unreadKit{k.Stem, "an ACP entry " + e.problem})
				continue
			}
			t.byAgent[key] = append(t.byAgent[key], e)
		}
	}
	return t
}

// readEntry returns the normalized agent key, or "" when the entry names no
// usable agent. The Kit placeholder check runs before decoding, because a
// placeholder in protocolVersion would otherwise surface as a type error and a
// placeholder in command would decode into a command that runs it literally.
func readEntry(stem string, c spec.Capability) (key string, e entry) {
	e = entry{kit: stem}
	agent, _ := c.Config["agent"].(string)
	if p, err := spec.ParseProvide(agent); err == nil && p.Version == "" && !spec.ContainsKitPlaceholder(agent) {
		key = p.Name
	}
	if ph := placeholderIn(c.Config); ph != "" {
		e.problem = fmt.Sprintf("uses %s, a create-time value sbx-acp cannot read from outside the sandbox", ph)
		return key, e
	}
	acps, err := spec.ACPsOf([]spec.Capability{c})
	if err != nil {
		e.problem = "is invalid: " + oneLine(err)
		return key, e
	}
	a := acps[0]
	e.argv, e.env, e.protocol = a.Command, a.Env, spec.ACPProtocolVersion(a.ACP)
	switch {
	case key == "":
		e.problem = "is invalid: agent must be an unversioned capability name"
	case len(e.argv) == 0 || strings.TrimSpace(e.argv[0]) == "":
		e.problem = "is invalid: command must name a command"
	}
	return key, e
}

func placeholderIn(v any) string {
	switch v := v.(type) {
	case string:
		if spec.ContainsKitPlaceholder(v) {
			return fmt.Sprintf("%q", v)
		}
	case []any:
		for _, x := range v {
			if s := placeholderIn(x); s != "" {
				return s
			}
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if s := placeholderIn(k); s != "" {
				return s
			}
			if s := placeholderIn(v[k]); s != "" {
				return s
			}
		}
	}
	return ""
}

func oneLine(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }

// candidates are the agents that some kit declares and some kit provides.
func (t table) candidates() []string {
	var out []string
	for key := range t.byAgent {
		if t.provides[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func displayAll(keys []string) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = spec.DisplayCapabilityName(k)
	}
	return strings.Join(names, ", ")
}

func (t table) declaredBy(key string) string {
	var stems []string
	for _, e := range t.byAgent[key] {
		if !slices.Contains(stems, e.kit) {
			stems = append(stems, e.kit)
		}
	}
	return strings.Join(stems, ", ")
}

// mayHold names the kits that were unreadable and so may be the ones that mattered.
func (t table) mayHold() string {
	var stems []string
	for _, u := range t.unread {
		if !slices.Contains(stems, u.stem) {
			stems = append(stems, u.stem)
		}
	}
	if len(stems) == 0 {
		return ""
	}
	return fmt.Sprintf(" (unreadable kits that may hold it: %s)", strings.Join(stems, ", "))
}

func (t table) choose(agent string) (string, error) {
	cands := t.candidates()
	if agent != "" {
		key := spec.NormalizeCapabilityName(agent)
		switch {
		case slices.Contains(cands, key):
			return key, nil
		case t.byAgent[key] != nil:
			return "", fmt.Errorf("kit %s declares an ACP launch for agent %s, but no kit in the sandbox provides %s, so it is not applicable",
				t.declaredBy(key), agent, agent)
		case len(cands) == 0:
			return "", fmt.Errorf("has no ACP agent named %s; it declares no launchable com.docker.sandbox/acp@1 agent%s", agent, t.mayHold())
		}
		return "", fmt.Errorf("has no ACP agent named %s; it declares: %s%s", agent, displayAll(cands), t.mayHold())
	}
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		if len(t.byAgent) == 0 {
			return "", fmt.Errorf("does not declare com.docker.sandbox/acp@1%s", t.mayHold())
		}
		var parts []string
		for _, key := range slices.Sorted(maps.Keys(t.byAgent)) {
			parts = append(parts, fmt.Sprintf("agent %s (kit %s)", spec.DisplayCapabilityName(key), t.declaredBy(key)))
		}
		return "", fmt.Errorf("declares com.docker.sandbox/acp@1 for %s, but no kit in the sandbox provides that agent, so it is not applicable",
			strings.Join(parts, " and "))
	}
	return "", fmt.Errorf("declares ACP agents %s; choose one with --agent", displayAll(cands))
}

func (t table) launch(key string) (Launch, error) {
	name := spec.DisplayCapabilityName(key)
	entries := t.byAgent[key]
	for _, e := range entries {
		if e.problem != "" {
			return Launch{}, fmt.Errorf("the ACP launch for agent %s in kit %s %s", name, e.kit, e.problem)
		}
	}
	first := entries[0]
	stems := []string{first.kit}
	for _, e := range entries[1:] {
		if !first.sameLaunch(e) {
			return Launch{}, fmt.Errorf("kits %s and %s declare different ACP launches for agent %s, so the sandbox composition is in conflict",
				first.kit, e.kit, name)
		}
		if !slices.Contains(stems, e.kit) {
			stems = append(stems, e.kit)
		}
	}
	return Launch{
		Agent:           name,
		Argv:            first.argv,
		Env:             first.env,
		ProtocolVersion: first.protocol,
		DeclaredBy:      strings.Join(stems, ", "),
	}, nil
}
