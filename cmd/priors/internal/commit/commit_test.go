package commit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

func TestMain(m *testing.M) {
	committest.UnsetRepoEnv()
	os.Exit(m.Run())
}

const scannerHit = "FAKE-SCANNER-HIT"

// hitScanner reports every scanned file, or stdin, that holds scannerHit.
const hitScanner = `#!/bin/sh
if [ "$1" = dir ]; then
  sep=''
  printf '['
  for f in $(grep -rl '` + scannerHit + `' "$2"); do
    printf '%s{"RuleID":"fake","File":"%s"}' "$sep" "$f"
    sep=','
  done
  printf ']'
elif grep -q '` + scannerHit + `'; then
  printf '[{"RuleID":"fake"}]'
else
  printf '[]'
fi
`

var workOrgs = []string{"github.com/factify-inc"}

type fixture struct {
	cfg     config.Config
	root    store.Root
	rules   gate.Rules
	scanner gate.Scanner
}

func (fx fixture) dir() string { return fx.root.Path }

func (fx fixture) checkout(t *testing.T) string {
	t.Helper()
	return Checkout(context.Background(), fx.cfg, fx.root, fx.rules, fx.scanner, workOrgs, nil, "priors: test")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func head(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func lsFiles(t *testing.T, dir string) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(git(t, dir, "ls-files", "-z"), "\x00"), "\x00")
}

func staged(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "diff", "--cached", "--name-only"))
}

func cleanFact(name string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: "a clean fact about " + name,
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       "project",
			Scope:      "global",
			Confidence: "proposed",
			Modified:   "2026-09-01T00:00:00Z",
			Provenance: &fact.Provenance{Engine: "claude", Host: "laptop"},
		},
		Body: "plain body\n",
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func (fx fixture) put(t *testing.T, rel string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fx.dir(), filepath.FromSlash(rel)), b)
}

func (fx fixture) index(t *testing.T) {
	t.Helper()
	if _, _, err := fx.root.WriteIndex(fx.rules); err != nil {
		t.Fatal(err)
	}
}

// setup makes a personal store checkout whose HEAD holds one fact, its index
// and a .gitignore.
func setup(t *testing.T) fixture {
	t.Helper()
	tmp := t.TempDir()
	gitconfig := filepath.Join(tmp, "gitconfig")
	writeFile(t, gitconfig, []byte("[user]\n\tname = Priors Test\n\temail = priors@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"))
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	bin := filepath.Join(tmp, "scanner")
	writeFile(t, bin, []byte(hitScanner))
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	rules, err := gate.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "store")
	fx := fixture{
		cfg:     config.Config{Profile: "personal", PersonalStore: dir, StateDir: filepath.Join(tmp, "state")},
		root:    store.Root{Store: route.StorePersonal, Kind: store.KindCheckout, Path: dir},
		rules:   rules,
		scanner: gate.Scanner{Bin: bin},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	fx.put(t, "_global/base-fact.md", cleanFact("base-fact"))
	writeFile(t, filepath.Join(dir, ".gitignore"), []byte("# nothing ignored\n"))
	fx.index(t)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return fx
}

