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
					w := waits(linker, "b-target")
					return strings.HasPrefix(l, w.prefix) && strings.Contains(l, w.has[0])
				}) {
					t.Errorf("dry=%v: the linker is not set aside for its held-back target:\n%s", dryRun, strings.Join(got, "\n"))
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

func TestLinkersOfFactsAShadowHidUntilItWasRefusedPublish(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	const secret = "one two three four five six seven eight nine ten eleven twelve " + "AK" + "IA" + "IOSFODNN7EXAMPLE\n"
	type file struct{ name, description, body string }
	tests := []struct {
		name            string
		files           []file
		refused, landed []string
	}{
		// c-bravo is shadowed by a-secret until a-secret is refused; b-alpha
		// and c-bravo link each other.
		{"link cycle", []file{
			{"a-secret", "the same description", secret},
			{"b-alpha", "links to bravo", "See [[c-bravo]]. " + body},
			{"c-bravo", "the same description", "See [[b-alpha]]. a different body\n"},
		}, []string{"a-secret"}, []string{"b-alpha", "c-bravo"}},
		// e-lima links c-tango, which links d-uniform; each of those is
		// shadowed in turn by a refused near duplicate, d-uniform by b-secret
		// only once a-secret is refused.
		{"link chain", []file{
			{"a-secret", "alpha", secret},
			{"b-secret", "beta", secret},
			{"c-tango", "alpha", "See [[d-uniform]]. " + body},
			{"d-uniform", "beta", "something else entirely here\n"},
			{"e-lima", "gamma", "See [[c-tango]]. A link to tango with words.\n"},
		}, []string{"a-secret", "b-secret"}, []string{"c-tango", "d-uniform", "e-lima"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem := fx.memDir(repo(t, personalRemote))
			for _, f := range tt.files {
				writeFile(t, filepath.Join(mem, f.name+".md"), native(f.name, f.description, f.body))
			}
			for _, dryRun := range []bool{true, false} {
				landed := "published personal "
				if dryRun {
					landed = "would publish personal "
				}
				var want []string
				for _, n := range tt.refused {
					want = append(want, "skipped "+sourcePath(filepath.Join(mem, n+".md"))+": refused: ")
				}
				for _, n := range tt.landed {
					want = append(want, landed+filepath.Join(fx.cfg.PersonalStore, "demo", n+".md"))
				}
				wantLines(t, fx.run(t, dryRun), want)
			}
			fx.checkoutCommits(t)
		})
	}
}

func TestALinkerSetAsideOrRefusedForItsLinkDoesNotShadowItsNearDuplicate(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	tests := []struct {
		name     string
		link     string
		target   string
		setAside bool
		want     func(dryRun bool) []string
	}{
		{"link names no fact", "ghost", "", false, func(bool) []string { return nil }},
		{"link target lands flagged", "c-flag", "see https://example.com/doc for the layout of the store\n", true, func(dryRun bool) []string {
			if dryRun {
				return []string{"would flag personal "}
			}
			return []string{"flagged personal "}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "the same description", "See [["+tt.link+"]]. "+body))
			writeFile(t, filepath.Join(mem, "b-dup.md"), native("b-dup", "the same description", "a different body\n"))
			if tt.target != "" {
				writeFile(t, filepath.Join(mem, tt.link+".md"), native(tt.link, "the target", tt.target))
			}
			linker := sourcePath(filepath.Join(mem, "a-linker.md"))
			for _, dryRun := range []bool{true, false} {
				landed := "published personal "
				if dryRun {
					landed = "would publish personal "
				}
				got := fx.run(t, dryRun)
				linkerLine, detail := "skipped "+linker+": refused: ", "wikilink-dangling"
				if tt.setAside {
					linkerLine, detail = "propose "+linker+" -> ", ": waits for [["+tt.link+"]], which is held back"
				}
				wantLines(t, got, append(tt.want(dryRun),
					linkerLine,
					landed+filepath.Join(fx.cfg.PersonalStore, "demo", "b-dup.md"),
				))
				if !slices.ContainsFunc(got, func(l string) bool {
					return strings.HasPrefix(l, linkerLine) && strings.Contains(l, detail)
				}) {
					t.Errorf("dry=%v: the linker is not reported with %q:\n%s", dryRun, detail, strings.Join(got, "\n"))
				}
			}
			fx.checkoutCommits(t)
		})
	}
}

