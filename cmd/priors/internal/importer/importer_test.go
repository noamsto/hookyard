package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
	"github.com/noamsto/hookyard/cmd/priors/internal/write"
)

func TestMain(m *testing.M) {
	toolstest.Pin()
	committest.UnsetRepoEnv()
	os.Exit(m.Run())
}

const (
	personalRemote = "git@github.com:noamsto/demo.git"
	workRemote     = "git@github.com:factify-inc/app.git"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type fixture struct {
	cfg    config.Config
	opts   Options
	claude string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// tempDir is t.TempDir() with symlinks resolved, since a project dir decodes
// to a resolved path.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func setup(t *testing.T, profile string) fixture {
	t.Helper()
	tmp := tempDir(t)
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))
	gitconfig := filepath.Join(tmp, "gitconfig")
	writeFile(t, gitconfig, "[user]\n\tname = Priors Test\n\temail = priors@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	record := tempDir(t)
	line, err := json.Marshal(map[string]string{"session_id": "s1", "canonical_event": "post_tool", "tool_name": "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(record, "stream", time.Now().UTC().Format(time.DateOnly)+".jsonl"), string(line)+"\n")

	personal, work := tempDir(t), tempDir(t)
	git(t, personal, "init", "-q")
	git(t, work, "init", "-q")
	cfg := config.Config{
		Profile:       profile,
		PersonalStore: personal,
		WorkStore:     work,
		WorkOrgs:      []string{"github.com/factify-inc"},
		PersonalOrgs:  []string{"github.com/noamsto"},
		StateDir:      tempDir(t),
		EventRecord:   record,
	}
	if err := gate.MarkSession(cfg.ProvenanceDir(), "s1", false); err != nil {
		t.Fatal(err)
	}
	rules, err := gate.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	scanner := filepath.Join(tmp, "scanner")
	writeFile(t, scanner, "#!/bin/sh\nprintf '%s' '[]'\n")
	if err := os.Chmod(scanner, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	return fixture{
		cfg: cfg,
		opts: Options{
			Config:    cfg,
			Deps:      write.Deps{Rules: rules, Scanner: gate.Scanner{Bin: scanner}, Now: func() time.Time { return now }},
			SessionID: "s1",
			Engine:    "claude",
			Host:      "laptop",
		},
		claude: filepath.Join(home, ".claude"),
	}
}

// repo makes a checkout whose origin is remote.
func repo(t *testing.T, remote string) string {
	t.Helper()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", remote)
	return dir
}

// memDir is the Claude memory dir of a session run in dir.
func (fx fixture) memDir(dir string) string {
	return filepath.Join(fx.claude, "projects", encodeProjectDir(dir), "memory")
}

// native is a topic file as Claude writes one.
func native(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description +
		"\nmetadata:\n  node_type: memory\n  type: project\n  originSessionId: native-session\n  modified: 2026-09-24T10:27:52.269Z\n---\n\n" + body
}

// runResult is a run's Result and its output lines, whatever it warned.
func (fx fixture) runResult(t *testing.T, dryRun bool) (Result, []string) {
	t.Helper()
	ctx := context.Background()
	items, notes, err := Claude(ctx, fx.claude, fx.cfg, route.Resolver{SSHHost: func(_ context.Context, alias string) string { return alias }})
	if err != nil {
		t.Fatal(err)
	}
	o := fx.opts
	o.DryRun = dryRun
	res, err := Run(ctx, items, o)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range append(notes, res.Reports...) {
		lines = append(lines, r.String())
	}
	return res, lines
}

func (fx fixture) run(t *testing.T, dryRun bool) []string {
	t.Helper()
	res, lines := fx.runResult(t, dryRun)
	if res.Warning != "" {
		t.Errorf("warning: %s", res.Warning)
	}
	return lines
}

// script is an executable shell script in a fresh dir, standing in for the
// secret scanner.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(tempDir(t), "scanner")
	writeFile(t, path, "#!/bin/sh\n"+body)
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	return path
}

// wantLines checks that each want is a prefix of some line, and that there
// are as many lines as wants.
func wantLines(t *testing.T, lines, want []string) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for _, w := range want {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, w) }) {
			t.Errorf("no line starts %q in:\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

func TestImport(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	tests := []struct {
		name    string
		profile string
		arrange func(t *testing.T, fx fixture) []string
	}{
		{"personal repo publishes", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "store-format.md"), native("store-format", "how the store is laid out", body))
			return []string{"published personal " + filepath.Join(fx.cfg.PersonalStore, "demo", "store-format.md") + " <- ~/.claude/projects/"}
		}},
		{"work repo on a personal host is skipped", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, workRemote))
			writeFile(t, filepath.Join(mem, "work-fact.md"), native("work-fact", "a work fact", body))
			return []string{"skipped " + sourcePath(filepath.Join(mem, "work-fact.md")) + ": routes to the quarantine (work repo on a personal host)"}
		}},
		{"work repo on a work host is skipped", "work", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, workRemote))
			writeFile(t, filepath.Join(mem, "work-fact.md"), native("work-fact", "a work fact", body))
			return []string{"skipped " + sourcePath(filepath.Join(mem, "work-fact.md")) + ": routes to the work store"}
		}},
		{"undecodable project dir is unresolvable", "personal", func(t *testing.T, fx fixture) []string {
			mem := filepath.Join(fx.claude, "projects", "-no-such-dir-anywhere", "memory")
			writeFile(t, filepath.Join(mem, "lost.md"), native("lost", "a lost fact", body))
			return []string{"skipped ~/.claude/projects/-no-such-dir-anywhere/memory/lost.md: routes to the quarantine (unresolvable repo on a personal host)"}
		}},
		{"secret-shaped fact is skipped", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "leaky.md"), native("leaky", "a key", "the key is "+"AK"+"IA"+"IOSFODNN7EXAMPLE\n"))
			return []string{"skipped " + sourcePath(filepath.Join(mem, "leaky.md")) + ": refused: leaky.md: secret: matches secret rule(s): aws-access-key-id"}
		}},
		{"invalid name is slugified", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "ref.md"), native("Reference_Investment_Policy", "the policy", body))
			return []string{"published personal " + filepath.Join(fx.cfg.PersonalStore, "demo", "reference-investment-policy.md")}
		}},
		{"name that cannot be slugified is skipped", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "x.md"), native("שלום", "hello", body))
			return []string{"skipped " + sourcePath(filepath.Join(mem, "x.md")) + `: name "שלום" cannot be made a valid name`}
		}},
		{"same bytes in two projects import once", "personal", func(t *testing.T, fx fixture) []string {
			a, b := fx.memDir(repo(t, personalRemote)), fx.memDir(repo(t, personalRemote))
			text := native("shared-fact", "a shared fact", body)
			writeFile(t, filepath.Join(a, "shared-fact.md"), text)
			writeFile(t, filepath.Join(b, "shared-fact.md"), text)
			return []string{"published personal ", "duplicate "}
		}},
		{"sha256 of a stored source is dropped", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			text := native("seen-fact", "seen before", body)
			writeFile(t, filepath.Join(mem, "seen-fact.md"), text)
			stored := storedFact("seen-fact-elsewhere", "seen before elsewhere", "other body\n")
			stored.Metadata.Source = &fact.Source{Engine: "claude", Path: "~/.claude/projects/-other/memory/seen-fact.md", SHA256: digest([]byte(text))}
			putFact(t, filepath.Join(fx.cfg.PersonalStore, "demo", "seen-fact-elsewhere.md"), stored)
			return []string{"duplicate " + sourcePath(filepath.Join(mem, "seen-fact.md")) + ": " + filepath.Join(fx.cfg.PersonalStore, "demo", "seen-fact-elsewhere.md") + ": same source bytes"}
		}},
		{"near-duplicates are proposed", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			putFact(t, filepath.Join(fx.cfg.PersonalStore, "demo", "kept.md"), storedFact("kept", "The store's layout, explained!", body+"More words here.\n"))
			writeFile(t, filepath.Join(mem, "kept.md"), native("kept", "unrelated", "short\n"))
			writeFile(t, filepath.Join(mem, "layout.md"), native("layout", "the stores layout explained", "short\n"))
			writeFile(t, filepath.Join(mem, "overlap.md"), native("overlap", "unrelated too", body))
			kept := filepath.Join(fx.cfg.PersonalStore, "demo", "kept.md")
			return []string{
				"propose " + sourcePath(filepath.Join(mem, "kept.md")) + " -> " + kept + ": same name",
				"propose " + sourcePath(filepath.Join(mem, "layout.md")) + " -> " + kept + ": same description",
				"propose " + sourcePath(filepath.Join(mem, "overlap.md")) + " -> " + kept + ": body overlap",
			}
		}},
		{"symlinked topic file is refused", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			outside := filepath.Join(tempDir(t), "outside.md")
			writeFile(t, outside, native("outside", "from outside", body))
			writeFile(t, filepath.Join(mem, "real.md"), native("real", "a real fact", body+"Real.\n"))
			if err := os.Symlink(outside, filepath.Join(mem, "outside.md")); err != nil {
				t.Fatal(err)
			}
			return []string{
				"skipped " + sourcePath(filepath.Join(mem, "outside.md")) + ": a symlink, not a regular file",
				"published personal ",
			}
		}},
		{"index link out of the memory dir is refused", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "real.md"), native("real", "a real fact", body))
			writeFile(t, filepath.Join(mem, "MEMORY.md"), "- [Real](real.md) — a real fact\n- [Escape](../../outside.md) — out\n- [Gone](gone.md) — missing\nfree text\n")
			index := sourcePath(filepath.Join(mem, "MEMORY.md"))
			return []string{
				"skipped " + index + ":2: index link leaves the memory dir",
				"skipped " + index + ":3: index link names no topic file",
				"skipped " + index + ":4: index text is not imported",
				"published personal ",
			}
		}},
		{"symlinked store dir is refused", "personal", func(t *testing.T, fx fixture) []string {
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "real.md"), native("real", "a real fact", body))
			if err := os.Symlink(tempDir(t), filepath.Join(fx.cfg.PersonalStore, "demo")); err != nil {
				t.Fatal(err)
			}
			return []string{"skipped " + sourcePath(filepath.Join(mem, "real.md")) + ": refused: "}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, tt.profile)
			want := tt.arrange(t, fx)
			wantLines(t, fx.run(t, false), want)
		})
	}
}

