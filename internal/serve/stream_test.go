package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/record"
)

func writeDayFile(t *testing.T, stateDir, day string, lines []string) string {
	t.Helper()
	dir := record.StreamDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, day+".jsonl")
	var content string
	for _, l := range lines {
		content += l
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func recLine(t *testing.T, rec record.Record) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b) + "\n"
}

func TestScanDayNewestFirstAndLimit(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	var lines []string
	for i := range 5 {
		lines = append(lines, recLine(t, record.Record{Engine: "codex", SessionID: fmt.Sprintf("s%d", i)}))
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := ScanDay(stateDir, day, 0, 3, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 3 {
		t.Fatalf("got %d records, want 3", len(resp.Records))
	}
	want := []string{"s4", "s3", "s2"}
	for i, w := range want {
		if got := resp.Records[i].Rec.SessionID; got != w {
			t.Errorf("Records[%d].SessionID = %q, want %q", i, got, w)
		}
	}
}

// TestScanDaySetsHitsUnderABranchFilter verifies ScanDay hands each Entry
// the branch indices the request's filter matched.
func TestScanDaySetsHitsUnderABranchFilter(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "codex", Router: record.RouterOK, Verdict: "deny",
		Handlers: []record.RecordHandler{
			{Name: "h0", Outcome: "abstain"},
			{Name: "h1", Outcome: "deny"},
		},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := ScanDay(stateDir, day, 0, 10, Filter{Outcomes: []string{"deny"}})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(resp.Records))
	}
	if got := resp.Records[0].Hits; len(got) != 1 || got[0] != 1 {
		t.Errorf("Hits = %v, want [1]", got)
	}
}

