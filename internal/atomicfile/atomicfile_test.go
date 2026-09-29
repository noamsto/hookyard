package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func noTempFilesLeft(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".hookyard-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestWriteLandsRequestedModeOnNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	if err := Write(path, []byte("content"), 0o640); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q, want %q", got, "content")
	}
	noTempFilesLeft(t, dir)
}

func TestWriteReplacesExistingFileInOneRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	noTempFilesLeft(t, dir)
}

func TestWriteLeavesNoTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	// A directory sitting at the destination makes the final rename fail, so
	// the temp file must be cleaned up rather than left behind.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("content"), 0o600); err == nil {
		t.Fatal("expected Write to fail when path is a directory")
	}
	noTempFilesLeft(t, dir)
}