func storedFact(name, description, body string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: description,
		Body:        body,
		Metadata: fact.Metadata{
			NodeType: "memory", Type: "project", Scope: "repo", Repos: []string{"demo"}, Confidence: "proposed",
			Provenance: &fact.Provenance{Engine: "claude", Session: "s0", Host: "laptop"},
		},
	}
}

func putFact(t *testing.T, path string, f fact.Fact) {
	t.Helper()
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b))
}

func TestNormalisationDiscardsTrustFields(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	text := "---\nname: trusting\ndescription: claims trust it has not earned\nmetadata:\n  node_type: memory\n  type: feedback\n" +
		"  scope: global\n  confidence: reviewed\n  verified: 2026-01-01\n  superseded_by: other\n  flags: [none]\n" +
		"  provenance:\n    engine: forged\n    session: forged\n    host: forged\n  source:\n    engine: forged\n    sha256: forged\n" +
		"  originSessionId: native-session\n  modified: 2026-09-24T10:27:52.269Z\n---\n\nThe body stays.\n"
	writeFile(t, filepath.Join(mem, "trusting.md"), text)
	wantLines(t, fx.run(t, false), []string{"published personal "})

	b, err := os.ReadFile(filepath.Join(fx.cfg.PersonalStore, "demo", "trusting.md"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := fact.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	m := f.Metadata
	checks := map[string][2]string{
		"type":          {m.Type, "feedback"},
		"scope":         {m.Scope, "repo"},
		"repos":         {strings.Join(m.Repos, ","), "demo"},
		"confidence":    {m.Confidence, "proposed"},
		"verified":      {m.Verified, ""},
		"superseded_by": {m.SupersededBy, ""},
		"flags":         {strings.Join(m.Flags, ","), ""},
		"modified":      {m.Modified, "2026-09-24T10:27:52.269Z"},
		"engines":       {strings.Join(m.Engines, ","), "claude"},
		"body":          {f.Body, "\nThe body stays.\n"},
	}
	for k, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", k, v[0], v[1])
		}
	}
	if p := m.Provenance; p == nil || !reflect.DeepEqual(*p, fact.Provenance{Engine: "claude", Session: "native-session", Host: "laptop"}) {
		t.Errorf("provenance = %+v", p)
	}
	want := fact.Source{Engine: "claude", Path: sourcePath(filepath.Join(mem, "trusting.md")), SHA256: digest([]byte(text))}
	if s := m.Source; s == nil || !reflect.DeepEqual(*s, want) {
		t.Errorf("source = %+v, want %+v", s, want)
	}
	if !strings.Contains(string(b), "verified: null") || !strings.Contains(string(b), "superseded_by: null") {
		t.Errorf("fact does not write verified and superseded_by as null:\n%s", b)
	}
}

