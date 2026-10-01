package fact

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const fullFact = `---
name: haiku-workers-stall-on-prompts
description: Claude haiku dispatch workers stop on a permission prompt for every shell command
metadata:
  node_type: memory
  type: project
  scope: repo
  repos: [dispatcher, hookyard]
  engines: [claude, codex]
  valid_from: 2026-09-16
  superseded_by: null
  verified: 2026-09-16
  confidence: proposed
  provenance:
    engine: claude
    session: 98a49727-b288-4f1c-9e6c-e7453ce01ef6
    host: tp-g5
    pid: 42
  source:
    engine: claude
    path: ~/.claude/projects/p/memory/x.md
    thread_id: t-1
    sha256: abc123
    mtime: 1700000000
  originSessionId: 98a49727-b288-4f1c-9e6c-e7453ce01ef6
  modified: 2026-09-16T13:42:44.668Z
  flags: ['provenance:web', 'content:url']
  reviewer: noam
  zeta: last
last_reviewed: 2026-09-17
tags:
  - a
  - b
---

Observed 2026-09-16.

---

**Why:** body keeps its own rules.
`

func TestParseFullFactReadsEveryField(t *testing.T) {
	f, err := Parse([]byte(fullFact))
	if err != nil {
		t.Fatal(err)
	}
	m := f.Metadata
	if f.Name != "haiku-workers-stall-on-prompts" || !strings.HasPrefix(f.Description, "Claude haiku") {
		t.Errorf("name/description = %q / %q", f.Name, f.Description)
	}
	if m.NodeType != "memory" || m.Type != "project" || m.Scope != "repo" || m.Confidence != "proposed" {
		t.Errorf("metadata scalars = %+v", m)
	}
	if !reflect.DeepEqual(m.Repos, []string{"dispatcher", "hookyard"}) || !reflect.DeepEqual(m.Engines, []string{"claude", "codex"}) {
		t.Errorf("repos/engines = %v / %v", m.Repos, m.Engines)
	}
	if m.ValidFrom != "2026-09-16" || m.Verified != "2026-09-16" || m.SupersededBy != "" {
		t.Errorf("dates = %q %q %q", m.ValidFrom, m.Verified, m.SupersededBy)
	}
	if m.Modified != "2026-09-16T13:42:44.668Z" || m.OriginSessionID != "98a49727-b288-4f1c-9e6c-e7453ce01ef6" {
		t.Errorf("modified/originSessionId = %q / %q", m.Modified, m.OriginSessionID)
	}
	if m.Provenance == nil || m.Provenance.Engine != "claude" || m.Provenance.Host != "tp-g5" || m.Provenance.Extra["pid"] != 42 {
		t.Errorf("provenance = %+v", m.Provenance)
	}
	if m.Source == nil || m.Source.ThreadID != "t-1" || m.Source.SHA256 != "abc123" || m.Source.Extra["mtime"] != 1700000000 {
		t.Errorf("source = %+v", m.Source)
	}
	if !reflect.DeepEqual(m.Flags, []string{"provenance:web", "content:url"}) {
		t.Errorf("flags = %v", m.Flags)
	}
	if m.Extra["zeta"] != "last" || m.Extra["reviewer"] != "noam" {
		t.Errorf("metadata extra = %v", m.Extra)
	}
	if f.Extra["tags"] == nil || len(f.Extra) != 2 {
		t.Errorf("top-level extra = %v", f.Extra)
	}
	wantBody := "\nObserved 2026-09-16.\n\n---\n\n**Why:** body keeps its own rules.\n"
	if f.Body != wantBody {
		t.Errorf("body = %q, want %q", f.Body, wantBody)
	}
}

func TestMarshalFullFactIsCanonical(t *testing.T) {
	f, err := Parse([]byte(fullFact))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Extra keys come out sorted, so tags (a list) lands after last_reviewed.
	if string(got) != fullFact {
		t.Errorf("Marshal differs from the canonical text:\n%s", got)
	}
}

