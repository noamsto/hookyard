package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

// pinTools gives a test real tool paths and a repo-agnostic environment.
func pinTools(t *testing.T) {
	t.Helper()
	toolstest.Pin()
	committest.UnsetRepoEnv()
	if tools.Git == "" {
		t.Skip("git not installed")
	}
}

// gitIn runs plain git in dir under a clean environment and returns trimmed stdout.
func gitIn(t *testing.T, dir string, stdin string, args ...string) string {
	t.Helper()
	cmd := tools.Command(context.Background(), tools.Git, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func TestMirrorEnvBuiltFromNothing(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_CONFIG_GLOBAL", "/elsewhere/config")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("HOME", "/home/elsewhere")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent")
	t.Setenv("GIT_EXEC_PATH", "/elsewhere/exec")
	t.Setenv("LD_PRELOAD", "/elsewhere/lib.so")
	oldSSH, oldCfg, oldKH := tools.SSH, tools.SSHConfig, tools.KnownHosts
	t.Cleanup(func() { tools.SSH, tools.SSHConfig, tools.KnownHosts = oldSSH, oldCfg, oldKH })
	tools.SSH, tools.SSHConfig, tools.KnownHosts = "/bin/ssh", "/etc/ssh config", "/etc/known_hosts"

	m := mirror{dir: "/p/private/mirror.git", private: "/p/private", credential: "/p/key"}
	want := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_DIR=/p/private/mirror.git",
		"GIT_ALLOW_PROTOCOL=ssh",
		"GIT_SSH_VARIANT=ssh",
		"GIT_SSH_COMMAND='/bin/ssh' -F '/etc/ssh config' -o 'UserKnownHostsFile=/etc/known_hosts'" +
			" -o 'IdentitiesOnly=yes' -o 'IdentityAgent=none' -o 'BatchMode=yes' -o 'ConnectTimeout=30'" +
			" -o 'LogLevel=VERBOSE' -i '/p/key'",
	}
	if got := m.env(); !slices.Equal(got, want) {
		t.Fatalf("env =\n%q\nwant\n%q", got, want)
	}
	if got := m.cmd(context.Background(), "x").Dir; got != "/p/private" {
		t.Fatalf("Dir = %q, want /p/private", got)
	}
}