func TestFactsThatPublishAfterPlanningFlaggedThemMayLinkEachOther(t *testing.T) {
	const body = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-target.md"), native("a-target", "the target", "The target fact body.\n"))
	writeFile(t, filepath.Join(mem, "b-linker.md"), native("b-linker", "links to the target", "See [[a-target]]. "+body))

	// The importing session has no tool record, so the plan flags both facts;
	// the scanner records one on the first write's scan, so both writes find
	// the session clean.
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
		"published personal " + filepath.Join(fx.cfg.PersonalStore, "demo", "a-target.md"),
		"published personal " + filepath.Join(fx.cfg.PersonalStore, "demo", "b-linker.md"),
	})
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0", res.Failed)
	}
	if res.Warning != "" {
		t.Errorf("warning: %s", res.Warning)
	}
	fx.checkoutCommits(t)
}

const (
	inRunBody   = "Facts are plain markdown files with frontmatter, one per file, in a store.\n"
	inRunSecret = "the key is " + "AK" + "IA" + "IOSFODNN7EXAMPLE\n"
	inRunURL    = "see https://example.com/doc for the layout of the store\n"
)

// report is what one output line must be: it starts with prefix and holds
// every string of has.
type report struct {
	prefix string
	has    []string
}

// from narrows a published or flagged report to the item it came from.
func (r report) from(src string) report {
	r.has = append(slices.Clone(r.has), "<- "+src)
	return r
}

func source(mem, name string) string {
	return sourcePath(filepath.Join(mem, name+".md"))
}

func (fx fixture) storePath(name string) string {
	return filepath.Join(fx.cfg.PersonalStore, "demo", name+".md")
}

func (fx fixture) localPath(name string) string {
	return filepath.Join(store.LocalRoot(fx.cfg, route.StorePersonal).Path, "demo", name+".md")
}

func (fx fixture) published(name string) report {
	return report{prefix: "published personal " + fx.storePath(name) + " <- "}
}

func (fx fixture) flagged(name string) report {
	return report{prefix: "flagged personal " + fx.localPath(name) + " <- "}
}

func duplicate(src string) report {
	return report{prefix: "duplicate " + src + ": "}
}

func refused(src string, has ...string) report {
	return report{prefix: "skipped " + src + ": refused: ", has: has}
}

// waits is a linker set aside for a link to the held-back target.
func waits(src, target string) report {
	return report{prefix: "propose " + src + " -> ", has: []string{": waits for [[" + target + "]], which is held back"}}
}

// waitsAt is waits, naming the path of the held-back target.
func waitsAt(src, path, target string) report {
	r := waits(src, target)
	r.prefix += path + ": "
	return r
}

func wantReports(t *testing.T, lines []string, want ...report) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for _, w := range want {
		if !slices.ContainsFunc(lines, func(l string) bool {
			return strings.HasPrefix(l, w.prefix) && !slices.ContainsFunc(w.has, func(h string) bool { return !strings.Contains(l, h) })
		}) {
			t.Errorf("no line starts %q and holds %q in:\n%s", w.prefix, w.has, strings.Join(lines, "\n"))
		}
	}
}

// runBoth runs the import dry and then for real, and fails when the dry run
// reported anything but what the real run did. It returns the real run's lines.
func (fx fixture) runBoth(t *testing.T) []string {
	t.Helper()
	_, dry := fx.runResult(t, true)
	res, got := fx.runResult(t, false)
	for i, l := range dry {
		if rest, ok := strings.CutPrefix(l, "would publish "); ok {
			dry[i] = "published " + rest
		} else if rest, ok := strings.CutPrefix(l, "would flag "); ok {
			dry[i] = "flagged " + rest
		}
	}
	if !slices.Equal(dry, got) {
		t.Errorf("the dry run differs from the real run:\ndry:\n%s\nreal:\n%s", strings.Join(dry, "\n"), strings.Join(got, "\n"))
	}
	if res.Failed != 0 || res.Warning != "" {
		t.Errorf("failed %d, warning %q", res.Failed, res.Warning)
	}
	return got
}

func (fx fixture) wantNotInCheckout(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Stat(fx.storePath(n)); err == nil {
			t.Errorf("%s reached the checkout", n)
		}
	}
}

