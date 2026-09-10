package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

func TestStreamFindingsCountsEnforcedFalseAndSkipsTornLine(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	path := record.StreamPath(stateDir, now)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"enforced":true}`,
		`{"enforced":false}`,
		`{"enforced":true}`,
		`{"enforced":false}`,
		`{"enforced":`, // torn/malformed, e.g. a crash mid-write
		`{"enforced":true}`,
	}
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	findings := streamFindings(stateDir, now)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass", f.Status)
	}
	want := "2/5 events today had a computed verdict the engine could not enforce"
	if f.Detail != want {
		t.Errorf("detail = %q, want %q", f.Detail, want)
	}
}

func TestStreamFindingsUnknownWhenNoFile(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	findings := streamFindings(stateDir, now)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	if findings[0].Status != Unknown {
		t.Errorf("status = %v, want Unknown", findings[0].Status)
	}
}
