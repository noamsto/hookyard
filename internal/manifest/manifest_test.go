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
	}, {
		name: "fire-and-forget on the canonical decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"],
		  "lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "fire-and-forget on claude-code's scoped decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["claude-code:PreToolUse"],
		  "engines":["claude-code"],"lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "fire-and-forget on cursor's scoped decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["cursor:preToolUse"],
		  "engines":["cursor"],"lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "fire-and-forget on cursor's other scoped decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["cursor:beforeShellExecution"],
		  "engines":["cursor"],"lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "fire-and-forget on codex's scoped decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["codex:PreToolUse"],
		  "engines":["codex"],"lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "fire-and-forget on pi's scoped decision event",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pi:tool_call"],
		  "engines":["pi"],"lane":"fire_and_forget"}]}`,
		want: "cannot guard this event",
	}, {
		name: "unknown lane value",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["post_tool"],"engines":["cursor"],
		  "lane":"detached"}]}`,
		want: `lane "detached" must be`,
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

// The lane's timeout_ms rule must run before the 0..4300 range check, or the
// exact manifest issue #28 quotes would be refused with "outside 0..4300", a
// message that says nothing about the lane the author just declared.
func TestLoadRejectsFireAndForgetTimeoutBeforeRangeCheck(t *testing.T) {
	body := `{"handlers":[{"id":"a","exec":"EXEC","events":["post_tool"],"engines":["cursor"],
	  "lane":"fire_and_forget","timeout_ms":30000}]}`
	_, err := Load(writeManifest(t, body))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "fire-and-forget") {
		t.Errorf("want an error mentioning the lane, got: %v", err)
	}
	if strings.Contains(err.Error(), "outside 0..4300") {
		t.Errorf("want the lane rule to fire first, got the range message instead: %v", err)
	}
}

func TestLoadAcceptsFireAndForgetOnPostToolAcrossAllEngines(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["post_tool"],
	   "engines":["claude-code","codex","cursor","pi"],"lane":"fire_and_forget"}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

// WriteTable emits "timeout_ms":0 on every entry, so an explicit zero must be
// accepted on a fire-and-forget handler: 0 already means "no override" in
// this schema and is not a claim to have bounded anything.
func TestLoadAcceptsFireAndForgetWithExplicitZeroTimeout(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["post_tool"],
	   "engines":["claude-code","codex","cursor","pi"],"lane":"fire_and_forget","timeout_ms":0}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

// An event scoped to an engine the handler does not declare is skipped by the
// lane check, exactly as validateCoverage already skips it: the handler here
// declares only cursor, so claude-code:PreToolUse is not this handler's
// problem even though it names a decision event.
func TestLoadAcceptsFireAndForgetWithDecisionEventScopedToUndeclaredEngine(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["claude-code:PreToolUse","post_tool"],
	   "engines":["cursor"],"lane":"fire_and_forget"}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadNormalizesAbsentLaneToVerdict(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"]},
	  {"id":"b","exec":"EXEC","events":["post_tool"],"engines":["codex"]}]}`)
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range m.Handlers {
		if h.Lane != LaneVerdict {
			t.Errorf("handler %q: lane = %q, want %q", h.ID, h.Lane, LaneVerdict)
		}
	}
}

func TestLaneSurvivesWriteTableReadTableRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		lane string
	}{
		{"absent lane normalizes to verdict", ""},
		{"fire_and_forget survives explicitly", LaneFireAndForget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			handlers := []Handler{
				{ID: "a", Exec: filepath.Join(dir, "guard.sh"), Events: []string{"post_tool"},
					Engines: []string{"cursor"}, Lane: tc.lane},
			}
			want := tc.lane
			if want == "" {
				want = LaneVerdict
			}

			path := filepath.Join(dir, "table.json")
			if err := WriteTable(path, handlers); err != nil {
				t.Fatal(err)
			}
			got, err := ReadTable(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d handlers, want 1", len(got))
			}
			if got[0].Lane != want {
				t.Errorf("lane = %q, want %q", got[0].Lane, want)
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

// The A9 unit half: emit's sandbox does not have the exec a manifest names,
// but LoadStatic must accept the manifest anyway because install will
// re-validate it, with a stat, at activation.
func TestLoadStaticAcceptsNonExistentExecButLoadRejectsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hookyard.json")
	body := `{"handlers":[{"id":"a","exec":"/nonexistent/guard","events":["pre_tool"],"engines":["cursor"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadStatic(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Handlers) != 1 {
		t.Fatalf("got %d handlers, want 1", len(m.Handlers))
	}

	if _, err := Load(path); err == nil {
		t.Fatal("want Load to reject the same manifest, got nil")
	}
}

// The absolute-path rule lives in validateStatic, not in the exec stat, so
// LoadStatic gives up nothing about it.
func TestLoadStaticRejectsRelativeExecLikeLoadDoes(t *testing.T) {
	body := `{"handlers":[{"id":"a","exec":"hooks/guard.sh","events":["pre_tool"],"engines":["cursor"]}]}`
	dir := t.TempDir()
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadStatic(path); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Errorf("LoadStatic: got %v, want an error about an absolute path", err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Errorf("Load: got %v, want an error about an absolute path", err)
	}
}

func TestLoadStaticRejectsDuplicateIDInsideOneManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hookyard.json")
	body := `{"handlers":[
	  {"id":"a","exec":"/nonexistent/guard","events":["pre_tool"],"engines":["cursor"]},
	  {"id":"a","exec":"/nonexistent/guard","events":["post_tool"],"engines":["cursor"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadStatic(path); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("got %v, want an error about a duplicate id", err)
	}
}

// An empty handlers array is not `install --allow-empty`, which is about a
// caller passing no --manifest at all. Were LoadStatic to accept this file the
// Nix build would succeed and home-manager activation would then fail on
// Load's refusal of the same bytes, after the store paths are realised.
func TestLoadStaticRejectsZeroHandlersLikeLoadDoes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(`{"handlers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadStatic(path); err == nil || !strings.Contains(err.Error(), "no handlers declared") {
		t.Errorf("LoadStatic: got %v, want an error about no handlers", err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "no handlers declared") {
		t.Errorf("Load: got %v, want an error about no handlers", err)
	}
}

// LoadStatic's shape composes with Merge, which is the caller's separate,
// later call that catches a duplicate id across manifests (R7) — the case
// LoadStatic itself cannot see because it only dedupes within one file.
func TestLoadStaticComposesWithMergeAcrossManifests(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	body := `{"handlers":[{"id":"shared","exec":"/nonexistent/guard","events":["pre_tool"],"engines":["cursor"]}]}`
	path1 := filepath.Join(dir1, "hookyard.json")
	path2 := filepath.Join(dir2, "hookyard.json")
	if err := os.WriteFile(path1, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path2, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := LoadStatic(path1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadStatic(path2)
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
