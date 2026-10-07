package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

func reviewedFact(name string) fact.Fact {
	f := newFact(name, "demo", "project")
	f.Metadata.Confidence = "reviewed"
	return f
}

// putReviewed writes a fact claiming review at root/rel, with the store
// indexed while the fact still reads proposed so no commit gate weighs the
// claim; index writes the index for the stores it is run against.
func (sb *sandbox) putReviewed(root, rel string, index func()) fact.Fact {
	sb.t.Helper()
	name := strings.TrimSuffix(filepath.Base(rel), ".md")
	sb.putFact(root, rel, newFact(name, "demo", "project"))
	index()
	f := reviewedFact(name)
	sb.putFact(root, rel, f)
	return f
}

// separateSandbox is a personal-profile sandbox on a separate trust root
// enrolling key, with a clean store holding one proposed fact.
func separateSandbox(t *testing.T) (*sandbox, attesttest.Key) {
	t.Helper()
	sb := newSandbox(t, "personal")
	key := attesttest.NewSKEd25519(t)
	sb.writeTrustWith(trustOpts{root: "separate", keys: []attesttest.Key{key}})
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	sb.indexWrite()
	return sb, key
}

func TestLintReportsLapsedAttestationOnStderr(t *testing.T) {
	sb, key := separateSandbox(t)
	sb.putReviewed(sb.personal, "demo/lapsed-fact.md", sb.indexWrite)
	digest := sb.attestFact(sb.personal, "demo/lapsed-fact.md", key, 0x05)
	sb.writeVerdicts("personal-test", digest+" lapsed")

	res := sb.run("", "lint")
	wantExit(t, res, 0)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want none", res.stdout)
	}
	wantContains(t, "lint stderr", res.stderr, "personal/demo/lapsed-fact.md: stale attestation: re-attest or revoke")
}

func TestLintReportsClaimWithoutEntryOnStderr(t *testing.T) {
	sb, _ := separateSandbox(t)
	sb.putReviewed(sb.personal, "demo/claim-fact.md", sb.indexWrite)
	sb.writeVerdicts("personal-test")

	res := sb.run("", "lint")
	wantExit(t, res, 0)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want none", res.stdout)
	}
	wantContains(t, "lint stderr", res.stderr, "personal/demo/claim-fact.md: proposed: check 3: no attest entry")
}

func TestLintReportsLocalLayerClaimOnStderr(t *testing.T) {
	sb, _ := separateSandbox(t)
	sb.putReviewed(filepath.Join(sb.state, "local", "personal"), "demo/local-fact.md", sb.indexWrite)
	sb.writeVerdicts("personal-test")

	res := sb.run("", "lint")
	wantExit(t, res, 0)
	wantContains(t, "lint stderr", res.stderr, "personal/demo/local-fact.md: proposed: check 2: not in a checkout")
}

func TestLintReportsAttestationOffOnStderr(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.writeTrustWith(trustOpts{root: "owner-admin"})
	sb.putReviewed(sb.personal, "demo/claim-fact.md", sb.indexWrite)

	res := sb.run("", "lint")
	wantExit(t, res, 0)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want none", res.stdout)
	}
	wantContains(t, "lint stderr", res.stderr, "personal store: attestation off: trust_root owner-admin")
}

func TestLintWithoutClaimsPrintsNoAttestationReport(t *testing.T) {
	sb, _ := separateSandbox(t)
	res := sb.run("", "lint")
	wantExit(t, res, 0)
	if res.stdout != "" || res.stderr != "" {
		t.Errorf("stdout %q, stderr %q, want both empty", res.stdout, res.stderr)
	}
}

