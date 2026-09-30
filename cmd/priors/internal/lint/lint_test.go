package lint

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

func cleanFact(name string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: "a clean fact",
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       "project",
			Scope:      "repo",
			Repos:      []string{"repo-a"},
			Modified:   "2026-09-01T00:00:00Z",
			Provenance: &fact.Provenance{Engine: "claude", Host: "laptop"},
		},
		Body: "plain body\n",
	}
}

func checkout(t *testing.T, id route.StoreID) store.Root {
	t.Helper()
	root := store.Root{Store: id, Kind: store.KindCheckout, Path: t.TempDir()}
	if err := os.Mkdir(filepath.Join(root.Path, "repo-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
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

func index(t *testing.T, root store.Root) {
	t.Helper()
	if _, _, err := root.WriteIndex(testOptions(t).Rules); err != nil {
		t.Fatal(err)
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	rules, err := gate.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Store: route.StoreWork, Rules: rules}
}

// fakeScanner writes a scanner that ignores its arguments and prints report.
func fakeScanner(t *testing.T, report string) *gate.Scanner {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "scanner")
	script := "#!/bin/sh\nprintf '%s' '" + report + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	return &gate.Scanner{Bin: bin}
}

func ruleIDs(fs []Finding) []string {
	var ids []string
	for _, f := range fs {
		if !slices.Contains(ids, f.Rule) {
			ids = append(ids, f.Rule)
		}
	}
	slices.Sort(ids)
	return ids
}

// assertOnly fails unless got holds findings of exactly one rule, and returns
// the first of them.
func assertOnly(t *testing.T, got []Finding, rule string) Finding {
	t.Helper()
	if ids := ruleIDs(got); !slices.Equal(ids, []string{rule}) || len(got) == 0 {
		t.Fatalf("rules = %v, want only %q; findings: %v", ids, rule, got)
	}
	return got[0]
}

func find(fs []Finding, rule string) (Finding, bool) {
	for _, f := range fs {
		if f.Rule == rule {
			return f, true
		}
	}
	return Finding{}, false
}

func secretToken() string { return "AK" + "IA" + "IOSFODNN7EXAMPLE" }

func TestFindingString(t *testing.T) {
	got := Finding{File: "a/b.md", Rule: "type", Msg: "bad"}.String()
	if want := "a/b.md: type: bad"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestCleanStore(t *testing.T) {
	root := checkout(t, route.StorePersonal)
	a := cleanFact("fact-a")
	a.Body = "see [[fact-g]] and [[fact-g|the global one]]\n"
	g := cleanFact("fact-g")
	g.Metadata.Scope = "global"
	g.Metadata.Repos = nil
	old := cleanFact("fact-old")
	put(t, root, "repo-a/fact-a.md", a)
	put(t, root, "_global/fact-g.md", g)
	put(t, root, "_archive/repo-a/fact-old.md", old)
	index(t, root)

	opts := testOptions(t)
	opts.Store = route.StorePersonal
	opts.WorkOrgs = []string{"github.com/factify-inc"}
	opts.Gates = true
	opts.Scanner = fakeScanner(t, "[]")
	if got := Store(context.Background(), root, opts); len(got) != 0 {
		t.Errorf("clean store findings = %v, want none", got)
	}
}

func TestFactRules(t *testing.T) {
	tests := []struct {
		rule   string
		rel    string
		mutate func(*fact.Fact)
	}{
		{"type", "", func(f *fact.Fact) { f.Metadata.Type = "" }},
		{"type", "", func(f *fact.Fact) { f.Metadata.Type = "bogus" }},
		{"name-missing", "repo-a/x.md", func(f *fact.Fact) { f.Name = "" }},
		{"description-missing", "", func(f *fact.Fact) { f.Description = "" }},
		{"provenance", "", func(f *fact.Fact) { f.Metadata.Provenance = nil }},
		{"provenance", "", func(f *fact.Fact) { f.Metadata.Provenance.Host = "" }},
		{"provenance", "", func(f *fact.Fact) { f.Metadata.Provenance.Engine = "" }},
		{"name-format", "repo-a/Bad_Name.md", func(f *fact.Fact) { f.Name = "Bad_Name" }},
		{"repos-format", "", func(f *fact.Fact) { f.Metadata.Repos = []string{"Bad Repo"} }},
		{"scope", "", func(f *fact.Fact) { f.Metadata.Scope = "team" }},
		{"scope", "", func(f *fact.Fact) { f.Metadata.Repos = nil }},
		{"filename", "repo-a/other.md", func(f *fact.Fact) {}},
		{"secret", "", func(f *fact.Fact) { f.Body = "key " + secretToken() + "\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.rule, func(t *testing.T) {
			root := checkout(t, route.StoreWork)
			f := cleanFact("subject")
			tt.mutate(&f)
			rel := tt.rel
			if rel == "" {
				rel = "repo-a/subject.md"
			}
			put(t, root, rel, f)
			index(t, root)

			first := assertOnly(t, Store(context.Background(), root, testOptions(t)), tt.rule)
			if first.File != rel {
				t.Errorf("File = %q, want %q", first.File, rel)
			}
		})
	}
}

func TestFactRulesDirect(t *testing.T) {
	rules := testOptions(t).Rules
	e := store.Entry{Rel: "repo-a/subject.md", Fact: cleanFact("subject")}
	if got := FactRules(e, rules); len(got) != 0 {
		t.Errorf("clean entry findings = %v, want none", got)
	}
	e.Fact.Metadata.Type = ""
	if got := FactRules(e, rules); len(got) != 1 || got[0].Rule != "type" {
		t.Errorf("findings = %v, want one type finding", got)
	}
}

func TestSecretFindingNamesRuleNotText(t *testing.T) {
	root := checkout(t, route.StoreWork)
	f := cleanFact("subject")
	f.Body = "key " + secretToken() + "\n"
	put(t, root, "repo-a/subject.md", f)
	index(t, root)

	got := Store(context.Background(), root, testOptions(t))
	found, ok := find(got, "secret")
	if !ok {
		t.Fatalf("no secret finding in %v", got)
	}
	if !strings.Contains(found.Msg, "aws-access-key-id") {
		t.Errorf("Msg = %q, want the rule id", found.Msg)
	}
	for _, f := range got {
		if strings.Contains(f.String(), secretToken()) {
			t.Errorf("finding leaks the token: %q", f.String())
		}
	}
}

func TestParse(t *testing.T) {
	root := checkout(t, route.StoreWork)
	putRaw(t, root, "repo-a/broken.md", []byte("no frontmatter"))
	index(t, root)
	first := assertOnly(t, Store(context.Background(), root, testOptions(t)), "parse")
	if first.File != "repo-a/broken.md" {
		t.Errorf("File = %q", first.File)
	}
}

func TestDuplicateName(t *testing.T) {
	root := checkout(t, route.StoreWork)
	put(t, root, "repo-a/dup.md", cleanFact("dup"))
	put(t, root, "repo-b/dup.md", cleanFact("dup"))
	put(t, root, "_archive/repo-a/dup.md", cleanFact("dup"))
	index(t, root)

	got := Store(context.Background(), root, testOptions(t))
	assertOnly(t, got, "duplicate-name")
	var files []string
	for _, f := range got {
		files = append(files, f.File)
	}
	if want := []string{"repo-a/dup.md", "repo-b/dup.md"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want the later ones %v", files, want)
	}
}

func TestSupersededDangling(t *testing.T) {
	root := checkout(t, route.StoreWork)
	f := cleanFact("subject")
	f.Metadata.SupersededBy = "ghost"
	put(t, root, "repo-a/subject.md", f)
	index(t, root)
	assertOnly(t, Store(context.Background(), root, testOptions(t)), "superseded-dangling")

	ok := cleanFact("successor")
	put(t, root, "repo-a/successor.md", ok)
	f.Metadata.SupersededBy = "successor"
	put(t, root, "repo-a/subject.md", f)
	index(t, root)
	if got := Store(context.Background(), root, testOptions(t)); len(got) != 0 {
		t.Errorf("findings = %v, want none for a resolved superseded_by", got)
	}
}

func TestReposUnknown(t *testing.T) {
	root := checkout(t, route.StoreWork)
	f := cleanFact("subject")
	f.Metadata.Repos = []string{"repo-a", "ghost"}
	put(t, root, "repo-a/subject.md", f)
	index(t, root)
	got := Store(context.Background(), root, testOptions(t))
	assertOnly(t, got, "repos-unknown")
	if len(got) != 1 {
		t.Errorf("findings = %v, want one (only ghost)", got)
	}
}

func TestWikilinkDangling(t *testing.T) {
	root := checkout(t, route.StoreWork)
	f := cleanFact("subject")
	f.Body = "see [[other]] [[other|alias]] [[other#heading]] [[ other | padded ]] and [[nope|alias]]\n"
	put(t, root, "repo-a/subject.md", f)
	put(t, root, "repo-a/other.md", cleanFact("other"))
	index(t, root)

	got := Store(context.Background(), root, testOptions(t))
	assertOnly(t, got, "wikilink-dangling")
	if len(got) != 1 || !strings.Contains(got[0].Msg, "nope") {
		t.Errorf("findings = %v, want one naming nope", got)
	}
}

func TestOutsideTreeLink(t *testing.T) {
	for _, target := range []string{"../x", "a/b", `a\b`, "..", "../x|alias"} {
		t.Run(target, func(t *testing.T) {
			root := checkout(t, route.StoreWork)
			f := cleanFact("subject")
			f.Body = "see [[" + target + "]]\n"
			put(t, root, "repo-a/subject.md", f)
			index(t, root)
			assertOnly(t, Store(context.Background(), root, testOptions(t)), "outside-tree")
		})
	}
}

func TestOutsideTreeSymlink(t *testing.T) {
	root := checkout(t, route.StoreWork)
	outside := store.Root{Path: t.TempDir()}
	put(t, outside, "subject.md", cleanFact("subject"))
	if err := os.Symlink(filepath.Join(outside.Path, "subject.md"), filepath.Join(root.Path, "repo-a", "subject.md")); err != nil {
		t.Fatal(err)
	}
	index(t, root)

	first := assertOnly(t, Store(context.Background(), root, testOptions(t)), "outside-tree")
	if first.File != "repo-a/subject.md" {
		t.Errorf("File = %q", first.File)
	}
}

func TestWorkName(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
		msg  string
	}{
		{"org owner", "migration notes for Factify-Inc onboarding\n", true, "github.com/factify-inc"},
		{"work name", "ask the ACME team\n", true, "acme"},
		{"not a word match", "factify-incorporated and acmeware\n", false, ""},
		{"absent", "nothing here\n", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := checkout(t, route.StorePersonal)
			f := cleanFact("subject")
			f.Body = tt.body
			put(t, root, "repo-a/subject.md", f)
			index(t, root)

			opts := testOptions(t)
			opts.WorkOrgs = []string{"github.com/factify-inc"}
			opts.WorkNames = []string{"acme"}

			opts.Store = route.StorePersonal
			got := Store(context.Background(), root, opts)
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("findings = %v, want none", got)
				}
				return
			}
			if first := assertOnly(t, got, "work-name"); !strings.Contains(first.Msg, tt.msg) {
				t.Errorf("Msg = %q, want it to name %q", first.Msg, tt.msg)
			}

			opts.Store = route.StoreWork
			if got := Store(context.Background(), root, opts); len(got) != 0 {
				t.Errorf("work store findings = %v, want none", got)
			}
		})
	}
}

