package attestcmd

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// listing returns every path under dir with its type and size.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %s %d", p, info.Mode(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCleanGit(t *testing.T) {
	ctx := context.Background()

	t.Run("fetchDefault", func(t *testing.T) {
		f := newFixture(t)
		r, err := newRepo(f.root, f.root+"/.git")
		if err != nil {
			t.Fatal(err)
		}
		want := f.commitPush(map[string][]byte{"facts/new.md": []byte("new\n")})
		refs := gitOut(t, f.root, "", "for-each-ref")
		pushDir := listing(t, tools.PushGitDir)

		branch, tip, err := r.fetchDefault(ctx, f.url)
		if err != nil {
			t.Fatal(err)
		}
		if branch != "main" || tip != want {
			t.Fatalf("fetchDefault = %q, %q; want main, %s", branch, tip, want)
		}
		gitOut(t, f.root, "", "cat-file", "-e", tip+"^{commit}")
		if got := gitOut(t, f.root, "", "for-each-ref"); got != refs {
			t.Fatalf("checkout refs changed:\n%s\nwant:\n%s", got, refs)
		}
		if got := listing(t, tools.PushGitDir); !slices.Equal(got, pushDir) {
			t.Fatalf("PushGitDir changed:\n%v\nwant:\n%v", got, pushDir)
		}
	})

	t.Run("isAncestor and has", func(t *testing.T) {
		f := newFixture(t)
		r, err := newRepo(f.root, f.root+"/.git")
		if err != nil {
			t.Fatal(err)
		}
		first := f.head(f.root)
		second := f.commitPush(nil)
		f.syncStore()

		for _, c := range []struct {
			a, b string
			want bool
		}{{first, second, true}, {second, first, false}, {first, first, true}} {
			got, err := r.isAncestor(ctx, c.a, c.b)
			if err != nil || got != c.want {
				t.Errorf("isAncestor(%.7s, %.7s) = %v, %v; want %v", c.a, c.b, got, err, c.want)
			}
		}
		if !r.has(ctx, first) {
			t.Error("has(known) = false")
		}
		if r.has(ctx, "0123456789abcdef0123456789abcdef01234567") {
			t.Error("has(unknown) = true")
		}
	})

	t.Run("history across a merge", func(t *testing.T) {
		f := newFixture(t)
		r, err := newRepo(f.root, f.root+"/.git")
		if err != nil {
			t.Fatal(err)
		}
		f.commitPush(map[string][]byte{".attest/h": []byte("base\n")})
		gitOut(t, f.work, "", "switch", "-q", "-c", "side")
		f.commitPush(map[string][]byte{".attest/h": []byte("side\n")})
		gitOut(t, f.work, "", "switch", "-q", "main")
		f.commitPush(map[string][]byte{".attest/h": []byte("main\n")})
		gitOut(t, f.work, "", "merge", "-q", "-s", "ours", "-m", "merge", "side")
		gitOut(t, f.work, "", "push", "-q", f.remote, "HEAD")
		f.syncStore()

		got, err := r.history(ctx, f.head(f.root), "h")
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"base\n", "side\n", "main\n"} {
			if !slices.ContainsFunc(got, func(b []byte) bool { return bytes.Equal(b, []byte(v)) }) {
				t.Errorf("history lacks %q: %q", v, got)
			}
		}
		if len(got) != 3 {
			t.Errorf("history has %d versions, want 3 distinct: %q", len(got), got)
		}
	})

	t.Run("blobAt", func(t *testing.T) {
		f := newFixture(t)
		r, err := newRepo(f.root, f.root+"/.git")
		if err != nil {
			t.Fatal(err)
		}
		c := f.commitPush(map[string][]byte{".attest/h": []byte("entry\n")})
		f.syncStore()

		got, ok, err := r.blobAt(ctx, c, ".attest/h", 1<<20)
		if err != nil || !ok || string(got) != "entry\n" {
			t.Fatalf("blobAt = %q, %v, %v", got, ok, err)
		}
		if _, ok, err := r.blobAt(ctx, c, ".attest/none", 1<<20); err != nil || ok {
			t.Fatalf("blobAt(missing) ok=%v err=%v; want false, nil", ok, err)
		}
	})

	t.Run("replace refs and planted graph", func(t *testing.T) {
		f := newFixture(t)
		r, err := newRepo(f.root, f.root+"/.git")
		if err != nil {
			t.Fatal(err)
		}
		first := f.head(f.root)
		f.commitPush(map[string][]byte{".attest/h": []byte("one\n")})
		head := f.commitPush(map[string][]byte{".attest/h": []byte("two\n")})
		f.syncStore()

		plantGraph(t, f.root, head)
		gitOut(t, f.root, "", "replace", "--graft", head)
		if got := gitOut(t, f.root, "", "rev-list", "--count", head); got != "1" {
			t.Fatalf("plain rev-list --count = %s, want 1", got)
		}

		hist, err := r.history(ctx, head, "h")
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Errorf("history has %d versions, want 2: %q", len(hist), hist)
		}
		got, err := r.isAncestor(ctx, first, head)
		if err != nil || !got {
			t.Errorf("isAncestor = %v, %v; want true", got, err)
		}
	})
}
