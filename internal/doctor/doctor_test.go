package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
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
	// LookPath must find cursor-agent for the trust check to reach its Fail
	// arm, so point PATH at a stub rather than whatever the host has.
	stub := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(stub, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(stub))

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

func TestCodexWorkspaceTrustUnknownWhenAbsentFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	root := t.TempDir()
	dir := filepath.Join(root, "project")
	codexHome := filepath.Join(root, "codex")
	writeFile(t, filepath.Join(codexHome, "config.toml"), []byte(
		"[projects.\""+dir+"\"]\ntrust_level = \"untrusted\"\n"))

	f := findByCheck(t, codexFindings(Paths{CodexHome: codexHome}, dir), "workspace trust")
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
	if f.Engine != vocab.Codex {
		t.Errorf("engine = %v, want Codex", f.Engine)
	}
	if !strings.Contains(f.Detail, "PATH") {
		t.Errorf("detail = %q, want it to say codex is not on PATH", f.Detail)
	}
}

func TestCursorWorkspaceTrustUnknownWhenAbsentFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	root := t.TempDir()
	dir := filepath.Join(root, "project")

	trust := cursorFindings(Paths{CursorHome: root}, dir, "")[0]
	if trust.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", trust.Status, trust.Detail)
	}
	if trust.Engine != vocab.Cursor {
		t.Errorf("engine = %v, want Cursor", trust.Engine)
	}
	if !strings.Contains(trust.Detail, "PATH") {
		t.Errorf("detail = %q, want it to say cursor-agent is not on PATH", trust.Detail)
	}
}
