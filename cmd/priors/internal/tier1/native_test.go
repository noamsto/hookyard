package tier1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
)

// writeNative writes a topic file and links it from the dir's MEMORY.md, the
// way Claude does.
func writeNative(t *testing.T, dir, file string, f fact.Fact) {
	t.Helper()
	writeNativeUnlinked(t, dir, file, f)
	appendIndex(t, dir, "- [x]("+file+") - hook\n")
}

func writeNativeUnlinked(t *testing.T, dir, file string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendIndex(t *testing.T, dir, text string) {
	t.Helper()
	fh, err := os.OpenFile(filepath.Join(dir, "MEMORY.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIndexReadsTopicFilesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "p", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeNative(t, dir, "good.md", mk("good-name", "d", "global"))
	writeNative(t, dir, "notes.txt", mk("txt-name", "d", "global"))
	appendIndex(t, dir, "- [m](MEMORY.md)\n")
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendIndex(t, dir, "- [b](broken.md)\n")
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeNativeUnlinked(t, filepath.Dir(outside), "outside.md", mk("link-name", "d", "global"))
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendIndex(t, dir, "- [l](link.md)\n- [p](pipe.md)\n- [esc](../outside.md)\n")

	n := nativeIndex(context.Background(), dir)
	if _, ok := n.names["good-name"]; len(n.names) != 1 || !ok {
		t.Errorf("names = %v, want only good-name", n.names)
	}
	if _, ok := n.sources["~/.claude/projects/p/memory/good.md"]; len(n.sources) != 1 || !ok {
		t.Errorf("sources = %v, want only ~/.claude/projects/p/memory/good.md", n.sources)
	}
}

func TestNativeIndexMissingDir(t *testing.T) {
	n := nativeIndex(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if len(n.names) != 0 || len(n.sources) != 0 {
		t.Errorf("index of a missing dir = %+v, want empty", n)
	}
}

func TestNativeIndexIgnoresUnlinkedTopics(t *testing.T) {
	dir := nativeDir(t)
	writeNativeUnlinked(t, dir, "loose.md", mk("loose", "d", "global"))
	writeNative(t, dir, "linked.md", mk("linked", "d", "global"))

	n := nativeIndex(context.Background(), dir)
	if _, loose := n.names["loose"]; loose || len(n.names) != 1 {
		t.Errorf("names = %v, want linked only", n.names)
	}
}

func TestNativeIndexWithoutMemoryMDIsEmpty(t *testing.T) {
	dir := nativeDir(t)
	writeNativeUnlinked(t, dir, "loose.md", mk("loose", "d", "global"))
	if n := nativeIndex(context.Background(), dir); len(n.names) != 0 {
		t.Errorf("names = %v, want none without MEMORY.md", n.names)
	}
}

func TestNativeIndexIgnoresLinksPastLoadedHead(t *testing.T) {
	dir := nativeDir(t)
	writeNativeUnlinked(t, dir, "late.md", mk("late", "d", "global"))
	appendIndex(t, dir, strings.Repeat("filler\n", 200)+"- [l](late.md)\n")
	if n := nativeIndex(context.Background(), dir); len(n.names) != 0 {
		t.Errorf("a link past line 200 counted: %v", n.names)
	}
}

func TestNativeIndexStopsWhenContextDone(t *testing.T) {
	dir := nativeDir(t)
	writeNative(t, dir, "a.md", mk("a", "d", "global"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := nativeIndex(ctx, dir); len(n.names) != 0 {
		t.Errorf("names = %v, want none under a done context", n.names)
	}
}

func TestNativeIndexSkipsHugeFrontmatter(t *testing.T) {
	dir := nativeDir(t)
	writeNative(t, dir, "ok.md", mk("ok", "d", "global"))
	huge := "---\nname: big\ndescription: d\n" + strings.Repeat("k: v\n", 1<<18) + "---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	appendIndex(t, dir, "- [b](big.md)\n")

	start := time.Now()
	n := nativeIndex(context.Background(), dir)
	if d := time.Since(start); d > time.Second {
		t.Errorf("nativeIndex took %v", d)
	}
	if _, ok := n.names["big"]; ok {
		t.Errorf("a frontmatter past the read head counted: %v", n.names)
	}
	if _, ok := n.names["ok"]; !ok {
		t.Errorf("names = %v, want ok", n.names)
	}
}

func TestNativeHasNeedsMatchingDescription(t *testing.T) {
	dir := nativeDir(t)
	writeNative(t, dir, "a.md", mk("a", "same", "global"))
	n := nativeIndex(context.Background(), dir)

	if !n.has(mk("a", " same ", "global")) {
		t.Error("same name and description not native")
	}
	if n.has(mk("a", "different", "global")) {
		t.Error("same name, different description counted as native")
	}
	claude := mk("a", "edited", "global")
	claude.Metadata.Source = &fact.Source{Engine: "claude", Path: importer.SourcePath(filepath.Join(dir, "a.md"))}
	if n.has(claude) {
		t.Error("an import whose native file was edited counted as native")
	}
	claude.Description = "same"
	if !n.has(claude) {
		t.Error("a genuine import did not count as native")
	}
}
