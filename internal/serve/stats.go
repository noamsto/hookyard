package serve

import "sort"

// Accumulator holds SPEC 4.5's cumulative counters for one day. It is fed
// records in offset order and never rescans: Add's monotonic-offset guard is
// what makes double-counting structurally impossible, rather than merely
// avoided by careful callers.
//
// Accumulator is not internally locked — the Day (step 6) owns serializing
// access to it.
type Accumulator struct {
	day       string
	cursor    int64
	calls     int64
	handlers  map[string]*HandlerStat
	verdicts  map[string]*VerdictCount
	router    map[string]int64
	truncated int64
	since     string
}

// NewAccumulator returns an empty accumulator for day.
func NewAccumulator(day string) *Accumulator {
	return &Accumulator{
		day:      day,
		handlers: make(map[string]*HandlerStat),
		verdicts: make(map[string]*VerdictCount),
		router:   make(map[string]int64),
	}
}

// Add counts e. It returns false and counts nothing when e.Offset <= the
// cursor, which is what makes double-counting structurally impossible
// (SPEC 4.5 step 3).
func (a *Accumulator) Add(e Entry) bool {
	if e.Offset <= a.cursor {
		return false
	}
	a.cursor = e.Offset

	if a.calls == 0 {
		a.since = e.Rec.TS
	}
	a.calls++

	vc, ok := a.verdicts[e.Rec.Verdict]
	if !ok {
		vc = &VerdictCount{}
		a.verdicts[e.Rec.Verdict] = vc
	}
	vc.Total++
	if e.Rec.Enforced {
		vc.Enforced++
	} else {
		vc.Unenforced++
	}

	a.router[e.Rec.Router]++

	if e.Rec.Truncated {
		a.truncated++
	}

	// SPEC 4.5a: a truncated record's handlers list is a prefix of what
	// actually ran, not the full list — but the prefix it does carry is
	// still real and counted.
	for _, h := range e.Rec.Handlers {
		hs, ok := a.handlers[h.Name]
		if !ok {
			hs = &HandlerStat{Name: h.Name}
			a.handlers[h.Name] = hs
		}
		hs.Calls++
		hs.TotalMS += h.MS
		hs.MaxMS = max(hs.MaxMS, h.MS)
	}

	return true
}

// Cursor is the byte offset of the last record counted.
func (a *Accumulator) Cursor() int64 {
	return a.cursor
}

// Snapshot renders the current counters. Maps are never nil on the wire, even
// for an accumulator that has counted nothing.
func (a *Accumulator) Snapshot() Snapshot {
	handlers := make([]HandlerStat, 0, len(a.handlers))
	for _, hs := range a.handlers {
		handlers = append(handlers, *hs)
	}
	sort.Slice(handlers, func(i, j int) bool { return handlers[i].Name < handlers[j].Name })

	verdicts := make(map[string]VerdictCount, len(a.verdicts))
	for k, vc := range a.verdicts {
		verdicts[k] = *vc
	}

	router := make(map[string]int64, len(a.router))
	for k, v := range a.router {
		router[k] = v
	}

	return Snapshot{
		Day:       a.day,
		Calls:     a.calls,
		Handlers:  handlers,
		Verdicts:  verdicts,
		Router:    router,
		Truncated: a.truncated,
		Since:     a.since,
	}
}

// StatsForDay computes a snapshot for a day the server is not tailing, by the
// one composition that keeps a single implementation of the counting rules: a
// fresh Accumulator fed every record of the day by ScanAll. This is what
// /api/stats?day=<older> calls (SPEC 4.5). A missing day file yields an empty
// snapshot, not an error — ScanAll's own contract for that case.
func StatsForDay(stateDir, day string) (Snapshot, error) {
	acc := NewAccumulator(day)
	if _, err := ScanAll(stateDir, day, func(e Entry) { acc.Add(e) }); err != nil {
		return Snapshot{}, err
	}
	return acc.Snapshot(), nil
}
