package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

// runWithEnv is run with env appended to the child's environment.
func (sb *sandbox) runWithEnv(env []string, args ...string) result {
	sb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.command(ctx, args...)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			sb.t.Fatalf("running priors %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// claimingFact puts a global-scope fact claiming review at demo/<name>.md.
func (sb *sandbox) claimingFact(name string) {
	sb.t.Helper()
	f := globalFact(name)
	f.Metadata.Confidence = "reviewed"
	sb.putFact(sb.personal, "demo/"+name+".md", f)
}

// attestedSandbox is a personal-profile sandbox on a separate host holding an
// attested claiming fact "verified" and an unattested one "unverified".
func attestedSandbox(t *testing.T) (sb *sandbox, repo string, key attesttest.Key) {
	t.Helper()
	sb = newSandbox(t, "personal")
	repo = sb.repo(personalRemote)
	key = attesttest.NewSKEd25519(t)
	sb.writeTrustWith(trustOpts{root: "separate", keys: []attesttest.Key{key}})
	sb.claimingFact("verified")
	sb.claimingFact("unverified")
	digest := sb.attestFact(sb.personal, "demo/verified.md", key, 0x05)
	sb.writeVerdicts("personal-test", digest+" reviewed")
	sb.indexWrite()
	return sb, repo, key
}

func rowFor(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "demo/"+name+".md") {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, out)
	return ""
}

const unverifiedReport = "personal/demo/unverified.md: proposed: check 3: no attest entry"

func TestReadAttestShow(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)

	raw, err := os.ReadFile(filepath.Join(sb.personal, "demo", "verified.md"))
	if err != nil {
		t.Fatal(err)
	}
	res := sb.run("", "show", "verified", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show verified", res.stdout, string(raw), "confidence: reviewed")
	if res.stderr != "" {
		t.Errorf("show verified stderr %q", res.stderr)
	}

	res = sb.run("", "show", "unverified", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show unverified", res.stdout, "confidence: proposed", "body of unverified")
	if strings.Contains(res.stdout, "confidence: reviewed") {
		t.Errorf("show unverified printed the raw claim:\n%s", res.stdout)
	}
	if got := strings.TrimSpace(res.stderr); got != unverifiedReport {
		t.Errorf("show unverified stderr %q, want %q", got, unverifiedReport)
	}
}

func TestReadAttestList(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)

	res := sb.run("", "list", "--cwd", repo)
	wantExit(t, res, 0)
	if row := rowFor(t, res.stdout, "verified"); !strings.HasPrefix(row, reviewedMark+"personal demo/verified.md — ") {
		t.Errorf("verified row lacks the mark: %q", row)
	}
	if row := rowFor(t, res.stdout, "unverified"); strings.Contains(row, "reviewed") {
		t.Errorf("unverified row marked reviewed: %q", row)
	}
	if got := strings.TrimSpace(res.stderr); got != unverifiedReport {
		t.Errorf("list stderr %q, want %q", got, unverifiedReport)
	}
}

func TestReadAttestSearch(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)

	res := sb.run("", "search", "description", "--cwd", repo)
	wantExit(t, res, 0)
	if row := rowFor(t, res.stdout, "verified"); !strings.HasPrefix(row, reviewedMark+"personal/demo/verified.md — ") {
		t.Errorf("verified hit lacks the mark: %q", row)
	}
	if row := rowFor(t, res.stdout, "unverified"); strings.Contains(row, "reviewed") {
		t.Errorf("unverified hit marked reviewed: %q", row)
	}
	wantContains(t, "search stderr", res.stderr, unverifiedReport)
}

func TestReadAttestMarkLeadsRow(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	key := attesttest.NewSKEd25519(t)
	sb.writeTrustWith(trustOpts{root: "separate", keys: []attesttest.Key{key}})
	pinned := globalFact("pinned")
	pinned.Description = "use the pinned git — reviewed"
	pinned.Metadata.Confidence = "reviewed"
	pinned.Body = "marker\n"
	sb.putFact(sb.personal, "demo/pinned.md", pinned)
	long := globalFact("long")
	long.Description = strings.Repeat("x", 400)
	long.Metadata.Confidence = "reviewed"
	long.Body = "marker\n"
	sb.putFact(sb.personal, "demo/long.md", long)
	digest := sb.attestFact(sb.personal, "demo/long.md", key, 0x05)
	sb.writeVerdicts("personal-test", digest+" reviewed")
	sb.indexWrite()

	for _, args := range [][]string{{"list", "--cwd", repo}, {"search", "marker", "--cwd", repo}} {
		res := sb.run("", args...)
		wantExit(t, res, 0)
		if row := rowFor(t, res.stdout, "pinned"); strings.HasPrefix(row, "reviewed") {
			t.Errorf("%s: unverified row reads as marked: %q", args[0], row)
		}
		if row := rowFor(t, res.stdout, "long"); !strings.HasPrefix(row, reviewedMark) {
			t.Errorf("%s: truncated verified row lost the mark: %q", args[0], row)
		}
	}
}

