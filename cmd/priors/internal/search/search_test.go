package search

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

func needRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
}

func newConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Profile:       "work",
		PersonalStore: t.TempDir(),
		WorkStore:     t.TempDir(),
		StateDir:      t.TempDir(),
	}
}

func newFact(name, desc, repo string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: desc,
		Metadata: fact.Metadata{
			NodeType: "memory",
			Type:     "project",
			Scope:    "repo",
			Repos:    []string{repo},
			Modified: "2026-09-01T00:00:00.000Z",
		},
		Body: "body\n",
	}
}

func put(t *testing.T, root store.Root, rel string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, root, rel, b)
}

func putRaw(t *testing.T, root store.Root, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(root.Path, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func personalSession(repo string) route.Session {
	return route.Session{Class: route.ClassPersonal, Repo: repo}
}

func builtinRules(t *testing.T) gate.Rules {
	t.Helper()
	r, err := gate.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, roots []store.Root, s route.Session, q Query) []Hit {
	t.Helper()
	hits, _, err := Run(context.Background(), Rg{}, roots, s, q, builtinRules(t))
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func names(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Entry.Fact.Name)
	}
	return out
}

func checkAndLocal(cfg config.Config, id route.StoreID) []store.Root {
	return []store.Root{store.CheckoutRoot(cfg, id), store.LocalRoot(cfg, id)}
}

func TestAllTermsMustMatchCaseInsensitively(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	both := newFact("both-terms", "Uses Postgres", "hookyard")
	both.Body = "The CACHE is warmed on boot.\n"
	onlyOne := newFact("only-one", "Uses postgres", "hookyard")
	put(t, co, "hookyard/both-terms.md", both)
	put(t, co, "hookyard/only-one.md", onlyOne)

	got := names(run(t, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"POSTGRES", "cache"}}))

	if want := []string{"both-terms"}; !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v", got, want)
	}
}

func TestTermMatchedOnlyInFrontmatterKeysIsNotAHit(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/a-fact.md", newFact("a-fact", "about caching", "hookyard"))

	got := run(t, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"confidence"}})

	if len(got) != 0 {
		t.Errorf("hits = %v, want none: frontmatter keys are not searchable text", names(got))
	}
}

func TestTypeScopeAndRepoFilters(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	ref := newFact("ref-fact", "shared topic", "hookyard")
	ref.Metadata.Type = "reference"
	proj := newFact("proj-fact", "shared topic", "hookyard")
	glob := newFact("glob-fact", "shared topic", "")
	glob.Metadata.Scope = "global"
	glob.Metadata.Repos = nil
	other := newFact("other-fact", "shared topic", "other")
	put(t, co, "hookyard/ref-fact.md", ref)
	put(t, co, "hookyard/proj-fact.md", proj)
	put(t, co, "_global/glob-fact.md", glob)
	put(t, co, "other/other-fact.md", other)
	s := personalSession("hookyard")
	roots := []store.Root{co}

	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"default session repo plus global", Query{}, []string{"glob-fact", "proj-fact", "ref-fact"}},
		{"type", Query{Type: "reference"}, []string{"ref-fact"}},
		{"scope global", Query{Scope: "global"}, []string{"glob-fact"}},
		{"scope repo", Query{Scope: "repo"}, []string{"proj-fact", "ref-fact"}},
		{"repo override", Query{Repo: "other"}, []string{"glob-fact", "other-fact"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.q.Terms = []string{"shared topic"}
			got := names(run(t, roots, s, c.q))
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("hits = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInsideStoreFilterVersusAnyRepo(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/mine.md", newFact("mine", "shared topic", "hookyard"))
	put(t, co, "other/theirs.md", newFact("theirs", "shared topic", "other"))
	roots := []store.Root{co}

	scoped := names(run(t, roots, personalSession("hookyard"), Query{Terms: []string{"shared topic"}}))
	anyRepo := names(run(t, roots, personalSession("hookyard"), Query{Terms: []string{"shared topic"}, AnyRepo: true}))
	noRepo := names(run(t, roots, route.Session{Class: route.ClassNoRepo}, Query{Terms: []string{"shared topic"}}))

	if want := []string{"mine"}; !slices.Equal(scoped, want) {
		t.Errorf("scoped = %v, want %v", scoped, want)
	}
	slices.Sort(anyRepo)
	if want := []string{"mine", "theirs"}; !slices.Equal(anyRepo, want) {
		t.Errorf("any-repo = %v, want %v", anyRepo, want)
	}
	if len(noRepo) != 0 {
		t.Errorf("no-repo session saw %v, want only global facts", noRepo)
	}
}

func TestArchivedAndSupersededAreHiddenUnlessAll(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/live.md", newFact("live", "shared topic", "hookyard"))
	old := newFact("old-one", "shared topic", "hookyard")
	put(t, co, "_archive/hookyard/old-one.md", old)
	sup := newFact("replaced", "shared topic", "hookyard")
	sup.Metadata.SupersededBy = "live"
	put(t, co, "hookyard/replaced.md", sup)
	roots := []store.Root{co}
	s := personalSession("hookyard")
	terms := []string{"shared topic"}

	def := names(run(t, roots, s, Query{Terms: terms}))
	all := names(run(t, roots, s, Query{Terms: terms, All: true, AnyRepo: true}))

	if want := []string{"live"}; !slices.Equal(def, want) {
		t.Errorf("default = %v, want %v", def, want)
	}
	slices.Sort(all)
	if want := []string{"live", "old-one", "replaced"}; !slices.Equal(all, want) {
		t.Errorf("all = %v, want %v", all, want)
	}
}

func TestRootLevelAndDotPathsAreNotFacts(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/real.md", newFact("real", "shared topic", "hookyard"))
	put(t, co, "top-level.md", newFact("top-level", "shared topic", "hookyard"))
	put(t, co, ".github/ci.md", newFact("ci", "shared topic", "hookyard"))
	put(t, co, "hookyard/.hidden/h.md", newFact("hidden", "shared topic", "hookyard"))
	putRaw(t, co, store.IndexFile, []byte("- [real](hookyard/real.md) shared topic\n"))

	got := names(run(t, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"shared topic"}, AnyRepo: true}))

	if want := []string{"real"}; !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v", got, want)
	}
}

