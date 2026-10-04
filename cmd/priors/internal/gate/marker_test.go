package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMarkSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provenance")
	sum := sha256.Sum256([]byte("s1"))
	hash := hex.EncodeToString(sum[:])

	if err := MarkSession(dir, "s1", false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("dir: err=%v info=%v, want private dir", err, info)
	}
	assertFiles(t, dir, hash+".seen")
	assertPrivate(t, filepath.Join(dir, hash+".seen"))

	if err := MarkSession(dir, "s1", true); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, hash+".ingest", hash+".seen")
	assertPrivate(t, filepath.Join(dir, hash+".ingest"))

	if err := MarkSession(dir, "s1", false); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, hash+".ingest", hash+".seen")

	if seen, ingest, err := sessionMarkers(dir, "s1"); err != nil || !seen || !ingest {
		t.Errorf("sessionMarkers(s1) = %v, %v, %v; want true, true, nil", seen, ingest, err)
	}
	if seen, ingest, err := sessionMarkers(dir, "s2"); err != nil || seen || ingest {
		t.Errorf("sessionMarkers(s2) = %v, %v, %v; want false, false, nil", seen, ingest, err)
	}

	if err := MarkSession(dir, "../../x", false); err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(dir, markerPath(dir, "../../x", ".seen")); err != nil || filepath.Dir(rel) != "." {
		t.Errorf("marker for ../../x escaped the dir: rel=%q err=%v", rel, err)
	}
}

func TestMarkSessionIngestFailureLeavesUnseen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provenance")
	if err := os.MkdirAll(markerPath(dir, "s1", ".ingest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MarkSession(dir, "s1", true); err == nil {
		t.Fatal("MarkSession succeeded with an unwritable ingest marker")
	}
	if _, err := os.Lstat(markerPath(dir, "s1", ".seen")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("seen marker after failed ingest write: err=%v, want not-exist", err)
	}
}

func TestSessionMarkersUnreadable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "provenance")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := sessionMarkers(file, "s1")
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("sessionMarkers under a file: err=%v, want a non-not-exist error", err)
	}

	missing := filepath.Join(t.TempDir(), "absent")
	if seen, ingest, err := sessionMarkers(missing, "s1"); err != nil || seen || ingest {
		t.Errorf("sessionMarkers(missing dir) = %v, %v, %v; want false, false, nil", seen, ingest, err)
	}
}

func assertFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("files in %s = %v, want %v", dir, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files in %s = %v, want %v", dir, got, want)
		}
	}
}

func assertPrivate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s mode %v has group/other bits", path, info.Mode().Perm())
	}
}