func TestIndexSync(t *testing.T) {
	t.Run("stale line set", func(t *testing.T) {
		root := checkout(t, route.StoreWork)
		put(t, root, "repo-a/first.md", cleanFact("first"))
		index(t, root)
		put(t, root, "repo-a/second.md", cleanFact("second"))
		first := assertOnly(t, Store(context.Background(), root, testOptions(t)), "index-sync")
		if first.File != "MEMORY.md" {
			t.Errorf("File = %q, want MEMORY.md", first.File)
		}
	})
	t.Run("missing index", func(t *testing.T) {
		root := checkout(t, route.StoreWork)
		put(t, root, "repo-a/first.md", cleanFact("first"))
		assertOnly(t, Store(context.Background(), root, testOptions(t)), "index-sync")
	})
	t.Run("empty store needs no index", func(t *testing.T) {
		root := checkout(t, route.StoreWork)
		if got := Store(context.Background(), root, testOptions(t)); len(got) != 0 {
			t.Errorf("findings = %v, want none", got)
		}
	})
}

func TestGateSecret(t *testing.T) {
	root := checkout(t, route.StoreWork)
	put(t, root, "repo-a/subject.md", cleanFact("subject"))
	put(t, root, "repo-a/other.md", cleanFact("other"))
	index(t, root)

	report := `[{"RuleID":"fake-rule","File":"` + filepath.Join(root.Path, "repo-a", "subject.md") + `"},` +
		`{"RuleID":"fake-rule","File":"` + filepath.Join(root.Path, "MEMORY.md") + `"}]`
	opts := testOptions(t)
	opts.Gates = true
	opts.Scanner = fakeScanner(t, report)

	got := Store(context.Background(), root, opts)
	assertOnly(t, got, "gate:secret")
	if len(got) != 1 || got[0].File != "repo-a/subject.md" || !strings.Contains(got[0].Msg, "fake-rule") {
		t.Errorf("findings = %v, want one fake-rule finding on repo-a/subject.md", got)
	}
}

