package serve

import (
	"net/url"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/internal/record"
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
	if len(f.Events) > 0 && !slices.Contains(f.Events, r.CanonicalEvent) && !slices.Contains(f.Events, r.NativeEvent) {
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
