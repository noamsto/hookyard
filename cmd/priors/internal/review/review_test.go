package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

func TestMain(m *testing.M) {
	toolstest.Pin()
	committest.UnsetRepoEnv()
	os.Exit(m.Run())
}

const personal = route.StorePersonal

func testFact(name string, prov *fact.Provenance, src *fact.Source) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: "about " + name,
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       "reference",
			Scope:      "global",
			Confidence: "proposed",
			Provenance: prov,
			Source:     src,
			Flags:      []string{"provenance:shell", "external"},
		},
		Body: "body of " + name + "\n",
	}
}

func put(t *testing.T, path string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
}

type fixture struct {
	cfg      config.Config
	checkout store.Root
	local    store.Root
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), PersonalStore: t.TempDir(), Push: true}
	return fixture{
		cfg:      cfg,
		checkout: store.Root{Store: personal, Kind: store.KindCheckout, Path: cfg.PersonalStore},
		local:    store.LocalRoot(cfg, personal),
	}
}

func (fx fixture) putLocal(t *testing.T, f fact.Fact) {
	t.Helper()
	put(t, fx.local.PathFor(f, "hookyard"), f)
}

func (fx fixture) putCheckout(t *testing.T, f fact.Fact) {
	t.Helper()
	put(t, fx.checkout.PathFor(f, ""), f)
}

func (fx fixture) pending(t *testing.T) []string {
	t.Helper()
	names, err := markers(fx.cfg.ReviewDir(string(personal)))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func (fx fixture) mark(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := MarkPending(fx.cfg, personal, n); err != nil {
			t.Fatal(err)
		}
	}
}

func fakePropose(t *testing.T, outcome commit.ReviewOutcome, detail string) *[]commit.ReviewRequest {
	t.Helper()
	var got []commit.ReviewRequest
	old := propose
	propose = func(_ context.Context, _ config.Config, _ store.Root, _ gate.Rules, _ gate.Scanner, req commit.ReviewRequest) (commit.ReviewOutcome, string) {
		got = append(got, req)
		return outcome, detail
	}
	t.Cleanup(func() { propose = old })
	return &got
}

func syncFx(fx fixture) (lines, warnings []string) {
	return SyncLocked(context.Background(), fx.cfg, personal, gate.Rules{}, gate.Scanner{})
}

func TestBranch(t *testing.T) {
	data := []byte("x")
	h8 := Branch("h", "n", data)[len("review/h/n-"):]
	if len(h8) != 8 {
		t.Fatalf("hash part %q", h8)
	}
	long := strings.Repeat("a", 60)
	tests := []struct{ host, want string }{
		{"Laptop.local", "review/laptop-local/n-" + h8},
		{"", "review/unknown/n-" + h8},
		{"..--..", "review/unknown/n-" + h8},
		{"-x_y-", "review/x-y/n-" + h8},
		{"ünï", "review/n/n-" + h8},
		{long, "review/" + long[:40] + "/n-" + h8},
		{strings.Repeat("a", 39) + "-b", "review/" + strings.Repeat("a", 39) + "/n-" + h8},
	}
	for _, tt := range tests {
		if got := Branch(tt.host, "n", data); got != tt.want {
			t.Errorf("Branch(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
	if Branch("h", "n", data) == Branch("h", "n", []byte("y")) {
		t.Error("the branch must follow the bytes")
	}
}

// Branch names are valid refs by construction, so Branch has no runtime check.
func TestBranchIsAValidRef(t *testing.T) {
	for _, host := range []string{"", "a..b", ".x", "-x", "x.lock", "a b/c", "日本", "A.B.C", strings.Repeat("-", 50)} {
		b := Branch(host, "name-1", []byte("d"))
		for seg := range strings.SplitSeq(b, "/") {
			if seg == "" || strings.Contains(seg, "..") || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "-") || strings.HasSuffix(seg, ".lock") {
				t.Errorf("Branch(%q) = %q has a bad segment %q", host, b, seg)
			}
		}
		if strings.ContainsAny(b, " ~^:?*[\\") {
			t.Errorf("Branch(%q) = %q has a forbidden character", host, b)
		}
	}
}

func TestBodyAndMessage(t *testing.T) {
	f := testFact("tip", &fact.Provenance{Engine: "claude", Session: "s1", Host: "laptop"}, &fact.Source{Engine: "claude", Path: "/m/tip.md"})
	f.Description = "line one\n```\n---\nline two"
	body := Body(f, personal, "_global/tip.md", f.Metadata.Flags)
	lines := strings.Split(body, "\n")
	for _, line := range []string{
		"priors flagged this fact for review. Merge to publish it as reviewed; close to keep it out of the store.",
		"priors lint ran locally on this branch's tree and was clean.",
		"- fact: tip (personal store, _global/tip.md)",
		"- flagged by: provenance:shell, external",
		"- source: engine claude, path /m/tip.md, session s1, host laptop",
	} {
		if !slices.Contains(lines, line) {
			t.Errorf("body lacks line %q:\n%s", line, body)
		}
	}
	desc := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- description: ") {
			desc++
		}
		if strings.HasPrefix(l, "```") || l == "---" || l == "line two" {
			t.Errorf("description reshaped the body with %q:\n%s", l, body)
		}
	}
	if desc != 1 || strings.Count(body, "\n- ") != 4 {
		t.Errorf("description reshaped the body:\n%s", body)
	}
	if got := Message(f, personal, "_global/tip.md", f.Metadata.Flags); got != "priors: review tip\n\n"+body {
		t.Errorf("Message = %q", got)
	}

	bare := Body(testFact("tip", nil, nil), personal, "a.md", nil)
	if !strings.Contains(bare, "- source: engine -, path -, session -, host -") {
		t.Errorf("missing values should read '-':\n%s", bare)
	}
}