func TestGateSecretUnavailable(t *testing.T) {
	root := checkout(t, route.StoreWork)
	put(t, root, "repo-a/subject.md", cleanFact("subject"))
	index(t, root)

	failing := fakeScanner(t, "not json")
	for name, scanner := range map[string]*gate.Scanner{
		"nil scanner":        nil,
		"unparsable report":  failing,
		"missing executable": {Bin: filepath.Join(t.TempDir(), "absent")},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions(t)
			opts.Gates = true
			opts.Scanner = scanner
			first := assertOnly(t, Store(context.Background(), root, opts), "gate:secret-unavailable")
			if first.File != "." {
				t.Errorf("File = %q, want \".\"", first.File)
			}
		})
	}
}

func TestGateContentAndSize(t *testing.T) {
	root := checkout(t, route.StoreWork)
	url := cleanFact("has-url")
	url.Body = "docs at https://example.com/page\n"
	big := cleanFact("too-big")
	big.Body = strings.Repeat("x", gate.MaxFactBytes+1) + "\n"
	put(t, root, "repo-a/has-url.md", url)
	put(t, root, "repo-a/too-big.md", big)
	index(t, root)

	opts := testOptions(t)
	if got := Store(context.Background(), root, opts); len(got) != 0 {
		t.Fatalf("gates off: findings = %v, want none", got)
	}

	opts.Gates = true
	opts.Scanner = fakeScanner(t, "[]")
	got := Store(context.Background(), root, opts)
	if ids, want := ruleIDs(got), []string{"gate:content:url", "gate:size"}; !slices.Equal(ids, want) {
		t.Fatalf("rules = %v, want %v; findings: %v", ids, want, got)
	}
	if f, _ := find(got, "gate:content:url"); f.File != "repo-a/has-url.md" {
		t.Errorf("url finding File = %q", f.File)
	}
	if f, _ := find(got, "gate:size"); f.File != "repo-a/too-big.md" {
		t.Errorf("size finding File = %q", f.File)
	}
	for _, f := range got {
		if strings.Contains(f.String(), "example.com") {
			t.Errorf("finding leaks matched text: %q", f.String())
		}
	}
}

