package tier1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/sanitize"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

type env struct {
	cfg                      config.Config
	personal, work           store.Root
	localPersonal, localWork store.Root
	rules                    gate.Rules
}

func newEnv(t *testing.T) env {
	t.Helper()
	cfg := config.Config{
		Profile:       "work",
		PersonalStore: t.TempDir(),
		WorkStore:     t.TempDir(),
		StateDir:      t.TempDir(),
	}
	rules, err := gate.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	return env{
		cfg:           cfg,
		personal:      store.CheckoutRoot(cfg, route.StorePersonal),
		work:          store.CheckoutRoot(cfg, route.StoreWork),
		localPersonal: store.LocalRoot(cfg, route.StorePersonal),
		localWork:     store.LocalRoot(cfg, route.StoreWork),
		rules:         rules,
	}
}

func (e env) assemble(s route.Session) (string, []string) {
	return Assemble(context.Background(), s, e.cfg, e.rules)
}

func mk(name, desc, scope string, repos ...string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: desc,
		Metadata: fact.Metadata{
			NodeType: "memory",
			Type:     "project",
			Scope:    scope,
			Repos:    repos,
			Modified: "2026-09-01T00:00:00Z",
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
	p := filepath.Join(root.Path, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func index(t *testing.T, roots ...store.Root) {
	t.Helper()
	for _, r := range roots {
		if _, err := r.WriteIndex(); err != nil {
			t.Fatal(err)
		}
	}
}

// addIndexLines appends hand-written lines to a root's index, the only way to
// get a line WriteIndex would never produce (or one whose file misbehaves).
func addIndexLines(t *testing.T, root store.Root, extra ...string) {
	t.Helper()
	lines, err := root.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, extra...)
	body := strings.Join(lines, "\n") + "\n"
	content := sanitize.Fence(sanitize.Header("test"), body, sanitize.NewDelimiter())
	if err := os.WriteFile(filepath.Join(root.Path, store.IndexFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type block struct {
	header, delim string
	lines         []string
	closed        bool
}

var (
	beginRE = regexp.MustCompile(`^===== BEGIN (priors-[0-9a-f]{16}) =====$`)
	endRE   = regexp.MustCompile(`^===== END (priors-[0-9a-f]{16}) =====$`)
)

// parseBlocks splits output into its fenced blocks; each block's lines are the
func parseBlocks(t *testing.T, out string) []block {
	t.Helper()
	var blocks []block
	var cur *block
	prev := ""
	for l := range strings.SplitSeq(out, "\n") {
		switch {
		case cur == nil && beginRE.MatchString(l):
			blocks = append(blocks, block{header: prev, delim: beginRE.FindStringSubmatch(l)[1]})
			cur = &blocks[len(blocks)-1]
		case cur != nil && endRE.MatchString(l):
			if endRE.FindStringSubmatch(l)[1] != cur.delim {
				t.Fatalf("END %q does not close BEGIN %q", l, cur.delim)
			}
			cur.closed = true
			cur = nil
		case cur != nil:
			cur.lines = append(cur.lines, l)
		}
		prev = l
	}
	for _, b := range blocks {
		if !b.closed {
			t.Fatalf("block %q has no END line:\n%s", b.header, out)
		}
	}
	return blocks
}

func innerRunes(blocks []block) int {
	n := 0
	for _, b := range blocks {
		for _, l := range b.lines {
			n += utf8.RuneCountInString(l) + 1
		}
	}
	return n
}

func hasLine(lines []string, prefix string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

var (
	workSession     = route.Session{Class: route.ClassWork, Repo: "acme-app"}
	personalSession = route.Session{Class: route.ClassPersonal, Repo: "repo-a"}
)

func seedStores(t *testing.T, e env) {
	t.Helper()
	put(t, e.personal, "_global/p-global.md", mk("p-global", "personal global", "global"))
	put(t, e.personal, "repo-a/p-own.md", mk("p-own", "personal own repo", "repo", "repo-a"))
	put(t, e.personal, "repo-b/p-other.md", mk("p-other", "personal other repo", "repo", "repo-b"))
	put(t, e.work, "_global/w-global.md", mk("w-global", "work global", "global"))
	put(t, e.work, "acme-app/w-own.md", mk("w-own", "work own repo", "repo", "acme-app"))
	index(t, e.personal, e.work)
}

func TestWorkSessionReadsWorkThenPersonal(t *testing.T) {
	e := newEnv(t)
	put(t, e.personal, "_global/p-global.md", mk("p-global", "personal global", "global"))
	put(t, e.work, "_global/w-global.md", mk("w-global", "work global", "global"))
	index(t, e.personal, e.work)

	out, _ := e.assemble(workSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2:\n%s", len(blocks), out)
	}
	if !strings.Contains(blocks[0].header, "work store") || !hasLine(blocks[0].lines, "- [w-global]") {
		t.Errorf("first block is not the work store:\n%s", out)
	}
	if !strings.Contains(blocks[1].header, "personal store") || !hasLine(blocks[1].lines, "- [p-global]") {
		t.Errorf("second block is not the personal store:\n%s", out)
	}
	if blocks[0].delim == blocks[1].delim {
		t.Errorf("both blocks share delimiter %q", blocks[0].delim)
	}
}

func TestPersonalOnlySessions(t *testing.T) {
	e := newEnv(t)
	seedStores(t, e)
	personalProfile := e
	personalProfile.cfg.Profile = "personal"

	cases := []struct {
		name string
		env  env
		s    route.Session
	}{
		{"personal org", e, personalSession},
		{"no repo", e, route.Session{Class: route.ClassNoRepo}},
		{"unresolvable", e, route.Session{Class: route.ClassUnresolvable, Repo: "mystery"}},
		{"work org on a personal profile", personalProfile, workSession},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := c.env.assemble(c.s)
			blocks := parseBlocks(t, out)
			if len(blocks) != 1 || !strings.Contains(blocks[0].header, "personal store") {
				t.Fatalf("want exactly the personal block:\n%s", out)
			}
			for _, name := range []string{"w-global", "w-own", "work store"} {
				if strings.Contains(out, name) {
					t.Errorf("output leaks %q:\n%s", name, out)
				}
			}
		})
	}
}

func TestInsideStoreFilter(t *testing.T) {
	e := newEnv(t)
	seedStores(t, e)

	out, _ := e.assemble(personalSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), out)
	}
	for _, want := range []string{"- [p-global]", "- [p-own]"} {
		if !hasLine(blocks[0].lines, want) {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "p-other") {
		t.Errorf("another repo's fact leaked:\n%s", out)
	}

	out, _ = e.assemble(route.Session{Class: route.ClassNoRepo})
	blocks = parseBlocks(t, out)
	if len(blocks) != 1 || len(blocks[0].lines) != 1 || !hasLine(blocks[0].lines, "- [p-global]") {
		t.Errorf("no-repo session must see only global facts:\n%s", out)
	}
}

func TestStaleIndexEntriesDropped(t *testing.T) {
	e := newEnv(t)
	put(t, e.personal, "_global/keep.md", mk("keep", "kept", "global"))
	put(t, e.personal, "_global/old.md", mk("old", "to be superseded", "global"))
	put(t, e.personal, "_global/renamed.md", mk("renamed", "to be renamed", "global"))
	index(t, e.personal)

	superseded := mk("old", "to be superseded", "global")
	superseded.Metadata.SupersededBy = "keep"
	put(t, e.personal, "_global/old.md", superseded)
	put(t, e.personal, "_global/renamed.md", mk("someone-else", "name no longer matches", "global"))

	out, _ := e.assemble(personalSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 1 || len(blocks[0].lines) != 1 || !hasLine(blocks[0].lines, "- [keep]") {
		t.Errorf("only the live, matching fact may remain:\n%s", out)
	}
}

func TestLocalLayer(t *testing.T) {
	e := newEnv(t)
	put(t, e.personal, "_global/p-global.md", mk("p-global", "checkout copy", "global"))
	index(t, e.personal)
	put(t, e.localPersonal, "repo-a/learned.md", mk("learned", "learned in repo-a", "repo", "repo-a"))
	put(t, e.localPersonal, "repo-b/elsewhere.md", mk("elsewhere", "learned in repo-b", "repo", "repo-b"))
	put(t, e.localPersonal, "_norepo/loose.md", mk("loose", "learned with no repo", "repo"))
	put(t, e.localPersonal, "repo-a/p-global.md", mk("p-global", "local copy of a checkout name", "repo", "repo-a"))
	index(t, e.localPersonal)

	const flagged = "- (flagged, unreviewed) "

	out, _ := e.assemble(personalSession)
	if !strings.Contains(out, flagged+"[learned](repo-a/learned.md)") {
		t.Errorf("own-repo local fact not shown flagged:\n%s", out)
	}
	for _, leak := range []string{"elsewhere", "loose"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked into a repo-a session:\n%s", leak, out)
		}
	}
	if n := strings.Count(out, "[p-global]"); n != 1 {
		t.Errorf("checkout name appears %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "local copy of a checkout name") {
		t.Errorf("local copy shadowed the checkout:\n%s", out)
	}

	out, _ = e.assemble(route.Session{Class: route.ClassPersonal, Repo: "repo-b"})
	if !strings.Contains(out, flagged+"[elsewhere](repo-b/elsewhere.md)") || strings.Contains(out, "[learned]") {
		t.Errorf("repo-b session sees the wrong local facts:\n%s", out)
	}

	out, _ = e.assemble(route.Session{Class: route.ClassNoRepo})
	if !strings.Contains(out, flagged+"[loose](_norepo/loose.md)") || strings.Contains(out, "[learned]") || strings.Contains(out, "[elsewhere]") {
		t.Errorf("no-repo session sees the wrong local facts:\n%s", out)
	}
}

func TestRedactionRuleExcludesFact(t *testing.T) {
	e := newEnv(t)
	token := "gh" + "p_" + strings.Repeat("Ab1", 12)
	leaky := mk("leaky", "has a secret", "global")
	leaky.Body = "value: " + token + "\n"
	put(t, e.personal, "_global/leaky.md", leaky)
	put(t, e.personal, "_global/clean.md", mk("clean", "no secret", "global"))
	index(t, e.personal)

	out, reports := e.assemble(personalSession)
	if strings.Contains(out, "leaky") || !strings.Contains(out, "[clean]") {
		t.Errorf("leaky fact not excluded, or clean fact lost:\n%s", out)
	}
	found := false
	for _, r := range reports {
		if strings.Contains(r, token) {
			t.Errorf("report leaks the token: %q", r)
		}
		if strings.HasPrefix(r, "excluded personal/_global/leaky.md: rule ") && strings.Contains(r, "github-token") {
			found = true
		}
	}
	if !found {
		t.Errorf("no report names the rel and rule id: %q", reports)
	}
}

func TestEscapingIndexLinesDropped(t *testing.T) {
	e := newEnv(t)
	put(t, e.personal, "_global/keep.md", mk("keep", "kept", "global"))
	put(t, e.personal, "_global/readable.md", mk("readable", "readable", "global"))
	index(t, e.personal)

	outside := t.TempDir()
	put(t, store.Root{Path: outside}, "escape.md", mk("escape", "outside the root", "global"))
	put(t, store.Root{Path: filepath.Dir(e.personal.Path)}, "outside.md", mk("outside", "outside the root", "global"))
	if err := os.Symlink(filepath.Join(outside, "escape.md"), filepath.Join(e.personal.Path, "_global", "link.md")); err != nil {
		t.Fatal(err)
	}
	addIndexLines(t, e.personal,
		"- [outside](../outside.md) — traversal",
		"- [escape](_global/link.md) — symlink out of the root",
	)

	out, reports := e.assemble(personalSession)
	if strings.Contains(out, "outside") || strings.Contains(out, "escape") {
		t.Errorf("escaping line survived:\n%s", out)
	}
	if !strings.Contains(out, "[keep]") || !strings.Contains(out, "[readable]") {
		t.Errorf("good lines lost:\n%s", out)
	}
	if len(reports) < 2 {
		t.Errorf("reports = %q, want one per dropped line", reports)
	}
}

func TestNonRegularFilesNeverOpened(t *testing.T) {
	e := newEnv(t)
	put(t, e.personal, "_global/keep.md", mk("keep", "kept", "global"))
	index(t, e.personal)
	if err := syscall.Mkfifo(filepath.Join(e.personal.Path, "_global", "pipe.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.personal.Path, "_global", "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	addIndexLines(t, e.personal, "- [pipe](_global/pipe.md) — a fifo", "- [dir](_global/dir.md) — a directory")

	out, _ := e.assemble(personalSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 1 || len(blocks[0].lines) != 1 || !hasLine(blocks[0].lines, "- [keep]") {
		t.Errorf("only the regular file may remain:\n%s", out)
	}
}

func TestUnreadableFactDropped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads chmod 000 files")
	}
	e := newEnv(t)
	put(t, e.personal, "_global/keep.md", mk("keep", "kept", "global"))
	put(t, e.personal, "_global/locked.md", mk("locked", "unreadable", "global"))
	index(t, e.personal)
	locked := filepath.Join(e.personal.Path, "_global", "locked.md")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	out, reports := e.assemble(personalSession)
	if strings.Contains(out, "locked") || !strings.Contains(out, "[keep]") {
		t.Errorf("unreadable fact not dropped alone:\n%s", out)
	}
	if len(reports) == 0 {
		t.Error("no report for the unreadable fact")
	}
}

func TestMissingIndexIsReported(t *testing.T) {
	e := newEnv(t)
	out, reports := e.assemble(personalSession)
	if out != "" || len(reports) == 0 {
		t.Errorf("out = %q, reports = %q; want empty output and a report", out, reports)
	}
}

func bulkFacts(t *testing.T, root store.Root, prefix string, n, descRunes int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s%02d", prefix, i)
		f := mk(name, strings.Repeat("x", descRunes), "global")
		// Newest first, so index order is name order.
		f.Metadata.Modified = fmt.Sprintf("2026-09-01T00:%02d:00Z", 59-i)
		put(t, root, "_global/"+name+".md", f)
	}
	index(t, root)
}

func TestBudgetTruncatesFromTheMiddle(t *testing.T) {
	e := newEnv(t)
	const n = 30
	bulkFacts(t, e.personal, "f", n, 200)
	want, err := e.personal.ReadIndex()
	if err != nil || len(want) != n {
		t.Fatalf("index = %d lines, err %v", len(want), err)
	}

	out, _ := e.assemble(personalSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), out)
	}
	lines := blocks[0].lines
	marker := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "… ") })
	if marker < 1 || marker == len(lines)-1 {
		t.Fatalf("marker at %d of %d lines; want one with lines on both sides", marker, len(lines))
	}
	if strings.Count(strings.Join(lines, "\n"), "entries omitted") != 1 {
		t.Errorf("want exactly one marker")
	}
	head, tail := lines[:marker], lines[marker+1:]
	if !slices.Equal(head, want[:len(head)]) {
		t.Errorf("head is not the first %d index lines", len(head))
	}
	if !slices.Equal(tail, want[n-len(tail):]) {
		t.Errorf("tail is not the last %d index lines", len(tail))
	}
	if wantMarker := fmt.Sprintf("… %d entries omitted …", n-len(head)-len(tail)); lines[marker] != wantMarker {
		t.Errorf("marker = %q, want %q", lines[marker], wantMarker)
	}
	if got := innerRunes(blocks); got > Budget || got < Budget-2*250 {
		t.Errorf("inner runes = %d, want just under %d", got, Budget)
	}
}