func TestCatFileReadsTreesAndCapsBlobs(t *testing.T) {
	pinTools(t)
	repo := t.TempDir()
	gitIn(t, repo, "", "init", "-q", "-b", "main")
	write := func(rel string, data []byte) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".attest/a", []byte("small"))
	write(".attest/big", bytes.Repeat([]byte("x"), 5000))
	write(".attest/sub/inner", []byte("inner"))
	write("facts/x.md", []byte("fact"))
	if err := os.Symlink("a", filepath.Join(repo, ".attest/link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "", "add", "-A")
	gitIn(t, repo, "", "commit", "-q", "-m", "one")
	first := gitIn(t, repo, "", "rev-parse", "HEAD")
	write("facts/y.md", []byte("more"))
	gitIn(t, repo, "", "add", "-A")
	gitIn(t, repo, "", "commit", "-q", "-m", "two")
	second := gitIn(t, repo, "", "rev-parse", "HEAD")
	rootTree := gitIn(t, repo, "", "rev-parse", "HEAD^{tree}")
	smallOID := gitIn(t, repo, "", "rev-parse", "HEAD:.attest/a")
	bigOID := gitIn(t, repo, "", "rev-parse", "HEAD:.attest/big")
	factOID := gitIn(t, repo, "", "rev-parse", "HEAD:facts/x.md")

	private := t.TempDir()
	m := mirror{dir: filepath.Join(repo, ".git"), private: private}
	cf, err := newCatFile(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.close)

	tree, parents, err := cf.commit(second)
	if err != nil {
		t.Fatal(err)
	}
	if tree != rootTree || !slices.Equal(parents, []string{first}) {
		t.Fatalf("commit = %s %v, want %s [%s]", tree, parents, rootTree, first)
	}
	if _, parents, err = cf.commit(first); err != nil || len(parents) != 0 {
		t.Fatalf("root commit parents = %v, %v", parents, err)
	}

	entries, err := cf.attestEntries(tree)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := map[string]string{"a": smallOID, "big": bigOID}
	if len(entries) != len(wantEntries) || entries["a"] != smallOID || entries["big"] != bigOID {
		t.Fatalf("attestEntries = %v, want %v", entries, wantEntries)
	}

	got, err := cf.blob(smallOID, 4096)
	if err != nil || string(got) != "small" {
		t.Fatalf("blob(small) = %q, %v", got, err)
	}
	if _, err := cf.blob(bigOID, 4096); !errors.Is(err, errTooBig) {
		t.Fatalf("blob(big) err = %v, want errTooBig", err)
	}
	// The process stays in sync after a refused blob.
	if got, err = cf.blob(smallOID, 4096); err != nil || string(got) != "small" {
		t.Fatalf("blob(small) after errTooBig = %q, %v", got, err)
	}

	oid, ok, err := cf.lookupPath(tree, "facts/x.md")
	if err != nil || !ok || oid != factOID {
		t.Fatalf("lookupPath = %q %v %v, want %s", oid, ok, err, factOID)
	}
	for _, rel := range []string{"facts/none.md", "nodir/x.md", "facts", ".attest/link", "facts/x.md/deeper"} {
		if oid, ok, err := cf.lookupPath(tree, rel); oid != "" || ok || err != nil {
			t.Fatalf("lookupPath(%q) = %q %v %v, want empty", rel, oid, ok, err)
		}
	}
}

// TestCatFileCommitHeader: git reads a commit's tree from the header's first
// line only, and fsck lets an extra header after committer through, so the
// parser must not let a later tree line win.
func TestCatFileCommitHeader(t *testing.T) {
	pinTools(t)
	repo := t.TempDir()
	gitIn(t, repo, "", "init", "-q", "--bare", repo)
	empty := gitIn(t, repo, "", "hash-object", "-t", "tree", "-w", "--stdin")
	blob := gitIn(t, repo, "x", "hash-object", "-w", "--stdin")
	other := gitIn(t, repo, "100644 blob "+blob+"\tx\n", "mktree")
	parent := gitIn(t, repo, "", "commit-tree", "-m", "p", empty)
	sig := "t <t@example.com> 1577836800 +0000"
	commit := func(literally bool, header string) string {
		t.Helper()
		args := []string{"hash-object", "-t", "commit", "-w", "--stdin"}
		if literally {
			args = append(args, "--literally")
		}
		return gitIn(t, repo, header+"\nmsg\n", args...)
	}
	second := commit(false, "tree "+empty+"\nparent "+parent+"\nauthor "+sig+"\ncommitter "+sig+"\ntree "+other+"\nparent "+parent+"\n")
	gitIn(t, repo, "", "fsck", "--strict", "--no-dangling")

	cf, err := newCatFile(t.Context(), mirror{dir: repo, private: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.close)
	tree, parents, err := cf.commit(second)
	if err != nil || tree != empty || !slices.Equal(parents, []string{parent}) {
		t.Fatalf("commit = %s %v %v, want %s [%s]", tree, parents, err, empty, parent)
	}

	for name, header := range map[string]string{
		"tree not first": "author " + sig + "\ntree " + empty + "\ncommitter " + sig + "\n",
		"tree not hex":   "tree " + strings.Repeat("z", 40) + "\nauthor " + sig + "\ncommitter " + sig + "\n",
		"tree short":     "tree " + empty[:39] + "\nauthor " + sig + "\ncommitter " + sig + "\n",
		"tree uppercase": "tree " + strings.ToUpper(empty) + "\nauthor " + sig + "\ncommitter " + sig + "\n",
		"parent not hex": "tree " + empty + "\nparent " + strings.Repeat("z", 40) + "\nauthor " + sig + "\ncommitter " + sig + "\n",
	} {
		oid := commit(true, header)
		if tree, parents, err := cf.commit(oid); err == nil {
			t.Errorf("%s: commit = %s %v, want an error", name, tree, parents)
		}
	}
}

func TestCatFileCancelKillsProcess(t *testing.T) {
	pinTools(t)
	repo := t.TempDir()
	gitIn(t, repo, "", "init", "-q", "-b", "main")
	ctx, cancel := context.WithCancel(t.Context())
	cf, err := newCatFile(ctx, mirror{dir: filepath.Join(repo, ".git"), private: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.close)
	cancel()
	if _, _, err := cf.info(strings.Repeat("0", 40)); err == nil {
		t.Fatal("info after cancel succeeded")
	}
}
