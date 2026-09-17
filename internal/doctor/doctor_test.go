package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// The expected value is the directory name Cursor actually created for the
// probe run that produced docs/design/fixtures/hook-payloads.
func TestCursorProjectSlugMatchesCursorsOwnNaming(t *testing.T) {
	dir := "/tmp/claude-1000/-home-noams-Data-git--worktrees-noamsto-hookyard-feat-1-design-spec-hookyard-as-a-standalone-cro/33cb05fe-2fe0-4094-841d-3c21fcc2dc5c/scratchpad/capture/cursor-probe"
	want := "tmp-claude-1000-home-noams-Data-git-worktrees-noamsto-hookyard-feat-1-design-spec-hookyard-as-a-standalone-cro-33cb05fe-2fe0-4094-841d-3c21fcc2dc5c-scratchpad-capture-cursor-probe"
	if got := cursorProjectSlug(dir); got != want {
		t.Errorf("slug mismatch\nwant %s\ngot  %s", want, got)
	}
}

func TestCursorWorkspaceTrustFailureHasExactFix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	marker := filepath.Join(root, "projects", cursorProjectSlug(dir), ".workspace-trusted")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	// Use a different workspace marker so the target remains untrusted.
	if err := os.MkdirAll(filepath.Join(root, "projects", "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "other", ".workspace-trusted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	findings := cursorFindings(Paths{CursorHome: root}, dir, "")
	trust := findings[0]
	if trust.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", trust.Status, trust.Detail)
	}
	if trust.Fix != "Open the target workspace in Cursor and accept its workspace-trust prompt." {
		t.Errorf("fix = %q, want exact Cursor trust repair", trust.Fix)
	}
}
