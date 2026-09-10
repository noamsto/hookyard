package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest writes a manifest whose exec points at a real executable, so
// tests exercise the rule under test rather than the exec check.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	exec := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "EXEC", exec)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsAValidManifest(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"aeye/images","exec":"EXEC","events":["post_tool"],
	   "engines":["claude-code","codex","cursor"],"match":["Read","Write","Bash"],"timeout_ms":1500}]}`)
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Handlers) != 1 {
		t.Fatalf("want 1 handler, got %d", len(m.Handlers))
	}
}

// Unknown fields are ignored so a manifest written against a newer hookyard
// degrades instead of failing the whole install.
func TestLoadIgnoresUnknownFields(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"],
	   "match":["Bash"],"future_field":{"nested":true}}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{{
		// Cursor has no Glob equivalent and drops the matcher silently, so the
		// handler would be registered and never fire.
		name: "match that renders empty for a claimed engine",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"],"match":["Glob"]}]}`,
		want: "renders empty for cursor",
	}, {
		name: "engine-native matcher in the normalized field",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"],"match":["Shell"]}]}`,
		want: "not a normalized tool name",
	}, {
		name: "timeout above the router sub-budget",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"],"timeout_ms":9000}]}`,
		want: "outside 0..4300",
	}, {
		name: "exec that is not executable",
		body: `{"handlers":[{"id":"a","exec":"/nonexistent/guard","events":["pre_tool"],"engines":["cursor"]}]}`,
		want: "exec",
	}, {
		name: "unknown engine",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["emacs"]}]}`,
		want: "unknown engine",
	}, {
		name: "event that is neither canonical nor engine-scoped",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["whenever"],"engines":["cursor"]}]}`,
		want: "neither a canonical event",
	}, {
		// An engine-scoped native name reaches a TOML table header verbatim.
		name: "engine-scoped event name that could break out of a TOML header",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["codex:Pre\"]]\ninjected = 1"],"engines":["codex"]}]}`,
		want: "must match",
	}, {
		name: "duplicate id inside one manifest",
		body: `{"handlers":[
		  {"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"]},
		  {"id":"a","exec":"EXEC","events":["post_tool"],"engines":["cursor"]}]}`,
		want: "declared twice",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, tc.body))
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// A handler may subscribe to an event scoped to an engine it also claims
// elsewhere; the scoped event simply does not apply to the other engines.
func TestLoadAcceptsEngineScopedEventAlongsideOtherEngines(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["cursor:beforeShellExecution"],
	   "engines":["cursor","codex"],"match":["Bash"]}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestMergeRejectsDuplicateIDAcrossManifests(t *testing.T) {
	body := `{"handlers":[{"id":"shared","exec":"EXEC","events":["pre_tool"],"engines":["cursor"]}]}`
	first, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Merge([]*Manifest{first, second})
	if err == nil {
		t.Fatal("want an error for a duplicate id across manifests, got nil")
	}
	for _, want := range []string{first.Source, second.Source} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name both manifests, missing %q: %v", want, err)
		}
	}
}