func TestBudgetSharedAcrossStores(t *testing.T) {
	e := newEnv(t)
	// 22 work lines cost 177 each (3894), leaving 106: room for one 47-rune
	// personal line and the marker, not for two lines and the marker.
	bulkFacts(t, e.work, "w", 22, 150)
	bulkFacts(t, e.personal, "p", 10, 20)
	personalIndex, err := e.personal.ReadIndex()
	if err != nil || len(personalIndex) == 0 {
		t.Fatalf("index = %d lines, err %v", len(personalIndex), err)
	}

	out, _ := e.assemble(workSession)
	blocks := parseBlocks(t, out)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), out)
	}
	if got := len(blocks[0].lines); got != 22 || strings.Contains(strings.Join(blocks[0].lines, "\n"), "omitted") {
		t.Errorf("work block has %d lines, want all 22 untruncated", got)
	}
	personal := blocks[1].lines
	if len(personal) != 2 || personal[0] != personalIndex[0] || personal[1] != "… 9 entries omitted …" {
		t.Errorf("personal block = %q, want first line plus a marker", personal)
	}
	if got := innerRunes(blocks); got > Budget {
		t.Errorf("inner runes = %d > %d", got, Budget)
	}
}

func TestBothStoresOverBudget(t *testing.T) {
	e := newEnv(t)
	bulkFacts(t, e.work, "w", 30, 200)
	bulkFacts(t, e.personal, "p", 30, 200)

	out, _ := e.assemble(workSession)
	blocks := parseBlocks(t, out)
	if len(blocks) == 0 || !strings.Contains(blocks[0].header, "work store") {
		t.Fatalf("work block must come first:\n%s", out)
	}
	if got := innerRunes(blocks); got > Budget {
		t.Errorf("inner runes = %d > %d", got, Budget)
	}
	if !strings.Contains(strings.Join(blocks[0].lines, "\n"), "omitted") {
		t.Error("work block should be truncated")
	}
	if len(blocks) == 2 && !strings.Contains(blocks[1].header, "personal store") {
		t.Errorf("second block is not personal:\n%s", out)
	}
}

func TestDelimiterDiffersBetweenCalls(t *testing.T) {
	e := newEnv(t)
	seedStores(t, e)
	a, _ := e.assemble(personalSession)
	b, _ := e.assemble(personalSession)
	first, second := parseBlocks(t, a), parseBlocks(t, b)
	if len(first) == 0 || len(second) == 0 || first[0].delim == second[0].delim {
		t.Errorf("delimiters %v and %v must differ", first, second)
	}
}

func TestCancelledContext(t *testing.T) {
	e := newEnv(t)
	seedStores(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, _ := Assemble(ctx, workSession, e.cfg, e.rules); out != "" {
		t.Errorf("out = %q, want empty", out)
	}
}
