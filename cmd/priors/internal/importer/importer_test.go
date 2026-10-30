package importer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
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

func (fx fixture) run(t *testing.T, dryRun bool) []string {
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
	if res.Warning != "" {
		t.Errorf("warning: %s", res.Warning)
	}
	var lines []string
	for _, r := range append(notes, res.Reports...) {
		lines = append(lines, r.String())
	}
	return lines
}

// wantLines checks that each want is a prefix of exactly one line, and that
// lines holds nothing else.
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