func TestUnparsableFileIsSkippedAndReported(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/ok.md", newFact("ok", "shared topic", "hookyard"))
	putRaw(t, co, "hookyard/broken.md", []byte("shared topic but no frontmatter\n"))

	hits, reports, err := Run(context.Background(), Rg{}, []store.Root{co}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ok"}; !slices.Equal(names(hits), want) {
		t.Errorf("hits = %v, want %v", names(hits), want)
	}
	if want := []string{"unparsable personal/hookyard/broken.md"}; !slices.Equal(reports, want) {
		t.Errorf("reports = %v, want %v", reports, want)
	}
}

func TestSymlinkOutOfRootIsSkipped(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	outside := store.Root{Path: t.TempDir()}
	put(t, outside, "escape.md", newFact("escape", "shared topic", "hookyard"))
	if err := os.MkdirAll(filepath.Join(co.Path, "hookyard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside.Path, "escape.md"), filepath.Join(co.Path, "hookyard", "escape.md")); err != nil {
		t.Fatal(err)
	}

	got := run(t, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"shared topic"}})

	if len(got) != 0 {
		t.Errorf("hits = %v, want none: the symlink leaves the store", names(got))
	}
}

func TestLocalFactIsVisibleOnlyToItsLearnedRepo(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	local := store.LocalRoot(cfg, route.StorePersonal)
	put(t, local, "hookyard/learned.md", newFact("learned", "shared topic", "hookyard"))
	put(t, local, "other/elsewhere.md", newFact("elsewhere", "shared topic", "hookyard"))
	noRepoFact := newFact("floating", "shared topic", "")
	noRepoFact.Metadata.Repos = nil
	put(t, local, "_norepo/floating.md", noRepoFact)
	globalFact := newFact("pushy", "shared topic", "")
	globalFact.Metadata.Scope = "global"
	put(t, local, "other/pushy.md", globalFact)
	roots := []store.Root{local}
	terms := []string{"shared topic"}

	inRepo := names(run(t, roots, personalSession("hookyard"), Query{Terms: terms}))
	anyRepo := names(run(t, roots, personalSession("hookyard"), Query{Terms: terms, AnyRepo: true}))
	noRepo := names(run(t, roots, route.Session{Class: route.ClassNoRepo}, Query{Terms: terms, AnyRepo: true}))

	if want := []string{"learned"}; !slices.Equal(inRepo, want) {
		t.Errorf("learned repo = %v, want %v", inRepo, want)
	}
	if !slices.Equal(anyRepo, inRepo) {
		t.Errorf("AnyRepo = %v, want %v: it must not widen the local layer", anyRepo, inRepo)
	}
	if want := []string{"floating"}; !slices.Equal(noRepo, want) {
		t.Errorf("no-repo session = %v, want %v", noRepo, want)
	}
}

