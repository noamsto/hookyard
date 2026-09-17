package serve

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"
)

// statsInterval coalesces the stats fan-out (SPEC 4.5): a burst of 500 calls
// sends one snapshot, not 500.
const statsInterval = time.Second

var errNotSeeded = errors.New("serve: hub.Run called before Seed")

// Hub owns the live day. One goroutine — Run's — mutates the accumulator and
// the subscriber set, and everything else reaches them over channels; that is
// why the accumulator itself carries no lock.
type Hub struct {
	stateDir string
	now      func() time.Time
	poll     time.Duration // 0 -> pollInterval; only a test shortens it

	day     string // the day Seed settled on
	seedEnd int64  // where Seed stopped, and therefore where the tailer starts

	acc   *Accumulator
	subs  map[*Sub]struct{}
	dirty bool

	// live is the newest snapshot, published by the owning goroutine so a
	// reader never has to reach into the accumulator. A published snapshot is
	// never mutated afterwards, so handing its maps to every caller is safe.
	live atomic.Pointer[Snapshot]

	subscribe   chan *Sub
	unsubscribe chan *Sub
	done        chan struct{}
}

// Sub is one subscriber's filtered feed. C is closed when the subscriber is
// dropped or the hub stops.
type Sub struct {
	C <-chan Frame

	ch     chan Frame
	filter Filter
}

// dayPayload is the body of the day and reset frames (SPEC 4.4a).
type dayPayload struct {
	Day string `json:"day"`
}

func NewHub(stateDir string, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{
		stateDir:    stateDir,
		now:         now,
		subs:        make(map[*Sub]struct{}),
		subscribe:   make(chan *Sub),
		unsubscribe: make(chan *Sub),
		done:        make(chan struct{}),
	}
}

// Seed runs the startup pass (SPEC 4.5 steps 1-2): ScanAll today into the
// accumulator, remembering the offset it stopped at. Run starts the tailer
// from exactly that offset, so the seed-to-live handoff stays inside the Hub
// and no caller can hand the tailer a different one, reopening the gap this
// design exists to close.
func (h *Hub) Seed() error {
	day := DayString(h.now())
	acc := NewAccumulator(day)
	seedEnd, err := ScanAll(h.stateDir, day, func(e Entry) { acc.Add(e) })
	if err != nil {
		return err
	}
	h.day, h.seedEnd, h.acc = day, seedEnd, acc
	h.publish()
	return nil
}

// Day is the Hub's current day, read from the published snapshot rather than
// the h.day field directly. h.day is mutated only by Run's owning goroutine,
// on rollover (SPEC 4.4a); reading it from any other goroutine, as this
// method is (server.go's handlers call it concurrently with Run), would race
// that write. The published snapshot already exists for exactly this kind of
// concurrent, lock-free read, and is updated in lockstep with h.day.
func (h *Hub) Day() string {
	if snap := h.live.Load(); snap != nil {
		return snap.Day
	}
	return ""
}

// Now is the Hub's own clock, so handlers that need "the current time" agree
// with the tailer and accumulator instead of calling time.Now() separately.
func (h *Hub) Now() time.Time {
	return h.now()
}

// Run owns the tailer and the subscriber set until ctx is done. It refuses to
// start before Seed rather than silently tailing from offset 0 and counting
// the whole day a second time — the ordering is enforced, not just documented.
func (h *Hub) Run(ctx context.Context) error {
	if h.acc == nil {
		return errNotSeeded
	}
	defer close(h.done)

	tailCtx, stopTail := context.WithCancel(ctx)
	defer stopTail()
	events := make(chan TailEvent, 64)
	go (&Tailer{
		StateDir: h.stateDir,
		Poll:     h.poll,
		Now:      h.now,
		Start:    map[string]int64{h.day: h.seedEnd},
	}).Run(tailCtx, events)

	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			stopTail()
			// Drain to the tailer's close, so "Run returned" also means
			// nothing is still reading the stream directory.
			for range events {
			}
			h.closeAll()
			return nil
		case ev, ok := <-events:
			if !ok {
				h.closeAll()
				return nil
			}
			h.handle(ev)
		case s := <-h.subscribe:
			h.subs[s] = struct{}{}
			// reset first, always: it is the whole connect handshake
			// (SPEC 4.4), identical for a first load, a reconnect and a
			// restart, and the client backfills off it.
			h.send(s, Frame{Event: "reset", Data: dayPayload{Day: h.acc.day}})
			h.send(s, Frame{Event: "stats", Data: h.acc.Snapshot()})
		case s := <-h.unsubscribe:
			h.drop(s)
		case <-ticker.C:
			if h.dirty {
				h.broadcastStats()
			}
		}
	}
}

