package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

func serveOpts(t *testing.T, stateDir string) Options {
	t.Helper()
	return Options{
		StateDir: stateDir,
		Port:     0,
		Out:      nil,
		Now:      time.Now,
	}
}

// runTestServer starts the server on port 0 and returns its base URL. The
// caller closes ctx to shut it down.
func runTestServer(t *testing.T, ctx context.Context, opts Options) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(opts.Port)))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	host, port, _ := net.SplitHostPort(addr)
	base := fmt.Sprintf("http://%s:%s", host, port)

	// Resolve the state dir early, same as the production Run.
	absDir, err := filepath.Abs(opts.StateDir)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("abs: %v", err)
	}

	hub := NewHub(absDir, opts.Now)
	if err := hub.Seed(); err != nil {
		_ = ln.Close()
		t.Fatalf("seed: %v", err)
	}

	srv := &http.Server{
		Handler: middleware(&serveMux{
			stateDir: absDir,
			hub:      hub,
			port:     port,
		}),
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case err := <-hubErr:
			if err != nil && err != context.Canceled {
				t.Errorf("hub run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("hub did not return")
		}
	})

	return base
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

func getWithHost(t *testing.T, url, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s (Host=%s): %v", url, host, err)
	}
	return resp
}

func TestListenAddressIsLoopback(t *testing.T) {
	for _, port := range []int{0, 7757, 9999} {
		addr := net.JoinHostPort(listenHost, strconv.Itoa(port))
		host := net.JoinHostPort(listenHost, "*")
		_ = host
		tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatalf("ResolveTCPAddr(%q): %v", addr, err)
		}
		if !tcpAddr.IP.IsLoopback() {
			t.Errorf("%q: IP %v is not loopback", addr, tcpAddr.IP)
		}
		if tcpAddr.IP.String() != "127.0.0.1" {
			t.Errorf("%q: IP is %v, want 127.0.0.1", addr, tcpAddr.IP)
		}
	}
}