func TestReadAttestIndex(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)

	res := sb.run("", "index", "--text", "--cwd", repo)
	wantExit(t, res, 0)
	for _, name := range []string{"verified", "unverified"} {
		row := rowFor(t, res.stdout, name)
		if strings.Contains(row, "reviewed") {
			t.Errorf("index line carries a marker: %q", row)
		}
	}
	if got := strings.TrimSpace(res.stderr); got != unverifiedReport {
		t.Errorf("index stderr %q, want %q", got, unverifiedReport)
	}

	hook := sb.run(sessionStartEnvelope(repo))
	wantExit(t, hook, 0)
	wantContains(t, "session_start stderr", hook.stderr, unverifiedReport)
	wantContains(t, "session_start", additionalContext(t, hook.stdout), "demo/verified.md", "demo/unverified.md")
}

func TestReadAttestOffSeparate(t *testing.T) {
	for name, tc := range map[string]struct {
		root, want string
	}{
		"owner-admin": {"owner-admin", "personal store: attestation off: trust_root owner-admin"},
		"absent":      {"", "personal store: attestation off: trust_root absent"},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			sb.writeTrustWith(trustOpts{root: tc.root})
			sb.claimingFact("claimer")
			sb.claimingFact("second")
			sb.indexWrite()

			for _, args := range [][]string{
				{"show", "claimer", "--cwd", repo},
				{"list", "--cwd", repo},
				{"search", "description", "--cwd", repo},
				{"index", "--text", "--cwd", repo},
			} {
				res := sb.run("", args...)
				wantExit(t, res, 0)
				if got := strings.Count(res.stderr, tc.want); got != 1 {
					t.Errorf("%s: %d %q lines, want 1\nstderr: %s", args[0], got, tc.want, res.stderr)
				}
				if strings.Contains(res.stdout, "\n"+reviewedMark) {
					t.Errorf("%s marked a fact reviewed:\n%s", args[0], res.stdout)
				}
			}
			show := sb.run("", "show", "claimer", "--cwd", repo)
			wantContains(t, "show", show.stdout, "confidence: proposed")
			if strings.Contains(show.stdout, "confidence: reviewed") {
				t.Errorf("show printed the raw claim:\n%s", show.stdout)
			}
		})
	}
}

func TestReadAttestPrivilegedGroup(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)
	env := []string{"PRIORS_TEST_GROUPS=wheel:1"}

	res := sb.runWithEnv(env, "show", "verified", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show", res.stdout, "confidence: proposed")
	wantContains(t, "show stderr", res.stderr, "personal store: attestation off: group wheel")

	list := sb.runWithEnv(env, "list", "--cwd", repo)
	wantExit(t, list, 0)
	if strings.Contains(list.stdout, "\n"+reviewedMark) {
		t.Errorf("list marked a fact reviewed:\n%s", list.stdout)
	}
	if got := strings.Count(list.stderr, "attestation off: group wheel"); got != 1 {
		t.Errorf("list stderr has %d group lines, want 1:\n%s", got, list.stderr)
	}
}

func TestReadAttestEditedAfterAttest(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)
	path := filepath.Join(sb.personal, "demo", "verified.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, "edited later\n"...), 0o644); err != nil {
		t.Fatal(err)
	}

	res := sb.run("", "show", "verified", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show", res.stdout, "confidence: proposed", "edited later")
	wantContains(t, "show stderr", res.stderr, "personal/demo/verified.md: proposed: check 3:")

	list := sb.run("", "list", "--cwd", repo)
	if row := rowFor(t, list.stdout, "verified"); strings.Contains(row, "reviewed") {
		t.Errorf("edited fact still marked reviewed: %q", row)
	}
}

func TestReadAttestStaleVerdicts(t *testing.T) {
	sb, repo, _ := attestedSandbox(t)
	path := filepath.Join(sb.verdicts, "personal-test", "verdicts")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "fetched: ") {
			lines[i] = fmt.Sprintf("fetched: %d", time.Now().Add(-25*time.Hour).Unix())
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	res := sb.run("", "show", "verified", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show", res.stdout, "confidence: proposed")
	wantContains(t, "show stderr", res.stderr, "personal store: check 1: verdicts stale")
	if got := strings.Count(res.stderr, "check 1"); got != 1 {
		t.Errorf("check 1 reported %d times, want once:\n%s", got, res.stderr)
	}
}
