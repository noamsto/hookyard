package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
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
	}, {
		name: "claude-code event outside the catalog",
		body: `{"handlers":[{"id":"a","exec":"EXEC","events":["claude-code:Notifcation"],"engines":["claude-code"]}]}`,
		want: "not a Claude Code event hookyard routes",
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

// D3: pi's turn_end decision slot (canonical, engine-scoped, or the settle
// native directly) requests a continuation rather than guarding a call, so
// HasGuardSlot excludes it and a fire-and-forget handler stays legal there —
// unlike pi's pre_tool, which still guards a call and still rejects (below).
func TestLoadAcceptsFireAndForgetOnPiTurnEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "canonical turn_end claiming pi",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["turn_end"],
			  "engines":["pi"],"lane":"fire_and_forget"}]}`,
		}, {
			name: "pi:turn_end (the per-turn native)",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pi:turn_end"],
			  "engines":["pi"],"lane":"fire_and_forget"}]}`,
		}, {
			name: "pi:agent_before_settle (the settle native)",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["pi:agent_before_settle"],
			  "engines":["pi"],"lane":"fire_and_forget"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeManifest(t, tc.body)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Regression: pi's pre_tool slot still guards a call and is unaffected by
// HasGuardSlot excluding turn_end.
func TestLoadRejectsFireAndForgetOnPiPreTool(t *testing.T) {
	body := `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],
	  "engines":["pi"],"lane":"fire_and_forget"}]}`
	_, err := Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "cannot guard this event") {
		t.Fatalf("got %v, want an error saying it cannot guard this event", err)
	}
}

// A verdict-lane handler on pi:turn_end has always been legal (the lane rule
// only ever restricts fire-and-forget); this pins that D1's remap didn't
// change that.
func TestLoadAcceptsVerdictLaneOnPiTurnEnd(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["pi:turn_end"],"engines":["pi"]}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

