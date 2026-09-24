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
		{Verdicts: []string{"deny"}}, // D2: call-level only — the consolidated verdict or router status, never a handler's own outcome
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

// TestFlowForDayHandlerFilterPrunesToOneHop covers D3's pruning rule under a
// branch filter: a matched path keeps only the matching branch's hop, and
// two calls differing only in a filtered-out handler's outcome merge.
func TestFlowForDayHandlerFilterPrunesToOneHop(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	mk := func(deslop, other string) record.Record {
		return record.Record{
			Engine: "claude-code", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "abstain",
			Handlers: []record.RecordHandler{
				{Name: "guards.deslop", Outcome: deslop},
				{Name: "guards.other", Outcome: other},
			},
		}
	}
	lines := []string{
		recLine(t, mk("abstain", "allow")),
		recLine(t, mk("abstain", "deny")), // differs only in guards.other's (filtered-out) outcome
		recLine(t, mk("deny", "allow")),
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{Handlers: []string{"guards.deslop"}})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	for _, p := range resp.Paths {
		if len(p.Handlers) != 1 || p.Handlers[0].Name != "guards.deslop" {
			t.Errorf("path %+v, want exactly one hop for guards.deslop", p)
		}
	}
	if resp.Calls != 3 {
		t.Errorf("calls = %d, want 3", resp.Calls)
	}
	if resp.Branches != resp.Calls {
		t.Errorf("branches = %d, calls = %d, want equal (one matched branch per call)", resp.Branches, resp.Calls)
	}
	if len(resp.Paths) != 2 {
		t.Fatalf("got %d paths, want 2 (the two abstain-deslop records merge): %+v", len(resp.Paths), resp.Paths)
	}
}

func TestFlowForDayOutcomeFilterPrunesToOneHop(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "claude-code", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "deny",
		Handlers: []record.RecordHandler{
			{Name: "guards.a", Outcome: "abstain"},
			{Name: "guards.deny", Outcome: "deny"},
			{Name: "guards.c", Outcome: "abstain"},
		},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{Outcomes: []string{"deny"}})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if len(resp.Paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(resp.Paths))
	}
	got := resp.Paths[0].Handlers
	if len(got) != 1 || got[0] != (FlowHop{Name: "guards.deny", Outcome: "deny"}) {
		t.Errorf("handlers = %+v, want exactly [{guards.deny deny}] (the denier only)", got)
	}
	if resp.Branches != 1 || resp.Calls != 1 {
		t.Errorf("branches = %d, calls = %d, want 1 and 1", resp.Branches, resp.Calls)
	}
}

func TestFlowForDayPseudoAndDirectBranchesCountAsOne(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	routerErr := record.Record{Engine: "codex", NativeEvent: "PreToolUse", Router: record.RouterError, Verdict: "abstain"}
	noHandler := record.Record{Engine: "pi", NativeEvent: "turn_end", Router: "ok", Verdict: "allow"}
	writeDayFile(t, stateDir, day, []string{recLine(t, routerErr), recLine(t, noHandler)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp.Branches != 2 {
		t.Errorf("branches = %d, want 2 (each pseudo/direct branch counts as 1)", resp.Branches)
	}
	for _, p := range resp.Paths {
		if len(p.Handlers) != 0 {
			t.Errorf("path %+v, want empty hops", p)
		}
	}
}

func TestFlowForDayFacetsHandlerFilterIgnoresOwnField(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "claude-code", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "abstain",
		Handlers: []record.RecordHandler{
			{Name: "guards.deslop", Outcome: "deny"},
			{Name: "guards.other", Outcome: "allow"},
		},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{Handlers: []string{"guards.deslop"}})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp.Facets.Handler["guards.other"] != 1 {
		t.Errorf("handler facet[guards.other] = %d, want 1 (the handler facet ignores its own field)", resp.Facets.Handler["guards.other"])
	}
	if resp.Facets.Handler["guards.deslop"] != 1 {
		t.Errorf("handler facet[guards.deslop] = %d, want 1", resp.Facets.Handler["guards.deslop"])
	}
	// Same-branch semantics: the outcome facet stays restricted to
	// guards.deslop's own branches, so guards.other's "allow" never appears.
	if resp.Facets.Outcome["deny"] != 1 {
		t.Errorf("outcome facet[deny] = %d, want 1", resp.Facets.Outcome["deny"])
	}
	if _, ok := resp.Facets.Outcome["allow"]; ok {
		t.Error("outcome facet has \"allow\", want absent (only guards.deslop's own branches count)")
	}
}

func TestFlowForDayFacetsOutcomeFilterIgnoresOwnField(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	rec := record.Record{
		Engine: "claude-code", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "deny",
		Handlers: []record.RecordHandler{
			{Name: "guards.a", Outcome: "abstain"},
			{Name: "guards.deny", Outcome: "deny"},
		},
	}
	writeDayFile(t, stateDir, day, []string{recLine(t, rec)})

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{Outcomes: []string{"deny"}})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	// Same-branch semantics: the handler facet stays restricted to deny
	// branches, so guards.a (abstain) never appears.
	if resp.Facets.Handler["guards.deny"] != 1 {
		t.Errorf("handler facet[guards.deny] = %d, want 1", resp.Facets.Handler["guards.deny"])
	}
	if _, ok := resp.Facets.Handler["guards.a"]; ok {
		t.Error("handler facet has guards.a, want absent (only deny branches count)")
	}
	// The outcome facet ignores its own field, so abstain still shows.
	if resp.Facets.Outcome["abstain"] != 1 {
		t.Errorf("outcome facet[abstain] = %d, want 1 (the outcome facet ignores its own field)", resp.Facets.Outcome["abstain"])
	}
	if resp.Facets.Outcome["deny"] != 1 {
		t.Errorf("outcome facet[deny] = %d, want 1", resp.Facets.Outcome["deny"])
	}
}

