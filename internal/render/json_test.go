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

const claudeInherited = `{
  "permissions": {
    "allow": [
      "Bash(git status)"
    ]
  },
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/nix/store/guards/bin/secret-read-guard",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
`

func TestWriteClaudeLeavesOtherHooksAndKeysAlone(t *testing.T) {
	path := writeFixture(t, "settings.json", claudeInherited)
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/nix/store/x/bin/hookyard route --registered-for claude-code --event pre_tool"}}
	if err := WriteClaude(path, entries); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, want := range []string{"secret-read-guard", `"theme"`, "Bash(git status)"} {
		if !strings.Contains(got, want) {
			t.Errorf("write dropped inherited content %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, "--registered-for claude-code") {
		t.Errorf("hookyard entry missing\n--- got ---\n%s", got)
	}
}

// settings.json is hand-edited, so a hook change should not reshuffle it.
func TestWriteClaudePreservesTopLevelKeyOrder(t *testing.T) {
	path := writeFixture(t, "settings.json", claudeInherited)
	if err := WriteClaude(path, []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	permissions := strings.Index(got, `"permissions"`)
	theme := strings.Index(got, `"theme"`)
	hooks := strings.Index(got, `"hooks"`)
	if permissions > theme || theme > hooks {
		t.Errorf("top-level keys were reordered (permissions=%d theme=%d hooks=%d)\n--- got ---\n%s",
			permissions, theme, hooks, got)
	}
}

func TestWriteClaudeIsIdempotentAndStaysValid(t *testing.T) {
	path := writeFixture(t, "settings.json", claudeInherited)
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/x/bin/hookyard route --event pre_tool"}}
	if err := WriteClaude(path, entries); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, path)
	if err := WriteClaude(path, entries); err != nil {
		t.Fatal(err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("second write differs\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(second), &probe); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, second)
	}
	if n := strings.Count(second, Marker); n != 1 {
		t.Errorf("want one hookyard entry, got %d", n)
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