func TestInRunLinksResolve(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, fx fixture, mem, mem2 string) []report
	}{
		{"2-cycle", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-one.md"), native("a-one", "one", "See [[b-two]]. one words here alpha\n"))
			writeFile(t, filepath.Join(mem, "b-two.md"), native("b-two", "two", "See [[a-one]]. two words here beta\n"))
			return []report{fx.published("a-one"), fx.published("b-two")}
		}},
		{"3-cycle", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-one.md"), native("a-one", "one", "See [[b-two]]. one words here alpha\n"))
			writeFile(t, filepath.Join(mem, "b-two.md"), native("b-two", "two", "See [[c-three]]. two words here beta\n"))
			writeFile(t, filepath.Join(mem, "c-three.md"), native("c-three", "three", "See [[a-one]]. three words here gamma\n"))
			return []report{fx.published("a-one"), fx.published("b-two"), fx.published("c-three")}
		}},
		{"non-canonical link to a landing target", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to bravo", "See [[C_Bravo]]. "+inRunBody))
			writeFile(t, filepath.Join(mem, "C_Bravo.md"), native("C_Bravo", "bravo", "bravo body words\n"))
			return []report{fx.published("a-linker"), fx.published("c-bravo")}
		}},
		{"non-canonical link to a target a refused near-duplicate hid", func(t *testing.T, fx fixture, mem, _ string) []report {
			// c-bravo is shadowed by a-secret until a-secret is refused.
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to bravo", "See [[C_Bravo]]. "+inRunBody))
			writeFile(t, filepath.Join(mem, "a-secret.md"), native("a-secret", "the same description", inRunSecret))
			writeFile(t, filepath.Join(mem, "b-bravo.md"), native("C_Bravo", "the same description", "a different body\n"))
			return []report{refused(source(mem, "a-secret")), fx.published("a-linker"), fx.published("c-bravo")}
		}},
		{"2-cycle in two dirs", func(t *testing.T, fx fixture, mem, mem2 string) []report {
			for _, m := range []string{mem, mem2} {
				writeFile(t, filepath.Join(m, "a-one.md"), native("a-one", "one", "See [[b-two]]. one words here alpha\n"))
				writeFile(t, filepath.Join(m, "b-two.md"), native("b-two", "two", "See [[a-one]]. two words here beta\n"))
			}
			return []report{
				fx.published("a-one").from(source(mem, "a-one")), fx.published("b-two").from(source(mem, "b-two")),
				duplicate(source(mem2, "a-one")), duplicate(source(mem2, "b-two")),
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem, mem2 := fx.memDir(repo(t, personalRemote)), fx.memDir(repo(t, personalRemote))
			want := tt.arrange(t, fx, mem, mem2)
			wantReports(t, fx.runBoth(t), want...)
			fx.checkoutCommits(t)
		})
	}
}

// writeFlaggedCycle writes a-one -> b-two -> c-three -> a-one, where b-two's
// URL flags it, into mem.
func writeFlaggedCycle(t *testing.T, mem string) {
	t.Helper()
	writeFile(t, filepath.Join(mem, "a-one.md"), native("a-one", "one", "See [[b-two]]. one words here alpha\n"))
	writeFile(t, filepath.Join(mem, "b-two.md"), native("b-two", "two", "See [[c-three]]. https://example.com/x two words here beta\n"))
	writeFile(t, filepath.Join(mem, "c-three.md"), native("c-three", "three", "See [[a-one]]. three words here gamma\n"))
}

func TestInRunFlaggedCycleMember(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFlaggedCycle(t, mem)

	wantReports(t, fx.runBoth(t),
		fx.flagged("b-two"),
		waitsAt(source(mem, "a-one"), fx.localPath("b-two"), "b-two"),
		waits(source(mem, "c-three"), "a-one"),
	)
	fx.wantNotInCheckout(t, "a-one", "b-two", "c-three")
	fx.checkoutCommits(t)
}

func TestInRunFlaggedCycleMemberTwoDirs(t *testing.T) {
	fx := setup(t, "personal")
	mem, mem2 := fx.memDir(repo(t, personalRemote)), fx.memDir(repo(t, personalRemote))
	writeFlaggedCycle(t, mem)
	writeFlaggedCycle(t, mem2)

	wantReports(t, fx.runBoth(t),
		fx.flagged("b-two").from(source(mem, "b-two")),
		waitsAt(source(mem, "a-one"), fx.localPath("b-two"), "b-two"),
		waits(source(mem, "c-three"), "a-one"),
		duplicate(source(mem2, "a-one")), duplicate(source(mem2, "b-two")), duplicate(source(mem2, "c-three")),
	)
	fx.wantNotInCheckout(t, "a-one", "b-two", "c-three")
	fx.checkoutCommits(t)
}

