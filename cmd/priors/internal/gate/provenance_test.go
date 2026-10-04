package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type recLine struct {
	Session string
	Event   string
	Tool    string
}

func (l recLine) json(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"v": 1, "ts": "2026-09-29T10:00:00Z", "engine": "claude",
		"session_id": l.Session, "canonical_event": l.Event, "tool_name": l.Tool,
		"verdict": "allow", "router": "ok", "handlers": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recordDir lays out <dir>/stream/<day>.jsonl, one file per key; raw lines
// are appended verbatim after the encoded ones.
func recordDir(t *testing.T, days map[string][]recLine, raw map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream")
	if err := os.MkdirAll(stream, 0o700); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for d := range days {
		names[d] = true
	}
	for d := range raw {
		names[d] = true
	}
	for day := range names {
		var b strings.Builder
		for _, l := range days[day] {
			b.WriteString(l.json(t) + "\n")
		}
		b.WriteString(raw[day])
		if err := os.WriteFile(filepath.Join(stream, day+".jsonl"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestProvenance(t *testing.T) {
	const s = "sess-1"
	other := recLine{"sess-other", "post_tool", "WebFetch"}

	tests := []struct {
		name     string
		days     map[string][]recLine
		raw      map[string]string
		markers  map[string]bool
		session  string
		external bool
		want     []string
	}{
		{
			name:    "bash post_tool only",
			markers: map[string]bool{s: false},
			days:    map[string][]recLine{"2026-09-29": {{s, "pre_tool", "Bash"}, {s, "post_tool", "Bash"}, other}},
			session: s,
		},
		{
			name:    "WebFetch",
			markers: map[string]bool{s: false},
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}, {s, "post_tool", "WebFetch"}}},
			session: s,
			want:    []string{"provenance:web"},
		},
		{
			name:    "codex web_search",
			markers: map[string]bool{s: false},
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "web_search"}}},
			session: s,
			want:    []string{"provenance:web"},
		},
		{
			name:    "mcp tool",
			markers: map[string]bool{s: false},
			days:    map[string][]recLine{"2026-09-29": {{s, "pre_tool", "mcp__x__y"}}},
			session: s,
			want:    []string{"provenance:mcp"},
		},
		{
			name:    "session across two day files",
			markers: map[string]bool{s: false},
			days: map[string][]recLine{
				"2026-09-28": {{s, "session_start", ""}, {s, "post_tool", "WebSearch"}},
				"2026-09-29": {{s, "post_tool", "mcp__gh__issue"}},
			},
			session: s,
			want:    []string{"provenance:web", "provenance:mcp"},
		},
		{
			name:    "shell ingest",
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			markers: map[string]bool{s: true},
			session: s,
			want:    []string{"provenance:shell"},
		},
		{
			name:    "covered but unseen",
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			session: s,
			want:    []string{"provenance:no-ingest-record"},
		},
		{
			name:    "web and shell",
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "WebFetch"}}},
			markers: map[string]bool{s: true},
			session: s,
			want:    []string{"provenance:web", "provenance:shell"},
		},
		{
			name:    "not covered, ingest marker",
			days:    map[string][]recLine{"2026-09-29": {{s, "session_start", ""}}},
			markers: map[string]bool{s: true},
			session: s,
			want:    []string{"provenance:no-tool-record", "provenance:shell"},
		},
		{
			name:    "other session's markers",
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			markers: map[string]bool{"sess-other": true},
			session: s,
			want:    []string{"provenance:no-ingest-record"},
		},
		{
			name:    "session absent",
			days:    map[string][]recLine{"2026-09-29": {other}},
			session: s,
			want:    []string{"provenance:no-tool-record"},
		},
		{
			name:    "only session_start",
			days:    map[string][]recLine{"2026-09-29": {{s, "session_start", ""}}},
			session: s,
			want:    []string{"provenance:no-tool-record"},
		},
		{
			name:    "empty session",
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			session: "",
			want:    []string{"provenance:unknown-session"},
		},
		{
			name:     "external",
			markers:  map[string]bool{s: false},
			days:     map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			session:  s,
			external: true,
			want:     []string{"provenance:external"},
		},
		{
			name:    "torn last line ignored",
			markers: map[string]bool{s: false},
			days:    map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}},
			raw:     map[string]string{"2026-09-29": `{"session_id":"sess-1","canonical_event":"post_tool","tool_name":"WebFe`},
			session: s,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := recordDir(t, tt.days, tt.raw)
			markerDir := filepath.Join(t.TempDir(), "provenance")
			for session, ingest := range tt.markers {
				if err := MarkSession(markerDir, session, ingest); err != nil {
					t.Fatal(err)
				}
			}
			got := Provenance(dir, markerDir, tt.session, tt.external)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Provenance = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProvenanceMissingStreamDir(t *testing.T) {
	got := Provenance(t.TempDir(), filepath.Join(t.TempDir(), "provenance"), "sess-1", false)
	if want := []string{"provenance:no-record"}; !slices.Equal(got, want) {
		t.Errorf("Provenance = %v, want %v", got, want)
	}
}

func TestProvenanceUnreadableMarkers(t *testing.T) {
	const s = "sess-1"
	dir := recordDir(t, map[string][]recLine{"2026-09-29": {{s, "post_tool", "Bash"}}}, nil)
	markerDir := filepath.Join(t.TempDir(), "provenance")
	if err := os.WriteFile(markerDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := Provenance(dir, markerDir, s, false)
	if want := []string{"provenance:no-ingest-record"}; !slices.Equal(got, want) {
		t.Errorf("Provenance = %v, want %v", got, want)
	}
}
