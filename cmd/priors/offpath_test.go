package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

var update = flag.Bool("update", false, "rewrite the testdata/offpath goldens")

var fenceRE = regexp.MustCompile(`priors-[0-9a-f]{16}`)

// offPathFact is a proposed fact with every date pinned, so the goldens do not
// move with the wall clock.
func offPathFact(name, repo, typ string) fact.Fact {
	f := newFact(name, repo, typ)
	f.Metadata.ValidFrom = "2026-01-01"
	f.Metadata.Verified = "2026-01-01"
	f.Metadata.Modified = "2026-01-01T00:00:00Z"
	return f
}

// writeOffPathTrust is writeTrust with an optional trust_root line before the
// store tables.
func (sb *sandbox) writeOffPathTrust(trustRoot string) {
	sb.t.Helper()
	lines := []string{
		fmt.Sprintf("profile = %q", sb.profile),
		`work_orgs = ["github.com/factify-inc"]`,
		`personal_orgs = ["github.com/noamsto"]`,
	}
	if trustRoot != "" {
		lines = append(lines, fmt.Sprintf("trust_root = %q", trustRoot))
	}
	lines = append(lines, "[stores.personal]", `id = "personal-test"`)
	if sb.profile == "work" {
		lines = append(lines, "[stores.work]", `id = "work-test"`)
	}
	sb.writeFile(sb.trustPath, strings.Join(lines, "\n")+"\n")
}

// offPathLog runs commands in a sandbox and renders their stdout, stderr and
// exit code into one golden, with the per-run paths and fence delimiters
// replaced.
type offPathLog struct {
	sb      *sandbox
	replace *strings.Replacer
	out     strings.Builder
}

func (l *offPathLog) normalise(s string) string {
	return fenceRE.ReplaceAllString(l.replace.Replace(s), "priors-FENCE")
}

func (l *offPathLog) run(label, stdin string, args ...string) {
	l.sb.t.Helper()
	res := l.sb.run(stdin, args...)
	fmt.Fprintf(&l.out, "=== %s\nexit: %d\n--- stdout\n%s\n--- stderr\n%s\n", label, res.code, l.normalise(res.stdout), l.normalise(res.stderr))
}

func TestOffPath(t *testing.T) {
	for _, trustRoot := range []string{"", "owner-admin"} {
		for _, profile := range []string{"personal", "work"} {
			name := "no-trust-root-" + profile
			if trustRoot != "" {
				name = trustRoot + "-" + profile
			}
			t.Run(name, func(t *testing.T) {
				checkOffPath(t, name, profile, trustRoot)
			})
		}
	}
}

func checkOffPath(t *testing.T, name, profile, trustRoot string) {
	sb := newSandbox(t, profile)
	sb.writeOffPathTrust(trustRoot)
	remote, repoName, store := personalRemote, "demo", sb.personal
	if profile == "work" {
		remote, repoName, store = workRemote, "app", sb.work
	}
	repo := sb.repo(remote)

	sb.putFact(store, repoName+"/alpha-fact.md", offPathFact("alpha-fact", repoName, "project"))
	sb.putFact(store, repoName+"/reference-fact.md", offPathFact("reference-fact", repoName, "reference"))
	needle := offPathFact("needle-fact", repoName, "project")
	needle.Body = "the zebra crossing rule\n"
	sb.putFact(store, repoName+"/needle-fact.md", needle)
	global := offPathFact("global-fact", "", "user")
	global.Metadata.Scope = "global"
	global.Metadata.Repos = nil
	sb.putFact(sb.personal, "_global/global-fact.md", global)

	l := &offPathLog{
		sb:      sb,
		replace: strings.NewReplacer(sb.dir, "<SANDBOX>", repo, "<REPO>"),
	}
	envelope := sessionStartEnvelope(repo)

	l.run("index --write", "", "index", "--write")
	l.run("index --write (again)", "", "index", "--write")
	l.run("hook session_start", envelope)
	l.run("index (envelope on stdin)", envelope, "index")
	l.run("index --text", "", "index", "--text", "--cwd", repo)
	l.run("lint", "", "lint")
	l.run("show alpha-fact", "", "show", "alpha-fact", "--cwd", repo)
	l.run("show no-such-fact", "", "show", "no-such-fact", "--cwd", repo)
	l.run("list", "", "list", "--cwd", repo)
	l.run("list --type reference", "", "list", "--cwd", repo, "--type", "reference")
	l.run("search zebra", "", "search", "zebra", "--cwd", repo)
	l.run("search no-such-term", "", "search", "no-such-term", "--cwd", repo)

	bad := offPathFact("bad-fact", repoName, "bogus")
	sb.putFact(store, repoName+"/bad-fact.md", bad)
	linky := offPathFact("linky-fact", repoName, "project")
	linky.Body = "see https://example.com/docs for details\n"
	sb.putFact(store, repoName+"/linky-fact.md", linky)
	l.run("index --write (with violations)", "", "index", "--write")
	l.run("lint (with violations)", "", "lint")
	l.run("list (with violations)", "", "list", "--cwd", repo)

	golden := filepath.Join("testdata", "offpath", name+".golden")
	got := l.out.String()
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (record it with -update)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update to accept)\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}
