package serve

import (
	"testing"

	"github.com/noamsto/hookyard/internal/record"
)

func TestAccumulatorMonotonicOffsetRejection(t *testing.T) {
	acc := NewAccumulator("2026-09-10")

	if !acc.Add(Entry{Rec: record.Record{Verdict: "allow"}, Offset: 10}) {
		t.Fatal("first Add at offset 10 should count")
	}
	if acc.Add(Entry{Rec: record.Record{Verdict: "allow"}, Offset: 10}) {
		t.Error("Add at the same offset should not count")
	}
	if acc.Add(Entry{Rec: record.Record{Verdict: "allow"}, Offset: 5}) {
		t.Error("Add at a lower offset should not count")
	}
	if got := acc.Snapshot().Calls; got != 1 {
		t.Errorf("Calls = %d, want 1 (duplicate/lower offsets must not count)", got)
	}
	if got := acc.Cursor(); got != 10 {
		t.Errorf("Cursor = %d, want 10", got)
	}
}

func TestAccumulatorHandlerMeanAndMax(t *testing.T) {
	acc := NewAccumulator("2026-09-10")
	acc.Add(Entry{
		Rec:    record.Record{Handlers: []record.RecordHandler{{Name: "guard", MS: 10}}},
		Offset: 1,
	})
	acc.Add(Entry{
		Rec:    record.Record{Handlers: []record.RecordHandler{{Name: "guard", MS: 30}}},
		Offset: 2,
	})
	acc.Add(Entry{
		Rec:    record.Record{Handlers: []record.RecordHandler{{Name: "guard", MS: 5}}},
		Offset: 3,
	})

	snap := acc.Snapshot()
	if len(snap.Handlers) != 1 {
		t.Fatalf("got %d handlers, want 1", len(snap.Handlers))
	}
	hs := snap.Handlers[0]
	if hs.Name != "guard" {
		t.Errorf("Name = %q, want guard", hs.Name)
	}
	if hs.Calls != 3 {
		t.Errorf("Calls = %d, want 3", hs.Calls)
	}
	if hs.TotalMS != 45 {
		t.Errorf("TotalMS = %d, want 45", hs.TotalMS)
	}
	if hs.MaxMS != 30 {
		t.Errorf("MaxMS = %d, want 30", hs.MaxMS)
	}
}

func TestAccumulatorVerdictEnforcedRouterMix(t *testing.T) {
	acc := NewAccumulator("2026-09-10")
	acc.Add(Entry{Rec: record.Record{Verdict: "deny", Enforced: true, Router: "ok"}, Offset: 1})
	acc.Add(Entry{Rec: record.Record{Verdict: "deny", Enforced: false, Router: "ok"}, Offset: 2})
	acc.Add(Entry{Rec: record.Record{Verdict: "allow", Enforced: true, Router: "timeout"}, Offset: 3})

	snap := acc.Snapshot()

	deny, ok := snap.Verdicts["deny"]
	if !ok {
		t.Fatal("missing deny verdict")
	}
	if deny.Total != 2 || deny.Enforced != 1 || deny.Unenforced != 1 {
		t.Errorf("deny = %+v, want {Total:2 Enforced:1 Unenforced:1}", deny)
	}

	allow, ok := snap.Verdicts["allow"]
	if !ok {
		t.Fatal("missing allow verdict")
	}
	if allow.Total != 1 || allow.Enforced != 1 || allow.Unenforced != 0 {
		t.Errorf("allow = %+v, want {Total:1 Enforced:1 Unenforced:0}", allow)
	}

	if snap.Router["ok"] != 2 {
		t.Errorf("Router[ok] = %d, want 2", snap.Router["ok"])
	}
	if snap.Router["timeout"] != 1 {
		t.Errorf("Router[timeout] = %d, want 1", snap.Router["timeout"])
	}

	for verdict, vc := range snap.Verdicts {
		if vc.Enforced+vc.Unenforced != vc.Total {
			t.Errorf("verdict %q: Enforced(%d)+Unenforced(%d) != Total(%d)", verdict, vc.Enforced, vc.Unenforced, vc.Total)
		}
	}
}

