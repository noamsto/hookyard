package serve

import (
	"net/url"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/internal/record"
)

// outcomeRouterError is the branch outcome assigned to a router-error call's
// pseudo-branch: the vocabulary is handler outcomes plus this one call-level
// value, so a router error can be selected through the same "outcome" field.
const outcomeRouterError = "router-error"

// ParseFilter reads the six filter params. Empty strings are dropped so a
// blank repeated value (e.g. "engine=") does not turn an otherwise-empty
// field into one that matches nothing.
func ParseFilter(q url.Values) Filter {
	return Filter{
		Engines:  nonEmpty(q["engine"]),
		Session:  q.Get("session"),
		Events:   nonEmpty(q["event"]),
		Handlers: nonEmpty(q["handler"]),
		Outcomes: nonEmpty(q["outcome"]),
		Verdicts: nonEmpty(q["verdict"]),
	}
}

func nonEmpty(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// branch is one of a call's branches: a router-error call gets one
// pseudo-branch, a routed call with no handlers gets one direct branch, and
// a routed call with handlers gets one branch per handler. idx is the index
// into record.Handlers, or -1 for the pseudo/direct branches, which have no
// handler entry to point at.
type branch struct {
	handler, outcome string
	idx              int
}

func branches(r record.Record) []branch {
	if r.Router == record.RouterError {
		return []branch{{handler: "", outcome: outcomeRouterError, idx: -1}}
	}
	if len(r.Handlers) == 0 {
		return []branch{{handler: "", outcome: r.Verdict, idx: -1}}
	}
	bs := make([]branch, len(r.Handlers))
	for i, h := range r.Handlers {
		bs[i] = branch{handler: h.Name, outcome: h.Outcome, idx: i}
	}
	return bs
}

func (f Filter) handlerOK(b branch) bool {
	return len(f.Handlers) == 0 || slices.Contains(f.Handlers, b.handler)
}

func (f Filter) outcomeOK(b branch) bool {
	return len(f.Outcomes) == 0 || slices.Contains(f.Outcomes, b.outcome)
}

// branchMatch reports whether b itself carries a handler and an outcome the
// filter accepts. Both must hold on the same branch: "handler=X&outcome=Y"
// means X's own Y branch, not "X ran somewhere and Y happened somewhere".
func (f Filter) branchMatch(b branch) bool {
	return f.handlerOK(b) && f.outcomeOK(b)
}

func (f Filter) engineOK(r record.Record) bool {
	return len(f.Engines) == 0 || slices.Contains(f.Engines, r.Engine)
}

func (f Filter) sessionOK(r record.Record) bool {
	return f.Session == "" || strings.Contains(strings.ToLower(r.SessionID), strings.ToLower(f.Session))
}

func (f Filter) eventOK(r record.Record) bool {
	return len(f.Events) == 0 || slices.Contains(f.Events, eventLabel(r))
}

// verdictOK is the call-level "call verdict" check: the consolidated verdict
// or the router status, never a handler outcome.
func (f Filter) verdictOK(r record.Record) bool {
	return len(f.Verdicts) == 0 || slices.Contains(f.Verdicts, r.Verdict) || slices.Contains(f.Verdicts, r.Router)
}

func (f Filter) callMatch(r record.Record) bool {
	return f.engineOK(r) && f.sessionOK(r) && f.eventOK(r) && f.verdictOK(r)
}

// Match reports whether r passes every call-level field of f, and, if any
// branch field (handler/outcome) is set, at least one branch of r satisfies
// both. Every call has at least one branch, so with no branch field set this
// reduces to callMatch.
func (f Filter) Match(r record.Record) bool {
	if !f.callMatch(r) {
		return false
	}
	for _, b := range branches(r) {
		if f.branchMatch(b) {
			return true
		}
	}
	return false
}

// Hits returns the indices into r.Handlers of r's matching branches, in
// record order, or nil when neither Handlers nor Outcomes is set — the
// "every handler is a hit" case a caller reads as "no branch filter". The
// pseudo/direct branches have idx -1 and never appear here: there is no
// handler entry for them to point at.
func (f Filter) Hits(r record.Record) []int {
	if len(f.Handlers) == 0 && len(f.Outcomes) == 0 {
		return nil
	}
	var hits []int
	for _, b := range branches(r) {
		if b.idx >= 0 && f.branchMatch(b) {
			hits = append(hits, b.idx)
		}
	}
	return hits
}

// eventLabel derives a record's event label: a router-error row's canonical
// event never ran, so it is addressed by its native_event; a routed record
// with a canonical event by that name only; a routed record with no
// canonical event (engine-scoped, e.g. pi's per-turn turn_end) by
// "engine:native_event"; anything else (a truncated record) has no event
// label at all.
func eventLabel(r record.Record) string {
	if r.Router == record.RouterError {
		return r.NativeEvent
	}
	if r.CanonicalEvent != "" {
		return r.CanonicalEvent
	}
	if r.Engine == "" || r.NativeEvent == "" {
		return ""
	}
	return r.Engine + ":" + r.NativeEvent
}
