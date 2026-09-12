package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cursorInherited = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "/nix/store/aeye/adapters/cursor/scripts/diagram-guidance.sh",
        "timeout": 15
      }
    ],
    "stop": [
      {
        "command": "/home/noams/.nix-profile/bin/cursor-status-hook done",
        "timeout": 15
      }
    ]
  }
}
`

func TestWriteCursorLeavesOtherWritersAlone(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorInherited)
	entries := []Entry{{Event: "preToolUse", Matcher: "Shell", Command: "/nix/store/x/bin/hookyard route --registered-for cursor --event pre_tool"}}
	if err := WriteCursor(path, entries); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, want := range []string{"diagram-guidance.sh", "cursor-status-hook done"} {
		if !strings.Contains(got, want) {
			t.Errorf("write dropped another writer's entry %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, "--registered-for cursor") {
		t.Errorf("hookyard entry missing\n--- got ---\n%s", got)
	}
}

func TestWriteCursorIsIdempotent(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorInherited)
	entries := []Entry{{Event: "preToolUse", Command: "/nix/store/x/bin/hookyard route --registered-for cursor --event pre_tool"}}
	if err := WriteCursor(path, entries); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, path)
	if err := WriteCursor(path, entries); err != nil {
		t.Fatal(err)
	}
	if second := readFile(t, path); first != second {
		t.Errorf("second write differs\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if n := strings.Count(readFile(t, path), Marker); n != 1 {
		t.Errorf("want one hookyard entry, got %d", n)
	}
}

// Moving a handler between events must not leave the old registration behind,
// which is why the strip spans every event key rather than only the ones being
// written.
func TestWriteCursorStripsHookyardEntriesUnderEveryEvent(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorInherited)
	if err := WriteCursor(path, []Entry{{Event: "preToolUse", Command: "/x/bin/hookyard route --event pre_tool"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCursor(path, []Entry{{Event: "postToolUse", Command: "/x/bin/hookyard route --event post_tool"}}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	if strings.Contains(got, "--event pre_tool") {
		t.Errorf("the old registration survived the move\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, "--event post_tool") {
		t.Errorf("the new registration is missing\n--- got ---\n%s", got)
	}
}

func TestWriteCursorRefusesMalformedJSON(t *testing.T) {
	path := writeFixture(t, "hooks.json", "{not json")
	before := readFile(t, path)
	if err := WriteCursor(path, []Entry{{Event: "preToolUse", Command: "/x/bin/hookyard route"}}); err == nil {
		t.Fatal("want an error for a malformed inherited file, got nil")
	}
	if got := readFile(t, path); got != before {
		t.Error("refusing to parse still modified the file")
	}
}

// Cursor's native converter fills in fields cursorEntry does not declare
// (loop_limit, failClosed), so a foreign row carrying them must round-trip
// byte-identically, even when hookyard registers nothing at all.
const cursorForeignFieldsInherited = `{
  "version": 1,
  "hooks": {
    "stop": [
      {
        "command": "/some/other/writer/hook",
        "loop_limit": 3,
        "failClosed": true
      }
    ]
  }
}
`

func TestWriteCursorPreservesForeignFieldsOnEmptyInstall(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorForeignFieldsInherited)
	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, want := range []string{`"loop_limit": 3`, `"failClosed": true`} {
		if !strings.Contains(got, want) {
			t.Errorf("foreign row lost a field %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestWriteCursorDoesNotAddTimeoutToAForeignRowMissingOne(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorForeignFieldsInherited)
	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	if strings.Contains(got, "timeout") {
		t.Errorf("foreign row without a timeout gained one\n--- got ---\n%s", got)
	}
}

// The strip must still find hookyard's own rows by command alone, even though
// rows now round-trip as raw JSON instead of a decoded struct.
func TestWriteCursorStillStripsItsOwnRowsAmongForeignOnes(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorForeignFieldsInherited)
	entries := []Entry{{Event: "stop", Command: "/x/bin/hookyard route --event stop"}}
	if err := WriteCursor(path, entries); err != nil {
		t.Fatal(err)
	}
	afterInstall := readFile(t, path)
	if !strings.Contains(afterInstall, "--event stop") {
		t.Fatalf("hookyard's own row is missing\n--- got ---\n%s", afterInstall)
	}
	if !strings.Contains(afterInstall, `"loop_limit": 3`) {
		t.Fatalf("foreign row was dropped alongside the install\n--- got ---\n%s", afterInstall)
	}

	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	afterRemoval := readFile(t, path)
	if strings.Contains(afterRemoval, Marker) {
		t.Errorf("hookyard's own row survived removal\n--- got ---\n%s", afterRemoval)
	}
	if !strings.Contains(afterRemoval, `"loop_limit": 3`) {
		t.Errorf("the foreign row was stripped alongside hookyard's own\n--- got ---\n%s", afterRemoval)
	}
}

// hooks.json is Cursor's own file: an empty plan with nothing to strip must
// take no rename over it at all, not even one that reproduces the same JSON
// with different formatting.
func TestWriteCursorOnAnEmptyPlanWithNoMarkerLeavesTheFileByteIdentical(t *testing.T) {
	path := writeFixture(t, "hooks.json", cursorInherited)
	before := readFile(t, path)

	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("file was rewritten with nothing to add or strip\n--- before ---\n%s\n--- got ---\n%s", before, got)
	}
}

// The first --allow-empty install a machine ever runs has no hooks.json at
// all yet. That must not conjure one into existence just to hold an empty
// hooks key.
func TestWriteCursorOnAnEmptyPlanWithNoFileWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")

	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("want no hooks.json created, got stat err: %v", err)
	}
}

// The skip requires *both* halves of the condition: zero entries is not
// enough on its own when a prior hookyard marker row is still registered,
// since leaving it behind would keep firing against a router that no longer
// wants it.
func TestWriteCursorOnAnEmptyPlanStillStripsAStaleMarkerEvenWithNoNewEntries(t *testing.T) {
	const stale = `{"hooks":{"stop":[{"command":"/x/bin/hookyard route --event stop"}]}}`
	path := writeFixture(t, "hooks.json", stale)

	if err := WriteCursor(path, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); strings.Contains(got, Marker) {
		t.Errorf("stale hookyard row survived\n--- got ---\n%s", got)
	}
}

// Shaped after the consumer's real --settings overlay, counted from the file:
// twelve entries, ten of them PreToolUse across three matcher groups (Bash 7,
// Read 2, Grep 1), and not one declaring a timeout. The event keys are
// deliberately not in alphabetical order, so re-encoding through a Go map is
// visible; so are the two foreign fields, one on a group and one on a hook.
const claudeOverlayInherited = `{
  "permissions": {
    "allow": [
      "Bash(git status)"
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "/nix/store/statusline/bin/statusline"
  },
  "enabledPlugins": {
    "superpowers@marketplace": true
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/session-start-guard"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/nix-stage-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/git-commit-autostage-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/git-default-branch-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/tmux-live-server-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/secret-read-guard",
            "statusMessage": "checking for secrets"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/deslop-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/gtrash-guard"
          }
        ]
      },
      {
        "matcher": "Read",
        "description": "shared agent-hooks guards",
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/secret-read-guard"
          },
          {
            "type": "command",
            "command": "/nix/store/guards/bin/claude-read-skeleton-guard"
          }
        ]
      },
      {
        "matcher": "Grep",
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/secret-read-guard"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/stop-guard"
          }
        ]
      }
    ]
  }
}
`

// A "timeout": 0 added to an entry that declared none stops that hook firing
// at all — measured twice against the live claude binary, on two runs of one
// fixture differing only in that field. Every entry in the overlay this is
// shaped after would go dead, silently, including seven invocations of the
// shared guards hookyard exists to route.
func TestClaudeSettingsDoesNotAddATimeoutToAnInheritedHookMissingOne(t *testing.T) {
	out, err := ClaudeSettings([]byte(claudeOverlayInherited), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if strings.Contains(got, "timeout") {
		t.Errorf("an inherited hook that declared no timeout gained one\n--- got ---\n%s", got)
	}
}

// The other half of the timeout rule, and the one the whole emit change turns
// on: hookyard's own row must always declare one (§4), so dropping the field —
// by tag, by omitempty, or by zeroing the constant — has to fail here rather
// than silently hand every router invocation the engine's own default.
func TestClaudeSettingsGivesItsOwnRowTheMandatoryTimeout(t *testing.T) {
	command := "/nix/store/x/bin/hookyard route --registered-for claude-code --event pre_tool"
	out, err := ClaudeSettings([]byte(claudeOverlayInherited), []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: command}})
	if err != nil {
		t.Fatal(err)
	}

	var probe struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout *int   `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	found := 0
	for _, group := range probe.Hooks["PreToolUse"] {
		for _, hook := range group.Hooks {
			if hook.Command != command {
				continue
			}
			found++
			if hook.Timeout == nil {
				t.Errorf("hookyard's own row declares no timeout\n--- got ---\n%s", out)
			} else if *hook.Timeout != EmittedTimeoutSeconds {
				t.Errorf("hookyard's own row has timeout %d, want %d", *hook.Timeout, EmittedTimeoutSeconds)
			}
		}
	}
	if found != 1 {
		t.Fatalf("want one hookyard row under PreToolUse, got %d\n--- got ---\n%s", found, out)
	}
}

