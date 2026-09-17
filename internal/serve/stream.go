package serve

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/noamsto/hookyard/internal/record"
)

// DayString is the canonical day key for t, matching record.StreamPath's own
// UTC formatting so the two can never disagree on which file a day names.
func DayString(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// dayFilePattern is what Days matches stream file names against.
var dayFilePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// validDayPattern is dayFilePattern's date shape without the file suffix,
// checked against every day value a request supplies before it reaches
// filepath.Join: an unvalidated day (e.g. "../../../../etc/passwd") would
// otherwise let a scan read any .jsonl file on the host, not just one under
// stateDir/stream.
var validDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Days lists the days with a stream file, newest first. A missing stream
// directory is the normal state of a fresh machine, not an error.
func Days(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(record.StreamDir(stateDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var days []string
	for _, e := range entries {
		if m := dayFilePattern.FindStringSubmatch(e.Name()); m != nil {
			days = append(days, m[1])
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days, nil
}

// newScanner matches doctor.streamFindings's raise: the default 64 KiB token
// cap equals record.maxRecordBytes, so a max-size record would fail Scan with
// ErrTooLong without it.
func newScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return scanner
}

// clampLimit is the one place limit bounds are enforced (PLAN step 2): the
// HTTP layer passes the raw parsed value straight through.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// ScanDay reads day file `day` under stateDir over the window ending at `end`
// (end <= 0 means the file size), keeps the last `limit` entries matching f,
// and returns them newest-first. A missing day file is not an error: it
// yields an empty, unwindowed result, the normal state before any call has
// landed that day.
func ScanDay(stateDir, day string, end int64, limit int, f Filter) (EventsResponse, error) {
	limit = clampLimit(limit)
	resp := EventsResponse{Day: day}

	if !validDayPattern.MatchString(day) {
		return resp, nil
	}
	path := filepath.Join(record.StreamDir(stateDir), day+".jsonl")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return resp, nil
	}
	if err != nil {
		return EventsResponse{}, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return EventsResponse{}, err
	}
	size := info.Size()
	// A genuine client "before" cursor names an offset in (0, size]: end<=0
	// means no cursor (first load), and end>size means a stale/bogus cursor,
	// both of which already mean "scan to the current end" below. Only the
	// genuine case needs the boundary-record exclusion after the scan.
	pagingOlder := end > 0 && end <= size
	if end <= 0 || end > size {
		end = size
	}

	start := max(0, end-scanWindow)
	if start > 0 {
		resp.Windowed = true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return EventsResponse{}, err
	}
	reader := io.LimitReader(file, end-start)
	scanner := newScanner(reader)
	offset := start
	if start > 0 && scanner.Scan() {
		// Resync to the first whole line: discard bytes up to and including
		// the first newline, which the seek may have landed inside.
		offset += int64(len(scanner.Bytes())) + 1
	}

	// A ring buffer of the last `limit` (or `limit+1` when paging older —
	// see below) matches keeps memory O(limit), never O(filesize) (SPEC 4.4).
	ringSize := limit
	if pagingOlder {
		// The boundary record's own bytes are always the newest entry inside
		// the scan window when a client cursor selects it, so the ring needs
		// one spare slot to still return `limit` results after that entry is
		// excluded below.
		ringSize = limit + 1
	}
	ring := make([]Entry, ringSize)
	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		offset += int64(len(line)) + 1 // +1 for the newline Scanner strips
		var rec record.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if !f.Match(rec) {
			continue
		}
		ring[count%ringSize] = Entry{Rec: rec, Day: day, Offset: offset}
		count++
	}
	if err := scanner.Err(); err != nil {
		return EventsResponse{}, err
	}

	n := min(count, ringSize)
	records := make([]Entry, n)
	for i := range n {
		// Newest first: the ring's most recently written slot is (count-1)%ringSize.
		records[i] = ring[(count-1-i+ringSize)%ringSize]
	}
	if pagingOlder && len(records) > 0 && records[0].Offset == end {
		// That record's own bytes end exactly at `end`, so the scan window
		// [start, end) necessarily re-included it — it was already shown as
		// the previous page's oldest entry.
		records = records[1:]
	}
	if len(records) > limit {
		// The spare ring slot exists to survive the strip above; if that
		// strip didn't happen (e.g. the boundary record was filtered out, or
		// `before` wasn't aligned to a record boundary), trim back to `limit`
		// so the endpoint's contract stays exactly "at most limit records"
		// regardless of which path produced them.
		records = records[:limit]
	}
	resp.Records = records
	if len(records) > 0 {
		resp.NextOffset = records[0].Offset
	} else {
		resp.NextOffset = offset
	}
	return resp, nil
}

// ScanAll streams the whole day file from offset 0, calling visit for every
// decodable record in order, and returns the offset just past the last
// complete (newline-terminated) record. A trailing partial line — the file's
// last bytes when a write is caught mid-record — is deliberately excluded
// from that offset: including it would mean the tailer starts past the
// in-progress record, and once the write completes the tailer would see only
// the suffix, fail to decode it, and silently drop it. Used to seed the
// accumulator.
//
// Known trade-off: this assumes the partial line is a record still being
// written (record.Append does one atomic write(2) per record, capped at 64
// KiB so a short write is not a practical concern — see record.go's
// maxRecordBytes doc — which leaves a concurrent reader racing an in-flight
// write as the ordinary way a partial line appears). If a partial line is
// instead permanently abandoned (e.g. a write error left a genuinely
// incomplete record on disk with no retry), the next distinct record
// appended after it is read glued to that dead prefix with no separating
// newline, fails to decode as one unit, and is dropped along with it — rarer
// than the in-flight case, and not distinguishable from the byte stream
// alone without a self-delimiting record format.
func ScanAll(stateDir, day string, visit func(Entry)) (int64, error) {
	if !validDayPattern.MatchString(day) {
		return 0, nil
	}
	path := filepath.Join(record.StreamDir(stateDir), day+".jsonl")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()

	scanner := newScanner(io.LimitReader(file, size))
	var offset int64
	for scanner.Scan() {
		line := scanner.Bytes()
		lineEnd := offset + int64(len(line)) + 1
		if lineEnd > size {
			// The final token has no trailing newline in the file: a write
			// still in progress. Stop before it — offset already sits just
			// past the last complete record.
			break
		}
		offset = lineEnd
		var rec record.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		visit(Entry{Rec: rec, Day: day, Offset: offset})
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return offset, nil
}