func TestReadRuleKeepsWorkOutOfPersonalSessions(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	put(t, store.CheckoutRoot(cfg, route.StorePersonal), "hookyard/p-fact.md", newFact("p-fact", "shared topic", "hookyard"))
	put(t, store.LocalRoot(cfg, route.StorePersonal), "hookyard/p-local.md", newFact("p-local", "shared topic", "hookyard"))
	put(t, store.CheckoutRoot(cfg, route.StoreWork), "hookyard/w-fact.md", newFact("w-fact", "shared topic", "hookyard"))
	put(t, store.LocalRoot(cfg, route.StoreWork), "hookyard/w-local.md", newFact("w-local", "shared topic", "hookyard"))
	q := Query{Terms: []string{"shared topic"}, AnyRepo: true}

	personal := personalSession("hookyard")
	pRoots := store.ReadRoots(cfg, route.ReadStores(personal, cfg))
	work := route.Session{Class: route.ClassWork, Repo: "hookyard"}
	wRoots := store.ReadRoots(cfg, route.ReadStores(work, cfg))

	gotPersonal := names(run(t, pRoots, personal, q))
	gotWork := names(run(t, wRoots, work, q))

	slices.Sort(gotPersonal)
	slices.Sort(gotWork)
	if want := []string{"p-fact", "p-local"}; !slices.Equal(gotPersonal, want) {
		t.Errorf("personal session = %v, want %v", gotPersonal, want)
	}
	if want := []string{"p-fact", "p-local", "w-fact", "w-local"}; !slices.Equal(gotWork, want) {
		t.Errorf("work session = %v, want %v", gotWork, want)
	}
}

func TestCheckoutWinsOverLocalByName(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	put(t, store.CheckoutRoot(cfg, route.StorePersonal), "hookyard/dup.md", newFact("dup", "shared topic reviewed", "hookyard"))
	put(t, store.LocalRoot(cfg, route.StorePersonal), "hookyard/dup.md", newFact("dup", "shared topic flagged", "hookyard"))
	roots := checkAndLocal(cfg, route.StorePersonal)

	got := run(t, roots, personalSession("hookyard"), Query{Terms: []string{"shared topic"}})

	if len(got) != 1 {
		t.Fatalf("hits = %v, want exactly one", names(got))
	}
	if got[0].Entry.Root.Kind != store.KindCheckout || got[0].Entry.Fact.Description != "shared topic reviewed" {
		t.Errorf("hit = %+v, want the checkout copy", got[0].Entry)
	}
}

func TestNameMatchRanksBeforeNewerBodyMatch(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	named := newFact("deploy-runbook", "older but named", "hookyard")
	named.Metadata.Modified = "2026-01-01T00:00:00.000Z"
	bodyOnly := newFact("unrelated", "newer", "hookyard")
	bodyOnly.Metadata.Modified = "2026-09-01T00:00:00.000Z"
	bodyOnly.Body = "mentions a DEPLOY step\n"
	newerBody := newFact("another", "newest", "hookyard")
	newerBody.Metadata.Modified = "2026-09-20T00:00:00.000Z"
	newerBody.Body = "also deploy\n"
	put(t, co, "hookyard/deploy-runbook.md", named)
	put(t, co, "hookyard/unrelated.md", bodyOnly)
	put(t, co, "hookyard/another.md", newerBody)

	got := names(run(t, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"deploy"}}))

	if want := []string{"deploy-runbook", "another", "unrelated"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLimit(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	for _, n := range []string{"aa", "bb", "cc", "dd", "ee"} {
		put(t, co, "hookyard/"+n+".md", newFact(n, "shared topic", "hookyard"))
	}
	s := personalSession("hookyard")

	limited := run(t, []store.Root{co}, s, Query{Terms: []string{"shared topic"}, Limit: 2})
	unlimited := run(t, []store.Root{co}, s, Query{Terms: []string{"shared topic"}})

	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d hits", len(limited))
	}
	if len(unlimited) != 5 {
		t.Errorf("default limit returned %d hits, want all 5", len(unlimited))
	}
}

func TestRedactionMatchIsExcludedAndReportedWithoutTheToken(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	token := "gh" + "p_" + strings.Repeat("a1", 18)
	leaky := newFact("leaky", "shared topic", "hookyard")
	leaky.Body = "token " + token + "\n"
	put(t, co, "hookyard/leaky.md", leaky)
	put(t, co, "hookyard/clean.md", newFact("clean", "shared topic", "hookyard"))

	hits, reports, err := Run(context.Background(), Rg{}, []store.Root{co}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"clean"}; !slices.Equal(names(hits), want) {
		t.Errorf("hits = %v, want %v", names(hits), want)
	}
	if want := []string{"excluded personal/hookyard/leaky.md: rule github-token"}; !slices.Equal(reports, want) {
		t.Errorf("reports = %v, want %v", reports, want)
	}
	for _, r := range reports {
		if strings.Contains(r, token) {
			t.Errorf("report leaks the token: %q", r)
		}
	}
}

