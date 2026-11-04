package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
)

func TestReadEnvelopeReadsEngine(t *testing.T) {
	got, err := readEnvelope(strings.NewReader(`{"engine":"claude-code","canonical_event":"session_start","cwd":"/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine != "claude-code" || got.CanonicalEvent != "session_start" || got.CWD != "/x" {
		t.Errorf("envelope = %+v", got)
	}
}

// fenceToken is random per run.
var fenceToken = regexp.MustCompile(`priors-[0-9a-f]{16}`)

func envelopeFor(engine, cwd string) string {
	return fmt.Sprintf(`{"engine":%q,"canonical_event":"session_start","session_id":"s1","cwd":%q}`, engine, cwd)
}

// nativeFixture is a personal store with a fact Claude's memory dir also holds
// and one it does not, a main repo and a linked worktree of it.
type nativeFixture struct {
	sb           *sandbox
	repo, linked string
}

func newNativeFixture(t *testing.T) nativeFixture {
	t.Helper()
	sb := newSandbox(t, "personal")
	repo, err := filepath.EvalSymlinks(sb.repo(personalRemote))
	if err != nil {
		t.Fatal(err)
	}
	sb.git(repo, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(sb.dir, "linked")
	sb.git(repo, "worktree", "add", "-q", "-b", "other", linked)

	sb.putFact(sb.personal, "demo/dup-fact.md", newFact("dup-fact", "demo", "project"))
	sb.putFact(sb.personal, "demo/kept-fact.md", newFact("kept-fact", "demo", "feedback"))
	sb.indexWrite()

	native := filepath.Join(sb.dir, ".claude", "projects", importer.EncodeProjectDir(repo), "memory")
	sb.putFact(native, "dup-fact.md", newFact("dup-fact", "demo", "project"))
	return nativeFixture{sb: sb, repo: repo, linked: linked}
}

func (f nativeFixture) hook(envelope string, env ...string) string {
	f.sb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30e9)
	defer cancel()
	cmd := f.sb.command(ctx)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(envelope)
	out, err := cmd.Output()
	if err != nil {
		f.sb.t.Fatalf("priors: %v", err)
	}
	return fenceToken.ReplaceAllString(additionalContext(f.sb.t, string(out)), "priors-FENCE")
}

func TestSessionStartDedupsAgainstClaudeMemory(t *testing.T) {
	f := newNativeFixture(t)
	flat := f.hook(`{"canonical_event":"session_start","session_id":"s1","cwd":` + fmt.Sprintf("%q", f.repo) + `}`)
	wantContains(t, "flat block", flat, "[dup-fact](demo/dup-fact.md)", "[kept-fact](demo/kept-fact.md)")
	if strings.Contains(flat, "also in MEMORY.md") || strings.Contains(flat, "## feedback") {
		t.Errorf("flat block is grouped:\n%s", flat)
	}

	for name, cwd := range map[string]string{"main repo": f.repo, "linked worktree": f.linked} {
		t.Run("claude-code "+name, func(t *testing.T) {
			got := f.hook(envelopeFor("claude-code", cwd))
			wantContains(t, "claude block", got, "## feedback", "[kept-fact](demo/kept-fact.md)", "1 fact also in MEMORY.md")
			if strings.Contains(got, "dup-fact") {
				t.Errorf("duplicate not omitted:\n%s", got)
			}
		})
	}

	for _, engine := range []string{"codex", "pi"} {
		t.Run(engine, func(t *testing.T) {
			if got := f.hook(envelopeFor(engine, f.repo)); got != flat {
				t.Errorf("%s block differs from the flat one:\n%s\n--- flat:\n%s", engine, got, flat)
			}
		})
	}
	t.Run("codex ignores CLAUDECODE", func(t *testing.T) {
		if got := f.hook(envelopeFor("codex", f.repo), "CLAUDECODE=1"); got != flat {
			t.Errorf("codex block differs from the flat one:\n%s", got)
		}
	})
}

func TestIndexTextEngineFlag(t *testing.T) {
	f := newNativeFixture(t)
	res := f.sb.run("", "index", "--text", "--cwd", f.repo, "--engine", "claude-code")
	wantExit(t, res, 0)
	wantContains(t, "stdout", res.stdout, "1 fact also in MEMORY.md")
	if strings.Contains(res.stdout, "dup-fact") {
		t.Errorf("duplicate not omitted:\n%s", res.stdout)
	}

	plain := f.sb.run("", "index", "--text", "--cwd", f.repo)
	wantExit(t, plain, 0)
	wantContains(t, "plain stdout", plain.stdout, "[dup-fact](demo/dup-fact.md)")

	flagWins := f.sb.run(envelopeFor("codex", f.repo), "index", "--text", "--engine", "claude-code")
	wantExit(t, flagWins, 0)
	wantContains(t, "flag over envelope", flagWins.stdout, "1 fact also in MEMORY.md")
}
