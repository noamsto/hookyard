package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

func TestFlowForDayGroupsIdenticalTuples(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	base := record.Record{Engine: "codex", CanonicalEvent: "pre_tool_use", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow"}
	r1 := base
	r1.Handlers = []record.RecordHandler{{Name: "h1", Outcome: "allow"}}
	r2 := base
	r2.Handlers = []record.RecordHandler{{Name: "h1", Outcome: "allow"}}
	r3 := base
	r3.Handlers = []record.RecordHandler{{Name: "h1", Outcome: "deny"}} // different outcome -> different path
	writeDayFile(t, stateDir, day, []string{recLine(t, r1), recLine(t, r2), recLine(t, r3)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if len(resp.Paths) != 2 {
		t.Fatalf("got %d paths, want 2: %+v", len(resp.Paths), resp.Paths)
	}
	// Sorted by total desc: the two identical "allow" records (count 2) before
	// the single "deny" one (count 1).
	if resp.Paths[0].Handlers[0].Outcome != "allow" || resp.Paths[0].Counts[0] != 2 {
		t.Errorf("paths[0] = %+v, want allow/2", resp.Paths[0])
	}
	if resp.Paths[1].Handlers[0].Outcome != "deny" || resp.Paths[1].Counts[0] != 1 {
		t.Errorf("paths[1] = %+v, want deny/1", resp.Paths[1])
	}
}

func TestFlowForDayPreservesHandlerOrder(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "codex", CanonicalEvent: "pre_tool_use", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow",
		Handlers: []record.RecordHandler{{Name: "b", Outcome: "allow"}, {Name: "a", Outcome: "deny"}},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if len(resp.Paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(resp.Paths))
	}
	got := resp.Paths[0].Handlers
	want := []FlowHop{{Name: "b", Outcome: "allow"}, {Name: "a", Outcome: "deny"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("handlers = %+v, want %+v (record order preserved)", got, want)
	}
}

func TestFlowForDayDistinctPathShapes(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"

	routerErr := record.Record{Engine: "codex", NativeEvent: "PreToolUse", Router: record.RouterError, Verdict: "abstain"}
	noHandler := record.Record{Engine: "pi", NativeEvent: "turn_end", Router: "ok", Verdict: "allow"} // engine-scoped pi:turn_end, no handlers
	twoHandlers := record.Record{
		Engine: "claude-code", CanonicalEvent: "pre_tool_use", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow",
		Handlers: []record.RecordHandler{{Name: "h1", Outcome: "allow"}, {Name: "h2", Outcome: "advise"}},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, routerErr), recLine(t, noHandler), recLine(t, twoHandlers)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if len(resp.Paths) != 3 {
		t.Fatalf("got %d paths, want 3: %+v", len(resp.Paths), resp.Paths)
	}

	var sawRouterErr, sawNoHandler, sawTwoHandlers bool
	for _, p := range resp.Paths {
		switch {
		case p.Router == record.RouterError:
			sawRouterErr = true
			if p.NativeEvent != "PreToolUse" || len(p.Handlers) != 0 {
				t.Errorf("router-error path = %+v", p)
			}
		case p.Engine == "pi" && p.NativeEvent == "turn_end":
			sawNoHandler = true
			if p.CanonicalEvent != "" || len(p.Handlers) != 0 {
				t.Errorf("no-handler pi:turn_end path = %+v", p)
			}
		case len(p.Handlers) == 2:
			sawTwoHandlers = true
		}
	}
	if !sawRouterErr || !sawNoHandler || !sawTwoHandlers {
		t.Fatalf("missing expected shapes (routerErr=%v noHandler=%v twoHandlers=%v): %+v",
			sawRouterErr, sawNoHandler, sawTwoHandlers, resp.Paths)
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"handlers":[]`) {
		t.Errorf("json has no handlers:[] (should be [] not null for the no-handler rows): %s", b)
	}
	if strings.Contains(string(b), `"handlers":null`) {
		t.Errorf("json has handlers:null, want []: %s", b)
	}
}

func TestFlowForDayFilterMatchesRecordSubset(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"

	recs := []record.Record{
		{Engine: "codex", CanonicalEvent: "pre_tool_use", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow",
			SessionID: "sess-a", Handlers: []record.RecordHandler{{Name: "h1", Outcome: "allow"}}},
		{Engine: "claude-code", CanonicalEvent: "pre_tool_use", NativeEvent: "PreToolUse", Router: "ok", Verdict: "deny",
			SessionID: "sess-b", Handlers: []record.RecordHandler{{Name: "h2", Outcome: "deny"}}},
		{Engine: "pi", NativeEvent: "turn_end", Router: "ok", Verdict: "allow", SessionID: "sess-c"},                      // engine-scoped pi:turn_end label
		{Engine: "codex", NativeEvent: "PreToolUse", Router: record.RouterError, Verdict: "abstain", SessionID: "sess-d"}, // router-error native_event
	}
	var lines []string
	for _, r := range recs {
		lines = append(lines, recLine(t, r))
	}
	writeDayFile(t, stateDir, day, lines)

	cases := []Filter{
		{Engines: []string{"codex"}},
		{Events: []string{"pi:turn_end"}},
		{Events: []string{"PreToolUse"}},
		{Handlers: []string{"h2"}},
		{Verdicts: []string{"deny"}}, // handler-outcome superset
		{Session: "sess-a"},
	}

	for _, f := range cases {
		t.Run(fmt.Sprintf("%+v", f), func(t *testing.T) {
			resp, err := FlowForDay(stateDir, day, 0, time.Now(), f)
			if err != nil {
				t.Fatalf("FlowForDay: %v", err)
			}

			// Oracle: count records via Filter.Match directly on the fixture,
			// never by re-calling FlowForDay.
			var want int64
			for _, r := range recs {
				if f.Match(r) {
					want++
				}
			}
			if resp.Calls != want {
				t.Errorf("calls = %d, want %d", resp.Calls, want)
			}

			var got int64
			for _, p := range resp.Paths {
				for _, c := range p.Counts {
					got += c
				}
			}
			if got != want {
				t.Errorf("sum of path counts = %d, want %d", got, want)
			}
		})
	}
}

func TestFlowForDayWindowBuckets(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	now := time.Date(2026, 9, 10, 12, 5, 30, 0, time.UTC)

	mk := func(ts string) record.Record {
		return record.Record{TS: ts, Engine: "codex", Verdict: "allow", Router: "ok"}
	}
	lines := []string{
		recLine(t, mk("2026-09-10T12:02:59.000000Z")), // before window start -> skipped
		recLine(t, mk("2026-09-10T12:03:00.000000Z")), // idx 0
		recLine(t, mk("2026-09-10T12:05:59.000000Z")), // idx 2
		recLine(t, mk("2026-09-10T12:07:00.000000Z")), // future, clamped -> idx 2
		recLine(t, mk("not-a-timestamp")),             // unparsable -> skipped
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := FlowForDay(stateDir, day, 3, now, Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}

	wantStart := time.Date(2026, 9, 10, 12, 3, 0, 0, time.UTC).UnixMilli()
	if resp.BucketStart != wantStart {
		t.Errorf("bucket_start = %d, want %d", resp.BucketStart, wantStart)
	}
	if resp.BucketMS != 60000 {
		t.Errorf("bucket_ms = %d, want 60000", resp.BucketMS)
	}
	if resp.Calls != 3 {
		t.Errorf("calls = %d, want 3 (only in-window, parseable records)", resp.Calls)
	}
	if len(resp.Paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(resp.Paths))
	}
	counts := resp.Paths[0].Counts
	want := []int64{1, 0, 2}
	if len(counts) != 3 || counts[0] != want[0] || counts[1] != want[1] || counts[2] != want[2] {
		t.Errorf("counts = %v, want %v", counts, want)
	}
}

func TestFlowForDayWholeDayIsOneBucket(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	writeDayFile(t, stateDir, day, []string{recLine(t, record.Record{Engine: "codex", Verdict: "allow", Router: "ok"})})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp.BucketStart != 0 {
		t.Errorf("bucket_start = %d, want 0", resp.BucketStart)
	}
	if resp.BucketMS != 0 {
		t.Errorf("bucket_ms = %d, want 0", resp.BucketMS)
	}
	if len(resp.Paths) != 1 || len(resp.Paths[0].Counts) != 1 {
		t.Fatalf("paths = %+v, want one path with one bucket", resp.Paths)
	}
}

func TestFlowForDayMissingOrInvalidDayIsEmptyNotError(t *testing.T) {
	stateDir := t.TempDir()
	for _, day := range []string{"2026-09-10", "../x"} {
		resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
		if err != nil {
			t.Fatalf("day %q: FlowForDay: %v", day, err)
		}
		if len(resp.Paths) != 0 {
			t.Errorf("day %q: paths = %+v, want empty", day, resp.Paths)
		}
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("day %q: marshal: %v", day, err)
		}
		if !strings.Contains(string(b), `"paths":[]`) {
			t.Errorf("day %q: json = %s, want paths:[]", day, b)
		}
	}
}

func TestFlowForDayNextOffsetExcludesPartialLine(t *testing.T) {
	stateDir := t.TempDir()
	line := recLine(t, record.Record{Engine: "codex", Verdict: "allow", Router: "ok"})

	partialDay := "2026-09-10"
	writeDayFile(t, stateDir, partialDay, []string{line, `{"engine":"codex"`}) // no trailing newline
	resp, err := FlowForDay(stateDir, partialDay, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp.NextOffset != int64(len(line)) {
		t.Errorf("next_offset = %d, want %d (partial trailing line excluded)", resp.NextOffset, len(line))
	}

	fullDay := "2026-09-11"
	writeDayFile(t, stateDir, fullDay, []string{line})
	resp2, err := FlowForDay(stateDir, fullDay, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp2.NextOffset != int64(len(line)) {
		t.Errorf("next_offset = %d, want %d (file size, ends with newline)", resp2.NextOffset, len(line))
	}
}

func TestClampWindow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"non-numeric", "abc", 0},
		{"negative", "-1", 0},
		{"zero", "0", 0},
		{"in-range", "10", 10},
		{"above-max", "61", 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampWindow(c.in); got != c.want {
				t.Errorf("clampWindow(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
