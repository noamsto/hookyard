// Package serve implements `hookyard serve`: a localhost, read-only HTTP
// view of the routed-call stream (SPEC.md, issue #57). This file is the
// package's frozen contract — every constant and type every other file in
// the package builds on — so that steps built in parallel cannot invent
// divergent signatures.
package serve

import (
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
)

// listenHost is C1: bind loopback only. Not a flag, not a variable — there is
// deliberately no code path that substitutes it.
const listenHost = "127.0.0.1"

// pollInterval is the tailer's stat frequency (SPEC 4.2).
const pollInterval = 250 * time.Millisecond

// maxCarry bounds the tailer's partial-line buffer at twice
// record.maxRecordBytes, so a crashed writer cannot wedge the reader forever
// (SPEC 4.2 case 2).
const maxCarry = 128 << 10

const defaultLimit = 500
const maxLimit = 2000

// DefaultPort is exported because cmd/hookyard's flag default is its only
// consumer and the value must live in one place.
const DefaultPort = 7757

// scanWindow is a var, not a const, solely so a test can shrink it instead of
// needing a 64 MiB fixture. Nothing outside the package may set it.
var scanWindow int64 = 64 << 20

// Entry is one stream record plus where it sat in the file.
type Entry struct {
	Rec    record.Record `json:"rec"`
	Day    string        `json:"day"`    // YYYY-MM-DD
	Offset int64         `json:"offset"` // byte offset just past this record
	// Hits is the branch indices a request's Filter.Hits found in Rec.
	// Absent (nil) means "no branch filter" — every handler is a hit.
	Hits []int `json:"hits,omitempty"`
}

// VerdictCount is the verdict mix's joint distribution: SPEC 4.5 wants the
// count per consolidated verdict *split by* enforced, which two independent
// maps cannot express.
type VerdictCount struct {
	Total      int64 `json:"total"`
	Enforced   int64 `json:"enforced"`
	Unenforced int64 `json:"unenforced"`
}

type HandlerStat struct {
	Name    string `json:"name"`
	Calls   int64  `json:"calls"`
	TotalMS int64  `json:"total_ms"`
	MaxMS   int64  `json:"max_ms"`
}

type Snapshot struct {
	Day       string                  `json:"day"`
	Calls     int64                   `json:"calls"`
	Handlers  []HandlerStat           `json:"handlers"` // sorted by name
	Verdicts  map[string]VerdictCount `json:"verdicts"`
	Router    map[string]int64        `json:"router"`
	Truncated int64                   `json:"truncated"`
	Since     string                  `json:"since"` // ts of the first counted record
}

// Filter is the branch-aware predicate. Engine, session, event and verdict
// are call-level; handler and outcome are branch-level and must hold on the
// same branch of the call (Filter.Match).
type Filter struct {
	Engines  []string
	Session  string   // case-insensitive substring
	Events   []string // canonical_event, engine:native_event, or native_event on a router error (SPEC 4.6)
	Handlers []string // branch: a branch's handler name (never "", so it excludes handler-less branches)
	Outcomes []string // branch: a branch's outcome (handler outcomes; "router-error"; or, for a handler-less call, its own verdict, e.g. "abstain"/"suppressed")
	Verdicts []string // call verdict: the consolidated verdict OR the router status — never a handler outcome
}

type EventsResponse struct {
	Records    []Entry `json:"records"` // newest first
	NextOffset int64   `json:"next_offset"`
	Windowed   bool    `json:"windowed"`
	Day        string  `json:"day"`
}

type DaysResponse struct {
	Days  []string `json:"days"` // newest first
	Today string   `json:"today"`
}

// TableResponse is everything the right-hand panel needs: the installed table
// itself, plus the vocabularies the filter bar offers as options. SPEC 4.1
// says internal/vocab supplies the engine and canonical-event lists; this is
// the endpoint that actually carries them to the page.
type TableResponse struct {
	Handlers []manifest.Handler `json:"handlers"`
	Engines  []string           `json:"engines"` // vocab.Engines
	Events   []string           `json:"events"`  // vocab.CanonicalEvents
	Error    string             `json:"error,omitempty"`
	Path     string             `json:"path"`
}

// FlowHop is one handler's contribution to a FlowPath's chain: its name and
// its own outcome, not the record's consolidated verdict.
type FlowHop struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

// FlowPath is one distinct route a call took, with its count per bucket.
type FlowPath struct {
	Engine         string    `json:"engine"`
	CanonicalEvent string    `json:"canonical_event"`
	NativeEvent    string    `json:"native_event"`
	Router         string    `json:"router"`
	Verdict        string    `json:"verdict"`
	Handlers       []FlowHop `json:"handlers"` // never null; record order
	Counts         []int64   `json:"counts"`   // len == max(window,1), oldest first
}

type FlowResponse struct {
	Day         string     `json:"day"`
	Window      int        `json:"window"`       // minutes; 0 = whole day
	BucketStart int64      `json:"bucket_start"` // unix ms of bucket 0; 0 when Window == 0
	BucketMS    int64      `json:"bucket_ms"`    // 60000; 0 when Window == 0
	Calls       int64      `json:"calls"`        // records counted into Counts
	Branches    int64      `json:"branches"`     // matched branches counted; pseudo/direct branches count as 1
	NextOffset  int64      `json:"next_offset"`  // ScanAll's end: offset past the last complete record
	Paths       []FlowPath `json:"paths"`        // never null; sorted by total desc, then key asc
	Facets      FlowFacets `json:"facets"`
}

// FlowFacets gives each filterable column its own counts, so a filtered
// column can still show the other values the client might add: each field's
// facet applies every filter *except that field's own*. engine and event
// facets count calls; handler and outcome facets count branches. Every map
// is non-nil, even when empty.
type FlowFacets struct {
	Engine  map[string]int64 `json:"engine"`
	Event   map[string]int64 `json:"event"`
	Handler map[string]int64 `json:"handler"`
	Outcome map[string]int64 `json:"outcome"`
}

// Frame is one SSE message.
type Frame struct {
	Event string // "call" | "stats" | "day" | "reset"
	ID    string // "<day>:<offset>" for call, empty otherwise
	Data  any
}

// TailEvent is exactly one of: a record, a day rollover, or a restart.
type TailEvent struct {
	Entry   *Entry // set for a record
	NewDay  string // set when the day rolled over
	Restart string // set when the open file was truncated or replaced; the reason
}