func TestCompareURL(t *testing.T) {
	const b = "review/laptop/tip-0123abcd"
	const u = "https://github.com/o/r/compare/review/laptop/tip-0123abcd?expand=1"
	tests := []struct{ remote, want string }{
		{"git@github.com:o/r.git", u},
		{"git@github.com:o/r", u},
		{"ssh://git@github.com/o/r.git", u},
		{"ssh://git@github.com/o/r", u},
		{"https://github.com/o/r.git", ""},
		{"git@gitlab.com:o/r.git", ""},
		{"git@github.com:o/r/extra.git", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := CompareURL(tt.remote, b); got != tt.want {
			t.Errorf("CompareURL(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
	if got := CompareURL("git@github.com:o/r", "review/a b/x#1"); got != "https://github.com/o/r/compare/review/a%20b/x%231?expand=1" {
		t.Errorf("escaping: %q", got)
	}
}

func TestReviewed(t *testing.T) {
	f := testFact("tip", &fact.Provenance{Host: "h"}, nil)
	data, err := Reviewed(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fact.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Confidence != "reviewed" || len(got.Metadata.Flags) != 0 || got.Metadata.Provenance.Host != "h" {
		t.Errorf("Reviewed = %+v", got.Metadata)
	}
	if f.Metadata.Confidence != "proposed" || len(f.Metadata.Flags) != 2 {
		t.Error("Reviewed changed its argument")
	}
}

func TestMarkPending(t *testing.T) {
	fx := newFixture(t)
	if err := MarkPending(fx.cfg, personal, "../evil"); err == nil {
		t.Error("a path-like name was accepted")
	}
	fx.mark(t, "tip")
	if got := fx.pending(t); !reflect.DeepEqual(got, []string{"tip"}) {
		t.Errorf("pending = %v", got)
	}
	info, err := os.Stat(filepath.Join(fx.cfg.ReviewDir("personal"), "tip"))
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("marker: %v %v", info, err)
	}

	link := filepath.Join(fx.cfg.State(), "review")
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := MarkPending(fx.cfg, personal, "tip"); err == nil {
		t.Error("marked through a symlink below the state dir")
	}
}

func TestSyncPushOffKeepsMarkersAndWarnsOnce(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.Push = false
	for _, n := range []string{"a", "b"} {
		fx.putLocal(t, testFact(n, nil, nil))
	}
	fx.mark(t, "a", "b")
	lines, warnings := syncFx(fx)
	if len(lines) != 0 {
		t.Errorf("lines = %v", lines)
	}
	want := []string{"review a: push is off", "review b: push is off"}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
	if got := fx.pending(t); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("markers = %v", got)
	}
}

func TestSyncOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		outcome  commit.ReviewOutcome
		detail   string
		line     bool
		warning  string
		wantKept bool
	}{
		{"pushed", commit.ReviewPushed, "", true, "", false},
		{"exists", commit.ReviewExists, "", true, "", false},
		{"permanent", commit.ReviewPermanent, "tip.md already exists", false, "review tip: not proposed: tip.md already exists", false},
		{"transient", commit.ReviewTransient, "no upstream", false, "review tip: no upstream", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t)
			f := testFact("tip", &fact.Provenance{Host: "Box"}, nil)
			fx.putLocal(t, f)
			fx.mark(t, "tip")
			got := fakePropose(t, tt.outcome, tt.detail)
			lines, warnings := syncFx(fx)

			if len(*got) != 1 {
				t.Fatalf("propose calls = %d", len(*got))
			}
			req := (*got)[0]
			data, _ := Reviewed(f)
			if req.Rel != "_global/tip.md" || !reflect.DeepEqual(req.Data, data) || req.Branch != Branch("Box", "tip", data) ||
				req.Msg != Message(f, personal, "_global/tip.md", f.Metadata.Flags) {
				t.Errorf("request = %+v", req)
			}
			if tt.line != (len(lines) == 1) || (tt.line && lines[0] != "review personal "+req.Branch) {
				t.Errorf("lines = %q", lines)
			}
			if tt.warning == "" && len(warnings) != 0 || tt.warning != "" && !reflect.DeepEqual(warnings, []string{tt.warning}) {
				t.Errorf("warnings = %q, want %q", warnings, tt.warning)
			}
			if kept := len(fx.pending(t)) == 1; kept != tt.wantKept {
				t.Errorf("marker kept = %v, want %v", kept, tt.wantKept)
			}
			if _, ok := fx.local.FindByName("tip"); !ok {
				t.Error("the local fact must stay")
			}
		})
	}
}

