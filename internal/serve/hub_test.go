package serve

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

// frameWait is generous on purpose: these tests poll a real file at 1 ms, so
// the bound is there to fail a wedged hub, not to time anything.
const frameWait = 5 * time.Second

func seededHub(t *testing.T, stateDir string, clock *tailClock) *Hub {
	t.Helper()
	h := NewHub(stateDir, clock.now)
	h.poll = time.Millisecond
	if err := h.Seed(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return h
}

func startHub(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hub run: %v", err)
			}
		case <-time.After(frameWait):
			t.Error("hub did not return after cancel")
		}
	})
}

func hubLine(t *testing.T, stateDir, day, session string) {
	t.Helper()
	tailAppend(t, stateDir, day, recLine(t, record.Record{
		Engine:    "codex",
		SessionID: session,
		Verdict:   "allow",
		Router:    record.RouterOK,
	}))
}

func nextFrame(t *testing.T, s *Sub) Frame {
	t.Helper()
	select {
	case f, ok := <-s.C:
		if !ok {
			t.Fatal("subscriber channel closed before the expected frame")
		}
		return f
	case <-time.After(frameWait):
		t.Fatal("timed out waiting for a frame")
	}
	return Frame{}
}

// nextLiveFrame skips stats, which the ticker can interleave anywhere and
// which no ordering assertion here is about.
func nextLiveFrame(t *testing.T, s *Sub) Frame {
	t.Helper()
	deadline := time.After(frameWait)
	for {
		select {
		case f, ok := <-s.C:
			if !ok {
				t.Fatal("subscriber channel closed before the expected frame")
			}
			if f.Event != "stats" {
				return f
			}
		case <-deadline:
			t.Fatal("timed out waiting for a non-stats frame")
		}
	}
}

func nextCall(t *testing.T, s *Sub) Entry {
	t.Helper()
	f := nextLiveFrame(t, s)
	if f.Event != "call" {
		t.Fatalf("want a call frame, got %q", f.Event)
	}
	entry, ok := f.Data.(Entry)
	if !ok {
		t.Fatalf("call frame carries %T, want Entry", f.Data)
	}
	if want := entry.Day + ":" + strconv.FormatInt(entry.Offset, 10); f.ID != want {
		t.Errorf("call frame id = %q, want %q", f.ID, want)
	}
	return entry
}

// handshake consumes the reset and stats every subscriber opens with.
func handshake(t *testing.T, s *Sub) {
	t.Helper()
	if f := nextFrame(t, s); f.Event != "reset" {
		t.Fatalf("first frame = %q, want reset", f.Event)
	}
	if f := nextFrame(t, s); f.Event != "stats" {
		t.Fatalf("second frame = %q, want stats", f.Event)
	}
}

func snapshotCalls(t *testing.T, h *Hub, day string) int64 {
	t.Helper()
	snap, ok := h.Snapshot(day)
	if !ok {
		t.Fatalf("no live snapshot for %q", day)
	}
	return snap.Calls
}