// §11.3: Claude Code and Codex's turn_end (canonical, or their own native
// Stop) is a decision slot but not a guard slot — same as pi's, above — so a
// fire-and-forget handler stays legal there too. This would fail if
// validateLane asked HasDecisionSlot instead of HasGuardSlot now that
// claude-code/codex have a turn_end decision slot.
func TestLoadAcceptsFireAndForgetOnClaudeAndCodexTurnEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "canonical turn_end claiming claude-code and codex",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["turn_end"],
			  "engines":["claude-code","codex"],"lane":"fire_and_forget"}]}`,
		}, {
			name: "claude-code:Stop (the native)",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["claude-code:Stop"],
			  "engines":["claude-code"],"lane":"fire_and_forget"}]}`,
		}, {
			name: "codex:Stop (the native)",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["codex:Stop"],
			  "engines":["codex"],"lane":"fire_and_forget"}]}`,
		}, {
			name: "houston-like observer on turn_end claiming all four engines",
			body: `{"handlers":[{"id":"a","exec":"EXEC","events":["turn_end"],
			  "engines":["claude-code","codex","cursor","pi"],"lane":"fire_and_forget"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeManifest(t, tc.body)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A verdict-lane handler on claude-code/codex turn_end is accepted, the same
// as pi's turn_end above.
func TestLoadAcceptsVerdictLaneOnClaudeAndCodexTurnEnd(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["turn_end"],"engines":["claude-code","codex"]}]}`)
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

// Every catalog native is accepted as "claude-code:<Native>", including the
// six canonical ones, which a manifest can also name scoped.
func TestLoadAcceptsEveryClaudeCodeCatalogNative(t *testing.T) {
	for _, row := range vocab.ClaudeCodeCatalog {
		t.Run(row.Native, func(t *testing.T) {
			path := writeManifest(t, `{"handlers":[
			  {"id":"a","exec":"EXEC","events":["claude-code:`+row.Native+`"],
			   "engines":["claude-code"]}]}`)
			if _, err := Load(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// pi:session_shutdown is PiCatalog's whole reason for existing
// beyond the six canonical natives, and pi:before_agent_start is refused by
// the same catalog rule — before_agent_start is deliberately absent from
// PiCatalog (vocab.go) rather than carrying a bespoke rejection message here.
func TestLoadAcceptsPiSessionShutdownAndRejectsBeforeAgentStart(t *testing.T) {
	path := writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["pi:session_shutdown"],"engines":["pi"]}]}`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}

	path = writeManifest(t, `{"handlers":[
	  {"id":"a","exec":"EXEC","events":["pi:before_agent_start"],"engines":["pi"]}]}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "not a Pi event hookyard routes") {
		t.Fatalf("got %v, want an error naming the Pi catalog", err)
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

// ReadTable calls validateStatic alone, not validateAll, so a table an older
// hookyard wrote before the catalog covered some event stays readable — the
// #59 carve-out — even though Load would refuse the same row today.
func TestReadTableAcceptsClaudeCodeEventOutsideCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.json")
	body := `{"handlers":[{"id":"a","exec":"/nonexistent/guard","events":["claude-code:Notifcation"],"engines":["claude-code"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d handlers, want 1", len(got))
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

// Mode gate: commands are build-mode-only. Load, LoadBuildTime and
// ReadTable all read yard-mode manifests and must refuse a non-empty commands
// rather than accept a surface yard mode has nothing to register it into.
func TestYardReadersRejectNonEmptyCommands(t *testing.T) {
	const commandsKey = `,"commands":[{"name":"aeye","description":"Open the viewer","exec":"scripts/aeye"}]`

	t.Run("Load", func(t *testing.T) {
		path := writeManifest(t, `{"handlers":[{"id":"a","exec":"EXEC","events":["pre_tool"],"engines":["cursor"]}]`+commandsKey+`}`)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), "yard mode has no command surface") {
			t.Fatalf("got %v, want an error about yard mode's command surface", err)
		}
	})

	t.Run("LoadBuildTime", func(t *testing.T) {
		path := writeFile(t, t.TempDir(), "hookyard.json",
			`{"handlers":[{"id":"a","exec":"/nonexistent","events":["pre_tool"],"engines":["cursor"]}]`+commandsKey+`}`)
		_, err := LoadBuildTime(path)
		if err == nil || !strings.Contains(err.Error(), "yard mode has no command surface") {
			t.Fatalf("got %v, want an error about yard mode's command surface", err)
		}
	})

	t.Run("ReadTable", func(t *testing.T) {
		path := writeFile(t, t.TempDir(), "table.json",
			`{"handlers":[{"id":"a","exec":"/opt/hookyard/guard.sh","events":["pre_tool"],"engines":["cursor"]}]`+commandsKey+`}`)
		_, err := ReadTable(path)
		if err == nil || !strings.Contains(err.Error(), "yard mode has no command surface") {
			t.Fatalf("got %v, want an error about yard mode's command surface", err)
		}
	})
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPluginExecForm(t *testing.T) {
	cases := []struct {
		name string
		exec string
		want string // empty means accepted
	}{
		{name: "plugin-relative", exec: "handlers/guard.sh"},
		{name: "absolute", exec: "/opt/hookyard/guard.sh", want: "must be relative to the plugin root"},
		{name: "escaping", exec: "../x", want: "must be relative to the plugin root"},
		{name: "empty", exec: "", want: "must be relative to the plugin root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "hookyard.json",
				`{"handlers":[{"id":"a","exec":"`+tc.exec+`","events":["pre_tool"],"engines":["claude-code"]}]}`)
			_, err := LoadPlugin(path)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if tc.exec != "" && tc.exec[0] == '/' && !strings.Contains(err.Error(), "hookyard install") {
				t.Errorf("error should name yard mode: %v", err)
			}
		})
	}
}

// The catalog check runs through loadStatic too, since LoadPlugin uses it.
func TestLoadPluginRejectsClaudeCodeEventOutsideCatalog(t *testing.T) {
	path := writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"a","exec":"handlers/guard.sh","events":["claude-code:Notifcation"],"engines":["claude-code"]}]}`)
	_, err := LoadPlugin(path)
	if err == nil || !strings.Contains(err.Error(), "not a Claude Code event hookyard routes") {
		t.Fatalf("got %v, want an error about the Claude Code catalog", err)
	}
}

// LoadPlugin is the one reader that accepts commands: name shape,
// non-empty description, and exec under the same ExecPluginRelative rule a
// handler's exec follows.
func TestLoadPluginCommands(t *testing.T) {
	cases := []struct {
		name    string
		command string // one commands[] entry
		want    string // empty means accepted
	}{
		{name: "valid", command: `{"name":"aeye","description":"Open the viewer","exec":"scripts/aeye"}`},
		{name: "empty name", command: `{"name":"","description":"d","exec":"scripts/aeye"}`,
			want: "name must match"},
		{name: "name with a slash reads as a nested command path", command: `{"name":"aeye/images","description":"d","exec":"scripts/aeye"}`,
			want: "name must match"},
		{name: "empty description", command: `{"name":"aeye","description":"","exec":"scripts/aeye"}`,
			want: "no description"},
		{name: "absolute exec", command: `{"name":"aeye","description":"d","exec":"/opt/aeye"}`,
			want: "must be relative to the plugin root"},
		{name: "exec escaping the plugin root", command: `{"name":"aeye","description":"d","exec":"../aeye"}`,
			want: "must be relative to the plugin root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "hookyard.json",
				`{"handlers":[{"id":"a","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"]}],`+
					`"commands":[`+tc.command+`]}`)
			m, err := LoadPlugin(path)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(m.Commands) != 1 || m.Commands[0].Name != "aeye" || m.Commands[0].Exec != "scripts/aeye" {
					t.Errorf("got %+v, want one command named aeye with exec scripts/aeye", m.Commands)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadPluginRejectsDuplicateCommandNameInOneManifest(t *testing.T) {
	path := writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"a","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"]}],
		  "commands":[
		    {"name":"aeye","description":"d","exec":"scripts/aeye"},
		    {"name":"aeye","description":"d2","exec":"scripts/aeye2"}]}`)
	_, err := LoadPlugin(path)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("got %v, want an error about a duplicate command name", err)
	}
}