func TestMarshalParseIsStable(t *testing.T) {
	f := Fact{
		Name:        "a-fact",
		Description: "a description: with a colon, and #hash",
		Metadata: Metadata{
			NodeType: "memory", Type: "reference", Scope: "global",
			Engines:      []string{"claude"},
			ValidFrom:    "2026-09-16",
			Verified:     "2026-09-16",
			Confidence:   "reviewed",
			Provenance:   &Provenance{Engine: "codex", Session: "s", Host: "h", Extra: map[string]any{"k": "v"}},
			Source:       &Source{Engine: "codex", Path: "/p", SHA256: "ff", Extra: map[string]any{"n": 3}},
			Modified:     "2026-09-16T13:42:44.668Z",
			Flags:        []string{"size"},
			Extra:        map[string]any{"nested": map[string]any{"x": []any{"1", "2"}}, "b": true},
			SupersededBy: "other.md",
		},
		Extra: map[string]any{"custom": "x", "when": "it is 2026"},
		Body:  "\nbody\n",
	}
	first, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, f) {
		t.Errorf("Parse(Marshal(f)) != f\n got: %+v\nwant: %+v", parsed, f)
	}
	second, err := parsed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("Marshal is not stable:\n%s\n---\n%s", first, second)
	}
}

func TestMarshalWritesNullForEmptySupersededByAndVerified(t *testing.T) {
	f := Fact{Name: "a-fact", Description: "d", Metadata: Metadata{Type: "user"}, Body: "b\n"}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: a-fact\ndescription: d\nmetadata:\n  type: user\n  superseded_by: null\n  verified: null\n---\nb\n"
	if string(got) != want {
		t.Errorf("Marshal = %q, want %q", got, want)
	}
	back, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.Metadata.SupersededBy != "" || back.Metadata.Verified != "" {
		t.Errorf("null parsed as %q / %q", back.Metadata.SupersededBy, back.Metadata.Verified)
	}
}

func TestMarshalUsesFlowStyleForRepoAndEngineLists(t *testing.T) {
	f := Fact{Name: "n", Description: "d", Metadata: Metadata{Repos: []string{"dispatcher"}, Engines: []string{"claude"}}}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"  repos: [dispatcher]\n", "  engines: [claude]\n"} {
		if !strings.Contains(string(got), line) {
			t.Errorf("output lacks %q:\n%s", line, got)
		}
	}
}

func TestMarshalKeepsDatesUnquotedAndDescriptionOnOneLine(t *testing.T) {
	long := strings.Repeat("word ", 40) + "end"
	f := Fact{
		Name:        "n",
		Description: long,
		Metadata:    Metadata{ValidFrom: "2026-09-16", Modified: "2026-09-16T13:42:44.668Z"},
	}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, line := range []string{"  valid_from: 2026-09-16\n", "  modified: 2026-09-16T13:42:44.668Z\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "description: "+long+"\n") {
		t.Errorf("description was wrapped or altered:\n%s", out)
	}
}

func TestMarshalKeepsMultilineDescriptionOnOneLine(t *testing.T) {
	f := Fact{Name: "n", Description: "line one\nline two"}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.Description != f.Description {
		t.Errorf("description = %q, want %q", back.Description, f.Description)
	}
	if !strings.Contains(string(got), "\nmetadata:\n") || strings.Contains(string(got), "line one\n") {
		t.Errorf("description spans lines:\n%s", got)
	}
}