// The other half of §4.2b: the typed decode dropped every field the structs
// did not declare, one on a group and one on a hook here.
func TestClaudeSettingsPreservesForeignFieldsOnInheritedGroupsAndHooks(t *testing.T) {
	out, err := ClaudeSettings([]byte(claudeOverlayInherited), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{`"description": "shared agent-hooks guards"`, `"statusMessage": "checking for secrets"`} {
		if !strings.Contains(got, want) {
			t.Errorf("an inherited entry lost a foreign field %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestClaudeSettingsLeavesOtherHooksAndKeysAlone(t *testing.T) {
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/nix/store/x/bin/hookyard route --registered-for claude-code --event pre_tool"}}
	out, err := ClaudeSettings([]byte(claudeOverlayInherited), entries)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"session-start-guard", "gtrash-guard", "claude-read-skeleton-guard", "stop-guard",
		`"statusLine"`, `"enabledPlugins"`, "Bash(git status)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the merge dropped inherited content %q\n--- got ---\n%s", want, got)
		}
	}
	if n := strings.Count(got, "secret-read-guard"); n != 3 {
		t.Errorf("want the three inherited secret-read-guard invocations, got %d\n--- got ---\n%s", n, got)
	}
	if !strings.Contains(got, "--registered-for claude-code") {
		t.Errorf("hookyard entry missing\n--- got ---\n%s", got)
	}
}

// The base is hand-edited above and Nix-generated below, so a hook change must
// reshuffle neither its top level nor hooks' own event keys (A3). Both key
// sequences here are deliberately non-alphabetical, which is what a Go map
// would have re-sorted them into.
func TestClaudeSettingsPreservesKeyOrder(t *testing.T) {
	out, err := ClaudeSettings([]byte(claudeOverlayInherited), []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, ordered := range [][]string{
		{`"permissions"`, `"statusLine"`, `"enabledPlugins"`, `"hooks"`},
		{`"SessionStart"`, `"PreToolUse"`, `"Stop"`},
	} {
		for i := 1; i < len(ordered); i++ {
			if strings.Index(got, ordered[i-1]) > strings.Index(got, ordered[i]) {
				t.Errorf("%s and %s were reordered\n--- got ---\n%s", ordered[i-1], ordered[i], got)
			}
		}
	}
}

func TestClaudeSettingsIsIdempotentAndStaysValid(t *testing.T) {
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/x/bin/hookyard route --event pre_tool"}}
	first, err := ClaudeSettings([]byte(claudeOverlayInherited), entries)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ClaudeSettings(first, entries)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("the second merge differs\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	var probe map[string]any
	if err := json.Unmarshal(second, &probe); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, second)
	}
	if n := strings.Count(string(second), Marker); n != 1 {
		t.Errorf("want one hookyard entry, got %d", n)
	}
}

// Moving a handler between events must not leave the old registration behind,
// which is why the strip spans every event key rather than only the ones being
// written.
func TestClaudeSettingsStripsHookyardEntriesUnderEveryEvent(t *testing.T) {
	first, err := ClaudeSettings([]byte(claudeOverlayInherited), []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route --event pre_tool"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ClaudeSettings(first, []Entry{{Event: "Stop", Command: "/x/bin/hookyard route --event stop"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(second)

	if strings.Contains(got, "--event pre_tool") {
		t.Errorf("the old registration survived the move\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, "--event stop") {
		t.Errorf("the new registration is missing\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, "stop-guard") {
		t.Errorf("the inherited Stop hook was stripped alongside hookyard's own\n--- got ---\n%s", got)
	}
}

// An event key whose only content was hookyard's own goes away entirely, so a
// removal leaves neither an empty group nor an empty array behind — and with
// nothing left under hooks, the key itself is gone (R1).
func TestClaudeSettingsDropsGroupsAndEventKeysItEmpties(t *testing.T) {
	registered, err := ClaudeSettings([]byte("{}"), []Entry{{Event: "Notification", Command: "/x/bin/hookyard route --event notify"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(registered), `"Notification"`) {
		t.Fatalf("the entry was never registered\n--- got ---\n%s", registered)
	}

	out, err := ClaudeSettings(registered, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if strings.Contains(got, `"Notification"`) {
		t.Errorf("an emptied event key survived\n--- got ---\n%s", got)
	}
	if strings.Contains(got, `"hooks"`) {
		t.Errorf("an emptied hooks key survived\n--- got ---\n%s", got)
	}
}

// A refusal must return no document at all: emit redirects stdout into $out,
// so half a document written before the error would be captured as the build's
// result (R1).
func TestClaudeSettingsRefusesAMalformedBase(t *testing.T) {
	for name, base := range map[string]string{
		"not JSON at all":         "{not json",
		"a JSON array":            `[]`,
		"an unreadable hooks key": `{"hooks": 7}`,
		"an unreadable group":     `{"hooks": {"PreToolUse": [7]}}`,
		"an unreadable hook":      `{"hooks": {"PreToolUse": [{"hooks": [7]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := ClaudeSettings([]byte(base), []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}})
			if err == nil {
				t.Fatalf("want a refusal, got\n%s", out)
			}
			if out != nil {
				t.Errorf("a refusal still returned a document\n%s", out)
			}
		})
	}
}

// Pi's settings.json is hand-edited like Claude Code's, and the key hookyard
// touches is a flat array of paths rather than a nested hook table — so the
// shared shape is the same three questions, asked of extensions[].
const piInherited = `{
  "model": "kimi-k2",
  "extensions": [
    "/home/noams/.pi/agent/extensions/foreign-extension.ts"
  ],
  "defaultProjectTrust": "ask"
}
`

func TestWritePiLeavesOtherExtensionsAndKeysAlone(t *testing.T) {
	path := writeFixture(t, "settings.json", piInherited)
	entries := []Entry{{Event: "tool_call", Matcher: "bash", Command: "/nix/store/x/bin/hookyard route --registered-for pi --event pre_tool"}}
	if err := WritePi(path, entries, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, want := range []string{"foreign-extension.ts", `"model"`, `"defaultProjectTrust"`} {
		if !strings.Contains(got, want) {
			t.Errorf("write dropped inherited content %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, PiBridgePath(path)) {
		t.Errorf("the bridge is not registered\n--- got ---\n%s", got)
	}
	if !strings.Contains(readFile(t, PiBridgePath(path)), "--registered-for pi") {
		t.Error("the bridge carries no hookyard entry")
	}
}

// The settings file is hand-edited, so registering an extension should not
// reshuffle it.
func TestWritePiPreservesTopLevelKeyOrder(t *testing.T) {
	path := writeFixture(t, "settings.json", piInherited)
	if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	model := strings.Index(got, `"model"`)
	extensions := strings.Index(got, `"extensions"`)
	trust := strings.Index(got, `"defaultProjectTrust"`)
	if model > extensions || extensions > trust {
		t.Errorf("top-level keys were reordered (model=%d extensions=%d defaultProjectTrust=%d)\n--- got ---\n%s",
			model, extensions, trust, got)
	}
}

func TestWritePiIsIdempotentAndStaysValid(t *testing.T) {
	path := writeFixture(t, "settings.json", piInherited)
	entries := []Entry{{Event: "tool_call", Matcher: "bash", Command: "/x/bin/hookyard route --event pre_tool"}}
	if err := WritePi(path, entries, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, path)
	firstBridge := readFile(t, PiBridgePath(path))
	if err := WritePi(path, entries, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("second write differs\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if secondBridge := readFile(t, PiBridgePath(path)); firstBridge != secondBridge {
		t.Error("the second write produced a different bridge")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(second), &probe); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, second)
	}
	if n := strings.Count(second, Marker); n != 1 {
		t.Errorf("want one hookyard entry, got %d", n)
	}
}

// Refusing to parse must also mean refusing to write the bridge: the bridge
// goes first precisely so no entry ever names a missing file, which would
// otherwise leave executable code behind for a settings write that never
// happened.
func TestWritePiRefusesMalformedJSONAndWritesNoBridge(t *testing.T) {
	path := writeFixture(t, "settings.json", "{not json")
	before := readFile(t, path)
	if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err == nil {
		t.Fatal("want an error for a malformed inherited file, got nil")
	}
	if got := readFile(t, path); got != before {
		t.Error("refusing to parse still modified the file")
	}
	if _, err := os.Stat(PiBridgePath(path)); !os.IsNotExist(err) {
		t.Errorf("want no bridge written by the refused install, got stat err: %v", err)
	}
}
