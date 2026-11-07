package verify

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

func TestObjects(t *testing.T) {
	pinTools(t)
	dir := t.TempDir()
	gitIn(t, dir, "", "init", "-q", "-b", "main")
	commit := func(msg string) string {
		for path, body := range map[string]string{".attest/x": msg, "facts/a.md": "fact " + msg + "\n"} {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, dir, "", "add", "-A")
		gitIn(t, dir, "", "commit", "-q", "-m", msg)
		return gitIn(t, dir, "", "rev-parse", "HEAD")
	}
	first := commit("first")
	head := commit("second")

	cmd := exec.Command(tools.Git, append(slices.Clone(GitConfigArgs), "cat-file", "--batch-command")...)
	cmd.Env = []string{
		"GIT_DIR=" + filepath.Join(dir, ".git"),
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	o, err := OpenObjects(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	tree, parents, err := o.Commit(head)
	if err != nil {
		t.Fatal(err)
	}
	if want := gitIn(t, dir, "", "rev-parse", head+"^{tree}"); tree != want {
		t.Fatalf("tree = %s, want %s", tree, want)
	}
	if !slices.Equal(parents, []string{first}) {
		t.Fatalf("parents = %v, want [%s]", parents, first)
	}

	wantAttest := gitIn(t, dir, "", "rev-parse", head+":.attest/x")
	if got, ok, err := o.AttestBlob(tree, "x"); err != nil || !ok || got != wantAttest {
		t.Fatalf("AttestBlob(x) = %q, %v, %v; want %q, true", got, ok, err, wantAttest)
	}
	if got, ok, err := o.AttestBlob(tree, "missing"); err != nil || ok {
		t.Fatalf("AttestBlob(missing) = %q, %v, %v; want not ok", got, ok, err)
	}

	wantFact := gitIn(t, dir, "", "rev-parse", head+":facts/a.md")
	got, ok, err := o.PathBlob(tree, "facts/a.md")
	if err != nil || !ok || got != wantFact {
		t.Fatalf("PathBlob(facts/a.md) = %q, %v, %v; want %q, true", got, ok, err, wantFact)
	}

	if _, err := o.Blob(wantFact, 3); !errors.Is(err, ErrTooBig) {
		t.Fatalf("Blob over cap err = %v, want ErrTooBig", err)
	}
	if b, err := o.Blob(wantFact, 1<<10); err != nil || string(b) != "fact second\n" {
		t.Fatalf("Blob = %q, %v", b, err)
	}
}

func TestSSHURL(t *testing.T) {
	for remote, want := range map[string]bool{
		"ssh://h/p":   true,
		"git@h:p":     true,
		"/p":          false,
		"file:///p":   false,
		"ext::x":      false,
		"https://h/p": false,
	} {
		if got := SSHURL(remote); got != want {
			t.Errorf("SSHURL(%q) = %v, want %v", remote, got, want)
		}
	}
}
