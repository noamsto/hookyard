package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreAuth(t *testing.T) {
	patterns := []string{
		"Host key verification failed.",
		"git@host: Permission denied (publickey).",
		"ssh: Could not resolve hostname example.invalid: Name or service not known",
		"ssh: connect to host h port 22: Connection refused",
		"ssh: connect to host h port 22: Connection timed out",
		"ssh: connect to host h port 22: No route to host",
		"ssh: connect to host h port 22: Network is unreachable",
	}
	for _, line := range patterns {
		if !preAuth("fatal: Could not read from remote repository.\n" + line + "\n") {
			t.Errorf("preAuth(%q) = false, want true", line)
		}
		for _, prefixed := range []string{"remote: " + line, "  remote: " + line} {
			if preAuth(prefixed + "\n") {
				t.Errorf("preAuth(%q) = true, want false", prefixed)
			}
		}
	}
	for _, s := range []string{"ERROR: Repository not found.", "fatal: the remote end hung up unexpectedly", ""} {
		if preAuth(s) {
			t.Errorf("preAuth(%q) = true, want false", s)
		}
	}
}

func TestCleanTmp(t *testing.T) {
	gitDir := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(gitDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("objects/pack/tmp_pack_x")
	write("objects/pack/tmp_idx_y")
	write("objects/incoming-abc/f")
	write("objects/ab/tmp_obj_z")
	write("objects/pack/pack-1.pack")
	write("objects/ab/real")

	if err := cleanTmp(gitDir); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"objects/pack/tmp_pack_x", "objects/pack/tmp_idx_y", "objects/incoming-abc", "objects/ab/tmp_obj_z"} {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err %v)", rel, err)
		}
	}
	for _, rel := range []string{"objects/pack/pack-1.pack", "objects/ab/real"} {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err != nil {
			t.Errorf("%s removed: %v", rel, err)
		}
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := dirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Fatalf("dirSize = %d, want 8", got)
	}
}

func TestRecordTip(t *testing.T) {
	pinTools(t)
	ctx := context.Background()
	repo := t.TempDir()
	gitIn(t, repo, "", "init", "-q", "--bare", repo)
	empty := gitIn(t, repo, "", "hash-object", "-t", "tree", "-w", "--stdin")
	a := gitIn(t, repo, "", "commit-tree", "-m", "a", empty)
	b := gitIn(t, repo, "", "commit-tree", "-m", "b", "-p", a, empty)
	m := mirror{dir: repo, private: t.TempDir()}

	tips := func() string {
		return gitIn(t, repo, "", "for-each-ref", "--format=%(refname)", "refs/priors/tips/")
	}
	step := func(oid, want string) {
		t.Helper()
		if err := m.recordTip(ctx, oid); err != nil {
			t.Fatal(err)
		}
		if got := tips(); got != want {
			t.Fatalf("after recordTip(%s) tips = %q, want %q", oid[:7], got, want)
		}
	}
	step(a, "refs/priors/tips/"+a)
	step(b, "refs/priors/tips/"+b)
	step(b, "refs/priors/tips/"+b)
	step(a, strings.Join([]string{"refs/priors/tips/" + a, "refs/priors/tips/" + b}, "\n"))
}