// Subscribe registers a filtered feed. buf is the depth past which the
// subscriber is considered too slow to keep.
func (h *Hub) Subscribe(f Filter, buf int) *Sub {
	ch := make(chan Frame, buf)
	s := &Sub{C: ch, ch: ch, filter: f}
	select {
	case h.subscribe <- s:
	case <-h.done:
		// The hub stopped between the handler accepting the request and this
		// send; a closed feed is what every subscriber already handles.
		close(ch)
	}
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	select {
	case h.unsubscribe <- s:
	case <-h.done:
	}
}

// Snapshot answers for the live day only. An empty day means "whatever the
// live day is"; any other mismatch reports false so the caller falls back to
// StatsForDay instead of being handed the wrong day's counters.
func (h *Hub) Snapshot(day string) (Snapshot, bool) {
	snap := h.live.Load()
	if snap == nil {
		return Snapshot{}, false
	}
	if day != "" && day != snap.Day {
		return Snapshot{}, false
	}
	return *snap, true
}

func (h *Hub) handle(ev TailEvent) {
	switch {
	case ev.Entry != nil:
		if !h.acc.Add(*ev.Entry) {
			// Already counted. A restart's re-seed and the tailer's re-read of
			// the same bytes overlap by construction, so the record is neither
			// counted twice nor shown twice.
			return
		}
		// Publishing before the fan-out is what keeps /api/stats from lagging
		// the feed: a client that has seen a call frame cannot then read a
		// snapshot that is missing it.
		h.publish()
		h.fanOut(*ev.Entry)
		h.dirty = true
	case ev.NewDay != "":
		// The day frame goes out before any of the new day's call frames
		// (SPEC 4.4a), so a page left open overnight re-points its stats panel
		// rather than watching the counts collapse to near-zero unannounced.
		h.day = ev.NewDay
		h.acc = NewAccumulator(ev.NewDay)
		h.publish()
		h.broadcast(Frame{Event: "day", Data: dayPayload{Day: ev.NewDay}})
		h.broadcastStats()
	case ev.Restart != "":
		// The file under the cursor is not the file the cursor describes, so
		// the day is rebuilt from what is actually there. The re-seed runs
		// here, on the owning goroutine, and blocks the fan-out for its
		// duration: mid-rebuild there is no coherent state to fan out, and
		// every subscriber is about to be told to reset and re-backfill
		// anyway. It is one bounded scan of a file that was just truncated or
		// replaced, so small by construction.
		acc := NewAccumulator(h.acc.day)
		// The scan error is dropped deliberately: after a restart the tailer
		// re-reads this file from offset 0, so the fresh accumulator refills
		// from the live path even when the scan gives up early.
		_, _ = ScanAll(h.stateDir, acc.day, func(e Entry) { acc.Add(e) })
		h.acc = acc
		h.publish()
		h.broadcast(Frame{Event: "reset", Data: dayPayload{Day: acc.day}})
		h.broadcastStats()
	}
}

// fanOut applies the filters server-side, so the client renders what it is
// sent and the five-filter semantics live in one tested place (SPEC 4.6).
func (h *Hub) fanOut(e Entry) {
	f := Frame{Event: "call", ID: e.Day + ":" + strconv.FormatInt(e.Offset, 10), Data: e}
	for s := range h.subs {
		if s.filter.Match(e.Rec) {
			h.send(s, f)
		}
	}
}

func (h *Hub) broadcast(f Frame) {
	for s := range h.subs {
		h.send(s, f)
	}
}

func (h *Hub) broadcastStats() {
	h.broadcast(Frame{Event: "stats", Data: h.acc.Snapshot()})
	h.dirty = false
}

// send never blocks: a subscriber that cannot keep up is dropped and its
// EventSource reconnects and re-backfills (SPEC 4.4). Waiting on it would let
// one stalled tab stall the tailer and every other tab with it.
func (h *Hub) send(s *Sub, f Frame) {
	select {
	case s.ch <- f:
	default:
		h.drop(s)
	}
}

// drop is idempotent through the map: membership, not a flag, is what keeps a
// dropped subscriber's channel from being closed twice.
func (h *Hub) drop(s *Sub) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	close(s.ch)
}

func (h *Hub) closeAll() {
	for s := range h.subs {
		h.drop(s)
	}
}

func (h *Hub) publish() {
	snap := h.acc.Snapshot()
	h.live.Store(&snap)
}
