package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config with the two trust stores hookyard must never disturb, plus
// hand-edited content around them.
const codexInherited = `model_reasoning_effort = "low"

# lazytmux-managed: codex status-line hooks
[[hooks.SessionStart]]
matcher = "startup|resume"

[[hooks.SessionStart.hooks]]
type = "command"
command = "/etc/profiles/per-user/noams/bin/claude-status-update idle"
timeout = 30

[hooks.state]

[hooks.state."/home/noams/.codex/config.toml:session_start:0:0"]
trusted_hash = "sha256:09216baa019df4f66b6b388b6708cda968a48714d8e8aac1106137104d86c2ab"

[projects."/home/noams/Data/git/noamsto/hookyard"]
trust_level = "trusted"

[mcp_servers.context7]
command = "context7-mcp"
`

func writeCodexFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestWriteCodexPreservesTrustStoresAndForeignEntries(t *testing.T) {
	path := writeCodexFixture(t, codexInherited)
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool"}}

	if err := WriteCodex(path, entries); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	mustSurvive := []string{
		`trusted_hash = "sha256:09216baa019df4f66b6b388b6708cda968a48714d8e8aac1106137104d86c2ab"`,
		`[projects."/home/noams/Data/git/noamsto/hookyard"]`,
		`trust_level = "trusted"`,
		`command = "/etc/profiles/per-user/noams/bin/claude-status-update idle"`,
		`[mcp_servers.context7]`,
		`model_reasoning_effort = "low"`,
	}
	for _, want := range mustSurvive {
		if !strings.Contains(got, want) {
			t.Errorf("write dropped inherited content %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, "--registered-for codex") {
		t.Errorf("hookyard entry missing\n--- got ---\n%s", got)
	}
}

// A second install must replace hookyard's block, not stack another copy of
// it: on Codex a duplicated entry means the handler runs twice per event.
func TestWriteCodexIsIdempotent(t *testing.T) {
	path := writeCodexFixture(t, codexInherited)
	entries := []Entry{{Event: "PreToolUse", Matcher: "Bash", Command: "/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool"}}

	if err := WriteCodex(path, entries); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, path)
	if err := WriteCodex(path, entries); err != nil {
		t.Fatal(err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("second write differs from the first\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if n := strings.Count(second, codexBegin); n != 1 {
		t.Errorf("want exactly one hookyard block, got %d", n)
	}
	if n := strings.Count(second, "--registered-for codex"); n != 1 {
		t.Errorf("want exactly one hookyard entry, got %d", n)
	}
}

// Removing every handler for an engine must take hookyard's block with it,
// leaving the rest of the file intact.
func TestWriteCodexWithNoEntriesRemovesOnlyItsOwnBlock(t *testing.T) {
	path := writeCodexFixture(t, codexInherited)
	entries := []Entry{{Event: "PreToolUse", Command: "/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool"}}
	if err := WriteCodex(path, entries); err != nil {
		t.Fatal(err)
	}
	if err := WriteCodex(path, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	if strings.Contains(got, codexBegin) || strings.Contains(got, "--registered-for") {
		t.Errorf("hookyard's own block survived removal\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, `[projects."/home/noams/Data/git/noamsto/hookyard"]`) {
		t.Errorf("removal dropped the project trust store\n--- got ---\n%s", got)
	}
}

func TestWriteCodexRefusesInvalidTOML(t *testing.T) {
	path := writeCodexFixture(t, "this = = not toml\n")
	before := readFile(t, path)

	err := WriteCodex(path, []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}})
	if err == nil {
		t.Fatal("want an error for a malformed inherited file, got nil")
	}
	if got := readFile(t, path); got != before {
		t.Errorf("refusing to parse still modified the file\n--- got ---\n%s", got)
	}
}

// config.toml is Codex's own file: an empty plan with nothing to strip must
// take no rename over it at all, not even one that reproduces the same TOML
// with different formatting.
func TestWriteCodexOnAnEmptyPlanWithNoBlockLeavesTheFileByteIdentical(t *testing.T) {
	path := writeCodexFixture(t, codexInherited)
	before := readFile(t, path)

	if err := WriteCodex(path, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("file was rewritten with nothing to add or strip\n--- before ---\n%s\n--- got ---\n%s", before, got)
	}
}

// The first --allow-empty install a machine ever runs has no config.toml at
// all yet. That must not conjure one into existence just to hold an empty
// hookyard block.
func TestWriteCodexOnAnEmptyPlanWithNoConfigFileWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	if err := WriteCodex(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("want no config.toml created, got stat err: %v", err)
	}
}

// The skip requires *both* halves of the condition: zero entries is not
// enough on its own when a prior hookyard block is still present, since
// leaving it behind would keep firing against a router that no longer wants
// it.
func TestWriteCodexOnAnEmptyPlanStillStripsAStaleBlockEvenWithNoNewEntries(t *testing.T) {
	stale := codexInherited + "\n" + codexBegin + "\n\n[[hooks.PreToolUse]]\nmatcher = \"Bash\"\n\n" +
		"[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = \"/x/bin/hookyard route\"\n\n" + codexEnd + "\n"
	path := writeCodexFixture(t, stale)

	if err := WriteCodex(path, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, codexBegin) {
		t.Errorf("stale hookyard block survived\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, `model_reasoning_effort = "low"`) {
		t.Errorf("stripping the stale block dropped inherited content\n--- got ---\n%s", got)
	}
}

func TestWriteCodexCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	entries := []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route --registered-for codex --event pre_tool"}}
	if err := WriteCodex(path, entries); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "--registered-for codex") {
		t.Error("entry missing from a freshly created config")
	}
}