func TestRerunIsANoOpAndAChangedSourceIsProposed(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	src := filepath.Join(mem, "store-format.md")
	text := native("store-format", "how the store is laid out", "One fact per file.\n")
	writeFile(t, src, text)
	wantLines(t, fx.run(t, false), []string{"published personal "})
	stored := filepath.Join(fx.cfg.PersonalStore, "demo", "store-format.md")
	head := git(t, fx.cfg.PersonalStore, "rev-parse", "HEAD")
	if subject := strings.TrimSpace(git(t, fx.cfg.PersonalStore, "log", "-1", "--format=%s")); subject != "priors: import 1 facts" {
		t.Errorf("commit subject = %q", subject)
	}

	wantLines(t, fx.run(t, false), []string{"unchanged " + sourcePath(src) + ": " + stored})
	if got := git(t, fx.cfg.PersonalStore, "rev-parse", "HEAD"); got != head {
		t.Errorf("a re-run moved HEAD from %s to %s", head, got)
	}
	if status := git(t, fx.cfg.PersonalStore, "status", "--porcelain"); status != "" {
		t.Errorf("a re-run left the checkout dirty:\n%s", status)
	}

	writeFile(t, src, native("store-format", "how the store is laid out", "One fact per file, now with more.\n"))
	wantLines(t, fx.run(t, false), []string{"propose " + sourcePath(src) + " -> " + stored + ": changed source"})
	if got, err := os.ReadFile(src); err != nil || !strings.Contains(string(got), "now with more") {
		t.Errorf("native source changed: %q, %v", got, err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-fact.md"), native("a-fact", "first", "See [[b_fact]].\n"))
	writeFile(t, filepath.Join(mem, "b-fact.md"), native("b_fact", "second", "See [[a-fact]].\n"))
	fx.opts.SessionID = ""
	local := filepath.Join(fx.cfg.State(), "local", "personal", "demo")
	wantLines(t, fx.run(t, true), []string{
		"would flag personal " + filepath.Join(local, "a-fact.md") + " <- " + sourcePath(filepath.Join(mem, "a-fact.md")) + ": provenance:unknown-session",
		"would flag personal " + filepath.Join(local, "b-fact.md"),
	})
	for _, dir := range []string{fx.cfg.PersonalStore, filepath.Join(fx.cfg.State(), "local")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != ".git" {
				t.Errorf("dry run wrote %s", filepath.Join(dir, e.Name()))
			}
		}
	}
}

func TestRelinkFollowsSlugifiedNames(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-fact.md"), native("a-fact", "first", "See [[b_fact|the second]].\n"))
	writeFile(t, filepath.Join(mem, "b-fact.md"), native("b_fact", "second", "See [[a-fact]].\n"))
	wantLines(t, fx.run(t, false), []string{"published personal ", "published personal "})
	b, err := os.ReadFile(filepath.Join(fx.cfg.PersonalStore, "demo", "a-fact.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "See [[b-fact|the second]].") {
		t.Errorf("link not relinked:\n%s", b)
	}
	if status := git(t, fx.cfg.PersonalStore, "status", "--porcelain"); status != "" {
		t.Errorf("the import left the checkout uncommitted:\n%s", status)
	}
}

func TestDecodeProjectDir(t *testing.T) {
	base := tempDir(t)
	dirs := []string{
		filepath.Join(base, "git", "hook-yard"),
		filepath.Join(base, "Data", ".worktrees", "feat-1"),
		filepath.Join(base, "amb", "a-b"),
		filepath.Join(base, "amb", "a", "b"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, encoded, want string
	}{
		{"dash in a segment", encodeProjectDir(dirs[0]), dirs[0]},
		{"dot dir", encodeProjectDir(dirs[1]), dirs[1]},
		{"ambiguous", encodeProjectDir(dirs[2]), ""},
		{"missing", encodeProjectDir(filepath.Join(base, "nope")), ""},
		{"not absolute", "home-x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeProjectDir(tt.encoded)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("decodeProjectDir(%q) = %q, %v; want %q", tt.encoded, got, ok, tt.want)
			}
		})
	}
}

// checkoutCommits fails when the personal store's commit gate is wedged: a
// later commit of the checkout must neither be refused nor leave it dirty.
func (fx fixture) checkoutCommits(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	checkout, err := store.CheckoutRoot(ctx, fx.cfg, route.StorePersonal)
	if err != nil {
		t.Fatal(err)
	}
	if w := commit.Checkout(ctx, fx.cfg, checkout, fx.opts.Deps.Rules, fx.opts.Deps.Scanner, "priors: test"); w != "" {
		t.Errorf("the commit gate is held back: %s", w)
	}
	if status := git(t, fx.cfg.PersonalStore, "status", "--porcelain"); status != "" {
		t.Errorf("the checkout is dirty after a commit:\n%s", status)
	}
}

func TestLinkToATargetThatDoesNotPublishIsNotPublished(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	tests := []struct {
		name   string
		target string
		want   func(dryRun bool, source string) string
	}{
		{"target lands flagged", "see https://example.com/doc for the layout of the store\n",
			func(dryRun bool, source string) string {
				if dryRun {
					return "would flag personal "
				}
				return "flagged personal "
			}},
		{"target is refused", "the key is " + "AK" + "IA" + "IOSFODNN7EXAMPLE\n",
			func(_ bool, source string) string { return "skipped " + source + ": refused: " }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-target]]. "+body))
			writeFile(t, filepath.Join(mem, "b-target.md"), native("b-target", "the target", tt.target))
			linker := sourcePath(filepath.Join(mem, "a-linker.md"))
			targetSource := sourcePath(filepath.Join(mem, "b-target.md"))
			for _, dryRun := range []bool{true, false} {
				got := fx.run(t, dryRun)
				if len(got) != 2 {
					t.Fatalf("dry=%v: lines = %v", dryRun, got)
				}
				if !slices.ContainsFunc(got, func(l string) bool {
					return strings.HasPrefix(l, "skipped "+linker+": refused: ") && strings.Contains(l, "wikilink-dangling")
				}) {
					t.Errorf("dry=%v: the linker is not refused for its dangling link:\n%s", dryRun, strings.Join(got, "\n"))
				}
				if want := tt.want(dryRun, targetSource); !slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, want) }) {
					t.Errorf("dry=%v: no line starts %q in:\n%s", dryRun, want, strings.Join(got, "\n"))
				}
			}
			if _, err := os.Stat(filepath.Join(fx.cfg.PersonalStore, "demo", "a-linker.md")); err == nil {
				t.Error("the linker reached the checkout")
			}
			fx.checkoutCommits(t)
		})
	}
}