func TestFindingsSorted(t *testing.T) {
	root := checkout(t, route.StoreWork)
	bad := cleanFact("zz")
	bad.Metadata.Type = ""
	bad.Description = ""
	put(t, root, "repo-a/zz.md", bad)
	putRaw(t, root, "repo-a/aa.md", []byte("broken"))
	index(t, root)

	got := Store(context.Background(), root, testOptions(t))
	want := []string{"repo-a/aa.md: parse", "repo-a/zz.md: description-missing", "repo-a/zz.md: type"}
	var have []string
	for _, f := range got {
		have = append(have, f.File+": "+f.Rule)
	}
	if !slices.Equal(have, want) {
		t.Errorf("order = %v, want %v", have, want)
	}
}

func TestMoveFlagged(t *testing.T) {
	root := checkout(t, route.StorePersonal)
	local := store.Root{Store: route.StorePersonal, Kind: store.KindLocal, Path: t.TempDir()}
	flagged := cleanFact("has-url")
	flagged.Metadata.Repos = []string{"repo-a", "repo-b"}
	flagged.Body = "docs at https://example.com/page\n"
	put(t, root, "repo-a/has-url.md", flagged)
	put(t, root, "repo-a/keeper.md", cleanFact("keeper"))
	index(t, root)

	moved, err := MoveFlagged(context.Background(), root, local, testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"repo-a/has-url.md"}; !slices.Equal(moved, want) {
		t.Errorf("moved = %v, want %v", moved, want)
	}
	if _, err := os.Stat(filepath.Join(root.Path, "repo-a", "has-url.md")); !os.IsNotExist(err) {
		t.Errorf("checkout still has the fact: %v", err)
	}
	entry, ok := local.FindByName("has-url")
	if !ok || entry.Rel != "repo-a/has-url.md" {
		t.Fatalf("local entry = %+v, %v; want repo-a/has-url.md", entry, ok)
	}
	if got := entry.Fact.Metadata.Flags; !slices.Equal(got, []string{"content:url"}) {
		t.Errorf("flags = %v, want [content:url]", got)
	}
	if got := entry.Fact.Metadata.Confidence; got != "proposed" {
		t.Errorf("confidence = %q, want proposed", got)
	}

	checkoutLines, err := root.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(checkoutLines, "\n"); strings.Contains(joined, "has-url") || !strings.Contains(joined, "keeper") {
		t.Errorf("checkout index = %q, want keeper only", joined)
	}
	localLines, err := local.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(localLines, "\n"); !strings.Contains(joined, "has-url") {
		t.Errorf("local index = %q, want has-url", joined)
	}
}

