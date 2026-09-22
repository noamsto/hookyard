package serve

import (
	"net/url"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/vocab"
)

// ParseFilter reads the five filter params (SPEC 4.6). Empty strings are
// dropped so a blank repeated value (e.g. "engine=") does not turn an
// otherwise-empty field into one that matches nothing.
func ParseFilter(q url.Values) Filter {
	return Filter{
		Engines:  nonEmpty(q["engine"]),
		Session:  q.Get("session"),
		Events:   nonEmpty(q["event"]),
		Handlers: nonEmpty(q["handler"]),
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

// Match reports whether r passes every field of f. Fields are AND-combined; values
// within a field are ORed; an empty field matches everything.
func (f Filter) Match(r record.Record) bool {
	if len(f.Engines) > 0 && !slices.Contains(f.Engines, r.Engine) {
		return false
	}
	if f.Session != "" && !strings.Contains(strings.ToLower(r.SessionID), strings.ToLower(f.Session)) {
		return false
	}
	if len(f.Events) > 0 && !matchesEvent(f.Events, r) {
		return false
	}
	if len(f.Handlers) > 0 && !matchesHandler(f.Handlers, r) {
		return false
	}
	if len(f.Verdicts) > 0 && !matchesVerdict(f.Verdicts, r) {
		return false
	}
	return true
}

// matchesEvent treats each wanted value independently: a canonical event name
// (vocab.CanonicalEvents) matches only r.CanonicalEvent, never r.NativeEvent,
// so filtering on the canonical "turn_end" doesn't also pick up pi's native
// per-turn turn_end records, which carry canonical_event "" and
// native_event "turn_end". A non-canonical value (an engine-scoped native
// name) keeps matching either field, as before.
func matchesEvent(want []string, r record.Record) bool {
	for _, w := range want {
		if slices.Contains(vocab.CanonicalEvents, w) {
			if w == r.CanonicalEvent {
				return true
			}
			continue
		}
		if w == r.CanonicalEvent || w == r.NativeEvent {
			return true
		}
	}
	return false
}

func matchesHandler(want []string, r record.Record) bool {
	for _, h := range r.Handlers {
		if slices.Contains(want, h.Name) {
			return true
		}
	}
	return false
}

// matchesVerdict is the SPEC 4.6 superset: verdict, router status, and every
// handler outcome are all candidates, so "show me the calls where anything
// went wrong" is one control rather than three.
func matchesVerdict(want []string, r record.Record) bool {
	if slices.Contains(want, r.Verdict) || slices.Contains(want, r.Router) {
		return true
	}
	for _, h := range r.Handlers {
		if slices.Contains(want, h.Outcome) {
			return true
		}
	}
	return false
}