func TestCommitsCleanDirtySet(t *testing.T) {
	fx := setup(t)
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)

	if w := fx.checkout(t); w != "" {
		t.Fatalf("warning = %q", w)
	}
	committed := strings.Fields(git(t, fx.dir(), "log", "-1", "--name-only", "--format="))
	if !slices.Equal(committed, []string{"MEMORY.md", "_global/good-fact.md"}) {
		t.Errorf("last commit touched %v", committed)
	}
	if subject := strings.TrimSpace(git(t, fx.dir(), "log", "-1", "--format=%s")); subject != "priors: test" {
		t.Errorf("subject = %q", subject)
	}
	if out := git(t, fx.dir(), "status", "--porcelain"); out != "" {
		t.Errorf("checkout still dirty: %q", out)
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestCheckoutIgnoresInheritedRepoEnv(t *testing.T) {
	fx := setup(t)
	outer := filepath.Join(t.TempDir(), "outer")
	writeFile(t, filepath.Join(outer, "keep.txt"), []byte("keep\n"))
	git(t, outer, "init", "-q")
	git(t, outer, "add", "-A")
	git(t, outer, "commit", "-q", "-m", "outer")
	outerHead := head(t, outer)
	outerIndex, err := os.ReadFile(filepath.Join(outer, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	storeHead := head(t, fx.dir())
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)

	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(outer, ".git", "index"))
	w := fx.checkout(t)
	_ = os.Unsetenv("GIT_DIR")
	_ = os.Unsetenv("GIT_INDEX_FILE")

	if w != "" {
		t.Fatalf("warning = %q", w)
	}
	if got := head(t, outer); got != outerHead {
		t.Errorf("outer HEAD moved: %s -> %s", outerHead, got)
	}
	if got, err := os.ReadFile(filepath.Join(outer, ".git", "index")); err != nil || !slices.Equal(got, outerIndex) {
		t.Errorf("outer index changed (read err %v)", err)
	}
	if head(t, fx.dir()) == storeHead {
		t.Error("the store checkout got no commit")
	}
	if out := git(t, fx.dir(), "status", "--porcelain"); out != "" {
		t.Errorf("store still dirty: %q", out)
	}
}

func TestCommitsDeletion(t *testing.T) {
	fx := setup(t)
	if err := os.Remove(filepath.Join(fx.dir(), "_global", "base-fact.md")); err != nil {
		t.Fatal(err)
	}
	fx.index(t)

	if w := fx.checkout(t); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if got := strings.TrimSpace(git(t, fx.dir(), "log", "-1", "--name-status", "--format=")); !strings.Contains(got, "D\t_global/base-fact.md") {
		t.Errorf("last commit = %q, want the deletion", got)
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestNothingDirtyIsNoCommit(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	if w := fx.checkout(t); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("a clean checkout produced a commit")
	}
}

func TestRefusesUngatedPaths(t *testing.T) {
	tests := []struct {
		name    string
		plant   func(t *testing.T, fx fixture)
		path    string
		tracked bool
		// leak is text from the planted file the warning must not repeat.
		leak string
	}{
		{"secret fact", func(t *testing.T, fx fixture) {
			f := cleanFact("secret-fact")
			f.Body = "value " + scannerHit + "\n"
			fx.put(t, "_global/secret-fact.md", f)
		}, "_global/secret-fact.md", false, scannerHit},
		{"symlink", func(t *testing.T, fx fixture) {
			if err := os.Symlink("/etc/passwd", filepath.Join(fx.dir(), "_global", "passwd.md")); err != nil {
				t.Fatal(err)
			}
		}, "_global/passwd.md", false, ""},
		{"non-fact file", func(t *testing.T, fx fixture) {
			writeFile(t, filepath.Join(fx.dir(), "notes.txt"), []byte("jotted notes\n"))
		}, "notes.txt", false, "jotted"},
		{"gitignore edit", func(t *testing.T, fx fixture) {
			writeFile(t, filepath.Join(fx.dir(), ".gitignore"), []byte("*.secret\n"))
		}, ".gitignore", true, "*.secret"},
		{"work-name fact", func(t *testing.T, fx fixture) {
			f := cleanFact("leak-fact")
			f.Body = "deploys to factify-inc infrastructure\n"
			fx.put(t, "_global/leak-fact.md", f)
		}, "_global/leak-fact.md", false, "infrastructure"},
		{"url fact", func(t *testing.T, fx fixture) {
			f := cleanFact("linky-fact")
			f.Body = "see https://example.com/docs\n"
			fx.put(t, "_global/linky-fact.md", f)
		}, "_global/linky-fact.md", false, "example.com"},
		{"flagged fact", func(t *testing.T, fx fixture) {
			f := cleanFact("flagged-fact")
			f.Metadata.Flags = []string{"provenance:external"}
			fx.put(t, "_global/flagged-fact.md", f)
		}, "_global/flagged-fact.md", false, ""},
		{"self-reviewed fact", func(t *testing.T, fx fixture) {
			f := cleanFact("reviewed-fact")
			f.Metadata.Confidence = "reviewed"
			fx.put(t, "_global/reviewed-fact.md", f)
		}, "_global/reviewed-fact.md", false, ""},
		{"unparsable fact", func(t *testing.T, fx fixture) {
			writeFile(t, filepath.Join(fx.dir(), "_global", "broken.md"), []byte("no frontmatter\n"))
		}, "_global/broken.md", false, "frontmatter"},
		{"fact failing a fact rule", func(t *testing.T, fx fixture) {
			f := cleanFact("typeless-fact")
			f.Metadata.Type = "bogus"
			fx.put(t, "_global/typeless-fact.md", f)
		}, "_global/typeless-fact.md", false, ""},
		{"dot directory", func(t *testing.T, fx fixture) {
			fx.put(t, ".hidden/hidden-fact.md", cleanFact("hidden-fact"))
		}, ".hidden/hidden-fact.md", false, ""},
		{"root-level markdown", func(t *testing.T, fx fixture) {
			writeFile(t, filepath.Join(fx.dir(), "README.md"), []byte("readme\n"))
		}, "README.md", false, ""},
		{"deleted non-fact", func(t *testing.T, fx fixture) {
			if err := os.Remove(filepath.Join(fx.dir(), ".gitignore")); err != nil {
				t.Fatal(err)
			}
		}, ".gitignore", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t)
			before := head(t, fx.dir())
			tt.plant(t, fx)
			fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
			fx.index(t)

			w := fx.checkout(t)
			if !strings.Contains(w, tt.path) {
				t.Errorf("warning = %q, want it to name %s", w, tt.path)
			}
			if tt.leak != "" && strings.Contains(w, tt.leak) {
				t.Errorf("warning %q repeats the file's content", w)
			}
			if head(t, fx.dir()) != before {
				t.Error("HEAD moved")
			}
			if s := staged(t, fx.dir()); s != "" {
				t.Errorf("staged %q, want nothing", s)
			}
			files := lsFiles(t, fx.dir())
			if !tt.tracked && slices.Contains(files, tt.path) {
				t.Errorf("%s reached the git index", tt.path)
			}
			if slices.Contains(files, "_global/good-fact.md") {
				t.Error("the good fact was staged alongside a refused path")
			}
			committest.AssertHeadIndexInTree(t, fx.dir())

			if !tt.tracked {
				if err := os.RemoveAll(filepath.Join(fx.dir(), filepath.FromSlash(tt.path))); err != nil {
					t.Fatal(err)
				}
				fx.index(t)
				if w := fx.checkout(t); w != "" {
					t.Fatalf("after removing %s: warning = %q", tt.path, w)
				}
				if !slices.Contains(lsFiles(t, fx.dir()), "_global/good-fact.md") {
					t.Error("the good fact was not committed once the checkout was clean")
				}
				committest.AssertHeadIndexInTree(t, fx.dir())
			}
		})
	}
}

