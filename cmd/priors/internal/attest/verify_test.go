package attest

import (
	"bytes"
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	personalID = "personal-test"
	workID     = "work-test"
	factRel    = "repo-a/some-fact.md"
	factName   = "some-fact"
	factReport = "personal/" + factRel
)

// recordingFS is an ownedFS that records every path read through it.
type recordingFS struct {
	ownedFS
	accessed []string
}

func (r *recordingFS) Lstat(name string) (fs.FileInfo, error) {
	r.accessed = append(r.accessed, "lstat "+name)
	return r.ownedFS.Lstat(name)
}

func (r *recordingFS) Readlink(name string) (string, error) {
	r.accessed = append(r.accessed, "readlink "+name)
	return r.ownedFS.Readlink(name)
}

func (r *recordingFS) ReadFile(name string) ([]byte, error) {
	r.accessed = append(r.accessed, "read "+name)
	return r.ownedFS.ReadFile(name)
}

func (r *recordingFS) reads(name string) int {
	return countOf(r.accessed, "read "+name)
}

func countOf[T comparable](s []T, v T) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// fixture is a separate host with one enrolled sk key, a checkout per store,
// and a verdict file per store that lists nothing yet.
type fixture struct {
	t      *testing.T
	key    attesttest.Key
	cfg    config.Config
	groups *fakeGroups
	fs     *recordingFS
	roots  map[route.StoreID]store.Root
	listed map[string][]string
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	old := VerifyUID
	VerifyUID = "993"
	t.Cleanup(func() { VerifyUID = old })

	key := attesttest.NewSKEd25519(t)
	cfg := separateCfg(key.PublicKey())
	cfg.PersonalStoreID, cfg.WorkStoreID, cfg.TrustDigest = personalID, workID, testTrustDigest

	tree := ownedFS{}
	for d := VerdictRoot; ; d = filepath.Dir(d) {
		tree[d] = ownedEntry{mode: fs.ModeDir | 0o755}
		if d == "/" {
			break
		}
	}
	f := &fixture{
		t:      t,
		key:    key,
		cfg:    cfg,
		groups: &fakeGroups{gids: []int{100}, names: map[int]string{100: "users"}},
		fs:     &recordingFS{ownedFS: tree},
		roots:  map[route.StoreID]store.Root{},
		listed: map[string][]string{},
		now:    time.Unix(testFetched, 0).Add(time.Hour),
	}
	for id, sid := range map[route.StoreID]string{route.StorePersonal: personalID, route.StoreWork: workID} {
		f.roots[id] = store.Root{Store: id, Kind: store.KindCheckout, Path: t.TempDir()}
		tree[filepath.Dir(VerdictPath(sid))] = ownedEntry{uid: testOwner, mode: fs.ModeDir | 0o755}
		f.writeVerdicts(sid)
	}
	return f
}

func (f *fixture) writeVerdicts(sid string) {
	lines := []string{
		"priors-verdicts v1",
		"store: " + sid,
		"trust: " + testTrustDigest,
		"tip: " + testTip,
		fmt.Sprintf("fetched: %d", testFetched),
		"status: ok",
	}
	f.fs.ownedFS[VerdictPath(sid)] = ownedEntry{uid: testOwner, mode: 0o644, data: verdictLines(append(lines, f.listed[sid]...)...)}
}

// list adds entry's digest to the store's verdict file in state.
func (f *fixture) list(sid string, entry []byte, state string) {
	f.listed[sid] = append(f.listed[sid], hexSHA256(entry)+" "+state)
	f.writeVerdicts(sid)
}

func claimingFact(name string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: "a reviewed fact",
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       "project",
			Scope:      "repo",
			Repos:      []string{"repo-a"},
			Modified:   "2026-09-01T00:00:00Z",
			Confidence: "reviewed",
			Provenance: &fact.Provenance{Engine: "claude", Host: "laptop"},
		},
		Body: "plain body\n",
	}
}

func (f *fixture) entry(id route.StoreID, rel string, ft fact.Fact) store.Entry {
	f.t.Helper()
	raw, err := ft.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	return store.Entry{Root: f.roots[id], Rel: rel, Fact: ft, Raw: raw}
}

func (f *fixture) claim(id route.StoreID, rel, name string) store.Entry {
	return f.entry(id, rel, claimingFact(name))
}

