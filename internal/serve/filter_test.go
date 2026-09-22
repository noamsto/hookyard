package serve

import (
	"net/url"
	"testing"

	"github.com/noamsto/hookyard/internal/record"
)

func fullRecord() record.Record {
	return record.Record{
		Engine:         "codex",
		SessionID:      "SESS-abc123",
		CanonicalEvent: "pre_tool",
		NativeEvent:    "PreToolUse",
		Verdict:        "abstain",
		Router:         record.RouterOK,
		Handlers: []record.RecordHandler{
			{Name: "guard-a", Outcome: "allow"},
			{Name: "guard-b", Outcome: "deny"},
		},
	}
}

func truncatedRecord() record.Record {
	return record.Record{
		Engine:    "codex",
		SessionID: "SESS-abc123",
		Verdict:   "deny",
		Router:    record.RouterOK,
		Truncated: true,
	}
}

func TestFilterMatch(t *testing.T) {
	tests := []struct {
		name string
		f    Filter
		rec  record.Record
		want bool
	}{
		{"empty filter matches all", Filter{}, fullRecord(), true},
		{"engine match", Filter{Engines: []string{"codex"}}, fullRecord(), true},
		{"engine mismatch", Filter{Engines: []string{"cursor"}}, fullRecord(), false},
		{"engine OR within field", Filter{Engines: []string{"cursor", "codex"}}, fullRecord(), true},
		{"session substring case-insensitive", Filter{Session: "abc"}, fullRecord(), true},
		{"session mismatch", Filter{Session: "zzz"}, fullRecord(), false},
		{"event matches canonical", Filter{Events: []string{"pre_tool"}}, fullRecord(), true},
		{"event matches native", Filter{Events: []string{"PreToolUse"}}, fullRecord(), true},
		{"event mismatch", Filter{Events: []string{"post_tool"}}, fullRecord(), false},
		{"handler match", Filter{Handlers: []string{"guard-b"}}, fullRecord(), true},
		{"handler mismatch", Filter{Handlers: []string{"guard-z"}}, fullRecord(), false},
		{"verdict matches consolidated verdict", Filter{Verdicts: []string{"abstain"}}, fullRecord(), true},
		{"verdict matches router only", Filter{Verdicts: []string{record.RouterTimeout}}, record.Record{Router: record.RouterTimeout, Verdict: "allow"}, true},
		{"verdict matches handler outcome only", Filter{Verdicts: []string{"deny"}}, fullRecord(), true},
		{"verdict mismatch", Filter{Verdicts: []string{"ask"}}, fullRecord(), false},
		{
			"AND across fields",
			Filter{Engines: []string{"codex"}, Session: "abc"},
			fullRecord(),
			true,
		},
		{
			"AND across fields fails on one field",
			Filter{Engines: []string{"codex"}, Session: "zzz"},
			fullRecord(),
			false,
		},
		{"truncated record matches engine", Filter{Engines: []string{"codex"}}, truncatedRecord(), true},
		{"truncated record matches session", Filter{Session: "abc"}, truncatedRecord(), true},
		{"truncated record matches verdict", Filter{Verdicts: []string{"deny"}}, truncatedRecord(), true},
		{"truncated record never matches event", Filter{Events: []string{"pre_tool"}}, truncatedRecord(), false},
		{"truncated record never matches handler", Filter{Handlers: []string{"guard-a"}}, truncatedRecord(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Match(tt.rec); got != tt.want {
				t.Errorf("Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseFilter(t *testing.T) {
	q, err := url.ParseQuery("engine=codex&engine=&event=pre_tool&handler=guard-a&verdict=deny&session=abc")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	got := ParseFilter(q)
	want := Filter{
		Engines:  []string{"codex"},
		Session:  "abc",
		Events:   []string{"pre_tool"},
		Handlers: []string{"guard-a"},
		Verdicts: []string{"deny"},
	}
	if len(got.Engines) != len(want.Engines) || got.Engines[0] != want.Engines[0] {
		t.Errorf("Engines = %v, want %v (empty value must be dropped)", got.Engines, want.Engines)
	}
	if got.Session != want.Session {
		t.Errorf("Session = %q, want %q", got.Session, want.Session)
	}
}
