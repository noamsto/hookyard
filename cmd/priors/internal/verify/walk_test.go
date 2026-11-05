package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

type walkRepo struct {
	t    *testing.T
	repo string
	m    mirror
}

func newWalkRepo(t *testing.T) *walkRepo {
	t.Helper()
	pinTools(t)
	repo := t.TempDir()
	gitIn(t, repo, "", "init", "-q", "-b", "main")
	return &walkRepo{t: t, repo: repo, m: mirror{dir: filepath.Join(repo, ".git"), private: t.TempDir()}}
}

func (w *walkRepo) commit(files map[string][]byte, del ...string) string {
	w.t.Helper()
	for _, rel := range del {
		if err := os.Remove(filepath.Join(w.repo, rel)); err != nil {
			w.t.Fatal(err)
		}
	}
	for rel, data := range files {
		p := filepath.Join(w.repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	gitIn(w.t, w.repo, "", "add", "-A")
	gitIn(w.t, w.repo, "", "commit", "-q", "--allow-empty", "-m", "c")
	return gitIn(w.t, w.repo, "", "rev-parse", "HEAD")
}

func (w *walkRepo) tip(oid string) {
	w.t.Helper()
	gitIn(w.t, w.repo, "", "update-ref", tipsRef+oid, oid)
}

func (w *walkRepo) walkedRefs() []string {
	w.t.Helper()
	got, err := w.m.walkedRefs(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(got))
}

func (w *walkRepo) walk(ctx context.Context, r *Record, limit int) walkResult {
	w.t.Helper()
	cf, err := newCatFile(context.Background(), w.m)
	if err != nil {
		w.t.Fatal(err)
	}
	defer cf.close()
	res, err := w.m.walk(ctx, cf, r, "s", limit)
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func checkWalk(t *testing.T, got walkResult, n int, complete bool, frontier, covered []string) {
	t.Helper()
	if got.N != n || got.Complete != complete || !slices.Equal(got.Frontier, frontier) || !slices.Equal(got.Covered, covered) {
		t.Fatalf("walk = %+v, want N=%d Complete=%v Frontier=%v Covered=%v", got, n, complete, frontier, covered)
	}
}

func TestWalkIncrementalObserveAndLapse(t *testing.T) {
	w := newWalkRepo(t)
	ctx := context.Background()
	key := attesttest.NewSKEd25519(t)
	fact := []byte("fact a\n")
	entry := attesttest.Signed(key, attesttest.Entry("s", "a", "facts/a.md", hexSum(fact), 1, "attest"), 0x05, attest.Namespace)
	digest := hexSum(entry)
	unsigned := append(attesttest.Entry("s", "b", "facts/a.md", hexSum(fact), 3, "revoke"),
		"-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"...)

	r := newRecord()
	if res := w.walk(ctx, r, 10); res.N != 0 || !res.Complete {
		t.Fatalf("walk with no tips = %+v, want N=0 Complete", res)
	}

	c1 := w.commit(map[string][]byte{
		"facts/a.md":     fact,
		".attest/a":      entry,
		".attest/b":      unsigned,
		".attest/copy":   entry, // a valid entry under a name it does not name
		".attest/junk":   []byte("junk"),
		".attest/huge":   bytes.Repeat([]byte("x"), attest.MaxEntryBytes+1),
		".attest/other":  attesttest.Entry("t", "other", "x", hexSum(nil), 9, "attest"),
		"facts/other.md": []byte("o"),
	})
	// Removing the entry does not end its liveness: the edit below still lapses it.
	c2 := w.commit(map[string][]byte{"facts/other.md": []byte("o2")}, ".attest/a")
	c3 := w.commit(map[string][]byte{"facts/a.md": []byte("edited\n")})
	w.tip(c3)

	res := w.walk(ctx, r, 2)
	checkWalk(t, res, 2, false, []string{c2}, nil)
	ns := r.Names["a"]
	if ns == nil || ns.High != 1 || ns.Digest != digest || ns.Op != attest.OpAttest || ns.Tied ||
		!bytes.Equal(ns.Signer, key.PublicKey().Marshal()) {
		t.Fatalf("Names[a] = %+v", ns)
	}
	if nb := r.Names["b"]; nb == nil || nb.High != 3 || nb.Signer != nil {
		t.Fatalf("Names[b] = %+v, want unsigned high 3", nb)
	}
	if len(r.Names) != 2 {
		t.Fatalf("Names = %v, want only a and b", r.Names)
	}
	tr := r.Entries[digest]
	if len(r.Entries) != 1 || tr == nil || tr.Name != "a" || tr.Path != "facts/a.md" ||
		tr.SHA256 != hexSum(fact) || !slices.Equal(tr.Intros, []string{c1}) {
		t.Fatalf("Entries = %v (%+v)", r.Entries, tr)
	}
	if len(r.Lapsed) != 0 {
		t.Fatalf("Lapsed = %v before the edit was walked", r.Lapsed)
	}
	if err := w.m.checkpoint(ctx, res); err != nil {
		t.Fatal(err)
	}
	if got := w.walkedRefs(); !slices.Equal(got, []string{c2}) {
		t.Fatalf("walked refs = %v, want [%s]", got, c2)
	}

	// c3's parent was walked last run and no longer holds the entry: its live
	// set is rebuilt from the intros.
	res = w.walk(ctx, r, 10)
	checkWalk(t, res, 1, true, []string{c3}, []string{c2})
	if !r.Lapsed[digest] {
		t.Fatalf("Lapsed = %v, want %s lapsed by the edit", r.Lapsed, digest[:7])
	}
	if err := w.m.checkpoint(ctx, res); err != nil {
		t.Fatal(err)
	}
	if got := w.walkedRefs(); !slices.Equal(got, []string{c3}) {
		t.Fatalf("walked refs = %v, want [%s]", got, c3)
	}

	checkWalk(t, w.walk(ctx, r, 10), 0, true, nil, nil)

	if err := w.m.dropWalked(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.walkedRefs(); len(got) != 0 {
		t.Fatalf("walked refs after dropWalked = %v", got)
	}
	// One run over the whole history carries the live set from c1 through c2.
	fresh := newRecord()
	checkWalk(t, w.walk(ctx, fresh, 10), 3, true, []string{c3}, nil)
	if !fresh.Lapsed[digest] {
		t.Fatalf("fresh Lapsed = %v, want %s lapsed", fresh.Lapsed, digest[:7])
	}
	// A full re-walk into the same record is idempotent.
	before := cloneRecord(t, r)
	checkWalk(t, w.walk(ctx, r, 10), 3, true, []string{c3}, nil)
	if after := cloneRecord(t, r); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-walk changed the record:\n got %+v\nwant %+v", after, before)
	}
}

func TestWalkSideBranchLapseAndDeadline(t *testing.T) {
	w := newWalkRepo(t)
	key := attesttest.NewSKEd25519(t)
	fact := []byte("fact a\n")
	entry := attesttest.Signed(key, attesttest.Entry("s", "a", "facts/a.md", hexSum(fact), 1, "attest"), 0x05, attest.Namespace)
	digest := hexSum(entry)

	base := w.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": entry})
	side := w.commit(map[string][]byte{"facts/a.md": []byte("edited\n")})
	gitIn(t, w.repo, "", "reset", "-q", "--hard", base)
	w.commit(map[string][]byte{"other": []byte("x")})
	// The merge keeps main's fact, so the edit survives only on the side parent.
	gitIn(t, w.repo, "", "merge", "-q", "-s", "ours", "--no-edit", side)
	merge := gitIn(t, w.repo, "", "rev-parse", "HEAD")
	w.tip(merge)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { afterCommit = nil })
	afterCommit = func(_ context.Context, n int) {
		if n == 2 {
			cancel()
		}
	}
	r := newRecord()
	res := w.walk(ctx, r, 10)
	if res.N != 2 || res.Complete || len(res.Frontier) != 1 {
		t.Fatalf("walk cut by the deadline = %+v, want N=2 incomplete", res)
	}
	if err := w.m.checkpoint(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	afterCommit = nil

	res = w.walk(context.Background(), r, 10)
	if res.N != 2 || !res.Complete || !slices.Equal(res.Frontier, []string{merge}) {
		t.Fatalf("second walk = %+v, want the remaining 2 commits ending at the merge", res)
	}
	if !r.Lapsed[digest] {
		t.Fatalf("Lapsed = %v, want the side branch's edit to lapse %s", r.Lapsed, digest[:7])
	}
}