// A valid commands[] round-trips through LoadPlugin and Merge, and Merge
// rejects a duplicate command name across manifests the way it already
// rejects a duplicate handler id.
func TestMergeCommands(t *testing.T) {
	first, err := LoadPlugin(writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"a","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"]}],
		  "commands":[{"name":"aeye","description":"d","exec":"scripts/aeye"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadPlugin(writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"b","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"]}],
		  "commands":[{"name":"images","description":"d","exec":"scripts/images"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Merge([]*Manifest{first, second}); err != nil {
		t.Fatalf("want distinct command names to merge cleanly, got %v", err)
	}

	third, err := LoadPlugin(writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"c","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"]}],
		  "commands":[{"name":"aeye","description":"d","exec":"scripts/aeye"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Merge([]*Manifest{first, third})
	if err == nil {
		t.Fatal("want an error for a duplicate command name across manifests, got nil")
	}
	for _, want := range []string{first.Source, third.Source} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name both manifests, missing %q: %v", want, err)
		}
	}
}

// Each mode's refusal of the other's exec form names the other mode, so a
// manifest handed to the wrong command explains itself.
func TestYardReadersRejectRelativeExecNamingBuildMode(t *testing.T) {
	path := writeFile(t, t.TempDir(), "hookyard.json",
		`{"handlers":[{"id":"a","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["cursor"]}]}`)
	readers := map[string]func(string) error{
		"Load":      func(p string) error { _, err := Load(p); return err },
		"ReadTable": func(p string) error { _, err := ReadTable(p); return err },
	}
	for name, read := range readers {
		err := read(path)
		if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Errorf("%s: got %v, want an error about an absolute path", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "hookyard build") {
			t.Errorf("%s: error should name build mode: %v", name, err)
		}
	}
}

func TestWritePluginTableThenReadPluginTableRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.json")
	want := []Handler{{
		ID: "a", Exec: "handlers/guard.sh", Events: []string{"pre_tool"},
		Engines: []string{"claude-code"}, Match: []string{"Bash"}, Lane: LaneVerdict,
	}}
	if err := WritePluginTable(path, want); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
	got, err := ReadPluginTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" || got[0].Exec != "handlers/guard.sh" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadPluginTableRejectsAbsoluteExec(t *testing.T) {
	path := writeFile(t, t.TempDir(), "table.json",
		`{"handlers":[{"id":"a","exec":"/opt/hookyard/guard.sh","events":["pre_tool"],"engines":["claude-code"]}]}`)
	if _, err := ReadPluginTable(path); err == nil || !strings.Contains(err.Error(), "must be relative to the plugin root") {
		t.Errorf("got %v, want an error about a plugin-relative exec", err)
	}
}

func TestResolvePluginExec(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "handlers"), 0o755); err != nil {
		t.Fatal(err)
	}
	guard := writeFile(t, filepath.Join(root, "handlers"), "guard.sh", "#!/bin/sh\n")
	outside := writeFile(t, t.TempDir(), "evil.sh", "#!/bin/sh\n")
	if err := os.Symlink(outside, filepath.Join(root, "handlers", "escape.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(guard, filepath.Join(root, "handlers", "inside.sh")); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	realGuard, err := filepath.EvalSymlinks(guard)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		root string
		exec string
		want string // error substring; empty means resolves to realGuard
	}{
		{name: "file", root: root, exec: "handlers/guard.sh"},
		{name: "symlink to a file inside root", root: root, exec: "handlers/inside.sh"},
		{name: "root reached through a symlink", root: linkedRoot, exec: "handlers/guard.sh"},
		{name: "missing file", root: root, exec: "handlers/gone.sh", want: "handlers/gone.sh"},
		{name: "symlink escaping root", root: root, exec: "handlers/escape.sh", want: "outside the plugin root"},
		{name: "dot-dot exec", root: root, exec: "../evil.sh", want: "no .. segments"},
		{name: "relative root", root: "plugin", exec: "handlers/guard.sh", want: "must be an absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePluginExec(tc.root, tc.exec)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got != realGuard {
					t.Errorf("got %q, want %q", got, realGuard)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got (%q, %v), want an error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestCheckPluginExecs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "guard.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "plain.sh", "#!/bin/sh\n")
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := CheckPluginExecs(root, []Handler{{ID: "ok", Exec: "guard.sh"}}); err != nil {
		t.Fatalf("executable file: %v", err)
	}
	for exec, want := range map[string]string{"plain.sh": "not executable", "dir": "is a directory"} {
		err := CheckPluginExecs(root, []Handler{{ID: "ok", Exec: "guard.sh"}, {ID: "bad", Exec: exec}})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `handler "bad"`) {
			t.Errorf("%s: got %v, want an error naming handler \"bad\" containing %q", exec, err, want)
		}
	}
}

func TestCheckCommandExecs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "guard.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "plain.sh", "#!/bin/sh\n")
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := writeFile(t, t.TempDir(), "evil.sh", "#!/bin/sh\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape.sh")); err != nil {
		t.Fatal(err)
	}

	if err := CheckCommandExecs(root, []Command{{Name: "ok", Exec: "guard.sh"}}); err != nil {
		t.Fatalf("executable file: %v", err)
	}
	cases := map[string]string{
		"gone.sh":   "gone.sh",
		"plain.sh":  "not executable",
		"dir":       "is a directory",
		"escape.sh": "outside the plugin root",
	}
	for exec, want := range cases {
		err := CheckCommandExecs(root, []Command{{Name: "ok", Exec: "guard.sh"}, {Name: "bad", Exec: exec}})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `command "bad"`) {
			t.Errorf("%s: got %v, want an error naming command \"bad\" containing %q", exec, err, want)
		}
	}
}

// writeBuildTimeManifest writes a manifest naming exec verbatim, unlike
// writeManifest, which always points EXEC at a real executable it just
// created. LoadBuildTime's whole point is behaving differently depending on
// whether exec exists and whether its store root exists, so these tests need
// to control both independently.
func writeBuildTimeManifest(t *testing.T, id, exec string) string {
	t.Helper()
	body := `{"handlers":[{"id":"` + id + `","exec":"` + exec + `","events":["pre_tool"],"engines":["cursor"]}]}`
	return writeFile(t, t.TempDir(), "hookyard.json", body)
}

