package tier1

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

func writeNative(t *testing.T, dir, file string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
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
	writeNative(t, dir, "MEMORY.md", mk("index-name", "d", "global"))
	writeNative(t, dir, "notes.txt", mk("txt-name", "d", "global"))
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeNative(t, filepath.Dir(outside), "outside.md", mk("link-name", "d", "global"))
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o644); err != nil {
		t.Fatal(err)
	}

	n := nativeIndex(dir)
	if len(n.names) != 1 || !n.names["good-name"] {
		t.Errorf("names = %v, want only good-name", n.names)
	}
	if len(n.sources) != 1 || !n.sources["~/.claude/projects/p/memory/good.md"] {
		t.Errorf("sources = %v, want only ~/.claude/projects/p/memory/good.md", n.sources)
	}
}

func TestNativeIndexMissingDir(t *testing.T) {
	n := nativeIndex(filepath.Join(t.TempDir(), "absent"))
	if len(n.names) != 0 || len(n.sources) != 0 {
		t.Errorf("index of a missing dir = %+v, want empty", n)
	}
}