func TestMoveFlaggedNoRepoAndSize(t *testing.T) {
	root := checkout(t, route.StorePersonal)
	local := store.Root{Store: route.StorePersonal, Kind: store.KindLocal, Path: t.TempDir()}
	big := cleanFact("too-big")
	big.Metadata.Scope = "global"
	big.Metadata.Repos = nil
	big.Body = strings.Repeat("x", gate.MaxFactBytes+1) + "\n"
	put(t, root, "_global/too-big.md", big)
	index(t, root)

	moved, err := MoveFlagged(context.Background(), root, local, testOptions(t))
	if err != nil || len(moved) != 1 {
		t.Fatalf("moved = %v, err = %v", moved, err)
	}
	entry, ok := local.FindByName("too-big")
	if !ok || entry.Rel != "_norepo/too-big.md" {
		t.Fatalf("local entry = %+v, %v; want _norepo/too-big.md", entry, ok)
	}
	if got := entry.Fact.Metadata.Flags; !slices.Equal(got, []string{"size"}) {
		t.Errorf("flags = %v, want [size]", got)
	}
}

func TestMoveFlaggedRefusesExistingName(t *testing.T) {
	root := checkout(t, route.StorePersonal)
	local := store.Root{Store: route.StorePersonal, Kind: store.KindLocal, Path: t.TempDir()}
	flagged := cleanFact("has-url")
	flagged.Body = "docs at https://example.com/page\n"
	other := cleanFact("other-url")
	other.Body = "docs at https://example.com/other\n"
	put(t, root, "repo-a/has-url.md", flagged)
	put(t, root, "repo-a/other-url.md", other)
	index(t, root)
	put(t, local, "repo-a/has-url.md", cleanFact("has-url"))

	moved, err := MoveFlagged(context.Background(), root, local, testOptions(t))
	if err == nil || len(moved) != 0 {
		t.Fatalf("moved = %v, err = %v; want a refusal", moved, err)
	}
	for _, name := range []string{"has-url", "other-url"} {
		if _, err := os.Stat(filepath.Join(root.Path, "repo-a", name+".md")); err != nil {
			t.Errorf("%s was moved despite the refusal: %v", name, err)
		}
	}
	if _, ok := local.FindByName("other-url"); ok {
		t.Error("other-url reached local despite the refusal")
	}
}