func TestNoTermsIsAnError(t *testing.T) {
	_, _, err := Run(context.Background(), Rg{}, nil, personalSession("hookyard"), Query{}, builtinRules(t))

	if err == nil {
		t.Error("Run with no terms returned nil error")
	}
}

type stubBackend func(ctx context.Context, roots []string, term string) ([]string, error)

func (f stubBackend) Candidates(ctx context.Context, roots []string, term string) ([]string, error) {
	return f(ctx, roots, term)
}

func TestBackendErrorOnEveryRootIsReturned(t *testing.T) {
	boom := errors.New("boom")
	b := stubBackend(func(context.Context, []string, string) ([]string, error) { return nil, boom })
	roots := []store.Root{
		{Store: route.StorePersonal, Kind: store.KindCheckout, Path: t.TempDir()},
		{Store: route.StorePersonal, Kind: store.KindLocal, Path: t.TempDir()},
	}

	_, _, err := Run(context.Background(), b, roots, personalSession("hookyard"), Query{Terms: []string{"x"}}, builtinRules(t))

	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}
}

func TestBackendErrorOnOneRootKeepsTheOthers(t *testing.T) {
	cfg := newConfig(t)
	broken := store.CheckoutRoot(cfg, route.StorePersonal)
	good := store.CheckoutRoot(cfg, route.StoreWork)
	put(t, good, "hookyard/b.md", newFact("b", "shared topic", "hookyard"))
	b := stubBackend(func(_ context.Context, roots []string, _ string) ([]string, error) {
		if roots[0] == broken.Path {
			return nil, errors.New("boom")
		}
		return []string{filepath.Join(good.Path, "hookyard", "b.md")}, nil
	})

	hits, reports, err := Run(context.Background(), b, []store.Root{broken, good}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b"}; !slices.Equal(names(hits), want) {
		t.Errorf("hits = %v, want %v", names(hits), want)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "personal checkout") || !strings.Contains(reports[0], "boom") {
		t.Errorf("reports = %v, want one naming the personal checkout and its error", reports)
	}
}