func TestADroppedItemNoLongerShadowsItsNearDuplicate(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-first.md"), native("a-first", "the same description", "the key is "+"AK"+"IA"+"IOSFODNN7EXAMPLE\n"))
	writeFile(t, filepath.Join(mem, "b-second.md"), native("b-second", "the same description", "a different body\n"))
	wantLines(t, fx.run(t, false), []string{
		"skipped " + sourcePath(filepath.Join(mem, "a-first.md")) + ": refused: ",
		"published personal " + filepath.Join(fx.cfg.PersonalStore, "demo", "b-second.md"),
	})
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestAConcurrentWriterWaitsForTheImportsCommit(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "store-format.md"), native("store-format", "how the store is laid out", "One fact per file.\n"))
	ctx := context.Background()
	items, _, err := Claude(ctx, fx.claude, fx.cfg, route.Resolver{SSHHost: func(_ context.Context, alias string) string { return alias }})
	if err != nil {
		t.Fatal(err)
	}

	// write.Add scans before it takes the store's lock, so the import's scanner
	// is what holds the import inside the lock while the writer arrives.
	sig := tempDir(t)
	started, release, scanned := filepath.Join(sig, "started"), filepath.Join(sig, "release"), filepath.Join(sig, "scanned")
	slow := script(t, ": > '"+started+"'\ni=0\nwhile [ ! -e '"+release+"' ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\nprintf '[]'\n")
	fast := script(t, ": > '"+scanned+"'\nprintf '[]'\n")
	o := fx.opts
	o.Deps.Scanner = gate.Scanner{Bin: slow}

	importDone := make(chan error, 1)
	go func() {
		_, err := Run(ctx, items, o)
		importDone <- err
	}()
	waitFor(t, started)

	type added struct {
		res write.Result
		err error
	}
	addDone := make(chan added, 1)
	go func() {
		d := fx.opts.Deps
		d.Scanner = gate.Scanner{Bin: fast}
		res, err := write.Add(ctx, fx.cfg, write.Request{
			Fact:      storedFact("concurrent-note", "an unrelated note", "Written while the import runs.\n"),
			Session:   route.Session{Class: route.ClassPersonal, Repo: "demo"},
			SessionID: "s1", Engine: "claude", Host: "laptop",
		}, d)
		addDone <- added{res, err}
	}()
	waitFor(t, scanned)
	select {
	case a := <-addDone:
		t.Fatalf("the writer finished while the import held the lock: %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
	writeFile(t, release, "")

	select {
	case err := <-importDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the import never finished")
	}
	select {
	case a := <-addDone:
		if a.err != nil || a.res.Warning != "" || a.res.Outcome != "published" {
			t.Errorf("the writer = %+v, %v", a.res, a.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the writer never finished")
	}
	want := "priors: add concurrent-note\npriors: import 1 facts\n"
	if got := git(t, fx.cfg.PersonalStore, "log", "--format=%s"); got != want {
		t.Errorf("commits, newest first:\n%s\nwant:\n%s", got, want)
	}
}

func TestAFailureThatIsNotARefusalFailsTheRun(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	t.Run("a write that fails leaves the facts that link it unwritten", func(t *testing.T) {
		fx := setup(t, "personal")
		mem := fx.memDir(repo(t, personalRemote))
		writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-target]]. "+body))
		writeFile(t, filepath.Join(mem, "b-target.md"), native("b-target", "the target", "The target.\n"))
		linker := sourcePath(filepath.Join(mem, "a-linker.md"))
		target := sourcePath(filepath.Join(mem, "b-target.md"))
		blocker := filepath.Join(fx.cfg.PersonalStore, "demo", "b-target.md")
		if err := os.MkdirAll(blocker, 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}

		if res, lines := fx.runResult(t, true); res.Failed != 0 || len(lines) != 2 {
			t.Fatalf("the dry run passes the plan: failed %d, lines:\n%s", res.Failed, strings.Join(lines, "\n"))
		}
		res, lines := fx.runResult(t, false)
		if res.Failed != 2 {
			t.Errorf("Failed = %d, want 2", res.Failed)
		}
		wantLines(t, lines, []string{
			"skipped " + target + ": failed: ",
			"skipped " + linker + ": failed: it links b-target, which was not written",
		})
		if _, err := os.Stat(filepath.Join(fx.cfg.PersonalStore, "demo", "a-linker.md")); err == nil {
			t.Error("the linker is in the checkout")
		}
		if res.Warning != "" {
			t.Errorf("warning: %s", res.Warning)
		}
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
		fx.checkoutCommits(t)
		wantLines(t, fx.run(t, false), []string{"published personal ", "published personal "})
	})

	t.Run("a write that fails rolls back the facts of its link cycle written before it", func(t *testing.T) {
		fx := setup(t, "personal")
		mem := fx.memDir(repo(t, personalRemote))
		writeFile(t, filepath.Join(mem, "a-first.md"), native("a-first", "links to the second", "See [[b-second]]. "+body))
		writeFile(t, filepath.Join(mem, "b-second.md"), native("b-second", "links to the first", "See [[a-first]]. "+body+"More words.\n"))
		first := sourcePath(filepath.Join(mem, "a-first.md"))
		if err := os.MkdirAll(filepath.Join(fx.cfg.PersonalStore, "demo", "b-second.md"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}

		res, lines := fx.runResult(t, false)
		if res.Failed != 2 {
			t.Errorf("Failed = %d, want 2", res.Failed)
		}
		wantLines(t, lines, []string{
			"skipped " + first + ": failed: rolled back, it links b-second",
			"skipped " + sourcePath(filepath.Join(mem, "b-second.md")) + ": failed: ",
		})
		if _, err := os.Stat(filepath.Join(fx.cfg.PersonalStore, "demo", "a-first.md")); err == nil {
			t.Error("the rolled-back fact is still in the checkout")
		}
		if res.Warning != "" {
			t.Errorf("warning: %s", res.Warning)
		}
		if err := os.Remove(filepath.Join(fx.cfg.PersonalStore, "demo", "b-second.md")); err != nil {
			t.Fatal(err)
		}
		fx.checkoutCommits(t)
	})

	t.Run("an unavailable scanner is a failure, not a refusal", func(t *testing.T) {
		fx := setup(t, "personal")
		mem := fx.memDir(repo(t, personalRemote))
		writeFile(t, filepath.Join(mem, "store-format.md"), native("store-format", "how the store is laid out", body))
		fx.opts.Deps.Scanner = gate.Scanner{Bin: script(t, "exit 1\n")}
		for _, dryRun := range []bool{true, false} {
			res, lines := fx.runResult(t, dryRun)
			wantLines(t, lines, []string{"skipped " + sourcePath(filepath.Join(mem, "store-format.md")) + ": failed: secret scanner unavailable"})
			if res.Failed != 1 {
				t.Errorf("dry=%v: Failed = %d, want 1", dryRun, res.Failed)
			}
		}
	})
}

func TestACommitHeldBackLastRunIsRetried(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "store-format.md"), native("store-format", "how the store is laid out", "One fact per file.\n"))
	stray := filepath.Join(fx.cfg.PersonalStore, "demo", "stray.md")
	writeFile(t, stray, "not a fact\n")

	res, lines := fx.runResult(t, false)
	wantLines(t, lines, []string{"skipped " + stray + ": unreadable store fact: ", "published personal "})
	if !strings.Contains(res.Warning, "nothing committed") {
		t.Errorf("warning = %q, want the commit held back", res.Warning)
	}
	if status := git(t, fx.cfg.PersonalStore, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(status, "demo/store-format.md") {
		t.Fatalf("the imported fact is not waiting in the checkout:\n%s", status)
	}

	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	wantLines(t, fx.run(t, false), []string{"unchanged " + sourcePath(filepath.Join(mem, "store-format.md")) + ": "})
	if status := git(t, fx.cfg.PersonalStore, "status", "--porcelain"); status != "" {
		t.Errorf("the retry left the checkout dirty:\n%s", status)
	}
	if got := git(t, fx.cfg.PersonalStore, "log", "--format=%s"); got != "priors: import\n" {
		t.Errorf("commits:\n%s", got)
	}
}

func TestAnUnreadableStoreFactIsReportedAndNeverOverwritten(t *testing.T) {
	const damaged = "not a fact\n"
	tests := []struct {
		name      string
		sessionID string
		dir       func(fx fixture) string
		dry, real string
	}{
		{"checkout", "s1", func(fx fixture) string { return filepath.Join(fx.cfg.PersonalStore, "demo") }, "would publish", "published"},
		{"local layer", "", func(fx fixture) string { return filepath.Join(fx.cfg.State(), "local", "personal", "demo") }, "would flag", "flagged"},
	}
	for _, tt := range tests {
		for _, dryRun := range []bool{true, false} {
			name, landed := tt.name+" real run", tt.real
			if dryRun {
				name, landed = tt.name+" dry run", tt.dry
			}
			t.Run(name, func(t *testing.T) {
				fx := setup(t, "personal")
				fx.opts.SessionID = tt.sessionID
				mem := fx.memDir(repo(t, personalRemote))
				writeFile(t, filepath.Join(mem, "clashes.md"), native("clashes", "lands on the damaged file", "One fact per file.\n"))
				writeFile(t, filepath.Join(mem, "fine.md"), native("fine", "lands beside it", "A different body entirely.\n"))
				stray := filepath.Join(tt.dir(fx), "stray.md")
				clash := filepath.Join(tt.dir(fx), "clashes.md")
				writeFile(t, stray, damaged)
				writeFile(t, clash, damaged)

				res, lines := fx.runResult(t, dryRun)
				wantLines(t, lines, []string{
					"skipped " + stray + ": unreadable store fact: ",
					"skipped " + clash + ": unreadable store fact: ",
					"skipped " + sourcePath(filepath.Join(mem, "clashes.md")) + ": refused: it would overwrite the unreadable store fact " + clash,
					landed + " personal ",
				})
				if res.Failed != 0 {
					t.Errorf("Failed = %d, a refusal is not a failure", res.Failed)
				}
				for _, p := range []string{stray, clash} {
					if got, err := os.ReadFile(p); err != nil || string(got) != damaged {
						t.Errorf("%s = %q, %v; want it untouched", p, got, err)
					}
				}
			})
		}
	}
}

func TestAFactThatPublishesAfterPlanningFlaggedItDoesNotLinkAFlaggedSibling(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the sibling", "See [[b-sibling]]. "+body))
	writeFile(t, filepath.Join(mem, "b-sibling.md"), native("b-sibling", "the sibling", "see https://example.com/doc for the layout of the store\n"))
	linker := sourcePath(filepath.Join(mem, "a-linker.md"))

	// The importing session has no tool record, so the plan flags both facts;
	// the scanner records one on its trigger-th call, so the linker's write
	// finds the session clean while the sibling stays flagged for its URL.
	fx.opts.SessionID = "s2"
	if err := gate.MarkSession(fx.cfg.ProvenanceDir(), "s2", false); err != nil {
		t.Fatal(err)
	}
	sig := tempDir(t)
	count, trigger := filepath.Join(sig, "count"), filepath.Join(sig, "trigger")
	stream := filepath.Join(fx.cfg.EventRecord, "stream", time.Now().UTC().Format(time.DateOnly)+".jsonl")
	fx.opts.Deps.Scanner = gate.Scanner{Bin: script(t, "n=$(cat '"+count+"' 2>/dev/null || echo 0)\nn=$((n+1))\nprintf '%s' \"$n\" > '"+count+"'\n"+
		"if [ -f '"+trigger+"' ] && [ \"$n\" = \"$(cat '"+trigger+"')\" ]; then\n"+
		"  printf '%s\\n' '{\"session_id\":\"s2\",\"canonical_event\":\"post_tool\",\"tool_name\":\"Bash\"}' >> '"+stream+"'\nfi\nprintf '[]'\n")}

	_, lines := fx.runResult(t, true)
	wantLines(t, lines, []string{"would flag personal ", "would flag personal "})
	planCalls, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	var plan int
	if _, err := fmt.Sscan(string(planCalls), &plan); err != nil {
		t.Fatal(err)
	}
	writeFile(t, count, "0")
	writeFile(t, trigger, strconv.Itoa(plan+1))

	res, lines := fx.runResult(t, false)
	wantLines(t, lines, []string{
		"skipped " + linker + ": failed: it links b-sibling",
		"flagged personal ",
	})
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.Warning != "" {
		t.Errorf("warning: %s", res.Warning)
	}
	if _, err := os.Stat(filepath.Join(fx.cfg.PersonalStore, "demo", "a-linker.md")); err == nil {
		t.Error("the linker reached the checkout")
	}
	fx.checkoutCommits(t)
}

func TestARefusedLinkTargetThatLaterPublishesDoesNotLeaveItsLinkerRefused(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	// b-yankee is shadowed by a-zeta until a-zeta is refused, which only the
	// second round learns; c-xray links it.
	writeFile(t, filepath.Join(mem, "a-zeta.md"), native("a-zeta", "the same description", "the key is "+"AK"+"IA"+"IOSFODNN7EXAMPLE\n"))
	writeFile(t, filepath.Join(mem, "b-yankee.md"), native("b-yankee", "the same description", "a different body\n"))
	writeFile(t, filepath.Join(mem, "c-xray.md"), native("c-xray", "links to yankee", "See [[b-yankee]]. "+body))
	for _, dryRun := range []bool{true, false} {
		landed := "published personal "
		if dryRun {
			landed = "would publish personal "
		}
		_, lines := fx.runResult(t, dryRun)
		wantLines(t, lines, []string{
			"skipped " + sourcePath(filepath.Join(mem, "a-zeta.md")) + ": refused: ",
			landed + filepath.Join(fx.cfg.PersonalStore, "demo", "b-yankee.md"),
			landed + filepath.Join(fx.cfg.PersonalStore, "demo", "c-xray.md"),
		})
	}
	fx.checkoutCommits(t)
}

func TestFactsAreWrittenAfterTheFactsTheyLink(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[z-target]]. "+body))
	writeFile(t, filepath.Join(mem, "z-target.md"), native("z-target", "the target", "The target.\n"))
	log := filepath.Join(tempDir(t), "scanned")
	fx.opts.Deps.Scanner = gate.Scanner{Bin: script(t, "sed -n 's/^name: //p' >> '"+log+"'\nprintf '[]'\n")}

	res, lines := fx.runResult(t, false)
	if res.Failed != 0 || res.Warning != "" {
		t.Fatalf("failed %d, warning %q", res.Failed, res.Warning)
	}
	wantLines(t, lines, []string{"published personal ", "published personal "})
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	scanned := strings.Fields(string(b))
	// The plan scans every fact first; the writes are the last two scans.
	if got, want := scanned[len(scanned)-2:], []string{"z-target", "a-linker"}; !slices.Equal(got, want) {
		t.Errorf("write order = %v, want %v", got, want)
	}
}