func TestFlowForDayFacetsEngineFilterAppliesToEventNotEngine(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{Engine: "pi", NativeEvent: "turn_end", Router: "ok", Verdict: "allow"}),
		recLine(t, record.Record{Engine: "claude-code", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow"}),
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{Engines: []string{"pi"}})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	// The engine facet ignores its own field, so claude-code still shows.
	if resp.Facets.Engine["claude-code"] != 1 {
		t.Errorf("engine facet[claude-code] = %d, want 1 (the engine facet ignores its own field)", resp.Facets.Engine["claude-code"])
	}
	if resp.Facets.Engine["pi"] != 1 {
		t.Errorf("engine facet[pi] = %d, want 1", resp.Facets.Engine["pi"])
	}
	// ...but the event facet still applies the engine filter (not its own
	// field), so only pi's events appear.
	if resp.Facets.Event["pi:turn_end"] != 1 {
		t.Errorf("event facet[pi:turn_end] = %d, want 1", resp.Facets.Event["pi:turn_end"])
	}
	if _, ok := resp.Facets.Event["pre_tool"]; ok {
		t.Error("event facet has pre_tool, want absent (engine=pi excludes claude-code's event)")
	}
}

func TestFlowForDayFacetsRespectWindow(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	now := time.Date(2026, 9, 10, 12, 5, 30, 0, time.UTC)
	mk := func(ts, engine string) record.Record {
		return record.Record{TS: ts, Engine: engine, Verdict: "allow", Router: "ok"}
	}
	lines := []string{
		recLine(t, mk("2026-09-10T12:02:59.000000Z", "codex")), // before window -> excluded
		recLine(t, mk("2026-09-10T12:04:00.000000Z", "pi")),    // in window
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := FlowForDay(stateDir, day, 3, now, Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}
	if resp.Facets.Engine["codex"] != 0 {
		t.Errorf("engine facet[codex] = %d, want 0 (outside the window)", resp.Facets.Engine["codex"])
	}
	if resp.Facets.Engine["pi"] != 1 {
		t.Errorf("engine facet[pi] = %d, want 1", resp.Facets.Engine["pi"])
	}
}

// TestFlowForDayFacetsMatchDrawnTotalsWithNoFilter is the no-filter sanity
// check: with nothing to ignore, every facet equals the totals the paths
// themselves draw (engine/event per call, handler/outcome per branch).
func TestFlowForDayFacetsMatchDrawnTotalsWithNoFilter(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{Engine: "codex", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "allow",
			Handlers: []record.RecordHandler{{Name: "h1", Outcome: "allow"}, {Name: "h2", Outcome: "deny"}}}),
		recLine(t, record.Record{Engine: "pi", NativeEvent: "turn_end", Router: "ok", Verdict: "abstain"}),
		recLine(t, record.Record{Engine: "codex", NativeEvent: "PreToolUse", Router: record.RouterError, Verdict: "abstain"}),
	}
	writeDayFile(t, stateDir, day, lines)

	resp, err := FlowForDay(stateDir, day, 0, time.Now(), Filter{})
	if err != nil {
		t.Fatalf("FlowForDay: %v", err)
	}

	gotEngine, gotEvent := map[string]int64{}, map[string]int64{}
	gotHandler, gotOutcome := map[string]int64{}, map[string]int64{}
	for _, p := range resp.Paths {
		var n int64
		for _, c := range p.Counts {
			n += c
		}
		gotEngine[p.Engine] += n

		label := p.CanonicalEvent
		switch {
		case p.Router == record.RouterError:
			label = p.NativeEvent
		case label == "" && p.Engine != "" && p.NativeEvent != "":
			label = p.Engine + ":" + p.NativeEvent
		}
		gotEvent[label] += n

		if len(p.Handlers) == 0 {
			outcome := p.Verdict
			if p.Router == record.RouterError {
				outcome = outcomeRouterError
			}
			gotOutcome[outcome] += n
			continue
		}
		for _, h := range p.Handlers {
			gotHandler[h.Name] += n
			gotOutcome[h.Outcome] += n
		}
	}

	for k, v := range gotEngine {
		if resp.Facets.Engine[k] != v {
			t.Errorf("engine facet[%s] = %d, want %d", k, resp.Facets.Engine[k], v)
		}
	}
	for k, v := range gotEvent {
		if resp.Facets.Event[k] != v {
			t.Errorf("event facet[%s] = %d, want %d", k, resp.Facets.Event[k], v)
		}
	}
	for k, v := range gotHandler {
		if resp.Facets.Handler[k] != v {
			t.Errorf("handler facet[%s] = %d, want %d", k, resp.Facets.Handler[k], v)
		}
	}
	for k, v := range gotOutcome {
		if resp.Facets.Outcome[k] != v {
			t.Errorf("outcome facet[%s] = %d, want %d", k, resp.Facets.Outcome[k], v)
		}
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