func TestMoveFlaggedNothingToMove(t *testing.T) {
	root := checkout(t, route.StorePersonal)
	local := store.Root{Store: route.StorePersonal, Kind: store.KindLocal, Path: t.TempDir()}
	put(t, root, "repo-a/keeper.md", cleanFact("keeper"))
	index(t, root)

	moved, err := MoveFlagged(context.Background(), root, local, testOptions(t))
	if err != nil || len(moved) != 0 {
		t.Errorf("moved = %v, err = %v; want nothing", moved, err)
	}
}

func TestCandidate(t *testing.T) {
	tests := []struct {
		name  string
		root  store.Kind
		store route.StoreID
		edit  func(*fact.Fact)
		want  string
	}{
		{"clean", store.KindCheckout, route.StoreWork, func(*fact.Fact) {}, ""},
		{"duplicate name", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Name = "fact-a" }, "duplicate-name"},
		{"duplicate of an archived name", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Name = "fact-old" }, "duplicate-name"},
		{"superseded dangling", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Metadata.SupersededBy = "ghost" }, "superseded-dangling"},
		{"superseded resolved", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Metadata.SupersededBy = "fact-a" }, ""},
		{"first repo created by the write", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Metadata.Repos = []string{"repo-new"} }, ""},
		{"second repo unknown", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Metadata.Repos = []string{"repo-new", "ghost"} }, "repos-unknown"},
		{"global with unknown repo", store.KindCheckout, route.StoreWork, func(f *fact.Fact) {
			f.Metadata.Scope = "global"
			f.Metadata.Repos = []string{"repo-new"}
		}, "repos-unknown"},
		{"local root gets no exemption", store.KindLocal, route.StoreWork, func(f *fact.Fact) { f.Metadata.Repos = []string{"repo-new"} }, "repos-unknown"},
		{"link to existing and to itself", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Body = "see [[fact-a]] and [[new-fact|me]]\n" }, ""},
		{"link dangling", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Body = "see [[ghost]]\n" }, "wikilink-dangling"},
		{"link outside tree", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Body = "see [[../x]]\n" }, "outside-tree"},
		{"work name in personal", store.KindCheckout, route.StorePersonal, func(f *fact.Fact) { f.Body = "notes for factify-inc\n" }, "work-name"},
		{"work name in work", store.KindCheckout, route.StoreWork, func(f *fact.Fact) { f.Body = "notes for factify-inc\n" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := checkout(t, tt.store)
			root.Kind = tt.root
			put(t, root, "repo-a/fact-a.md", cleanFact("fact-a"))
			put(t, root, "_archive/repo-a/fact-old.md", cleanFact("fact-old"))

			f := cleanFact("new-fact")
			tt.edit(&f)
			rel := "repo-a/" + f.Name + ".md"
			if len(f.Metadata.Repos) > 0 {
				rel = f.Metadata.Repos[0] + "/" + f.Name + ".md"
			}
			opts := testOptions(t)
			opts.Store = tt.store
			opts.WorkOrgs = []string{"github.com/factify-inc"}

			got := Candidate(context.Background(), root, store.Entry{Root: root, Rel: rel, Fact: f}, opts)
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("findings = %v, want none", got)
				}
				return
			}
			first := assertOnly(t, got, tt.want)
			if len(got) != 1 || first.File != rel {
				t.Errorf("findings = %v, want one on %s", got, rel)
			}
		})
	}
}

func TestCandidateIgnoresStoreWideState(t *testing.T) {
	root := checkout(t, route.StoreWork)
	bad := cleanFact("bad")
	bad.Metadata.Type = "bogus"
	bad.Body = "see [[ghost]]\n"
	put(t, root, "repo-a/bad.md", bad)
	putRaw(t, root, "repo-a/broken.md", []byte("no frontmatter"))

	f := cleanFact("new-fact")
	got := Candidate(context.Background(), root, store.Entry{Root: root, Rel: "repo-a/new-fact.md", Fact: f}, testOptions(t))
	if len(got) != 0 {
		t.Errorf("findings = %v, want none: other facts' problems and a stale index are not the candidate's", got)
	}
}