func TestParseUnquotedDatesDecodeAsWritten(t *testing.T) {
	f, err := Parse([]byte("---\nname: n\ndescription: d\nmetadata:\n  valid_from: 2026-09-16\n  modified: 2026-09-16T13:42:44.668Z\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Metadata.ValidFrom != "2026-09-16" || f.Metadata.Modified != "2026-09-16T13:42:44.668Z" {
		t.Errorf("got %q / %q", f.Metadata.ValidFrom, f.Metadata.Modified)
	}
}

func TestUnknownDateValuesSurviveRoundTripUnchanged(t *testing.T) {
	in := "---\nname: a-fact\ndescription: d\nmetadata:\n  superseded_by: null\n  verified: null\n  at: 2026-09-16T13:42:44.668Z\n  seen: 2026-09-16\nlast: 2026-09-17\n---\n"
	f, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != in {
		t.Errorf("round-trip changed the file:\n%s", got)
	}
}

func TestParseClaudeShapedFile(t *testing.T) {
	in := "---\nname: feedback-testing\ndescription: Integration tests must hit a real database\nmetadata:\n  type: feedback\noriginSessionId: 98a49727-b288-4f1c-9e6c-e7453ce01ef6\nmodified: 2026-09-16T13:42:44.668Z\n---\nBody text.\n"
	f, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "feedback-testing" || f.Metadata.Type != "feedback" || f.Body != "Body text.\n" {
		t.Errorf("parsed = %+v", f)
	}
	if f.Extra["originSessionId"] != "98a49727-b288-4f1c-9e6c-e7453ce01ef6" || f.Extra["modified"] == nil {
		t.Errorf("Claude's top-level keys should land in Extra, got %v", f.Extra)
	}
}

func TestParseFoldedMultilineDescription(t *testing.T) {
	in := "---\nname: n\ndescription: first line\n  second line\n---\n"
	f, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if f.Description != "first line second line" {
		t.Errorf("description = %q", f.Description)
	}
}

func TestParseRejectsMissingFences(t *testing.T) {
	cases := map[string]string{
		"no opening fence":   "name: n\n---\nbody\n",
		"no closing fence":   "---\nname: n\ndescription: d\nbody\n",
		"empty input":        "",
		"opening without nl": "---",
		"invalid yaml":       "---\nname: [unclosed\n---\n",
		"frontmatter a list": "---\n- a\n- b\n---\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", in)
			}
		})
	}
}

func TestParseAcceptsEmptyFrontmatterAndFenceAtEOF(t *testing.T) {
	f, err := Parse([]byte("---\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Body != "body" {
		t.Errorf("body = %q", f.Body)
	}
	if _, err := Parse([]byte("---\nname: n\n---")); err != nil {
		t.Errorf("closing fence at EOF without newline: %v", err)
	}
}

func TestNameRE(t *testing.T) {
	ok := []string{"a", "a-b-9", "0abc", strings.Repeat("a", 81)}
	bad := []string{"", "-a", "A", "a_b", "../x", "a b", "a.md", strings.Repeat("a", 82)}
	for _, s := range ok {
		if !NameRE.MatchString(s) {
			t.Errorf("NameRE rejects %q", s)
		}
	}
	for _, s := range bad {
		if NameRE.MatchString(s) {
			t.Errorf("NameRE accepts %q", s)
		}
	}
}

func TestText(t *testing.T) {
	f := Fact{Name: "n", Description: "d", Body: "b\n"}
	if got := f.Text(); got != "n\nd\nb\n" {
		t.Errorf("Text = %q", got)
	}
}

func TestModifiedTime(t *testing.T) {
	want := time.Date(2026, 9, 16, 13, 42, 44, 668_000_000, time.UTC)
	cases := []struct {
		name string
		meta Metadata
		want time.Time
	}{
		{"modified nano", Metadata{Modified: "2026-09-16T13:42:44.668Z", ValidFrom: "2026-01-01"}, want},
		{"modified seconds", Metadata{Modified: "2026-09-16T13:42:44Z"}, want.Truncate(time.Second)},
		{"valid_from fallback", Metadata{ValidFrom: "2026-09-16"}, time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)},
		{"unparsable modified falls back", Metadata{Modified: "garbage", ValidFrom: "2026-09-16"}, time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)},
		{"neither", Metadata{}, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Fact{Metadata: c.meta}.ModifiedTime()
			if !got.Equal(c.want) {
				t.Errorf("ModifiedTime = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTypesVocabulary(t *testing.T) {
	want := []string{"project", "reference", "feedback", "user"}
	if !reflect.DeepEqual(Types, want) {
		t.Errorf("Types = %v", Types)
	}
}