func TestLoadBuildTimeAcceptsExecUnderAPresentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	exec := filepath.Join(store, "abc-jq", "bin", "jq")
	if err := os.MkdirAll(filepath.Dir(exec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadBuildTime(writeBuildTimeManifest(t, "a", exec)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBuildTimeRejectsMissingExecUnderAPresentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	root := filepath.Join(store, "abc-jq")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exec := filepath.Join(root, "bin", "jq") // never created

	_, err := LoadBuildTime(writeBuildTimeManifest(t, "missing-exec", exec))
	if err == nil {
		t.Fatal("want an error for a missing exec under a present store root, got nil")
	}
	if !strings.Contains(err.Error(), "missing-exec") {
		t.Errorf("error should name the handler id, got: %v", err)
	}
}

func TestLoadBuildTimeRejectsNonExecutableModeUnderAPresentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	exec := filepath.Join(store, "abc-jq", "bin", "jq")
	if err := os.MkdirAll(filepath.Dir(exec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadBuildTime(writeBuildTimeManifest(t, "not-executable", exec))
	if err == nil {
		t.Fatal("want an error for a non-executable exec under a present store root, got nil")
	}
	if !strings.Contains(err.Error(), "not-executable") {
		t.Errorf("error should name the handler id, got: %v", err)
	}
}

func TestLoadBuildTimeRejectsExecThatIsADirectoryUnderAPresentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	exec := filepath.Join(store, "abc-jq", "bin", "jq")
	if err := os.MkdirAll(exec, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadBuildTime(writeBuildTimeManifest(t, "a", exec)); err == nil {
		t.Fatal("want an error for an exec that is a directory, got nil")
	}
}

// This is the regression LoadBuildTime exists to prevent: a manifest added
// to the store as a source path (rather than a derivation output) never gets
// its exec references scanned, so the store root below is never created in
// the sandbox even though the exec is perfectly fine on the target machine.
// Failing the build here would be exactly the false positive §Background
// warns about.
func TestLoadBuildTimeAcceptsExecUnderAnAbsentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	exec := filepath.Join(store, "abc-jq", "bin", "jq") // store root "abc-jq" never created

	if _, err := LoadBuildTime(writeBuildTimeManifest(t, "a", exec)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBuildTimeAcceptsExecNotUnderTheStoreAtAll(t *testing.T) {
	t.Setenv("NIX_STORE", t.TempDir())

	path := writeBuildTimeManifest(t, "a", "/usr/local/bin/definitely-absent")
	if _, err := LoadBuildTime(path); err != nil {
		t.Fatal(err)
	}
}

// LoadBuildTime must still run every other validateAll rule; only the exec
// check gets the build-time carve-out.
func TestLoadBuildTimeStillRejectsANonExecIssue(t *testing.T) {
	t.Setenv("NIX_STORE", t.TempDir())
	body := `{"handlers":[{"id":"a","exec":"/usr/local/bin/anything","events":["pre_tool"],"engines":["emacs"]}]}`
	path := writeFile(t, t.TempDir(), "hookyard.json", body)

	_, err := LoadBuildTime(path)
	if err == nil || !strings.Contains(err.Error(), "unknown engine") {
		t.Errorf("want an error mentioning unknown engine, got: %v", err)
	}
}

// storeRoot must compare on a path-separator boundary: a sibling directory
// that merely starts with the store dir's name (.../storefoo/...) is not
// under .../store, the same way /nix/storefoo is not under /nix/store.
func TestLoadBuildTimeTreatsALookalikeSiblingDirAsNotUnderTheStore(t *testing.T) {
	parent := t.TempDir()
	store := filepath.Join(parent, "store")
	if err := os.Mkdir(store, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIX_STORE", store)

	lookalike := filepath.Join(parent, "storefoo", "abc-jq", "bin", "jq")
	if _, err := LoadBuildTime(writeBuildTimeManifest(t, "a", lookalike)); err != nil {
		t.Fatal(err)
	}
}
