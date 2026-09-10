package render

import (
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

const routerPath = "/nix/store/abc/bin/hookyard"

func TestBuildPlanRendersOneEntryPerEngineEvent(t *testing.T) {
	// Two handlers on the same event: the router is a single per-event exec, so
	// they must collapse into one entry whose matcher is their union.
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}, Match: []string{"Bash"}},
		{ID: "b", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}, Match: []string{"Read"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := plan[vocab.Cursor]
	if len(entries) != 1 {
		t.Fatalf("want 1 cursor entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].Event != "preToolUse" {
		t.Errorf("want preToolUse, got %q", entries[0].Event)
	}
	if entries[0].Matcher != "Read|Shell" {
		t.Errorf("want the union Read|Shell, got %q", entries[0].Matcher)
	}
}

// Cursor's pre_tool must not also register beforeShellExecution: both fire for
// one shell call on the allow path.
func TestBuildPlanRegistersOnlyPreToolUseForCursor(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}, Match: []string{"Bash"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan[vocab.Cursor] {
		if strings.Contains(e.Event, "beforeShell") {
			t.Errorf("pre_tool should not register %q", e.Event)
		}
	}
}

func TestBuildPlanTranslatesMatchersPerEngine(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"claude-code", "codex", "cursor"}, Match: []string{"Bash", "Write"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[vocab.Engine]string{
		vocab.ClaudeCode: "Bash|Write",
		vocab.Codex:      "Bash|apply_patch",
		vocab.Cursor:     "Shell|Write",
	}
	for engine, matcher := range want {
		if len(plan[engine]) != 1 {
			t.Fatalf("%s: want 1 entry, got %d", engine, len(plan[engine]))
		}
		if got := plan[engine][0].Matcher; got != matcher {
			t.Errorf("%s: want matcher %q, got %q", engine, matcher, got)
		}
	}
}

// A handler with no match watches every tool, which each engine spells as an
// absent matcher rather than a wildcard string.
func TestBuildPlanOmitsMatcherWhenAHandlerWatchesEveryTool(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}, Match: []string{"Bash"}},
		{ID: "b", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan[vocab.Cursor][0].Matcher; got != "" {
		t.Errorf("want no matcher once a handler watches every tool, got %q", got)
	}
}

func TestBuildPlanTagsEachRenderingWithItsEngine(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"claude-code", "cursor"}, Match: []string{"Bash"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	for engine, entries := range plan {
		want := "--registered-for " + string(engine)
		if !strings.Contains(entries[0].Command, want) {
			t.Errorf("%s: command should carry %q, got %q", engine, want, entries[0].Command)
		}
	}
}

// The marker is how every writer finds its own entries again; a router path
// without it would make the strip silently no-op and stack duplicates.
func TestBuildPlanRefusesARouterPathWithoutTheMarker(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"cursor"}},
	}
	if _, err := BuildPlan(handlers, "/usr/local/libexec/hooky"); err == nil {
		t.Fatal("want an error for a router path missing the marker, got nil")
	}
}

func TestBuildPlanRoutesEngineScopedEventsToTheirOwnEngine(t *testing.T) {
	handlers := []manifest.Handler{
		{ID: "a", Events: []string{"cursor:beforeShellExecution"}, Engines: []string{"cursor", "codex"}, Match: []string{"Bash"}},
	}
	plan, err := BuildPlan(handlers, routerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan[vocab.Cursor]) != 1 || plan[vocab.Cursor][0].Event != "beforeShellExecution" {
		t.Errorf("cursor should get the scoped event, got %+v", plan[vocab.Cursor])
	}
	if len(plan[vocab.Codex]) != 0 {
		t.Errorf("codex should get nothing from a cursor-scoped event, got %+v", plan[vocab.Codex])
	}
}
