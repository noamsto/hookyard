package serve

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

const maxFlowWindow = 60

// clampWindow parses the "window" query param into a minute count: a parse
// error or a non-positive value means "whole day" (0), and anything past
// maxFlowWindow is capped rather than rejected.
func clampWindow(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	if n > maxFlowWindow {
		return maxFlowWindow
	}
	return n
}

// flowKey groups records by the tuple the flow view draws as one path:
// engine, canonical_event, native_event, router, verdict, and each handler's
// name+outcome in record order. "\x00" separates fields (including within a
// hop's own name/outcome pair) and "\x01" separates hops, so neither can
// collide with a field value in practice and the two never nest.
func flowKey(rec record.Record) string {
	hops := make([]string, len(rec.Handlers))
	for i, h := range rec.Handlers {
		hops[i] = h.Name + "\x00" + h.Outcome
	}
	fields := []string{rec.Engine, rec.CanonicalEvent, rec.NativeEvent, rec.Router, rec.Verdict, strings.Join(hops, "\x01")}
	return strings.Join(fields, "\x00")
}

// flowAgg is one path's running aggregate while FlowForDay scans the day
// file; key and total exist only to sort the final Paths slice.
type flowAgg struct {
	path  FlowPath
	key   string
	total int64
}

// FlowForDay groups day's records matching f by path tuple (flowKey). When
// window > 0 each path's Counts is bucketed by minute over the window
// minutes ending at now's minute; window == 0 collapses everything into one
// whole-day bucket.
func FlowForDay(stateDir, day string, window int, now time.Time, f Filter) (FlowResponse, error) {
	resp := FlowResponse{Day: day, Window: window}

	buckets := 1
	var start time.Time
	if window > 0 {
		buckets = window
		end := now.UTC().Truncate(time.Minute)
		start = end.Add(-time.Duration(window-1) * time.Minute)
		resp.BucketStart = start.UnixMilli()
		resp.BucketMS = int64(time.Minute / time.Millisecond)
	}

	aggs := make(map[string]*flowAgg)
	visit := func(e Entry) {
		rec := e.Rec
		if !f.Match(rec) {
			return
		}

		idx := 0
		if window > 0 {
			ts, err := time.Parse(time.RFC3339Nano, rec.TS)
			if err != nil {
				return
			}
			ts = ts.UTC()
			if ts.Before(start) {
				return
			}
			idx = int(ts.Sub(start) / time.Minute)
			if idx >= window {
				idx = window - 1
			}
		}

		key := flowKey(rec)
		a, ok := aggs[key]
		if !ok {
			hops := make([]FlowHop, len(rec.Handlers))
			for i, h := range rec.Handlers {
				hops[i] = FlowHop{Name: h.Name, Outcome: h.Outcome}
			}
			a = &flowAgg{
				path: FlowPath{
					Engine:         rec.Engine,
					CanonicalEvent: rec.CanonicalEvent,
					NativeEvent:    rec.NativeEvent,
					Router:         rec.Router,
					Verdict:        rec.Verdict,
					Handlers:       hops,
					Counts:         make([]int64, buckets),
				},
				key: key,
			}
			aggs[key] = a
		}
		a.path.Counts[idx]++
		a.total++
		resp.Calls++
	}

	nextOffset, err := ScanAll(stateDir, day, visit)
	if err != nil {
		return FlowResponse{}, err
	}
	resp.NextOffset = nextOffset

	sorted := make([]*flowAgg, 0, len(aggs))
	for _, a := range aggs {
		sorted = append(sorted, a)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].total != sorted[j].total {
			return sorted[i].total > sorted[j].total
		}
		return sorted[i].key < sorted[j].key
	})

	resp.Paths = make([]FlowPath, len(sorted))
	for i, a := range sorted {
		resp.Paths[i] = a.path
	}
	return resp, nil
}