func TestAccumulatorTruncatedRecordCountsPartialHandlers(t *testing.T) {
	acc := NewAccumulator("2026-09-10")
	acc.Add(Entry{
		Rec: record.Record{
			Truncated: true,
			Handlers:  []record.RecordHandler{{Name: "guard", MS: 7}},
		},
		Offset: 1,
	})

	snap := acc.Snapshot()
	if snap.Truncated != 1 {
		t.Errorf("Truncated = %d, want 1", snap.Truncated)
	}
	if len(snap.Handlers) != 1 || snap.Handlers[0].Name != "guard" {
		t.Errorf("Handlers = %+v, want [{guard ...}]", snap.Handlers)
	}
	if snap.Handlers[0].Calls != 1 {
		t.Errorf("guard.Calls = %d, want 1", snap.Handlers[0].Calls)
	}
}

func TestAccumulatorEmptySnapshotHasNoNilMaps(t *testing.T) {
	snap := NewAccumulator("2026-09-10").Snapshot()
	if snap.Verdicts == nil {
		t.Error("Verdicts is nil, want empty map")
	}
	if snap.Router == nil {
		t.Error("Router is nil, want empty map")
	}
	if snap.Handlers == nil {
		t.Error("Handlers is nil, want empty slice")
	}
	if snap.Calls != 0 {
		t.Errorf("Calls = %d, want 0", snap.Calls)
	}
	if snap.Day != "2026-09-10" {
		t.Errorf("Day = %q, want 2026-09-10", snap.Day)
	}
}

func TestAccumulatorSinceIsFirstRecordTS(t *testing.T) {
	acc := NewAccumulator("2026-09-10")
	acc.Add(Entry{Rec: record.Record{TS: "2026-09-10T00:00:01.000000Z"}, Offset: 1})
	acc.Add(Entry{Rec: record.Record{TS: "2026-09-10T00:00:02.000000Z"}, Offset: 2})

	if got, want := acc.Snapshot().Since, "2026-09-10T00:00:01.000000Z"; got != want {
		t.Errorf("Since = %q, want %q", got, want)
	}
}

func TestStatsForDayMatchesHandFedAccumulator(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{
			TS: "2026-09-10T00:00:01.000000Z", Verdict: "allow", Enforced: true, Router: "ok",
			Handlers: []record.RecordHandler{{Name: "guard", MS: 5}},
		}),
		recLine(t, record.Record{
			TS: "2026-09-10T00:00:02.000000Z", Verdict: "deny", Enforced: false, Router: "ok",
			Truncated: true,
			Handlers:  []record.RecordHandler{{Name: "guard", MS: 12}},
		}),
	}
	writeDayFile(t, stateDir, day, lines)

	got, err := StatsForDay(stateDir, day)
	if err != nil {
		t.Fatalf("StatsForDay: %v", err)
	}

	want := NewAccumulator(day)
	if _, err := ScanAll(stateDir, day, func(e Entry) { want.Add(e) }); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	wantSnap := want.Snapshot()

	if got.Calls != wantSnap.Calls {
		t.Errorf("Calls = %d, want %d", got.Calls, wantSnap.Calls)
	}
	if got.Truncated != wantSnap.Truncated {
		t.Errorf("Truncated = %d, want %d", got.Truncated, wantSnap.Truncated)
	}
	if len(got.Handlers) != len(wantSnap.Handlers) {
		t.Fatalf("Handlers = %+v, want %+v", got.Handlers, wantSnap.Handlers)
	}
	for i := range got.Handlers {
		if got.Handlers[i] != wantSnap.Handlers[i] {
			t.Errorf("Handlers[%d] = %+v, want %+v", i, got.Handlers[i], wantSnap.Handlers[i])
		}
	}
}

func TestStatsForDayMissingFileYieldsEmptySnapshot(t *testing.T) {
	stateDir := t.TempDir()
	snap, err := StatsForDay(stateDir, "2026-01-01")
	if err != nil {
		t.Fatalf("StatsForDay: %v", err)
	}
	if snap.Calls != 0 {
		t.Errorf("Calls = %d, want 0", snap.Calls)
	}
	if snap.Verdicts == nil || snap.Router == nil {
		t.Error("expected empty, non-nil maps for a missing day file")
	}
}
