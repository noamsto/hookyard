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
		// Would resolve at hook-fire time against whatever directory the
		// agent's tool call runs in, not the installer's.
		name: "exec that is a relative path",
		body: `{"handlers":[{"id":"a","exec":"hooks/guard.sh","events":["pre_tool"],"engines":["cursor"]}]}`,
		want: "must be an absolute path",
	}, {
		// A bare name goes through LookPath, resolving from the hook process's
		// PATH — the very thing §9 refuses to trust.
		name: "exec that is a bare name",
		body: `{"handlers":[{"id":"a","exec":"guard.sh","events":["pre_tool"],"engines":["cursor"]}]}`,
		want: "must be an absolute path",
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

func TestWriteTableThenReadTableRoundTrips(t *testing.T) {
	dir := t.TempDir()
	exec := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "table.json")
	handlers := []Handler{
		{ID: "a", Exec: exec, Events: []string{"pre_tool"}, Engines: []string{"cursor"}, Match: []string{"Bash"}},
		{ID: "b", Exec: exec, Events: []string{"post_tool"}, Engines: []string{"claude-code"}},
	}

	if err := WriteTable(path, handlers); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(handlers) {
		t.Fatalf("got %d handlers, want %d", len(got), len(handlers))
	}
	for i, h := range handlers {
		if got[i].ID != h.ID {
			t.Errorf("handler %d: id = %q, want %q", i, got[i].ID, h.ID)
		}
	}
}

func TestReadTableRejectsEngineNativeMatcherInMatchField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.json")
	body := `{"handlers":[{"id":"a","exec":"/nonexistent","events":["pre_tool"],"engines":["cursor"],"match":["Shell"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadTable(path)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not a normalized tool name") {
		t.Errorf("got %v, want an error mentioning normalized tool name", err)
	}
}

// ReadTable is the hook-time reader, so it is where a non-absolute exec has to
// be caught: an install-time stat resolves against the installer's directory,
// but the router runs the handler from the agent's.
func TestReadTableRejectsNonAbsoluteExec(t *testing.T) {
	for _, exec := range []string{"hooks/guard.sh", "guard.sh", "./guard.sh", ""} {
		t.Run(exec, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "table.json")
			body := `{"handlers":[{"id":"a","exec":"` + exec + `","events":["pre_tool"],"engines":["cursor"]}]}`
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := ReadTable(path)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "must be an absolute path") {
				t.Errorf("got %v, want an error about an absolute path", err)
			}
		})
	}
}

// The counterpart: an absolute exec is accepted at hook time even though
// ReadTable never stats it.
func TestReadTableAcceptsAbsoluteExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.json")
	body := `{"handlers":[{"id":"a","exec":"/opt/hookyard/guard.sh","events":["pre_tool"],"engines":["cursor"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d handlers, want 1", len(got))
	}
}

func TestReadTableAcceptsZeroHandlers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.json")
	if err := os.WriteFile(path, []byte(`{"handlers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d handlers, want 0", len(got))
	}
}

// ReadTable does not re-stat exec, so a table naming a handler hookyard could
// never run is still accepted; Load is what catches that, at merge time.
func TestReadTableAcceptsNonExistentExecButLoadRejectsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.json")
	body := `{"handlers":[{"id":"a","exec":"/nonexistent/guard","events":["pre_tool"],"engines":["cursor"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d handlers, want 1", len(got))
	}

	if _, err := Load(path); err == nil {
		t.Fatal("want Load to reject the same table, got nil")
	}
}

func TestWriteTableLandsFixed0600EvenOverALooserExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.json")
	if err := os.WriteFile(path, []byte(`{"handlers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteTable(path, nil); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
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
