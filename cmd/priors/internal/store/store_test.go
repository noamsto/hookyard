package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
)

func newFact(name, desc, modified string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: desc,
		Metadata: fact.Metadata{
			NodeType: "memory",
			Type:     "project",
			Scope:    "repo",
			Repos:    []string{"hookyard"},
			Modified: modified,
		},
		Body: "body\n",
	}
}

func writeFact(t *testing.T, root Root, rel string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, root, rel, b)
}

func writeRaw(t *testing.T, root Root, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(root.Path, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkoutRoot(t *testing.T) Root {
	t.Helper()
	return Root{Store: route.StorePersonal, Kind: KindCheckout, Path: t.TempDir()}
}

func rels(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Rel)
	}
	return out
}

func TestWalk(t *testing.T) {
	root := checkoutRoot(t)
	writeFact(t, root, "hookyard/b-fact.md", newFact("b-fact", "b", "2026-09-01T00:00:00Z"))
	writeFact(t, root, "hookyard/a-fact.md", newFact("a-fact", "a", "2026-09-01T00:00:00Z"))
	writeFact(t, root, "_global/g-fact.md", newFact("g-fact", "g", "2026-09-01T00:00:00Z"))
	writeFact(t, root, "_archive/hookyard/old-fact.md", newFact("old-fact", "old", "2026-08-01T00:00:00Z"))
	writeFact(t, root, "top-level.md", newFact("top-level", "root-level file is not a fact", ""))
	writeFact(t, root, ".github/ci.md", newFact("ci", "dot dir is not a fact dir", ""))
	writeFact(t, root, "hookyard/.hidden/h.md", newFact("h", "nested dot dir", ""))
	writeRaw(t, root, "hookyard/notes.txt", []byte("not markdown"))
	writeRaw(t, root, "hookyard/broken.md", []byte("no frontmatter"))

	entries, errs := root.Walk()

	want := []string{"_archive/hookyard/old-fact.md", "_global/g-fact.md", "hookyard/a-fact.md", "hookyard/b-fact.md"}
	if got := rels(entries); !slices.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	for _, e := range entries {
		if wantArchived := e.Rel == "_archive/hookyard/old-fact.md"; e.Archived != wantArchived {
			t.Errorf("%s: Archived = %v, want %v", e.Rel, e.Archived, wantArchived)
		}
		if e.Root != root || e.Fact.Name == "" || e.Size <= 0 {
			t.Errorf("%s: entry not filled in: %+v", e.Rel, e)
		}
	}
	if len(errs) != 1 || errs[0].Rel != "hookyard/broken.md" || errs[0].Err == nil {
		t.Errorf("errs = %+v, want one for hookyard/broken.md", errs)
	}
}

func TestWalkMissingRoot(t *testing.T) {
	root := Root{Store: route.StorePersonal, Kind: KindCheckout, Path: filepath.Join(t.TempDir(), "absent")}
	entries, errs := root.Walk()
	if len(entries) != 0 || len(errs) != 0 {
		t.Errorf("entries = %v, errs = %v, want none", entries, errs)
	}
}

func TestWalkSymlinks(t *testing.T) {
	root := checkoutRoot(t)
	outside := t.TempDir()
	b, err := newFact("linked", "via symlink", "").Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "linked.md"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.Path, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "linked.md"), filepath.Join(root.Path, "repo", "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root.Path, "dirlink")); err != nil {
		t.Fatal(err)
	}

	writeFact(t, root, "repo/real.md", newFact("real", "d", ""))
	if err := os.Symlink(filepath.Join(root.Path, "repo", "real.md"), filepath.Join(root.Path, "repo", "inside.md")); err != nil {
		t.Fatal(err)
	}

	entries, errs := root.Walk()

	if got, want := rels(entries), []string{"repo/real.md"}; !slices.Equal(got, want) {
		t.Errorf("entries = %v, want %v (no symlinked file or dir followed)", got, want)
	}
	var errRels []string
	for _, e := range errs {
		errRels = append(errRels, e.Rel)
	}
	if want := []string{"repo/inside.md", "repo/linked.md"}; !slices.Equal(errRels, want) {
		t.Errorf("errs = %+v, want one per symlinked file %v", errs, want)
	}
}

func TestFindByName(t *testing.T) {
	root := checkoutRoot(t)
	writeFact(t, root, "_archive/repo/dup.md", newFact("dup", "archived copy", ""))
	writeFact(t, root, "repo/dup.md", newFact("dup", "live copy", ""))
	writeFact(t, root, "repo/only-archived.md", newFact("only-archived", "x", ""))
	writeFact(t, root, "_archive/repo/only-archived.md", newFact("only-archived", "archived", ""))
	writeFact(t, root, "_archive/repo/gone.md", newFact("gone", "archived only", ""))

	tests := []struct {
		name     string
		wantRel  string
		wantOK   bool
		wantArch bool
	}{
		{"dup", "repo/dup.md", true, false},
		{"gone", "_archive/repo/gone.md", true, true},
		{"absent", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := root.FindByName(tc.name)
			if ok != tc.wantOK || e.Rel != tc.wantRel || e.Archived != tc.wantArch {
				t.Errorf("FindByName(%q) = %q archived=%v ok=%v", tc.name, e.Rel, e.Archived, ok)
			}
		})
	}
}

