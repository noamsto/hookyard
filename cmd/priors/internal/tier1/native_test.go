package tier1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
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
	if len(n.names) != 1 || !n.names["good-name"] {
		t.Errorf("names = %v, want only good-name", n.names)
	}
	if len(n.sources) != 1 || !n.sources["~/.claude/projects/p/memory/good.md"] {
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
	if n.names["loose"] || !n.names["linked"] {
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
	if n := nativeIndex(context.Background(), dir); n.names["late"] {
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

func TestNativeIndexCapsTopicReads(t *testing.T) {
	dir := nativeDir(t)
	for i := range maxNativeTopics + 5 {
		file := fmt.Sprintf("t%03d.md", i)
		writeNativeUnlinked(t, dir, file, mk(fmt.Sprintf("t%03d", i), "d", "global"))
		appendIndex(t, dir, "- [t]("+file+")\n")
	}
	// Past line 200 these are ignored anyway; the cap bounds reads on top.
	if n := nativeIndex(context.Background(), dir); len(n.names) > maxNativeTopics {
		t.Errorf("read %d topics, cap %d", len(n.names), maxNativeTopics)
	}
}
