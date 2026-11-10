package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// #289 removed priors attestation: confidence: reviewed is a plain, unverified
// label. These tests pin that a store holding reviewed facts, and the
// .attest/ directories the removed subsystem may have left behind, keep
// working in every read path and in the commit that priors add makes.

const (
	vettedName = "vetted-fact"
	// plainName is a fact left at the default confidence.
	plainName = "plain-fact"
)

// leftoverAttestEntries writes .attest/<name> files into root: one that looks
// like a signed entry and one that is garbage. Neither is read any more.
func leftoverAttestEntries(sb *sandbox, root string) {
	sb.t.Helper()
	sb.writeFile(filepath.Join(root, ".attest", vettedName),
		"-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg\n-----END SSH SIGNATURE-----\n")
	sb.writeFile(filepath.Join(root, ".attest", plainName), "not an entry \x00\xff\n")
	sb.writeFile(filepath.Join(root, ".attest", "stray"), "")
}

// reviewedStore is a personal-profile sandbox whose store holds a global
// reviewed fact and a global proposed one, plus leftover .attest entries.
func reviewedStore(t *testing.T) (sb *sandbox, repo string) {
	t.Helper()
	sb = newSandbox(t, "personal")
	repo = sb.repo(personalRemote)
	reviewed := globalFact(vettedName)
	reviewed.Metadata.Confidence = "reviewed"
	sb.putFact(sb.personal, "demo/"+vettedName+".md", reviewed)
	sb.putFact(sb.personal, "demo/"+plainName+".md", globalFact(plainName))
	leftoverAttestEntries(sb, sb.personal)
	sb.indexWrite()
	return sb, repo
}

func plainRow(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "demo/"+name+".md") {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, out)
	return ""
}

func TestReviewedLintIgnoresLeftoverAttestDir(t *testing.T) {
	sb, _ := reviewedStore(t)

	res := sb.run("", "lint")
	wantExit(t, res, 0)
	if res.stdout != "" || res.stderr != "" {
		t.Errorf("lint printed stdout %q stderr %q, want nothing for a reviewed fact and a leftover .attest dir", res.stdout, res.stderr)
	}

	res = sb.run("", "lint", "--dir", sb.personal, "--kind", "personal")
	wantExit(t, res, 0)
	if res.stdout != "" || res.stderr != "" {
		t.Errorf("lint --dir printed stdout %q stderr %q, want nothing", res.stdout, res.stderr)
	}

	// Store CI copied from the old example still passes --since: it is
	// accepted and ignored, with a warning.
	res = sb.run("", "lint", "--dir", sb.personal, "--kind", "personal", "--since", "HEAD~1")
	wantExit(t, res, 0)
	wantContains(t, "lint --since stderr", res.stderr, "--since is deprecated and ignored")
	if res.stdout != "" {
		t.Errorf("lint --since printed stdout %q, want nothing", res.stdout)
	}
}

func TestReviewedListAndSearchShowTheStoredLabel(t *testing.T) {
	sb, repo := reviewedStore(t)

	for _, c := range []struct {
		args   []string
		prefix string
	}{
		{[]string{"list", "--cwd", repo}, "personal demo/"},
		{[]string{"search", "description", "--cwd", repo}, "personal/demo/"},
	} {
		args := c.args
		res := sb.run("", args...)
		wantExit(t, res, 0)
		row := plainRow(t, res.stdout, vettedName)
		if strings.Contains(row, "reviewed ·") || strings.HasPrefix(row, "reviewed") {
			t.Errorf("%s row carries a verification mark: %q", args[0], row)
		}
		if !strings.HasPrefix(row, c.prefix+vettedName+".md — ") {
			t.Errorf("%s row = %q, want the plain row shape", args[0], row)
		}
		plainRow(t, res.stdout, plainName)
		if res.stderr != "" {
			t.Errorf("%s stderr %q, want none: nothing to report about a reviewed fact", args[0], res.stderr)
		}
	}
}

func TestReviewedShowPrintsTheRawFrontmatter(t *testing.T) {
	sb, repo := reviewedStore(t)

	raw, err := os.ReadFile(filepath.Join(sb.personal, "demo", vettedName+".md"))
	if err != nil {
		t.Fatal(err)
	}
	res := sb.run("", "show", vettedName, "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show", res.stdout, string(raw), "confidence: reviewed")
	if strings.Contains(res.stdout, "confidence: proposed") {
		t.Errorf("show demoted the reviewed fact:\n%s", res.stdout)
	}
	if res.stderr != "" {
		t.Errorf("show stderr %q, want none", res.stderr)
	}
}