func TestRgMissingBinaryFailsTheSearch(t *testing.T) {
	cfg := newConfig(t)
	roots := checkAndLocal(cfg, route.StorePersonal)
	put(t, roots[0], "hookyard/a.md", newFact("a", "shared topic", "hookyard"))

	_, _, err := Run(context.Background(), Rg{Bin: filepath.Join(t.TempDir(), "no-such-rg")}, roots,
		personalSession("hookyard"), Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err == nil {
		t.Error("Run with a missing rg returned nil error")
	}
}

func TestBackendBlockingPastTheDeadlineReturnsDeadlineExceeded(t *testing.T) {
	b := stubBackend(func(ctx context.Context, _ []string, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, errors.New("backend gave up")
	})
	roots := []store.Root{{Store: route.StorePersonal, Kind: store.KindCheckout, Path: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := Run(ctx, b, roots, personalSession("hookyard"), Query{Terms: []string{"x"}}, builtinRules(t))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestCanceledContextStopsBetweenCandidates(t *testing.T) {
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	put(t, co, "hookyard/a.md", newFact("a", "shared topic", "hookyard"))
	ctx, cancel := context.WithCancel(context.Background())
	b := stubBackend(func(_ context.Context, roots []string, _ string) ([]string, error) {
		cancel()
		return []string{filepath.Join(roots[0], "hookyard", "a.md")}, nil
	})

	_, _, err := Run(ctx, b, []store.Root{co}, personalSession("hookyard"), Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRgMissingBinaryIsAnError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")

	for _, roots := range [][]string{{dir}, {missing}} {
		_, err := Rg{Bin: filepath.Join(t.TempDir(), "no-such-rg")}.Candidates(context.Background(), roots, "x")

		if err == nil {
			t.Errorf("Candidates(%v) with a missing binary returned nil error", roots)
		}
	}
}

func TestRgCandidates(t *testing.T) {
	needRg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hit.md"), []byte("Some Needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "miss.md"), []byte("nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "absent")

	got, err := Rg{}.Candidates(context.Background(), []string{dir, missing}, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, "hit.md")}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}

	none, err := Rg{}.Candidates(context.Background(), []string{dir}, "absent-term")
	if err != nil || len(none) != 0 {
		t.Errorf("no match = %v, %v; want none, nil", none, err)
	}

	noRoots, err := Rg{}.Candidates(context.Background(), []string{missing}, "needle")
	if err != nil || len(noRoots) != 0 {
		t.Errorf("only missing roots = %v, %v; want none, nil", noRoots, err)
	}
}

func TestRgTermStartingWithDashIsALiteral(t *testing.T) {
	needRg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("use --force carefully\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Rg{}.Candidates(context.Background(), []string{dir}, "--force")

	if err != nil || len(got) != 1 {
		t.Errorf("candidates = %v, %v; want one", got, err)
	}
}

func TestRgCanceledContextIsAnError(t *testing.T) {
	needRg(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Rg{}.Candidates(ctx, []string{t.TempDir()}, "x")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRedactionMatchesRawFileBytes(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	leaky := newFact("leaky", "shared topic", "hookyard")
	leaky.Metadata.OriginSessionID = "gh" + "p_" + strings.Repeat("a1", 18)
	put(t, co, "hookyard/leaky.md", leaky)

	hits, reports, err := Run(context.Background(), Rg{}, []store.Root{co}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %v, want none: the token sits in the frontmatter", names(hits))
	}
	if want := []string{"excluded personal/hookyard/leaky.md: rule github-token"}; !slices.Equal(reports, want) {
		t.Errorf("reports = %v, want %v", reports, want)
	}
}

func lockedFile(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("shared topic\n"), 0o000); err != nil {
		t.Fatal(err)
	}
}

func TestRgUnreadableFileKeepsOtherMatches(t *testing.T) {
	needRg(t)
	dir := t.TempDir()
	lockedFile(t, filepath.Join(dir, "locked.md"))
	if err := os.WriteFile(filepath.Join(dir, "hit.md"), []byte("shared topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Rg{}.Candidates(context.Background(), []string{dir}, "shared topic")

	if !errors.Is(err, ErrPartial) {
		t.Errorf("err = %v, want ErrPartial", err)
	}
	if want := []string{filepath.Join(dir, "hit.md")}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestRgUnreadableFileAndNoMatchIsPartial(t *testing.T) {
	needRg(t)
	dir := t.TempDir()
	lockedFile(t, filepath.Join(dir, "locked.md"))

	got, err := Rg{}.Candidates(context.Background(), []string{dir}, "shared topic")

	if !errors.Is(err, ErrPartial) || len(got) != 0 {
		t.Errorf("candidates = %v, %v; want none, ErrPartial", got, err)
	}
}

func TestUnreadableFileInOneRootKeepsOtherRootsHits(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	a := store.CheckoutRoot(cfg, route.StorePersonal)
	b := store.CheckoutRoot(cfg, route.StoreWork)
	lockedFile(t, filepath.Join(a.Path, "hookyard", "locked.md"))
	put(t, b, "hookyard/b.md", newFact("b", "shared topic", "hookyard"))

	hits, reports, err := Run(context.Background(), Rg{}, []store.Root{a, b}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b"}; !slices.Equal(names(hits), want) {
		t.Errorf("hits = %v, want %v", names(hits), want)
	}
	wantSkipped(t, reports, "skipped personal/hookyard/locked.md: ")
}

func TestUnreadableFileInARootKeepsItsOtherHits(t *testing.T) {
	needRg(t)
	cfg := newConfig(t)
	co := store.CheckoutRoot(cfg, route.StorePersonal)
	lockedFile(t, filepath.Join(co.Path, "hookyard", "locked.md"))
	put(t, co, "hookyard/ok.md", newFact("ok", "shared topic", "hookyard"))

	hits, reports, err := Run(context.Background(), Rg{}, []store.Root{co}, personalSession("hookyard"),
		Query{Terms: []string{"shared topic"}}, builtinRules(t))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ok"}; !slices.Equal(names(hits), want) {
		t.Errorf("hits = %v, want %v", names(hits), want)
	}
	wantSkipped(t, reports, "skipped personal/hookyard/locked.md: ")
}

// wantSkipped requires reports to be exactly one line, starting with prefix.
func wantSkipped(t *testing.T, reports []string, prefix string) {
	t.Helper()
	if len(reports) != 1 || !strings.HasPrefix(reports[0], prefix) {
		t.Errorf("reports = %q, want one starting %q", reports, prefix)
	}
}