// TestScanDayNoFilterOmitsHits verifies the "no branch filter" wire shape:
// Hits is nil, and the omitempty tag drops the key from the JSON entirely.
func TestScanDayNoFilterOmitsHits(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "codex", Router: record.RouterOK, Verdict: "allow",
		Handlers: []record.RecordHandler{{Name: "h0", Outcome: "allow"}},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := ScanDay(stateDir, day, 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(resp.Records))
	}
	if got := resp.Records[0].Hits; got != nil {
		t.Errorf("Hits = %v, want nil", got)
	}
	b, err := json.Marshal(resp.Records[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"hits"`) {
		t.Errorf("json = %s, want no \"hits\" key (omitempty)", b)
	}
}

func TestScanAllExcludesTrailingPartialLine(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"

	whole := recLine(t, record.Record{Engine: "codex", SessionID: "whole"})
	full := recLine(t, record.Record{Engine: "codex", SessionID: "torn"})
	torn := full[:len(full)-2] // drop the closing "}\n": a write caught mid-record
	writeDayFile(t, stateDir, day, []string{whole, torn})

	var got []Entry
	offset, err := ScanAll(stateDir, day, func(e Entry) { got = append(got, e) })
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(got) != 1 || got[0].Rec.SessionID != "whole" {
		t.Fatalf("visited %+v, want exactly the complete record", got)
	}
	if want := int64(len(whole)); offset != want {
		t.Fatalf("offset = %d, want %d (just past the complete record, excluding the torn suffix)", offset, want)
	}
}

func TestScanDayMalformedLineSkipped(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{SessionID: "good1"}),
		"not json\n",
		recLine(t, record.Record{SessionID: "good2"}),
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := ScanDay(stateDir, day, 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("got %d records, want 2 (malformed line should be skipped)", len(resp.Records))
	}
}

func TestScanDayMissingFile(t *testing.T) {
	stateDir := t.TempDir()
	resp, err := ScanDay(stateDir, "2026-01-01", 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 0 {
		t.Errorf("got %d records, want 0", len(resp.Records))
	}
	if resp.Windowed {
		t.Errorf("Windowed = true, want false for a missing file")
	}
}

// TestScanDayRejectsPathTraversal pins the day-value guard: an out-of-shape
// day must never reach filepath.Join, or a request could read any .jsonl
// file on the host, not just one under stateDir/stream.
func TestScanDayRejectsPathTraversal(t *testing.T) {
	stateDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(outside, []byte(recLine(t, record.Record{SessionID: "leaked"})), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	rel, err := filepath.Rel(record.StreamDir(stateDir), outside[:len(outside)-len(".jsonl")])
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}

	resp, err := ScanDay(stateDir, rel, 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 0 {
		t.Fatalf("got %d records from a path-traversal day value, want 0 (file outside stateDir was read)", len(resp.Records))
	}

	if n, err := ScanAll(stateDir, rel, func(Entry) {}); err != nil || n != 0 {
		t.Fatalf("ScanAll(%q) = %d, %v, want 0, nil", rel, n, err)
	}
}

func TestDaysMissingStreamDir(t *testing.T) {
	stateDir := t.TempDir()
	days, err := Days(stateDir)
	if err != nil {
		t.Fatalf("Days: %v", err)
	}
	if days != nil {
		t.Errorf("Days = %v, want nil for a missing stream dir", days)
	}
}

func TestDaysNewestFirst(t *testing.T) {
	stateDir := t.TempDir()
	line := recLine(t, record.Record{SessionID: "s"})
	writeDayFile(t, stateDir, "2026-09-08", []string{line})
	writeDayFile(t, stateDir, "2026-09-10", []string{line})
	writeDayFile(t, stateDir, "2026-09-09", []string{line})
	if err := os.WriteFile(filepath.Join(record.StreamDir(stateDir), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	days, err := Days(stateDir)
	if err != nil {
		t.Fatalf("Days: %v", err)
	}
	want := []string{"2026-09-10", "2026-09-09", "2026-09-08"}
	if len(days) != len(want) {
		t.Fatalf("Days = %v, want %v", days, want)
	}
	for i, w := range want {
		if days[i] != w {
			t.Errorf("Days[%d] = %q, want %q", i, days[i], w)
		}
	}
}

func TestScanDayWindowedPaging(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	var lines []string
	for i := range 30 {
		lines = append(lines, recLine(t, record.Record{SessionID: fmt.Sprintf("s%02d", i)}))
	}
	writeDayFile(t, stateDir, day, lines)

	// Shrink the window to a few lines' worth so paging is exercised without
	// a 64 MiB fixture: every line here is the same length (fixed-width
	// SessionID), so this comfortably fits a whole record after the
	// mandatory discard-the-partial-line resync.
	orig := scanWindow
	scanWindow = int64(len(lines[0]) * 4)
	defer func() { scanWindow = orig }()

	seen := map[string]bool{}
	sawWindowed := false
	var prevEnd int64
	end := int64(0)
	for page := 0; page < 30; page++ {
		resp, err := ScanDay(stateDir, day, end, 100, Filter{})
		if err != nil {
			t.Fatalf("ScanDay page %d: %v", page, err)
		}
		if resp.Windowed {
			sawWindowed = true
		}
		for _, e := range resp.Records {
			seen[e.Rec.SessionID] = true
			if prevEnd != 0 && e.Offset >= prevEnd {
				t.Errorf("page %d: entry offset %d >= previous page's end %d", page, e.Offset, prevEnd)
			}
		}
		if !resp.Windowed || len(resp.Records) == 0 {
			break
		}
		prevEnd = end
		end = resp.Records[len(resp.Records)-1].Offset
	}

	if !sawWindowed {
		t.Error("expected at least one windowed page")
	}
	if !seen["s00"] {
		t.Error("expected paging back to eventually reach the oldest record (s00)")
	}
}

func TestScanDayOffsetsAbsoluteAndMonotonic(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	var lines []string
	for i := range 5 {
		lines = append(lines, recLine(t, record.Record{SessionID: fmt.Sprintf("s%d", i)}))
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := ScanDay(stateDir, day, 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	for i := 1; i < len(resp.Records); i++ {
		if resp.Records[i].Offset >= resp.Records[i-1].Offset {
			t.Errorf("offsets not strictly decreasing at %d: %d >= %d", i, resp.Records[i].Offset, resp.Records[i-1].Offset)
		}
	}

	seedEnd, err := ScanAll(stateDir, day, func(Entry) {})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if resp.Records[0].Offset != seedEnd {
		t.Errorf("newest offset %d != file size %d", resp.Records[0].Offset, seedEnd)
	}
}

func TestScanDayTruncatedMinimalRecordDoesNotPanic(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	line := `{"v":1,"ts":"2026-09-10T00:00:00.000000Z","key":"k","engine":"codex",` +
		`"session_id":"s","verdict":"deny","router":"ok","truncated":true}` + "\n"
	writeDayFile(t, stateDir, day, []string{line})

	resp, err := ScanDay(stateDir, day, 0, 10, Filter{})
	if err != nil {
		t.Fatalf("ScanDay: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(resp.Records))
	}
	rec := resp.Records[0].Rec
	if rec.Handlers != nil {
		t.Errorf("Handlers = %v, want nil", rec.Handlers)
	}
	if !rec.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{0, defaultLimit},
		{-5, defaultLimit},
		{999999, maxLimit},
		{10, 10},
	}
	for _, tt := range tests {
		if got := clampLimit(tt.in); got != tt.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestScanAllForwardOrder(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{SessionID: "a"}),
		recLine(t, record.Record{SessionID: "b"}),
	}
	path := writeDayFile(t, stateDir, day, lines)

	var got []string
	seedEnd, err := ScanAll(stateDir, day, func(e Entry) { got = append(got, e.Rec.SessionID) })
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("visited %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if seedEnd != info.Size() {
		t.Errorf("seedEnd = %d, want file size %d", seedEnd, info.Size())
	}
}

func TestScanAllMissingFile(t *testing.T) {
	stateDir := t.TempDir()
	n, err := ScanAll(stateDir, "2026-01-01", func(Entry) { t.Fatal("visit should not be called for a missing file") })
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}