func TestRefusesIgnoredFactWalkWouldIndex(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	writeFile(t, filepath.Join(fx.dir(), ".git", "info", "exclude"), []byte("_global/x.md\n"))
	fx.put(t, "_global/x.md", cleanFact("x"))
	fx.index(t)

	if w := fx.checkout(t); !strings.Contains(w, "_global/x.md") {
		t.Errorf("warning = %q, want it to name _global/x.md", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("HEAD moved")
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestRefusesIgnoredDirectoryHoldingFacts(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	writeFile(t, filepath.Join(fx.dir(), ".git", "info", "exclude"), []byte("hidden-repo/\n"))
	fx.put(t, "hidden-repo/y.md", cleanFact("y"))
	fx.index(t)

	if w := fx.checkout(t); !strings.Contains(w, "hidden-repo/") {
		t.Errorf("warning = %q, want it to name hidden-repo/", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("HEAD moved")
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestIgnoredFileWalkSkipsDoesNotBlock(t *testing.T) {
	fx := setup(t)
	writeFile(t, filepath.Join(fx.dir(), ".git", "info", "exclude"), []byte("*.swp\n"))
	writeFile(t, filepath.Join(fx.dir(), "_global", ".good-fact.md.swp"), []byte("editor state\n"))
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)

	if w := fx.checkout(t); w != "" {
		t.Fatalf("warning = %q", w)
	}
	files := lsFiles(t, fx.dir())
	if !slices.Contains(files, "_global/good-fact.md") || slices.Contains(files, "_global/.good-fact.md.swp") {
		t.Errorf("git index holds %v", files)
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestRefusesSymlinkedIndex(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	index := filepath.Join(fx.dir(), store.IndexFile)
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", index); err != nil {
		t.Fatal(err)
	}
	if w := fx.checkout(t); !strings.Contains(w, store.IndexFile) {
		t.Errorf("warning = %q, want it to name %s", w, store.IndexFile)
	}
	if head(t, fx.dir()) != before {
		t.Error("HEAD moved")
	}
}

func TestIndexListingAbsentFileIsNotCommitted(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	fx.put(t, "_global/ghost-fact.md", cleanFact("ghost-fact"))
	fx.index(t)
	if err := os.Remove(filepath.Join(fx.dir(), "_global", "ghost-fact.md")); err != nil {
		t.Fatal(err)
	}

	if w := fx.checkout(t); !strings.Contains(w, "_global/ghost-fact.md") {
		t.Errorf("warning = %q, want it to name the unlisted file", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("HEAD moved")
	}
	if s := staged(t, fx.dir()); s != "" {
		t.Errorf("staged %q, want the index reset", s)
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}

func TestScannerUnavailableRefusesAll(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	fx.scanner = gate.Scanner{Bin: filepath.Join(t.TempDir(), "absent")}
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)

	if w := fx.checkout(t); !strings.Contains(w, "scanner") {
		t.Errorf("warning = %q, want a scanner refusal", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("HEAD moved")
	}
}

func TestNotARepoIsNothingToDo(t *testing.T) {
	fx := setup(t)
	fx.root.Path = t.TempDir()
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	if w := fx.checkout(t); w != "" {
		t.Errorf("warning = %q", w)
	}
}

func TestSubdirectoryOfRepoIsNotCommitted(t *testing.T) {
	fx := setup(t)
	parent := fx.dir()
	before := head(t, parent)
	fx.root.Path = filepath.Join(parent, "nested")
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)
	if w := fx.checkout(t); w != "" {
		t.Errorf("warning = %q", w)
	}
	if head(t, parent) != before {
		t.Error("the enclosing repo got a commit")
	}
}

func TestCommitDisabled(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	off := false
	fx.cfg.Commit = &off
	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)
	if w := fx.checkout(t); w != "" {
		t.Errorf("warning = %q", w)
	}
	if head(t, fx.dir()) != before {
		t.Error("committed with commit = false")
	}
}

func TestFailedCommitWarnsAndLaterCommitCatchesUp(t *testing.T) {
	fx := setup(t)
	before := head(t, fx.dir())
	hook := filepath.Join(fx.dir(), ".git", "hooks", "pre-commit")
	writeFile(t, hook, []byte("#!/bin/sh\necho hook says no\nexit 1\n"))
	if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec // a test hook
		t.Fatal(err)
	}
	fx.put(t, "_global/first-fact.md", cleanFact("first-fact"))
	fx.index(t)
	if w := fx.checkout(t); !strings.Contains(w, "git commit") {
		t.Fatalf("warning = %q, want a commit failure", w)
	}
	if head(t, fx.dir()) != before {
		t.Fatal("HEAD moved despite the failed commit")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(fx.dir(), "notes.txt"), []byte("jotted\n"))
	fx.put(t, "_global/second-fact.md", cleanFact("second-fact"))
	fx.index(t)
	if w := fx.checkout(t); !strings.Contains(w, "notes.txt") {
		t.Fatalf("warning = %q, want notes.txt refused", w)
	}
	if head(t, fx.dir()) != before {
		t.Fatal("HEAD moved with notes.txt in the checkout")
	}
	committest.AssertHeadIndexInTree(t, fx.dir())

	if err := os.Remove(filepath.Join(fx.dir(), "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if w := fx.checkout(t); w != "" {
		t.Fatalf("warning = %q", w)
	}
	files := lsFiles(t, fx.dir())
	for _, want := range []string{"_global/first-fact.md", "_global/second-fact.md"} {
		if !slices.Contains(files, want) {
			t.Errorf("HEAD lacks %s", want)
		}
	}
	committest.AssertHeadIndexInTree(t, fx.dir())
}