func TestInRunLinkerOfHeldBackTargetIsSetAside(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, fx fixture, mem, work string) []report
	}{
		{"flagged target", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-flag]]. "+inRunBody))
			writeFile(t, filepath.Join(mem, "b-flag.md"), native("b-flag", "the target", inRunURL))
			return []report{fx.flagged("b-flag"), waitsAt(source(mem, "a-linker"), fx.localPath("b-flag"), "b-flag")}
		}},
		{"refused target", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-target]]. "+inRunBody))
			writeFile(t, filepath.Join(mem, "b-target.md"), native("b-target", "the target", inRunSecret))
			return []report{refused(source(mem, "b-target")), waits(source(mem, "a-linker"), "b-target")}
		}},
		{"target only in the local layer", func(t *testing.T, fx fixture, mem, _ string) []report {
			// A previous run flagged b-flag into the local layer.
			writeFile(t, filepath.Join(mem, "b-flag.md"), native("b-flag", "the target", inRunURL))
			wantReports(t, fx.run(t, false), fx.flagged("b-flag"))
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-flag]]. "+inRunBody))
			return []report{
				{prefix: "unchanged " + source(mem, "b-flag") + ": " + fx.localPath("b-flag")},
				waitsAt(source(mem, "a-linker"), fx.localPath("b-flag"), "b-flag"),
			}
		}},
		{"target routed to the work store", func(t *testing.T, fx fixture, mem, work string) []report {
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-work]]. "+inRunBody))
			writeFile(t, filepath.Join(work, "b-work.md"), native("b-work", "the target", "a work note\n"))
			return []report{
				{prefix: "skipped " + source(work, "b-work") + ": routes to the quarantine"},
				refused(source(mem, "a-linker"), "wikilink-dangling"),
			}
		}},
		{"link names no fact", func(t *testing.T, fx fixture, mem, _ string) []report {
			writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to nothing", "See [[ghost]]. "+inRunBody))
			return []report{refused(source(mem, "a-linker"), "wikilink-dangling")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem, work := fx.memDir(repo(t, personalRemote)), fx.memDir(repo(t, workRemote))
			want := tt.arrange(t, fx, mem, work)
			wantReports(t, fx.runBoth(t), want...)
			fx.wantNotInCheckout(t, "a-linker")
			fx.checkoutCommits(t)
		})
	}

	t.Run("lands once the target is in the store", func(t *testing.T) {
		fx := setup(t, "personal")
		mem := fx.memDir(repo(t, personalRemote))
		target := filepath.Join(mem, "b-flag.md")
		targetText := native("b-flag", "the target", inRunURL)
		writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the target", "See [[b-flag]]. "+inRunBody))
		writeFile(t, target, targetText)
		wantReports(t, fx.runBoth(t), fx.flagged("b-flag"), waitsAt(source(mem, "a-linker"), fx.localPath("b-flag"), "b-flag"))

		// Review: the flagged copy leaves the local layer, and a clean copy of
		// the same source is committed to the store.
		if err := os.Remove(fx.localPath("b-flag")); err != nil {
			t.Fatal(err)
		}
		reviewed := storedFact("b-flag", "the target", "The target, reviewed.\n")
		reviewed.Metadata.Source = &fact.Source{Engine: "claude", Path: sourcePath(target), SHA256: digest([]byte(targetText))}
		putFact(t, fx.storePath("b-flag"), reviewed)
		git(t, fx.cfg.PersonalStore, "add", "-A")
		git(t, fx.cfg.PersonalStore, "commit", "-q", "-m", "review b-flag")

		wantReports(t, fx.runBoth(t),
			report{prefix: "unchanged " + source(mem, "b-flag") + ": " + fx.storePath("b-flag")},
			fx.published("a-linker"),
		)
		fx.checkoutCommits(t)
	})
}

func TestInRunHeldFactWaitingOnAShadowedTargetIsNotDropped(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, fx fixture, mem string) []report
	}{
		// c-held links the flagged z-flag, so it is set aside, and d-target is
		// its near-duplicate: hidden until c-held is dropped. e-linker links
		// d-target, so it must wait for d-target, not be dropped with c-held.
		{"linker of the shadowed target", func(t *testing.T, fx fixture, mem string) []report {
			writeFile(t, filepath.Join(mem, "e-linker.md"), native("e-linker", "links to the target", "See [[d-target]]. "+inRunBody))
			return []report{fx.published("e-linker")}
		}},
		{"linker of a linker of the shadowed target", func(t *testing.T, fx fixture, mem string) []report {
			writeFile(t, filepath.Join(mem, "b-mid.md"), native("b-mid", "points on to the target", "See [[d-target]]. A middle note that points on to the target.\n"))
			writeFile(t, filepath.Join(mem, "e-linker.md"), native("e-linker", "links to the middle", "See [[b-mid]]. "+inRunBody))
			return []report{fx.published("b-mid"), fx.published("e-linker")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setup(t, "personal")
			mem := fx.memDir(repo(t, personalRemote))
			writeFile(t, filepath.Join(mem, "c-held.md"), native("c-held", "the same description", "See [[z-flag]]. Held back until the flagged note is reviewed by hand.\n"))
			writeFile(t, filepath.Join(mem, "d-target.md"), native("d-target", "the same description", "The target fact body.\n"))
			writeFile(t, filepath.Join(mem, "z-flag.md"), native("z-flag", "the flagged note", inRunURL))
			want := append(tt.arrange(t, fx, mem),
				fx.flagged("z-flag"),
				waitsAt(source(mem, "c-held"), fx.localPath("z-flag"), "z-flag"),
				fx.published("d-target"),
			)
			wantReports(t, fx.runBoth(t), want...)
			fx.checkoutCommits(t)
		})
	}
}

