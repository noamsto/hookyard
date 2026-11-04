package tier1

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

// nativeDir is a Claude memory dir under a temp HOME, so that "~/" source
// paths resolve to it.
func nativeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "p", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func importedFrom(f fact.Fact, engine, path string) fact.Fact {
	f.Metadata.Source = &fact.Source{Engine: engine, Path: path}
	return f
}

const nativeSource = "~/.claude/projects/p/memory/"

func seedDuplicates(t *testing.T, e env, dir string) {
	t.Helper()
	// a was renamed in the store after import; only its source path still
	// matches the native file.
	writeNative(t, dir, "a-old.md", mk("a-renamed-since", "about a", "global"))
	writeNative(t, dir, "b-same.md", mk("b-same", "about b-same", "global"))
	put(t, e.personal, "_global/a.md", importedFrom(typed("a", "feedback", "2026-09-08T00:00:00Z"), "claude", nativeSource+"a-old.md"))
	put(t, e.personal, "_global/b-same.md", typed("b-same", "project", "2026-09-07T00:00:00Z"))
	put(t, e.personal, "_global/c.md", typed("c", "project", "2026-09-06T00:00:00Z"))
	index(t, e.personal)
}

func TestClaudeSessionDropsFactsAlsoInNativeMemory(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedDuplicates(t, e, dir)

	out := e.assembleClaude(personalSession, dir, Budget)
	if strings.Contains(out, "[a]") || strings.Contains(out, "[b-same]") {
		t.Errorf("native facts listed:\n%s", out)
	}
	if !strings.Contains(out, "[c](_global/c.md)") {
		t.Errorf("non-native fact missing:\n%s", out)
	}
	if !strings.Contains(out, "\n2 facts also in MEMORY.md\n") {
		t.Errorf("count line missing:\n%s", out)
	}
}

func TestClaudeSourcePathOnlyMatchesClaudeImports(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	writeNative(t, dir, "a-old.md", mk("a-renamed-since", "about a", "global"))
	put(t, e.personal, "_global/a.md", importedFrom(typed("a", "feedback", "2026-09-08T00:00:00Z"), "codex", nativeSource+"a-old.md"))
	index(t, e.personal)

	out := e.assembleClaude(personalSession, dir, Budget)
	if !strings.Contains(out, "[a](_global/a.md)") || strings.Contains(out, "also in MEMORY.md") {
		t.Errorf("a non-claude source dropped the fact:\n%s", out)
	}
}

func TestClaudeSourcePathDecidesForClaudeImports(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	writeNative(t, dir, "a.md", mk("a", "about a", "global"))
	put(t, e.personal, "_global/a.md", importedFrom(typed("a", "feedback", "2026-09-08T00:00:00Z"), "claude", "~/.claude/projects/other/memory/a.md"))
	index(t, e.personal)

	out := e.assembleClaude(personalSession, dir, Budget)
	if !strings.Contains(out, "[a](_global/a.md)") || strings.Contains(out, "also in MEMORY.md") {
		t.Errorf("a fact imported from another project's memory was dropped:\n%s", out)
	}
}

func TestClaudeUnlinkedNativeFileDoesNotHideFact(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	writeNativeUnlinked(t, dir, "c.md", mk("c", "about c", "global"))
	put(t, e.personal, "_global/c.md", typed("c", "project", "2026-09-06T00:00:00Z"))
	index(t, e.personal)

	out := e.assembleClaude(personalSession, dir, Budget)
	if !strings.Contains(out, "[c](_global/c.md)") || strings.Contains(out, "also in MEMORY.md") {
		t.Errorf("an unlinked native file hid the fact:\n%s", out)
	}
}

func TestClaudeSingularCount(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	writeNative(t, dir, "b-same.md", mk("b-same", "about b-same", "global"))
	put(t, e.personal, "_global/b-same.md", typed("b-same", "project", "2026-09-07T00:00:00Z"))
	put(t, e.personal, "_global/c.md", typed("c", "project", "2026-09-06T00:00:00Z"))
	index(t, e.personal)

	if out := e.assembleClaude(personalSession, dir, Budget); !strings.Contains(out, "\n1 fact also in MEMORY.md\n") {
		t.Errorf("singular count line missing:\n%s", out)
	}
}

func TestClaudeAllDuplicatedStillEmitsCount(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	for i := range 12 {
		name := fmt.Sprintf("dup-%02d", i)
		writeNative(t, dir, name+".md", mk(name, "about "+name, "global"))
		put(t, e.personal, "_global/"+name+".md", typed(name, "project", "2026-09-07T00:00:00Z"))
	}
	index(t, e.personal)

	blocks := parseBlocks(t, e.assembleClaude(personalSession, dir, Budget))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	want := []string{pointerLine, "12 facts also in MEMORY.md"}
	if !slices.Equal(blocks[0].lines, want) {
		t.Errorf("lines = %q, want %q", blocks[0].lines, want)
	}
}

