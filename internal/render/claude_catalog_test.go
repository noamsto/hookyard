package render

import (
	"encoding/json"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

func TestClaudeCatalogPlanRendersOneEntryPerCatalogEventInOrder(t *testing.T) {
	entries, err := ClaudeCatalogPlan(routerPath, "/state")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(vocab.ClaudeCodeCatalog) {
		t.Fatalf("got %d entries, want %d", len(entries), len(vocab.ClaudeCodeCatalog))
	}
	for i, catalog := range vocab.ClaudeCodeCatalog {
		want := Entry{
			Event:   catalog.Native,
			Matcher: "",
			Command: command(routerPath, vocab.ClaudeCode, catalog.Routed, "/state"),
		}
		if entries[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want)
		}
	}
}

func TestClaudeCatalogPlanEntriesRenderIntoOneGroupPerEventWithNoMatcher(t *testing.T) {
	entries, err := ClaudeCatalogPlan(routerPath, "/state")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ClaudeSettings(nil, entries)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}
	if len(parsed.Hooks) != len(vocab.ClaudeCodeCatalog) {
		t.Fatalf("got %d event keys, want %d: %+v", len(parsed.Hooks), len(vocab.ClaudeCodeCatalog), parsed.Hooks)
	}
	for _, catalog := range vocab.ClaudeCodeCatalog {
		groups, ok := parsed.Hooks[catalog.Native]
		if !ok {
			t.Errorf("no group for %s", catalog.Native)
			continue
		}
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Errorf("%s: want one group with one hook, got %+v", catalog.Native, groups)
			continue
		}
		if groups[0].Matcher != nil {
			t.Errorf("%s: matcher = %v, want no matcher key at all", catalog.Native, *groups[0].Matcher)
		}
	}
}

func TestClaudeCatalogPlanRefusesARelativeRouterPath(t *testing.T) {
	if _, err := ClaudeCatalogPlan("bin/hookyard", "/state"); err == nil {
		t.Fatal("want an error for a relative router path, got nil")
	}
}

func TestClaudeCatalogPlanRefusesARouterPathWithoutTheMarker(t *testing.T) {
	if _, err := ClaudeCatalogPlan("/opt/other/router", "/state"); err == nil {
		t.Fatal("want an error for a router path missing the marker, got nil")
	}
}
