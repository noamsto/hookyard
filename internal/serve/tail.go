package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

// Tailer follows the current day's stream file and reports every record, day
// rollover and restart it sees. It polls rather than watching (SPEC 4.2):
// inotify is Linux-only and kqueue is not portable either, and neither one
// removes the offset and file-identity bookkeeping the cases below need.
type Tailer struct {
	StateDir string
	Poll     time.Duration    // zero -> pollInterval
	Now      func() time.Time // zero -> time.Now
	Start    map[string]int64 // day -> starting offset; absent -> 0
}

func (t *Tailer) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}
	return t.Now()
}

func (t *Tailer) poll() time.Duration {
	if t.Poll <= 0 {
		return pollInterval
	}
	return t.Poll
}

// tailCursor is everything the tailer claims about the file under it: the day
// it follows, how far into that file it has read, the descriptor it read
// through, and the trailing bytes that are not yet a whole line.
type tailCursor struct {
	day    string
	offset int64       // absolute bytes taken from the file, carry included
	file   *os.File    // nil until the day's file exists
	opened os.FileInfo // stat of the descriptor, for os.SameFile against the path
	carry  []byte
}

func (c *tailCursor) closeFile() {
	if c.file == nil {
		return
	}
	_ = c.file.Close()
	c.file = nil
	c.opened = nil
}

// rewind drops everything the cursor claimed: the descriptor whose file the
// path no longer names, the offset into it, and the partial line.
func (c *tailCursor) rewind() {
	c.closeFile()
	c.offset = 0
	c.carry = nil
}

// Run polls until ctx is done, sending to out. It closes out on return.
func (t *Tailer) Run(ctx context.Context, out chan<- TailEvent) {
	defer close(out)
	cursor := &tailCursor{}
	defer cursor.closeFile()

	ticker := time.NewTicker(t.poll())
	defer ticker.Stop()
	for {
		if !t.tick(ctx, cursor, out) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Tailer) tick(ctx context.Context, c *tailCursor, out chan<- TailEvent) bool {
	now := t.now()
	want := DayString(now)
	if want != c.day {
		if c.file != nil {
			// A record written in the final milliseconds of the day is not
			// lost: drain the old descriptor to EOF before the cursor moves
			// off it (SPEC 4.2 case 4). The size comes from the descriptor,
			// not the path, which already names the new day.
			if info, err := c.file.Stat(); err == nil && !t.pump(ctx, c, info.Size(), out) {
				return false
			}
		}
		rolled := c.day != ""
		c.rewind()
		c.day, c.offset = want, t.Start[want]
		// The first day is where the tailer starts, not a rollover: announcing
		// it would make the hub throw away the accumulator Seed just built.
		if rolled && !sendTail(ctx, out, TailEvent{NewDay: want}) {
			return false
		}
	}

	path := record.StreamPath(t.StateDir, now)
	info, err := os.Stat(path)
	if err != nil {
		// No file is the normal state of a fresh day (SPEC 4.2 case 1), and
		// any other stat failure is equally nothing to do until the next tick.
		return true
	}

	if c.file != nil && !os.SameFile(info, c.opened) {
		// The path no longer names the file the cursor describes, so the
		// cursor is meaningless. Reopening quietly at 0 would leave the
		// accumulator's monotonic offsets above every record of the new file
		// and skip all of them, so the rebuild is reported (SPEC 4.2 case 3).
		c.rewind()
		if !sendTail(ctx, out, TailEvent{Restart: "stream file replaced"}) {
			return false
		}
	}
	if info.Size() < c.offset {
		c.offset = 0
		c.carry = nil
		if !sendTail(ctx, out, TailEvent{Restart: "stream file truncated"}) {
			return false
		}
	}

	if c.file == nil {
		file, err := os.Open(path)
		if err != nil {
			return true
		}
		opened, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return true
		}
		c.file, c.opened = file, opened
	}
	return t.pump(ctx, c, info.Size(), out)
}

// pump reads [offset, size) through the open descriptor and emits one event
// per complete line, returning false when ctx ended mid-send.
func (t *Tailer) pump(ctx context.Context, c *tailCursor, size int64, out chan<- TailEvent) bool {
	if size <= c.offset {
		return true
	}
	buf := make([]byte, size-c.offset)
	// A short read is the next tick's business, so the cursor advances by what
	// arrived rather than by what was asked for.
	n, _ := c.file.ReadAt(buf, c.offset)
	if n == 0 {
		return true
	}
	c.offset += int64(n)
	c.carry = append(c.carry, buf[:n]...)

	base := c.offset - int64(len(c.carry)) // absolute offset of carry[0]
	for {
		i := bytes.IndexByte(c.carry, '\n')
		if i < 0 {
			break
		}
		line := c.carry[:i]
		c.carry = c.carry[i+1:]
		base += int64(i) + 1
		var rec record.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// A torn line is discarded, not retried (record §6); the offset
			// has already advanced past it either way.
			continue
		}
		if !sendTail(ctx, out, TailEvent{Entry: &Entry{Rec: rec, Day: c.day, Offset: base}}) {
			return false
		}
	}
	if len(c.carry) > maxCarry {
		// A writer killed mid-record must not wedge the reader forever: drop
		// the runt. The cursor already counts the dropped bytes, and the next
		// newline resyncs on its own, its leading fragment failing to decode
		// like any other torn line (SPEC 4.2 case 2).
		c.carry = nil
	}
	return true
}

// sendTail reports false when ctx ended first, so a stopped server never
// leaves the tailer blocked on a channel nobody is draining.
func sendTail(ctx context.Context, out chan<- TailEvent, ev TailEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
