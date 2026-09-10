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