func TestHostGuard(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	// Normal localhost access.
	// base is http://127.0.0.1:<port>; extract just the port.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	localhostHost := "localhost:" + port
	loopbackHost := "127.0.0.1:" + port

	resp := getWithHost(t, base+"/api/days", localhostHost)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("localhost Host: %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	// 127.0.0.1 access.
	resp = getWithHost(t, base+"/api/days", loopbackHost)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("127.0.0.1 Host: %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	// Foreign host → 403.
	resp = getWithHost(t, base+"/api/days", "evil.example:"+port)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	_ = resp.Body.Close()
}

func TestMethodNotAllowed(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	for _, path := range []string{"/api/events", "/api/flow", "/api/days", "/api/stats", "/api/table", "/api/stream"} {
		req, _ := http.NewRequest(http.MethodPost, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s POST: %v", path, err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s POST: %d, want %d", path, resp.StatusCode, http.StatusMethodNotAllowed)
		}
		allow := resp.Header.Get("Allow")
		if allow != http.MethodGet {
			t.Errorf("%s Allow: %q, want %q", path, allow, http.MethodGet)
		}
		_ = resp.Body.Close()
	}
}

func TestEventsHonorsLimitAndFilter(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"

	var lines []string
	for i := range 10 {
		eng := "codex"
		if i%2 == 0 {
			eng = "claude-code"
		}
		lines = append(lines, recLine(t, record.Record{Engine: eng, SessionID: fmt.Sprintf("s%d", i)}))
	}
	writeDayFile(t, stateDir, day, lines)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	url := fmt.Sprintf("%s/api/events?day=%s&limit=3", base, day)
	resp := get(t, url)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var eventsResp EventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&eventsResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(eventsResp.Records) != 3 {
		t.Fatalf("got %d records, want 3", len(eventsResp.Records))
	}
	// Newest first (s9 first).
	if !strings.Contains(eventsResp.Records[0].Rec.SessionID, "s9") {
		t.Errorf("first record session = %q, want s9", eventsResp.Records[0].Rec.SessionID)
	}

	// Filter by engine.
	url2 := fmt.Sprintf("%s/api/events?day=%s&limit=50&engine=codex", base, day)
	resp2 := get(t, url2)
	defer func() { _ = resp2.Body.Close() }()
	if err := json.NewDecoder(resp2.Body).Decode(&eventsResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, rec := range eventsResp.Records {
		if rec.Rec.Engine != "codex" {
			t.Errorf("filtered record has engine %q, want codex", rec.Rec.Engine)
		}
	}
}

func TestFlowEndpointDefaultsDayAndFilters(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	day := DayString(now)
	ts := now.Format("2006-01-02T15:04:05.000000Z")

	lines := []string{
		recLine(t, record.Record{TS: ts, Engine: "codex", Verdict: "allow", Router: "ok"}),
		recLine(t, record.Record{TS: ts, Engine: "claude-code", Verdict: "deny", Router: "ok"}),
	}
	writeDayFile(t, stateDir, day, lines)

	opts := serveOpts(t, stateDir)
	opts.Now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runTestServer(t, ctx, opts)

	resp := get(t, base+"/api/flow")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var flowResp FlowResponse
	if err := json.NewDecoder(resp.Body).Decode(&flowResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if flowResp.Day != day {
		t.Errorf("day = %q, want hub day %q", flowResp.Day, day)
	}
	if len(flowResp.Paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(flowResp.Paths))
	}

	resp2 := get(t, base+"/api/flow?day="+day+"&window=5&engine=codex")
	defer func() { _ = resp2.Body.Close() }()
	var filtered FlowResponse
	if err := json.NewDecoder(resp2.Body).Decode(&filtered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if filtered.Window != 5 {
		t.Errorf("window = %d, want 5", filtered.Window)
	}
	if len(filtered.Paths) != 1 || filtered.Paths[0].Engine != "codex" {
		t.Fatalf("filtered paths = %+v, want one codex path", filtered.Paths)
	}
}

// TestFlowEndpointOutcomeFilter verifies the outcome query param reaches
// FlowForDay through ParseFilter, and the response carries pruned paths plus
// branches and facets.
func TestFlowEndpointOutcomeFilter(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	day := DayString(now)
	ts := now.Format("2006-01-02T15:04:05.000000Z")

	lines := []string{
		recLine(t, record.Record{
			TS: ts, Engine: "codex", CanonicalEvent: "pre_tool", NativeEvent: "PreToolUse", Router: "ok", Verdict: "deny",
			Handlers: []record.RecordHandler{
				{Name: "guards.a", Outcome: "abstain"},
				{Name: "guards.deny", Outcome: "deny"},
			},
		}),
	}
	writeDayFile(t, stateDir, day, lines)

	opts := serveOpts(t, stateDir)
	opts.Now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runTestServer(t, ctx, opts)

	resp := get(t, base+"/api/flow?outcome=deny&day="+day)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var flowResp FlowResponse
	if err := json.NewDecoder(resp.Body).Decode(&flowResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flowResp.Paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(flowResp.Paths))
	}
	if got := flowResp.Paths[0].Handlers; len(got) != 1 || got[0].Name != "guards.deny" {
		t.Errorf("handlers = %+v, want exactly [guards.deny] (pruned)", got)
	}
	if flowResp.Branches != 1 {
		t.Errorf("branches = %d, want 1", flowResp.Branches)
	}
	if flowResp.Facets.Handler["guards.deny"] != 1 {
		t.Errorf("facets.handler[guards.deny] = %d, want 1", flowResp.Facets.Handler["guards.deny"])
	}
}

// TestEventsEndpointOutcomeFilterCarriesHits verifies /api/events entries
// carry hits under a branch filter, reaching ScanDay through ParseFilter.
func TestEventsEndpointOutcomeFilterCarriesHits(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	lines := []string{
		recLine(t, record.Record{
			Engine: "codex", Router: record.RouterOK, Verdict: "deny",
			Handlers: []record.RecordHandler{
				{Name: "guards.a", Outcome: "abstain"},
				{Name: "guards.deny", Outcome: "deny"},
			},
		}),
	}
	writeDayFile(t, stateDir, day, lines)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	resp := get(t, fmt.Sprintf("%s/api/events?day=%s&outcome=deny", base, day))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var eventsResp EventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&eventsResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(eventsResp.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(eventsResp.Records))
	}
	if got := eventsResp.Records[0].Hits; len(got) != 1 || got[0] != 1 {
		t.Errorf("hits = %v, want [1]", got)
	}
}

func TestFlowEndpointIgnoresWindowForPastDay(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	pastDay := "2026-09-09"
	pastTS := time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000000Z")

	lines := []string{
		recLine(t, record.Record{TS: pastTS, Engine: "codex", Verdict: "allow", Router: "ok"}),
		recLine(t, record.Record{TS: pastTS, Engine: "claude-code", Verdict: "deny", Router: "ok"}),
	}
	writeDayFile(t, stateDir, pastDay, lines)

	opts := serveOpts(t, stateDir)
	opts.Now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runTestServer(t, ctx, opts)

	resp := get(t, base+"/api/flow?day="+pastDay+"&window=10")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var flowResp FlowResponse
	if err := json.NewDecoder(resp.Body).Decode(&flowResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if flowResp.Window != 0 {
		t.Errorf("window = %d, want 0 (whole-day aggregate for a past day)", flowResp.Window)
	}
	if flowResp.Calls != 2 {
		t.Errorf("calls = %d, want 2", flowResp.Calls)
	}
	if len(flowResp.Paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(flowResp.Paths))
	}
	for _, p := range flowResp.Paths {
		if len(p.Counts) != 1 {
			t.Errorf("path %+v counts len = %d, want 1", p, len(p.Counts))
		}
	}
}

func TestEventsBeforeParamPagesOlderRecords(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"

	var lines []string
	for i := range 10 {
		lines = append(lines, recLine(t, record.Record{Engine: "codex", SessionID: fmt.Sprintf("s%d", i)}))
	}
	writeDayFile(t, stateDir, day, lines)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	url := fmt.Sprintf("%s/api/events?day=%s&limit=3", base, day)
	resp := get(t, url)
	defer func() { _ = resp.Body.Close() }()
	var page1 EventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(page1.Records) != 3 {
		t.Fatalf("page1 got %d records, want 3", len(page1.Records))
	}
	oldest := page1.Records[len(page1.Records)-1]
	if oldest.Rec.SessionID != "s7" {
		t.Fatalf("page1 oldest session = %q, want s7", oldest.Rec.SessionID)
	}

	url2 := fmt.Sprintf("%s/api/events?day=%s&limit=3&before=%d", base, day, oldest.Offset)
	resp2 := get(t, url2)
	defer func() { _ = resp2.Body.Close() }()
	var page2 EventsResponse
	if err := json.NewDecoder(resp2.Body).Decode(&page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}

	want := []string{"s6", "s5", "s4"}
	if len(page2.Records) != len(want) {
		t.Fatalf("page2 = %+v, want exactly %v (s7 must not reappear as page2's newest entry)", page2.Records, want)
	}
	for i, w := range want {
		if got := page2.Records[i].Rec.SessionID; got != w {
			t.Errorf("page2.Records[%d].SessionID = %q, want %q", i, got, w)
		}
	}
}

func TestDaysEndpoint(t *testing.T) {
	stateDir := t.TempDir()
	writeDayFile(t, stateDir, "2026-09-10", []string{recLine(t, record.Record{SessionID: "x"})})
	writeDayFile(t, stateDir, "2026-09-09", []string{recLine(t, record.Record{SessionID: "y"})})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	resp := get(t, base+"/api/days")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var daysResp DaysResponse
	if err := json.NewDecoder(resp.Body).Decode(&daysResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(daysResp.Days) < 2 {
		t.Fatalf("got %d days, want >= 2", len(daysResp.Days))
	}
	if daysResp.Days[0] != "2026-09-10" {
		t.Errorf("first day = %q, want 2026-09-10", daysResp.Days[0])
	}
	if daysResp.Today != DayString(time.Now()) {
		t.Errorf("today = %q, want %q", daysResp.Today, DayString(time.Now()))
	}
}

// TestEndpointsSerializeListsAsArraysOnEmptyStateDir pins the wire contract
// that a list-valued response field is always [] and never null. On a state
// dir with no stream files the web view iterates each of these directly, and a
// null throws "not iterable" before the feed can load (#94). The check reads
// the raw JSON so a typed decode into a nil slice cannot mask a null field.
func TestEndpointsSerializeListsAsArraysOnEmptyStateDir(t *testing.T) {
	stateDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	cases := []struct {
		name  string
		url   string
		field string
		setup func(t *testing.T)
	}{
		{"days", "/api/days", "days", nil},
		{"events missing day", "/api/events?day=2026-01-01", "records", nil},
		{"table missing file", "/api/table", "handlers", nil},
		{"table present with no handlers key", "/api/table", "handlers", func(t *testing.T) {
			// A present table.json with no handlers key makes ReadTable return
			// (nil, nil); the handler's own nil guard is what keeps the field
			// an array. The missing-file case above takes the error branch and
			// never exercises that guard.
			if err := os.WriteFile(filepath.Join(stateDir, "table.json"), []byte("{}"), 0o600); err != nil {
				t.Fatalf("write table: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			resp := get(t, base+tc.url)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			raw, ok := body[tc.field]
			if !ok {
				t.Fatalf("%s response has no %q field", tc.url, tc.field)
			}
			if got := strings.TrimSpace(string(raw)); got != "[]" {
				t.Errorf("%s = %s, want []", tc.field, got)
			}
		})
	}
}

func TestTableEndpoint(t *testing.T) {
	stateDir := t.TempDir()

	// Write the table before starting the server so it exists on first request.
	writeTableFile(t, stateDir, manifest.Handler{
		ID:      "guard",
		Exec:    "/bin/true",
		Events:  []string{"pre_tool"},
		Engines: []string{"codex"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	// Real table: 200 with populated handlers and vocabularies.
	resp := get(t, base+"/api/table")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var tableResp TableResponse
	if err := json.NewDecoder(resp.Body).Decode(&tableResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tableResp.Error != "" {
		t.Errorf("real table: error = %q, want empty (path=%s)", tableResp.Error, tableResp.Path)
	}
	if len(tableResp.Handlers) != 1 {
		t.Fatalf("got %d handlers, want 1", len(tableResp.Handlers))
	}
	if len(tableResp.Engines) == 0 {
		t.Error("real table: engines must be populated")
	}
	if len(tableResp.Events) == 0 {
		t.Error("real table: events must be populated")
	}

	// Now delete the table file and verify the missing-table path.
	if err := os.Remove(filepath.Join(stateDir, "table.json")); err != nil {
		t.Fatalf("remove table: %v", err)
	}
	resp2 := get(t, base+"/api/table")
	defer func() { _ = resp2.Body.Close() }()
	if err := json.NewDecoder(resp2.Body).Decode(&tableResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tableResp.Error == "" {
		t.Error("missing table: want non-empty error")
	}
	if len(tableResp.Engines) == 0 {
		t.Error("missing table: engines must be populated")
	}
	if len(tableResp.Events) == 0 {
		t.Error("missing table: events must be populated")
	}
}

func TestStatsOlderDay(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-09"

	// Write records to a non-today day.
	writeDayFile(t, stateDir, day, []string{
		recLine(t, record.Record{Engine: "codex", SessionID: "s1", Verdict: "allow"}),
		recLine(t, record.Record{Engine: "codex", SessionID: "s2", Verdict: "deny"}),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	url := fmt.Sprintf("%s/api/stats?day=%s", base, day)
	resp := get(t, url)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Calls != 2 {
		t.Errorf("calls = %d, want 2", snap.Calls)
	}
	if snap.Day != day {
		t.Errorf("day = %q, want %q", snap.Day, day)
	}
	if len(snap.Verdicts) == 0 {
		t.Error("verdicts empty")
	}
}

func TestNoAccessControlAllowOrigin(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	for _, path := range []string{"/", "/api/days", "/api/events", "/api/flow", "/api/stats", "/api/table", "/api/stream"} {
		resp := get(t, base+path)
		_ = resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: got CORS header, want none", path)
		}
	}
}

// TestFlowBundleEmbedded guards the esbuild bundle: the embedded FS carries
// the built flow/ assets, index.html points at their /static/flow/ path (not
// the old /static/flow.js), and the server actually serves them.
func TestFlowBundleEmbedded(t *testing.T) {
	sub, err := staticFS()
	if err != nil {
		t.Fatalf("staticFS: %v", err)
	}
	for _, name := range []string{"flow/flow.js", "flow/flow.css", "flow/elk-worker.js"} {
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Errorf("%s: embedded but empty", name)
		}
	}

	index, err := indexHTML()
	if err != nil {
		t.Fatalf("indexHTML: %v", err)
	}
	if !strings.Contains(string(index), `/static/flow/flow.js`) {
		t.Error("index.html does not reference /static/flow/flow.js")
	}
	if !strings.Contains(string(index), `/static/flow/flow.css`) {
		t.Error("index.html does not reference /static/flow/flow.css")
	}
	if strings.Contains(string(index), `/static/flow.js`) {
		t.Error("index.html still references the old /static/flow.js path")
	}

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	resp := get(t, base+"/static/flow/flow.js")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/flow/flow.js: %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type: %q, want a javascript type", ct)
	}
}

func TestEndToEndStreamArrives(t *testing.T) {
	stateDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := runTestServer(t, ctx, serveOpts(t, stateDir))

	// Open SSE stream.
	req, err := http.NewRequest(http.MethodGet, base+"/api/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read past initial frames (reset, stats) until we get the call.
	reader := bufio.NewReader(resp.Body)

	// Append a record after connecting: the hub's tailer will see it and
	// emit a call frame.
	day := DayString(time.Now())
	rec := record.Record{
		Engine:         "codex",
		SessionID:      "e2e-test",
		Verdict:        "allow",
		CanonicalEvent: "pre_tool",
	}
	line := recLine(t, rec)
	tailAppend(t, stateDir, day, line)

	var entry Entry
	found := false
	for range 10 {
		id, data, err := readSSEEvent(reader)
		if err != nil {
			t.Fatalf("reading SSE: %v", err)
		}
		if id == "" {
			// reset or stats frame — skip
			continue
		}
		// call frame: has id
		if err := json.Unmarshal([]byte(data), &entry); err != nil {
			t.Fatalf("unmarshal call data: %v", err)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no call frame arrived")
	}
	if entry.Rec.SessionID != "e2e-test" {
		t.Errorf("SessionID = %q, want e2e-test", entry.Rec.SessionID)
	}
}

func readSSEEvent(r *bufio.Reader) (id string, data string, err error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return id, data, nil // blank line ends the event
		}
		if strings.HasPrefix(line, "id: ") {
			id = line[4:]
		}
		if strings.HasPrefix(line, "data: ") {
			data = line[6:]
		}
		// Comments (starting with :) and event: lines are ignored for these tests.
	}
}

// readySignal is an io.Writer that closes ready on its first Write, so a test
// can wait for Run's startup banner instead of polling with time.Sleep.
type readySignal struct {
	once  sync.Once
	ready chan struct{}
}

func newReadySignal() *readySignal {
	return &readySignal{ready: make(chan struct{})}
}

func (s *readySignal) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.ready) })
	return len(p), nil
}

// TestRunShutsDownCleanlyOnCancel exercises the production Run path directly,
// unlike runTestServer's hand-rolled srv.Close(): a normal Ctrl-C/SIGTERM
// must make Run return nil, not surface a listener-close error as a fatal
// exit.
func TestRunShutsDownCleanlyOnCancel(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := newReadySignal()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{StateDir: stateDir, Port: 0, Out: sig, Now: time.Now})
	}()

	select {
	case <-sig.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v after context cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func writeTableFile(t *testing.T, stateDir string, h manifest.Handler) {
	t.Helper()
	table := manifest.Manifest{Handlers: []manifest.Handler{h}}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatalf("marshal table: %v", err)
	}
	path := filepath.Join(stateDir, "table.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
}