func TestSyncDropsMarkerOfVanishedFact(t *testing.T) {
	fx := newFixture(t)
	fx.mark(t, "gone")
	got := fakePropose(t, commit.ReviewPushed, "")
	lines, warnings := syncFx(fx)
	if len(lines)+len(warnings)+len(*got) != 0 || len(fx.pending(t)) != 0 {
		t.Errorf("lines %q warnings %q calls %d markers %v", lines, warnings, len(*got), fx.pending(t))
	}
}

func TestSyncPrunesOnlyTheMatchingWriter(t *testing.T) {
	fx := newFixture(t)
	mine := &fact.Provenance{Engine: "claude", Session: "s1", Host: "h"}
	src := &fact.Source{Engine: "claude", Path: "/m/a.md", SHA256: "abc"}

	merged := testFact("merged", mine, src)
	other := testFact("other", &fact.Provenance{Engine: "claude", Session: "s2", Host: "h"}, src)
	diffSrc := testFact("diffsrc", mine, src)
	noSrc := testFact("nosrc", mine, nil)
	for _, f := range []fact.Fact{merged, other, diffSrc, noSrc} {
		fx.putLocal(t, f)
	}
	fx.putCheckout(t, merged)
	theirs := other
	theirs.Metadata.Provenance = &fact.Provenance{Engine: "claude", Session: "s9", Host: "h"}
	fx.putCheckout(t, theirs)
	theirDiff := diffSrc
	theirDiff.Metadata.Source = &fact.Source{Engine: "claude", Path: "/m/a.md", SHA256: "def"}
	fx.putCheckout(t, theirDiff)
	theirNo := noSrc
	theirNo.Metadata.Source = src
	fx.putCheckout(t, theirNo)
	fx.mark(t, "merged", "other")
	fakePropose(t, commit.ReviewTransient, "later")

	_, warnings := syncFx(fx)

	if _, ok := fx.local.FindByName("merged"); ok {
		t.Error("the matching copy was not pruned")
	}
	for _, n := range []string{"other", "diffsrc", "nosrc"} {
		if _, ok := fx.local.FindByName(n); !ok {
			t.Errorf("%s was pruned though the writer differs", n)
		}
	}
	if got := fx.pending(t); !reflect.DeepEqual(got, []string{"other"}) {
		t.Errorf("markers = %v", got)
	}
	if !reflect.DeepEqual(warnings, []string{"review other: later"}) {
		t.Errorf("warnings = %q", warnings)
	}
	index, err := os.ReadFile(filepath.Join(fx.local.Path, "MEMORY.md"))
	if err != nil || strings.Contains(string(index), "merged") {
		t.Errorf("local index not regenerated: %q %v", index, err)
	}
}