func TestReviewedIndexAndSessionStartListIt(t *testing.T) {
	sb, repo := reviewedStore(t)

	index, err := os.ReadFile(filepath.Join(sb.personal, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "MEMORY.md", string(index), "["+vettedName+"](demo/"+vettedName+".md)", "["+plainName+"](demo/"+plainName+".md)")

	res := sb.run("", "index", "--write")
	wantExit(t, res, 0)
	if res.stdout != "" {
		t.Errorf("a second index --write rewrote something: %q", res.stdout)
	}

	envelope := sessionStartEnvelope(repo)
	for what, res := range map[string]result{
		"index":         sb.run(envelope, "index"),
		"session start": sb.run(envelope),
	} {
		wantExit(t, res, 0)
		wantContains(t, what, additionalContext(t, res.stdout),
			"["+vettedName+"](demo/"+vettedName+".md)", "["+plainName+"](demo/"+plainName+".md)")
		if res.stderr != "" {
			t.Errorf("%s stderr %q, want none", what, res.stderr)
		}
	}
}

// TestAddCommitsAReviewedFactAndLeavesAttestOut: a hand-written reviewed fact
// rides into the commit priors add makes, and a leftover .attest/ stays out.
func TestAddCommitsAReviewedFactAndLeavesAttestOut(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	reviewed := globalFact(vettedName)
	reviewed.Metadata.Confidence = "reviewed"
	sb.putFact(sb.personal, "demo/"+vettedName+".md", reviewed)
	leftoverAttestEntries(sb, sb.personal)

	res := sb.run("the body\n", "add", "--stdin", "--name", "added-fact", "--description", "added next to it",
		"--type", "project", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)

	tracked := gitOutput(t, sb, sb.personal, "ls-tree", "-r", "--name-only", "HEAD")
	wantContains(t, "committed files", tracked, "demo/"+vettedName+".md", "demo/added-fact.md")
	if strings.Contains(tracked, ".attest") {
		t.Errorf("a leftover .attest entry was committed:\n%s", tracked)
	}
	if got := gitOutput(t, sb, sb.personal, "show", "HEAD:demo/"+vettedName+".md"); !strings.Contains(got, "confidence: reviewed") {
		t.Errorf("the committed reviewed fact lost its label:\n%s", got)
	}
}

// TestTrustRootIsDeprecated: hosts still generate trust_root until nix-config
// drops it. owner-admin and attest_keys load and are ignored; separate asked
// for the removed attestation and fails with a message that says so.
func TestTrustRootIsDeprecated(t *testing.T) {
	t.Run("owner-admin loads", func(t *testing.T) {
		sb := newSandbox(t, "personal")
		repo := sb.repo(personalRemote)
		sb.writeTrust(`trust_root = "owner-admin"`)
		res := sb.run("", "list", "--cwd", repo)
		wantExit(t, res, 0)
	})

	t.Run("attest_keys tables load", func(t *testing.T) {
		sb := newSandbox(t, "personal")
		repo := sb.repo(personalRemote)
		sb.writeTrust()
		// Appended after the store tables, where an array of tables belongs.
		path := sb.trustPath
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sb.writeFile(path, string(body)+"\n[[attest_keys]]\nkey = \"ssh-ed25519 AAAA\"\nattestation = \"YQ==\"\nchallenge = \"YQ==\"\n")
		res := sb.run("", "list", "--cwd", repo)
		wantExit(t, res, 0)
	})

	t.Run("separate fails", func(t *testing.T) {
		sb := newSandbox(t, "personal")
		repo := sb.repo(personalRemote)
		sb.writeTrust(`trust_root = "separate"`)
		res := sb.run("", "list", "--cwd", repo)
		if res.code == 0 {
			t.Fatalf("list succeeded with trust_root = \"separate\"\nstdout: %s", res.stdout)
		}
		wantContains(t, "stderr", res.stderr, "attestation was removed")
	})
}

// gitOutput runs git in dir as the sandbox's identity and returns stdout.
func gitOutput(t *testing.T, sb *sandbox, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(tools.Git, append([]string{"-C", dir}, args...)...)
	cmd.Env = sb.childEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