// dirStore is a git store holding one clean fact and its index, committed.
func dirStore(t *testing.T, sb *sandbox) string {
	t.Helper()
	dir := t.TempDir()
	sb.git(dir, "init", "-q")
	sb.putFact(dir, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	writeStoreIndex(t, sb, dir)
	return dir
}

func commitAll(sb *sandbox, dir, msg string) {
	sb.t.Helper()
	if sb.gitOut(dir, "status", "--porcelain") == "" {
		return
	}
	sb.git(dir, "add", "-A")
	sb.git(dir, "commit", "-q", "-m", msg)
}

// dirEntry writes a v1 entry for the fact at dir/rel as .attest/<name>. It
// is signed but never verified: lint --dir checks only shape.
func dirEntry(sb *sandbox, dir, rel string, key attesttest.Key) {
	sb.t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		sb.t.Fatal(err)
	}
	f, err := fact.Parse(raw)
	if err != nil {
		sb.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	entry := attesttest.Signed(key, attesttest.Entry("personal-test", f.Name, rel, hex.EncodeToString(sum[:]), 1, "attest"), 0x05, attest.Namespace)
	sb.writeFile(filepath.Join(dir, ".attest", f.Name), string(entry))
}

func TestLintDirLoadsNoTrustFileAndFlagsNothingWithoutAttestations(t *testing.T) {
	sb := newSandbox(t, "personal")
	dir := dirStore(t, sb)
	commitAll(sb, dir, "seed")
	if err := os.Remove(sb.trustPath); err != nil {
		t.Fatal(err)
	}

	base := sb.gitOut(dir, "rev-parse", "HEAD")
	for _, extra := range [][]string{nil, {"--since", strings.TrimSpace(base)}} {
		res := sb.run("", append([]string{"lint", "--dir", dir, "--kind", "personal"}, extra...)...)
		wantExit(t, res, 0)
		if res.stdout != "" || res.stderr != "" {
			t.Errorf("lint --dir %v: stdout %q, stderr %q, want both empty", extra, res.stdout, res.stderr)
		}
	}
}

func TestLintDirFlagsMalformedEntry(t *testing.T) {
	sb := newSandbox(t, "personal")
	dir := dirStore(t, sb)
	sb.writeFile(filepath.Join(dir, ".attest", "x"), "not an entry\n")

	res := sb.run("", "lint", "--dir", dir, "--kind", "personal")
	wantExit(t, res, 1)
	wantContains(t, "lint --dir", res.stdout, ".attest/x: attest-entry:", "is not a v1 attest entry")
}

func TestLintDirFlagsReviewedFactWithoutEntry(t *testing.T) {
	sb := newSandbox(t, "personal")
	dir := dirStore(t, sb)
	sb.putReviewed(dir, "demo/claim-fact.md", func() { writeStoreIndex(t, sb, dir) })

	res := sb.run("", "lint", "--dir", dir, "--kind", "personal")
	wantExit(t, res, 1)
	wantContains(t, "lint --dir", res.stdout, "demo/claim-fact.md: unattested-review:", ".attest/claim-fact")
}

func TestLintDirSinceUnresolvedRevFails(t *testing.T) {
	sb := newSandbox(t, "personal")
	dir := dirStore(t, sb)
	commitAll(sb, dir, "seed")

	res := sb.run("", "lint", "--dir", dir, "--kind", "personal", "--since", "bogus")
	wantExit(t, res, 1)
	wantContains(t, "lint --dir --since", res.stdout, "remote-range:")
}

func TestLintDirSinceFlagsDeletedEntry(t *testing.T) {
	sb := newSandbox(t, "personal")
	key := attesttest.NewSKEd25519(t)
	dir := dirStore(t, sb)
	sb.putReviewed(dir, "demo/claim-fact.md", func() { writeStoreIndex(t, sb, dir) })
	dirEntry(sb, dir, "demo/claim-fact.md", key)
	commitAll(sb, dir, "attest")
	base := strings.TrimSpace(sb.gitOut(dir, "rev-parse", "HEAD"))

	sb.putFact(dir, "demo/claim-fact.md", newFact("claim-fact", "demo", "project"))
	if err := os.Remove(filepath.Join(dir, ".attest", "claim-fact")); err != nil {
		t.Fatal(err)
	}
	commitAll(sb, dir, "drop the entry")

	args := []string{"lint", "--dir", dir, "--kind", "personal"}
	wantExit(t, sb.run("", args...), 0)
	res := sb.run("", append(args, "--since", base)...)
	wantExit(t, res, 1)
	wantContains(t, "lint --dir --since", res.stdout, ".attest/claim-fact: attest-deleted:")
}

func TestLintSinceNeedsDir(t *testing.T) {
	sb := newSandbox(t, "personal")
	res := sb.run("", "lint", "--since", "HEAD")
	wantExit(t, res, 1)
	wantContains(t, "lint --since", res.stderr, "--since")
}