func TestClaudeWithoutNativeDirKeepsFlatIndex(t *testing.T) {
	e := newEnv(t)
	seedTyped(t, e)
	flat, _ := e.assemble(personalSession)
	out := e.assembleClaude(personalSession, "", Budget)
	if delimRE.ReplaceAllString(out, "") != delimRE.ReplaceAllString(flat, "") {
		t.Errorf("Claude without a native dir changed the index:\n%s", out)
	}
}

func TestOtherEnginesKeepEveryFact(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedDuplicates(t, e, dir)

	out, _ := e.assemble(personalSession)
	for _, name := range []string{"[a]", "[b-same]", "[c]"} {
		if !strings.Contains(out, name) {
			t.Errorf("%s missing from the flat index:\n%s", name, out)
		}
	}
	if strings.Contains(out, "MEMORY.md") || strings.Contains(out, pointerLine) {
		t.Errorf("flat index carries Claude-only lines:\n%s", out)
	}
}

func TestClaudeGroupedLayout(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedTyped(t, e)
	writeNative(t, dir, "fb-two.md", mk("fb-two", "about fb-two", "global"))

	blocks := parseBlocks(t, e.assembleClaude(personalSession, dir, Budget))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	var headings []string
	for _, l := range blocks[0].lines {
		if strings.HasPrefix(l, "## ") {
			headings = append(headings, l)
		}
	}
	want := []string{"## feedback", "## project", "## reference", "## user", "## other", "## flagged, unreviewed"}
	if !slices.Equal(headings, want) {
		t.Errorf("headings = %q, want %q", headings, want)
	}
	if blocks[0].lines[0] != pointerLine {
		t.Errorf("first line = %q, want the pointer", blocks[0].lines[0])
	}
	if strings.Contains(strings.Join(blocks[0].lines, "\n"), flaggedPrefix) {
		t.Errorf("flagged facts keep their per-line prefix")
	}
}

func TestClaudeBudgetTruncates(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	const n = 30
	bulkFacts(t, e.personal, "f", n, 100)
	writeNative(t, dir, "f01.md", mk("f01", strings.Repeat("x", 100), "global"))

	const budget = 1200
	blocks := parseBlocks(t, e.assembleClaude(personalSession, dir, budget))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	lines := blocks[0].lines
	if got := innerRunes(blocks); got > budget {
		t.Errorf("inner runes = %d, over budget %d", got, budget)
	}
	if lines[0] != pointerLine {
		t.Errorf("block opens with %q, want the pointer", lines[0])
	}
	marker := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "… ") })
	if marker != 1 || !strings.HasSuffix(lines[marker], "entries omitted (project) …") {
		t.Fatalf("marker at %d: %q, want line 1 naming project", marker, lines)
	}
	if lines[2] != "## project" {
		t.Errorf("heading at line 2 = %q, want ## project", lines[2])
	}
	if last := lines[len(lines)-2]; !strings.HasPrefix(last, "- [f") {
		t.Errorf("tail ends with %q, want the last entry before the count", last)
	}
	if got := lines[len(lines)-1]; got != "1 fact also in MEMORY.md" {
		t.Errorf("last line = %q, want the count", got)
	}
	if got := strings.Count(strings.Join(lines, "\n"), "## "); got != 1 {
		t.Errorf("%d headings, want only ## project", got)
	}
}

func TestClaudeBudgetTooSmallForFraming(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedTyped(t, e)
	if out := e.assembleClaude(personalSession, dir, len(pointerLine)); out != "" {
		t.Errorf("out = %q, want the store dropped", out)
	}
}

func TestClaudeBudgetKeepsCountWhenNoEntryFits(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	bulkFacts(t, e.personal, "f", 5, 200)
	writeNative(t, dir, "f01.md", mk("f01", strings.Repeat("x", 200), "global"))

	blocks := parseBlocks(t, e.assembleClaude(personalSession, dir, 160))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	want := []string{pointerLine, "… 4 entries omitted (project) …", "1 fact also in MEMORY.md"}
	if !slices.Equal(blocks[0].lines, want) {
		t.Errorf("lines = %q, want %q", blocks[0].lines, want)
	}
}

func TestClaudeMarkerNamesDroppedGroups(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedTyped(t, e)
	writeNative(t, dir, "ref-one.md", mk("ref-one", "about ref-one", "global"))

	out := e.assembleClaude(personalSession, dir, 440)
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "… ") && !strings.Contains(l, "(") {
			t.Errorf("marker names no group: %q", l)
		}
	}
}

func TestClaudeDedupGolden(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedDuplicates(t, e, dir)
	checkGolden(t, "claude_dedup.golden", e.assembleClaude(personalSession, dir, Budget))
}

func TestClaudeGroupedFlaggedTruncatedGolden(t *testing.T) {
	e := newEnv(t)
	dir := nativeDir(t)
	seedTyped(t, e)
	writeNative(t, dir, "ref-one.md", mk("ref-one", "about ref-one", "global"))
	checkGolden(t, "claude_grouped_flagged_truncated.golden", e.assembleClaude(personalSession, dir, 440))
}