func TestPathFor(t *testing.T) {
	global := newFact("g", "d", "")
	global.Metadata.Scope = "global"
	global.Metadata.Repos = nil
	repo := newFact("r", "d", "")
	repo.Metadata.Repos = []string{"first", "second"}

	tests := []struct {
		name        string
		kind        Kind
		f           fact.Fact
		learnedRepo string
		want        string
	}{
		{"checkout global", KindCheckout, global, "ignored", "/s/_global/g.md"},
		{"checkout repo", KindCheckout, repo, "ignored", "/s/first/r.md"},
		{"local learned repo", KindLocal, repo, "learned", "/s/learned/r.md"},
		{"local no repo", KindLocal, global, "", "/s/_norepo/g.md"},
		{"quarantine learned repo", KindQuarantine, repo, "learned", "/s/learned/r.md"},
		{"quarantine no repo", KindQuarantine, repo, "", "/s/_norepo/r.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Root{Kind: tc.kind, Path: "/s"}
			if got := r.PathFor(tc.f, tc.learnedRepo); got != filepath.FromSlash(tc.want) {
				t.Errorf("PathFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfine(t *testing.T) {
	root := checkoutRoot(t)
	writeFact(t, root, "repo/ok.md", newFact("ok", "d", ""))

	outside := t.TempDir()
	b, err := newFact("escape", "d", "").Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.md"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "escape.md"), filepath.Join(root.Path, "repo", "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root.Path, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root.Path, "repo", "ok.md"), filepath.Join(root.Path, "repo", "inside.md")); err != nil {
		t.Fatal(err)
	}

	rejected := []string{"../x", "repo/../../x", "/abs/path.md", "repo\\x.md", "", "dirlink/escape.md", "repo/escape.md"}
	for _, rel := range rejected {
		t.Run("reject "+rel, func(t *testing.T) {
			if p, err := root.Confine(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Confine(%q) = %q, %v; want a confinement error", rel, p, err)
			}
		})
	}

	for _, rel := range []string{"repo/ok.md", "repo/inside.md"} {
		t.Run("accept "+rel, func(t *testing.T) {
			p, err := root.Confine(rel)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(root.Path, filepath.FromSlash(rel)); p != want {
				t.Errorf("Confine = %q, want %q", p, want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		p, err := root.Confine("repo/missing.md")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
		if want := filepath.Join(root.Path, "repo", "missing.md"); p != want {
			t.Errorf("path = %q, want %q", p, want)
		}
	})
}

func TestRoots(t *testing.T) {
	cfg := config.Config{PersonalStore: "/p", WorkStore: "/w", StateDir: "/state"}

	if got, want := CheckoutRoot(cfg, route.StorePersonal), (Root{route.StorePersonal, KindCheckout, "/p", ""}); got != want {
		t.Errorf("personal checkout = %+v, want %+v", got, want)
	}
	if got, want := CheckoutRoot(cfg, route.StoreWork), (Root{route.StoreWork, KindCheckout, "/w", ""}); got != want {
		t.Errorf("work checkout = %+v, want %+v", got, want)
	}
	if got, want := LocalRoot(cfg, route.StoreWork), (Root{route.StoreWork, KindLocal, filepath.Join("/state", "local", "work"), "/state"}); got != want {
		t.Errorf("work local = %+v, want %+v", got, want)
	}
	if got, want := QuarantineRoot(cfg), (Root{"", KindQuarantine, filepath.Join("/state", "quarantine"), "/state"}); got != want {
		t.Errorf("quarantine = %+v, want %+v", got, want)
	}

	got := ReadRoots(cfg, []route.StoreID{route.StoreWork, route.StorePersonal})
	want := []Root{
		CheckoutRoot(cfg, route.StoreWork), LocalRoot(cfg, route.StoreWork),
		CheckoutRoot(cfg, route.StorePersonal), LocalRoot(cfg, route.StorePersonal),
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadRoots = %+v, want %+v", got, want)
	}
}

func TestCheckWrite(t *testing.T) {
	newLocal := func(t *testing.T) Root {
		state := t.TempDir()
		return Root{Store: route.StoreWork, Kind: KindLocal, Path: filepath.Join(state, "local", "work"), State: state}
	}

	t.Run("clean state passes", func(t *testing.T) {
		r := newLocal(t)
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err != nil {
			t.Errorf("CheckWrite = %v, want nil", err)
		}
	})
	t.Run("layer dir symlinked elsewhere", func(t *testing.T) {
		r := newLocal(t)
		if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), r.Path); err != nil {
			t.Fatal(err)
		}
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err == nil {
			t.Error("CheckWrite through a symlinked layer dir = nil, want refusal")
		}
	})
	t.Run("git dir inside the layer", func(t *testing.T) {
		r := newLocal(t)
		if err := os.MkdirAll(filepath.Join(r.Path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err == nil {
			t.Error("CheckWrite inside a work tree = nil, want refusal")
		}
	})
	t.Run("git dir above the state dir", func(t *testing.T) {
		top := t.TempDir()
		if err := os.MkdirAll(filepath.Join(top, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(top, "state")
		r := Root{Store: route.StoreWork, Kind: KindLocal, Path: filepath.Join(state, "local", "work"), State: state}
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err == nil {
			t.Error("CheckWrite under a work tree = nil, want refusal")
		}
	})
	t.Run("empty state fails closed", func(t *testing.T) {
		r := newLocal(t)
		r.State = ""
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err == nil {
			t.Error("CheckWrite with no state dir = nil, want refusal")
		}
	})
	t.Run("checkout confines from its path", func(t *testing.T) {
		r := Root{Store: route.StorePersonal, Kind: KindCheckout, Path: t.TempDir()}
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err != nil {
			t.Fatalf("CheckWrite = %v, want nil", err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(r.Path, "repo")); err != nil {
			t.Fatal(err)
		}
		if err := r.CheckWrite(filepath.Join(r.Path, "repo")); err == nil {
			t.Error("CheckWrite through a symlink in the checkout = nil, want refusal")
		}
	})
}
