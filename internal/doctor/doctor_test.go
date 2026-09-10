package doctor

import "testing"

// The expected value is the directory name Cursor actually created for the
// probe run that produced docs/design/fixtures/hook-payloads.
func TestCursorProjectSlugMatchesCursorsOwnNaming(t *testing.T) {
	dir := "/tmp/claude-1000/-home-noams-Data-git--worktrees-noamsto-hookyard-feat-1-design-spec-hookyard-as-a-standalone-cro/33cb05fe-2fe0-4094-841d-3c21fcc2dc5c/scratchpad/capture/cursor-probe"
	want := "tmp-claude-1000-home-noams-Data-git-worktrees-noamsto-hookyard-feat-1-design-spec-hookyard-as-a-standalone-cro-33cb05fe-2fe0-4094-841d-3c21fcc2dc5c-scratchpad-capture-cursor-probe"
	if got := cursorProjectSlug(dir); got != want {
		t.Errorf("slug mismatch\nwant %s\ngot  %s", want, got)
	}
}