// d-target is shadowed by a-pub for good, and e-linker, the only held fact,
// waits on it, so nothing else could be set aside first.
func TestInRunEveryHeldFactWaitingIsStillSetAside(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	writeFile(t, filepath.Join(mem, "a-pub.md"), native("a-pub", "the same description", "A published fact body here.\n"))
	writeFile(t, filepath.Join(mem, "d-target.md"), native("d-target", "the same description", "The target fact body.\n"))
	writeFile(t, filepath.Join(mem, "e-linker.md"), native("e-linker", "links to the target", "See [[d-target]]. "+inRunBody))
	wantReports(t, fx.runBoth(t),
		fx.published("a-pub"),
		report{prefix: "propose " + source(mem, "d-target") + " -> " + source(mem, "a-pub") + " (this run): same description"},
		waitsAt(source(mem, "e-linker"), source(mem, "d-target"), "d-target"),
	)
	fx.checkoutCommits(t)
}

func TestInRunFlaggedFactThatPublishesLaterDoesNotLinkASetAsideSibling(t *testing.T) {
	fx := setup(t, "personal")
	mem := fx.memDir(repo(t, personalRemote))
	// b-sibling links c-secret, which is refused, so b-sibling is set aside;
	// a-linker links b-sibling and is planned flagged, which may name it.
	writeFile(t, filepath.Join(mem, "a-linker.md"), native("a-linker", "links to the sibling", "See [[b-sibling]]. "+inRunBody))
	writeFile(t, filepath.Join(mem, "b-sibling.md"), native("b-sibling", "the sibling", "See [[c-secret]]. The sibling points at a note that never lands.\n"))
	writeFile(t, filepath.Join(mem, "c-secret.md"), native("c-secret", "the secret", inRunSecret))
	linker := source(mem, "a-linker")

	// The importing session has no tool record, so the plan flags every
	// candidate; the scanner records one on its trigger-th call, so the
	// linker's write finds the session clean.
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
	wantReports(t, lines,
		refused(source(mem, "c-secret")),
		waits(source(mem, "b-sibling"), "c-secret"),
		report{prefix: "would flag personal " + fx.localPath("a-linker") + " <- "},
	)
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
	wantReports(t, lines,
		refused(source(mem, "c-secret")),
		waits(source(mem, "b-sibling"), "c-secret"),
		report{prefix: "skipped " + linker + ": failed: it links b-sibling"},
	)
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.Warning != "" {
		t.Errorf("warning: %s", res.Warning)
	}
	fx.wantNotInCheckout(t, "a-linker", "b-sibling")
	fx.checkoutCommits(t)
}

func TestInRunByteDuplicateOrder(t *testing.T) {
	fx := setup(t, "personal")
	mem, mem2 := fx.memDir(repo(t, personalRemote)), fx.memDir(repo(t, personalRemote))
	text := native("a-one", "one", "One words here alpha, in a fact both dirs hold.\n")

	// The store already holds the copy from the second dir.
	writeFile(t, filepath.Join(mem2, "a-one.md"), text)
	wantReports(t, fx.run(t, false), fx.published("a-one").from(source(mem2, "a-one")))

	// The first dir's copy comes first in the run, but the store's copy decides.
	writeFile(t, filepath.Join(mem, "a-one.md"), text)
	wantReports(t, fx.runBoth(t),
		report{prefix: "duplicate " + source(mem, "a-one") + ": " + fx.storePath("a-one") + ": same source bytes"},
		report{prefix: "unchanged " + source(mem2, "a-one") + ": " + fx.storePath("a-one")},
	)
	fx.checkoutCommits(t)
}