// sign is an attest entry for e signed by k with flags in namespace ns.
func (f *fixture) sign(k attesttest.Key, e store.Entry, sid string, flags byte, ns string) []byte {
	return attesttest.Signed(k, attesttest.Entry(sid, e.Fact.Name, e.Rel, hexSHA256(e.Raw), 1, "attest"), flags, ns)
}

func (f *fixture) signed(e store.Entry) []byte {
	return f.sign(f.key, e, TrustStoreID(f.cfg, e.Root.Store), 0x05, Namespace)
}

// put writes entry at .attest/<name> in the store's checkout.
func (f *fixture) put(id route.StoreID, name string, entry []byte) {
	f.t.Helper()
	dir := filepath.Join(f.roots[id].Path, ".attest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), entry, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// attested is a claiming personal fact with a valid entry listed reviewed.
func (f *fixture) attested(rel, name string) (store.Entry, []byte) {
	e := f.claim(route.StorePersonal, rel, name)
	raw := f.signed(e)
	f.put(route.StorePersonal, name, raw)
	f.list(personalID, raw, "reviewed")
	return e, raw
}

func (f *fixture) verifier() *Verifier {
	return NewVerifier(f.cfg, Options{Groups: f.groups, Now: func() time.Time { return f.now }, FS: f.fs})
}

// wantProposed asserts every e reads proposed and the reports are want, each
// either exact or, ending in "…", a prefix.
func wantProposed(t *testing.T, v *Verifier, want []string, es ...store.Entry) {
	t.Helper()
	for _, e := range es {
		if v.Reviewed(e) {
			t.Errorf("%s/%s reads reviewed", e.Root.Store, e.Rel)
		}
	}
	got := v.Reports()
	if len(got) != len(want) {
		t.Fatalf("reports = %q, want %q", got, want)
	}
	for i, w := range want {
		if p, ok := strings.CutSuffix(w, "…"); ok {
			if !strings.HasPrefix(got[i], p) {
				t.Errorf("report %d = %q, want prefix %q", i, got[i], p)
			}
		} else if got[i] != w {
			t.Errorf("report %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestReviewedValidEntry(t *testing.T) {
	f := newFixture(t)
	e, _ := f.attested(factRel, factName)
	v := f.verifier()

	if !v.Reviewed(e) {
		t.Fatalf("valid entry reads proposed: %q", v.Reports())
	}
	if r := v.Reports(); len(r) != 0 {
		t.Errorf("reports = %q, want none", r)
	}
}

func TestReviewedValidECDSAEntry(t *testing.T) {
	f := newFixture(t)
	k := attesttest.NewSKECDSA(t)
	f.cfg.AttestKeys = append(f.cfg.AttestKeys, config.AttestKey{Key: k.PublicKey()})
	e := f.claim(route.StorePersonal, factRel, factName)
	raw := f.sign(k, e, personalID, 0x05, Namespace)
	f.put(route.StorePersonal, factName, raw)
	f.list(personalID, raw, "reviewed")

	if v := f.verifier(); !v.Reviewed(e) {
		t.Fatalf("valid ecdsa entry reads proposed: %q", v.Reports())
	}
}

func TestReviewedNoEntry(t *testing.T) {
	for name, setup := range map[string]func(f *fixture){
		"no .attest dir": func(*fixture) {},
		"no entry file":  func(f *fixture) { f.put(route.StorePersonal, "other-fact", []byte("x")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			e := f.claim(route.StorePersonal, factRel, factName)
			setup(f)
			wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 3: no attest entry"}, e)
		})
	}
}

func TestReviewedVerdictState(t *testing.T) {
	for _, tc := range []struct {
		state, want string
	}{
		{"", factReport + ": proposed: check 3: attest entry not in the verdict file"},
		{"revoked", factReport + ": proposed: check 3: attest entry is revoked"},
		{"superseded 5", factReport + ": proposed: check 3: attest entry is superseded"},
		{"lapsed", factReport + ": stale attestation: re-attest or revoke"},
	} {
		t.Run(cmp.Or(tc.state, "absent"), func(t *testing.T) {
			f := newFixture(t)
			e := f.claim(route.StorePersonal, factRel, factName)
			raw := f.signed(e)
			f.put(route.StorePersonal, factName, raw)
			if tc.state != "" {
				f.list(personalID, raw, tc.state)
			}
			wantProposed(t, f.verifier(), []string{tc.want}, e)
		})
	}
}

func TestReviewedFactEditedAfterSigning(t *testing.T) {
	for name, edit := range map[string]func(ft *fact.Fact){
		"body":        func(ft *fact.Fact) { ft.Body = "plain body, edited\n" },
		"description": func(ft *fact.Fact) { ft.Description = "another description" },
		"scope":       func(ft *fact.Fact) { ft.Metadata.Scope = "global" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.attested(factRel, factName)
			ft := claimingFact(factName)
			edit(&ft)
			e := f.entry(route.StorePersonal, factRel, ft)
			wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 3: entry sha256 …"}, e)
		})
	}
}

func TestReviewedMoved(t *testing.T) {
	t.Run("wrong path", func(t *testing.T) {
		f := newFixture(t)
		e, _ := f.attested(factRel, factName)
		e.Rel = "repo-b/some-fact.md"
		wantProposed(t, f.verifier(), []string{"personal/repo-b/some-fact.md: proposed: check 3: entry path …"}, e)
	})
	t.Run("copied under another name", func(t *testing.T) {
		f := newFixture(t)
		_, raw := f.attested(factRel, factName)
		f.put(route.StorePersonal, "copied-fact", raw)
		e := f.claim(route.StorePersonal, "repo-a/copied-fact.md", "copied-fact")
		wantProposed(t, f.verifier(), []string{"personal/repo-a/copied-fact.md: proposed: check 6: entry name …"}, e)
	})
	t.Run("wrong store id", func(t *testing.T) {
		f := newFixture(t)
		e := f.claim(route.StorePersonal, factRel, factName)
		raw := f.sign(f.key, e, workID, 0x05, Namespace)
		f.put(route.StorePersonal, factName, raw)
		f.list(personalID, raw, "reviewed")
		wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 6: entry store …"}, e)
	})
}

func TestReviewedForgedVerdictLine(t *testing.T) {
	f := newFixture(t)
	e := f.claim(route.StorePersonal, factRel, factName)
	lines := attesttest.Entry(personalID, factName, factRel, hexSHA256(e.Raw), 1, "attest")
	raw := append(lines, "-----BEGIN SSH SIGNATURE-----\nZ2FyYmFnZQ==\n-----END SSH SIGNATURE-----\n"...)
	f.put(route.StorePersonal, factName, raw)
	f.list(personalID, raw, "reviewed")

	wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 4: …"}, e)
}

func TestReviewedBadSignature(t *testing.T) {
	cases := []struct {
		name  string
		sign  func(f *fixture, e store.Entry) []byte
		check int
	}{
		{"key not allowlisted", func(f *fixture, e store.Entry) []byte {
			return f.sign(attesttest.NewSKEd25519(t), e, personalID, 0x05, Namespace)
		}, 4},
		{"namespace git", func(f *fixture, e store.Entry) []byte {
			return f.sign(f.key, e, personalID, 0x05, "git")
		}, 4},
		{"allowlisted plain ed25519", func(f *fixture, e store.Entry) []byte {
			k := attesttest.NewEd25519(t)
			f.cfg.AttestKeys = append(f.cfg.AttestKeys, config.AttestKey{Key: k.PublicKey()})
			return f.sign(k, e, personalID, 0x05, Namespace)
		}, 5},
		{"no user presence", func(f *fixture, e store.Entry) []byte {
			return f.sign(f.key, e, personalID, 0x04, Namespace)
		}, 5},
		{"no user verification", func(f *fixture, e store.Entry) []byte {
			return f.sign(f.key, e, personalID, 0x01, Namespace)
		}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.claim(route.StorePersonal, factRel, factName)
			raw := tc.sign(f, e)
			f.put(route.StorePersonal, factName, raw)
			f.list(personalID, raw, "reviewed")
			wantProposed(t, f.verifier(), []string{fmt.Sprintf("%s: proposed: check %d: …", factReport, tc.check)}, e)
		})
	}
}

func TestReviewedKeyRemovedAfterSigning(t *testing.T) {
	f := newFixture(t)
	e, _ := f.attested(factRel, factName)
	f.cfg.AttestKeys = []config.AttestKey{{Key: attesttest.NewSKEd25519(t).PublicKey()}}

	wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 4: signing key is not on the allowlist"}, e)
}

func TestReviewedLocalLayer(t *testing.T) {
	f := newFixture(t)
	e, _ := f.attested(factRel, factName)
	e.Root.Kind = store.KindLocal

	wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 2: not in a checkout"}, e)
	if len(f.fs.accessed) != 0 {
		t.Errorf("verdict fs accessed: %q", f.fs.accessed)
	}
}

func TestReviewedHostileEntryFile(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, f *fixture)
		fact   string
		reason string
	}{
		{name: "symlinked .attest", reason: ".attest is not a directory", setup: func(t *testing.T, f *fixture) {
			real := filepath.Join(t.TempDir(), "attest")
			if err := os.Mkdir(real, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(f.roots[route.StorePersonal].Path, ".attest", factName), filepath.Join(real, factName)); err != nil {
				t.Fatal(err)
			}
			attestDir := filepath.Join(f.roots[route.StorePersonal].Path, ".attest")
			if err := os.Remove(attestDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, attestDir); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "entry is a symlink", reason: "attest entry is a symlink", setup: func(t *testing.T, f *fixture) {
			p := filepath.Join(f.roots[route.StorePersonal].Path, ".attest", factName)
			target := filepath.Join(t.TempDir(), "entry")
			if err := os.Rename(p, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "entry is a fifo", reason: "attest entry is not a regular file", setup: func(t *testing.T, f *fixture) {
			p := filepath.Join(f.roots[route.StorePersonal].Path, ".attest", factName)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "entry over 4 KiB", reason: "attest entry over 4 KiB", setup: func(t *testing.T, f *fixture) {
			big := bytes.Repeat([]byte("x"), MaxEntryBytes+1)
			f.put(route.StorePersonal, factName, big)
			f.list(personalID, big, "reviewed")
		}},
		{name: "bad fact name", fact: "../some-fact", reason: "bad name"},
		{name: "uppercase fact name", fact: "Some-fact", reason: "bad name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			e, _ := f.attested(factRel, factName)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			if tc.fact != "" {
				e = f.claim(route.StorePersonal, factRel, tc.fact)
			}
			wantProposed(t, f.verifier(), []string{factReport + ": proposed: check 3: " + tc.reason}, e)
		})
	}
}

func TestReviewedAttestationOff(t *testing.T) {
	cases := []struct {
		name string
		edit func(f *fixture)
		want string
	}{
		{"trust_root absent", func(f *fixture) { f.cfg.TrustRoot, f.cfg.TrustRootLabel = "", "absent" }, "attestation off: trust_root absent"},
		{"trust_root owner-admin", func(f *fixture) { f.cfg.TrustRoot, f.cfg.TrustRootLabel = "owner-admin", "owner-admin" }, "attestation off: trust_root owner-admin"},
		{"trust_root weird", func(f *fixture) { f.cfg.TrustRoot, f.cfg.TrustRootLabel = "weird", "weird" }, "attestation off: trust_root weird"},
		{"trust_root non-string", func(f *fixture) { f.cfg.TrustRoot, f.cfg.TrustRootLabel = "", "1" }, "attestation off: trust_root 1"},
		{"wheel group", func(f *fixture) {
			f.groups = &fakeGroups{gids: []int{100, 10}, names: map[int]string{100: "users", 10: "wheel"}}
		}, "attestation off: group wheel"},
		{"no key enrolled", func(f *fixture) { f.cfg.AttestKeys = nil }, "no attestation key enrolled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a, _ := f.attested(factRel, factName)
			b, _ := f.attested("repo-a/other-fact.md", "other-fact")
			tc.edit(f)

			wantProposed(t, f.verifier(), []string{"personal store: " + tc.want}, a, b)
			if len(f.fs.accessed) != 0 {
				t.Errorf("verdict fs accessed: %q", f.fs.accessed)
			}
		})
	}
}

func TestReviewedNoClaimingFact(t *testing.T) {
	f := newFixture(t)
	e, _ := f.attested(factRel, factName)
	e.Fact.Metadata.Confidence = "proposed"
	v := f.verifier()

	if v.Reviewed(e) {
		t.Error("unclaimed fact reads reviewed")
	}
	if r := v.Reports(); len(r) != 0 {
		t.Errorf("reports = %q, want none", r)
	}
	if len(f.fs.accessed) != 0 || f.groups.calls != 0 {
		t.Errorf("fs accessed %q, groups read %d times", f.fs.accessed, f.groups.calls)
	}
}

func TestReviewedCheck1Fails(t *testing.T) {
	path := VerdictPath(personalID)
	cases := []struct {
		name string
		edit func(f *fixture)
		want string
	}{
		{"verdict missing", func(f *fixture) { delete(f.fs.ownedFS, path) }, "no verdict file"},
		{"wrong owner", func(f *fixture) {
			e := f.fs.ownedFS[path]
			e.uid = 1000
			f.fs.ownedFS[path] = e
		}, "verdict file: " + path + " is owned by uid 1000, not uid 993…"},
		{"stale", func(f *fixture) { f.now = time.Unix(testFetched, 0).Add(25 * time.Hour) }, "verdicts stale: fetched 25h0m0s ago"},
		{"trust digest mismatch", func(f *fixture) { f.cfg.TrustDigest = digestA }, "verdict file is for another trust file"},
		{"owner not pinned", func(*fixture) { VerifyUID = "" }, "verdict owner not pinned"},
		{"owner not a uid", func(*fixture) { VerifyUID = "priors-verify" }, "verdict owner not pinned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a, _ := f.attested(factRel, factName)
			b, _ := f.attested("repo-a/other-fact.md", "other-fact")
			tc.edit(f)

			wantProposed(t, f.verifier(), []string{"personal store: check 1: " + tc.want}, a, b)
			if n := f.fs.reads(path); n > 1 {
				t.Errorf("verdict file read %d times", n)
			}
		})
	}
}

func TestReviewedOwnerNotPinnedReadsNothing(t *testing.T) {
	f := newFixture(t)
	e, _ := f.attested(factRel, factName)
	VerifyUID = ""

	wantProposed(t, f.verifier(), []string{"personal store: check 1: verdict owner not pinned"}, e)
	if len(f.fs.accessed) != 0 {
		t.Errorf("verdict fs accessed: %q", f.fs.accessed)
	}
}

func TestReviewedPerStoreCheck1(t *testing.T) {
	f := newFixture(t)
	a, _ := f.attested(factRel, factName)
	b, _ := f.attested("repo-a/other-fact.md", "other-fact")
	w := f.claim(route.StoreWork, factRel, factName)
	wraw := f.signed(w)
	f.put(route.StoreWork, factName, wraw)
	f.list(workID, wraw, "reviewed")
	delete(f.fs.ownedFS, VerdictPath(workID))
	v := f.verifier()

	for _, e := range []store.Entry{a, w, b, w} {
		got := v.Reviewed(e)
		if want := e.Root.Store == route.StorePersonal; got != want {
			t.Errorf("%s/%s: Reviewed = %v, want %v", e.Root.Store, e.Rel, got, want)
		}
	}
	if r, want := v.Reports(), []string{"work store: check 1: no verdict file"}; !slices.Equal(r, want) {
		t.Errorf("reports = %q, want %q", r, want)
	}
	if n := f.fs.reads(VerdictPath(personalID)); n != 1 {
		t.Errorf("personal verdict file read %d times, want 1", n)
	}
	if f.groups.calls == 0 || countOf(f.groups.nameCall, 100) != 1 {
		t.Errorf("groups classified %d times, want once", countOf(f.groups.nameCall, 100))
	}
}

func TestReportsDeduplicated(t *testing.T) {
	f := newFixture(t)
	e := f.claim(route.StorePersonal, factRel, factName)
	local := f.claim(route.StorePersonal, factRel, factName)
	local.Root.Kind = store.KindLocal
	v := f.verifier()

	for range 2 {
		v.Reviewed(local)
		v.Reviewed(e)
	}
	want := []string{
		factReport + ": proposed: check 2: not in a checkout",
		factReport + ": proposed: check 3: no attest entry",
	}
	if r := v.Reports(); !slices.Equal(r, want) {
		t.Errorf("reports = %q, want %q", r, want)
	}
}

func TestTrustStoreIDAndVerdictPath(t *testing.T) {
	cfg := config.Config{PersonalStoreID: personalID, WorkStoreID: workID}
	if got := TrustStoreID(cfg, route.StorePersonal); got != personalID {
		t.Errorf("personal = %q", got)
	}
	if got := TrustStoreID(cfg, route.StoreWork); got != workID {
		t.Errorf("work = %q", got)
	}
	if got, want := VerdictPath(personalID), filepath.Join(VerdictRoot, personalID, "verdicts"); got != want {
		t.Errorf("VerdictPath = %q, want %q", got, want)
	}
}