// waitCalls polls the published snapshot, which is the only way to observe a
// rebuild whose records the hub deliberately does not fan out again.
func waitCalls(t *testing.T, h *Hub, day string, want int64) {
	t.Helper()
	deadline := time.Now().Add(frameWait)
	for {
		if snap, ok := h.Snapshot(day); ok && snap.Calls == want {
			return
		}
		if time.Now().After(deadline) {
			snap, _ := h.Snapshot(day)
			t.Fatalf("timed out waiting for %d calls on %s, last saw %d", want, day, snap.Calls)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHubSeedDoesNotDropARecordCompletedAfterSeed(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")

	whole := recLine(t, record.Record{Engine: "codex", SessionID: "whole"})
	full := recLine(t, record.Record{Engine: "codex", SessionID: "torn"})
	torn := full[:len(full)-2] // drop the closing "}\n": a write Seed catches mid-record
	tailAppend(t, stateDir, day, whole+torn)

	h := seededHub(t, stateDir, clock)
	if got := snapshotCalls(t, h, day); got != 1 {
		t.Fatalf("seed counted %d calls, want 1 (the torn line must not be counted yet)", got)
	}
	startHub(t, h)

	sub := h.Subscribe(Filter{}, 32)
	handshake(t, sub)

	// Complete the torn line exactly where Seed left off. If Seed's returned
	// offset included the torn prefix, the tailer starts past it and only
	// ever sees this suffix, which fails to decode and is silently dropped.
	tailAppend(t, stateDir, day, full[len(full)-2:])

	entry := nextCall(t, sub)
	if entry.Rec.SessionID != "torn" {
		t.Fatalf("SessionID = %q, want %q (the record completed after seed)", entry.Rec.SessionID, "torn")
	}
}

func TestHubSeedHandsOffToTheTailerWithoutRecounting(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	hubLine(t, stateDir, day, "s0")
	hubLine(t, stateDir, day, "s1")

	h := seededHub(t, stateDir, clock)
	if h.Day() != day {
		t.Fatalf("Day() = %q, want %q", h.Day(), day)
	}
	if got := snapshotCalls(t, h, day); got != 2 {
		t.Fatalf("seeded calls = %d, want 2", got)
	}

	startHub(t, h)
	sub := h.Subscribe(Filter{}, 32)
	handshake(t, sub)

	hubLine(t, stateDir, day, "s2")
	if entry := nextCall(t, sub); entry.Rec.SessionID != "s2" {
		t.Fatalf("live call is for %q, want s2", entry.Rec.SessionID)
	}
	// The record counted at fan-out time is the whole point: the seed counted
	// the first two exactly once and the tailer did not re-read them.
	if got := snapshotCalls(t, h, day); got != 3 {
		t.Fatalf("calls after the handoff = %d, want 3", got)
	}
}

func TestHubFansOutOnlyMatchingCalls(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	codex := h.Subscribe(Filter{Engines: []string{"codex"}}, 32)
	claude := h.Subscribe(Filter{Engines: []string{"claude"}}, 32)
	handshake(t, codex)
	handshake(t, claude)

	hubLine(t, stateDir, day, "c0")
	tailAppend(t, stateDir, day, recLine(t, record.Record{Engine: "claude", SessionID: "k0"}))
	hubLine(t, stateDir, day, "c1")

	for _, want := range []string{"c0", "c1"} {
		if got := nextCall(t, codex).Rec.SessionID; got != want {
			t.Fatalf("codex subscriber saw %q, want %q", got, want)
		}
	}
	if got := nextCall(t, claude).Rec.SessionID; got != "k0" {
		t.Fatalf("claude subscriber saw %q, want k0", got)
	}
}

// TestFanOutHitsPerSubscriber verifies fanOut computes each subscriber's
// Hits from its own filter, against the one shared Entry, without mutating
// it for the next subscriber.
func TestFanOutHitsPerSubscriber(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	subA := h.Subscribe(Filter{Handlers: []string{"a"}}, 32)
	subB := h.Subscribe(Filter{Handlers: []string{"b"}}, 32)
	subNone := h.Subscribe(Filter{}, 32)
	handshake(t, subA)
	handshake(t, subB)
	handshake(t, subNone)

	tailAppend(t, stateDir, day, recLine(t, record.Record{
		Engine: "codex", Router: record.RouterOK, Verdict: "allow",
		Handlers: []record.RecordHandler{
			{Name: "a", Outcome: "allow"},
			{Name: "b", Outcome: "allow"},
		},
	}))

	if got := nextCall(t, subA).Hits; len(got) != 1 || got[0] != 0 {
		t.Errorf("subA Hits = %v, want [0]", got)
	}
	if got := nextCall(t, subB).Hits; len(got) != 1 || got[0] != 1 {
		t.Errorf("subB Hits = %v, want [1]", got)
	}
	if got := nextCall(t, subNone).Hits; got != nil {
		t.Errorf("subNone Hits = %v, want nil (no branch filter)", got)
	}
}

func TestHubRolloverSendsDayBeforeTheNewDaysCalls(t *testing.T) {
	stateDir := t.TempDir()
	day, next := "2026-09-10", "2026-09-11"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	sub := h.Subscribe(Filter{}, 32)
	handshake(t, sub)

	hubLine(t, stateDir, day, "s0")
	if got := nextCall(t, sub).Day; got != day {
		t.Fatalf("first call is on %q, want %q", got, day)
	}

	hubLine(t, stateDir, next, "s1")
	clock.set(t, "2026-09-11T00:00:30Z")

	f := nextLiveFrame(t, sub)
	if f.Event != "day" {
		t.Fatalf("frame after the rollover = %q, want day", f.Event)
	}
	if payload, ok := f.Data.(dayPayload); !ok || payload.Day != next {
		t.Fatalf("day frame carries %+v, want day %q", f.Data, next)
	}
	entry := nextCall(t, sub)
	if entry.Day != next || entry.Rec.SessionID != "s1" {
		t.Fatalf("first call after the rollover = %+v, want s1 on %s", entry, next)
	}

	waitCalls(t, h, next, 1)
	if _, ok := h.Snapshot(day); ok {
		t.Errorf("the old day is still the live day after a rollover")
	}
	if got := h.Day(); got != next {
		t.Errorf("Day() = %q after the rollover, want %q (a caller that omits ?day= would keep getting stale data)", got, next)
	}
}

// TestHubDayReadDoesNotRaceRollover exists to be run under `go test -race`.
// It asserts nothing about Day()'s return value — its only job is to hold a
// concurrent reader on Day() across the exact moment Run's own goroutine
// mutates the day on rollover, so the race detector would catch a regression
// back to reading an unsynchronized field from outside that goroutine.
func TestHubDayReadDoesNotRaceRollover(t *testing.T) {
	stateDir := t.TempDir()
	day, next := "2026-09-10", "2026-09-11"
	clock := newTailClock(t, "2026-09-10T23:59:59Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	// The tailer only announces a rollover once it has settled on a first day,
	// so wait for the old day's record before moving the clock.
	hubLine(t, stateDir, day, "s0")
	waitCalls(t, h, day, 1)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				h.Day()
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	clock.set(t, "2026-09-11T00:00:01Z")

	deadline := time.Now().Add(frameWait)
	for {
		if _, ok := h.Snapshot(next); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("rollover never happened")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHubRestartRebuildsFromTheFileThatIsActuallyThere(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	for _, id := range []string{"s0", "s1", "s2", "s3"} {
		hubLine(t, stateDir, day, id)
	}

	h := seededHub(t, stateDir, clock)
	if got := snapshotCalls(t, h, day); got != 4 {
		t.Fatalf("seeded calls = %d, want 4", got)
	}
	startHub(t, h)
	sub := h.Subscribe(Filter{}, 64)
	handshake(t, sub)

	rebuilt := recLine(t, record.Record{Engine: "codex", SessionID: "n0"}) +
		recLine(t, record.Record{Engine: "codex", SessionID: "n1"})
	if err := os.WriteFile(tailPath(stateDir, day), []byte(rebuilt), 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if f := nextLiveFrame(t, sub); f.Event != "reset" {
		t.Fatalf("frame after the truncation = %q, want reset", f.Event)
	}
	waitCalls(t, h, day, 2)

	// The hub rescans the truncated file while the tailer re-reads it from 0,
	// and the test's own rewrite races both, so n0 and n1 reach the subscriber
	// either inside the reset's snapshot or as live call frames. Which one is
	// scheduling, not a contract; the contract is that each is delivered
	// exactly once and that a record appended afterwards follows them.
	hubLine(t, stateDir, day, "n2")
	var before []string
	for {
		id := nextCall(t, sub).Rec.SessionID
		if id == "n2" {
			break
		}
		before = append(before, id)
	}
	if want := []string{"n0", "n1"}; !isSuffix(before, want) {
		t.Fatalf("live calls before n2 = %v, want a suffix of %v", before, want)
	}
	waitCalls(t, h, day, 3)
}

func isSuffix(got, of []string) bool {
	if len(got) > len(of) {
		return false
	}
	for i := range got {
		if got[i] != of[len(of)-len(got)+i] {
			return false
		}
	}
	return true
}

// TestHubRestartDeliversEveryRebuiltRecordOnceWhereverTheRescanLands drives the
// two interleavings of a truncate-and-rewrite that the live test cannot pin:
// the hub's rescan lands before the rewrite (n0 and n1 then arrive as live
// calls), between its records, or after it (the reset already counts them and
// the tailer's re-read is deduped). Either way the subscriber ends up with n0, n1, n2 exactly once.
func TestHubRestartDeliversEveryRebuiltRecordOnceWhereverTheRescanLands(t *testing.T) {
	const day = "2026-09-10"
	line := func(id string) string {
		return recLine(t, record.Record{Engine: "codex", SessionID: id})
	}
	n0, n1, n2 := line("n0"), line("n1"), line("n2")
	entry := func(id string, offset int) Entry {
		return Entry{Rec: record.Record{Engine: "codex", SessionID: id}, Day: day, Offset: int64(offset)}
	}
	rebuilt := []Entry{entry("n0", len(n0)), entry("n1", len(n0)+len(n1)), entry("n2", len(n0)+len(n1)+len(n2))}

	cases := []struct {
		name         string
		onDiskAtScan string
		wantInReset  int64
		wantLive     []string
	}{
		{"rescan before the rewrite finishes", "", 0, []string{"n0", "n1", "n2"}},
		{"rescan between the two records", n0, 1, []string{"n1", "n2"}},
		{"rescan after the rewrite finishes", n0 + n1, 2, []string{"n2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			clock := newTailClock(t, "2026-09-10T12:00:00Z")
			for _, id := range []string{"s0", "s1", "s2", "s3"} {
				hubLine(t, stateDir, day, id)
			}
			h := seededHub(t, stateDir, clock)

			ch := make(chan Frame, 64)
			h.subs[&Sub{C: ch, ch: ch}] = struct{}{}

			if err := os.WriteFile(tailPath(stateDir, day), []byte(tc.onDiskAtScan), 0o600); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			h.handle(TailEvent{Restart: "stream file truncated"})
			for _, e := range rebuilt {
				h.handle(TailEvent{Entry: &e})
			}
			close(ch)

			var resetSeen, statsSeen bool
			var inReset int64
			var live []string
			for f := range ch {
				switch f.Event {
				case "reset":
					resetSeen = true
				case "stats":
					if !resetSeen {
						t.Fatal("stats frame before the reset")
					}
					if !statsSeen {
						statsSeen, inReset = true, f.Data.(Snapshot).Calls
					}
				case "call":
					live = append(live, f.Data.(Entry).Rec.SessionID)
				}
			}
			if !resetSeen {
				t.Fatal("no reset frame after the restart")
			}
			if inReset != tc.wantInReset {
				t.Errorf("reset snapshot counted %d calls, want %d", inReset, tc.wantInReset)
			}
			if !slices.Equal(live, tc.wantLive) {
				t.Errorf("live calls = %v, want %v", live, tc.wantLive)
			}
			if got := snapshotCalls(t, h, day); got != 3 {
				t.Errorf("calls after the rebuild = %d, want 3", got)
			}
		})
	}
}

func TestHubCoalescesStats(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	sub := h.Subscribe(Filter{}, 256)
	handshake(t, sub)

	const burst = 50
	for i := range burst {
		hubLine(t, stateDir, day, "s"+strconv.Itoa(i))
	}

	calls, stats := 0, 0
	for calls < burst {
		switch nextFrame(t, sub).Event {
		case "call":
			calls++
		case "stats":
			stats++
		}
	}
	// A 1 s ticker over a burst that lands in milliseconds: one or two frames,
	// nowhere near one per call.
	if stats > 3 {
		t.Fatalf("%d stats frames for %d calls, want the ticker to coalesce them", stats, burst)
	}
}

func TestHubDropsASlowSubscriberWithoutStallingTheRest(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	// Two frames of room is exactly the handshake, so the first call frame
	// overflows a subscriber that reads nothing.
	slow := h.Subscribe(Filter{}, 2)
	fast := h.Subscribe(Filter{}, 64)
	handshake(t, fast)

	for _, id := range []string{"s0", "s1", "s2"} {
		hubLine(t, stateDir, day, id)
		if got := nextCall(t, fast).Rec.SessionID; got != id {
			t.Fatalf("fast subscriber saw %q, want %q", got, id)
		}
	}

	handshake(t, slow)
	select {
	case _, ok := <-slow.C:
		if ok {
			t.Fatal("the slow subscriber received a frame past its buffer instead of being dropped")
		}
	case <-time.After(frameWait):
		t.Fatal("the slow subscriber was never dropped")
	}
}

func TestHubRunBeforeSeedIsAnError(t *testing.T) {
	h := NewHub(t.TempDir(), newTailClock(t, "2026-09-10T12:00:00Z").now)
	if err := h.Run(context.Background()); err == nil {
		t.Fatal("Run before Seed returned nil, want an error")
	}
}

func TestHubFirstFrameIsAlwaysReset(t *testing.T) {
	stateDir := t.TempDir()
	day := "2026-09-10"
	clock := newTailClock(t, "2026-09-10T12:00:00Z")
	hubLine(t, stateDir, day, "s0")
	h := seededHub(t, stateDir, clock)
	startHub(t, h)

	for _, f := range []Filter{{}, {Engines: []string{"nothing-matches"}}} {
		sub := h.Subscribe(f, 8)
		first := nextFrame(t, sub)
		if first.Event != "reset" {
			t.Fatalf("first frame = %q, want reset", first.Event)
		}
		if payload, ok := first.Data.(dayPayload); !ok || payload.Day != day {
			t.Fatalf("reset frame carries %+v, want day %q", first.Data, day)
		}
		second := nextFrame(t, sub)
		if second.Event != "stats" {
			t.Fatalf("second frame = %q, want stats", second.Event)
		}
		if snap, ok := second.Data.(Snapshot); !ok || snap.Calls != 1 {
			t.Fatalf("stats frame carries %+v, want the seeded day's 1 call", second.Data)
		}
		h.Unsubscribe(sub)
	}
}
