package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/noamsto/hookyard/internal/doctor"
	"github.com/noamsto/hookyard/internal/vocab"
)

func TestRenderDoctorPlainOutputAndProblemCount(t *testing.T) {
	findings := []doctor.Finding{
		{Engine: vocab.Codex, Check: "trust", Status: doctor.Pass, Detail: "trusted"},
		{Engine: vocab.Cursor, Check: "registered", Status: doctor.Fail, Detail: "missing"},
	}
	var out bytes.Buffer
	problems, err := renderDoctor(&out, "/work/tree", findings, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "hookyard doctor — /work/tree\n\n" +
		"codex        trust                ok       trusted\n" +
		"cursor       registered           PROBLEM  missing\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	if problems != 1 {
		t.Fatalf("problems = %d, want 1", problems)
	}
}

func TestRenderDoctorTTYGroupsAndWrapsDetails(t *testing.T) {
	longPaths := strings.Repeat("/tmp/pi-extension, ", 8) + "/tmp/end"
	findings := []doctor.Finding{
		{Engine: vocab.Pi, Check: "router path", Status: doctor.Fail, Detail: longPaths, Fix: "Run hookyard install."},
		{Engine: vocab.Pi, Check: "launcher", Status: doctor.Unknown, Detail: "not readable"},
		{Check: "stream", Status: doctor.Fail, Detail: "broken"},
		{Engine: vocab.Codex, Check: "trust", Status: doctor.Pass, Detail: "trusted"},
		{Engine: vocab.ClaudeCode, Check: "launcher", Status: doctor.Unknown, Detail: "not readable"},
	}
	var out bytes.Buffer
	if problems, err := renderDoctor(&out, "ignored", findings, true, false); err != nil {
		t.Fatal(err)
	} else if problems != 2 {
		t.Fatalf("problems = %d, want 2", problems)
	}
	raw := out.String()
	for _, want := range []string{"✗ pi — 1 problem", "? launcher", "✓ codex — all ok", "✗ hookyard — 1 problem", "? claude-code — unknown", "fix: Run hookyard install.", "Affected: pi, hookyard"} {
		if !strings.Contains(raw, want) {
			t.Errorf("output missing %q: %q", want, raw)
		}
	}
	if !strings.Contains(raw, "\n                 ") {
		t.Error("long comma-separated detail was not wrapped and indented")
	}
}

func TestRenderDoctorJSONSchemaAndNoANSI(t *testing.T) {
	findings := []doctor.Finding{
		{Engine: vocab.Codex, Check: "trust", Status: doctor.Unknown, Detail: "unclear"},
		{Engine: vocab.Codex, Check: "registered", Status: doctor.Fail, Detail: "missing", Fix: "Run hookyard install."},
		{Check: "stream", Status: doctor.Pass, Detail: "healthy"},
	}
	var out bytes.Buffer
	if problems, err := renderDoctor(&out, "ignored", findings, true, true); err != nil {
		t.Fatal(err)
	} else if problems != 1 {
		t.Fatalf("problems = %d, want 1", problems)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("JSON output contains ANSI escape")
	}
	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Engines) != 2 || report.Engines[0].Engine != "codex" || report.Engines[0].Verdict != "problem" || report.Engines[0].Problems != 1 {
		t.Fatalf("groups = %#v", report.Engines)
	}
	if len(report.Engines[0].Checks) != 2 || report.Engines[0].Checks[0].Status != "unknown" || report.Engines[0].Checks[1].Fix == "" {
		t.Fatalf("checks = %#v", report.Engines[0].Checks)
	}
	if report.Engines[1].Engine != "hookyard" || report.Engines[1].Verdict != "ok" {
		t.Fatalf("global group = %#v", report.Engines[1])
	}
}

func TestDoctorDetailBoundsLongCommaSegment(t *testing.T) {
	detail := doctorDetail(strings.Repeat("/very-long-path-segment", 8)+", /短い", 0)
	for _, line := range strings.Split(detail, "\n") {
		if utf8.RuneCountInString(line) > 88 {
			t.Fatalf("detail line length = %d, want at most 88: %q", len(line), line)
		}
	}
	if !strings.Contains(detail, "…") || !strings.Contains(detail, "/短い") {
		t.Fatalf("detail = %q, want truncation and later segment", detail)
	}

	packed := doctorDetail(strings.Repeat("a", 40)+", "+strings.Repeat("b", 40)+", "+strings.Repeat("c", 40), 0)
	packedLines := strings.Split(packed, "\n")
	if utf8.RuneCountInString(packedLines[0]) <= 71 {
		t.Fatalf("first packed line used %d runes, want it to use first-line space", utf8.RuneCountInString(packedLines[0]))
	}
	for _, line := range packedLines {
		if utf8.RuneCountInString(line) > 88 {
			t.Fatalf("packed detail line length = %d, want at most 88: %q", utf8.RuneCountInString(line), line)
		}
	}

	if got := doctorTruncate("é"+strings.Repeat("x", 88), 88); utf8.RuneCountInString(got) != 88 || !strings.HasSuffix(got, "…") {
		t.Fatalf("exact-boundary truncation = %q, want 88 runes ending in ellipsis", got)
	}
	if got := doctorTruncate(strings.Repeat("界", 89), 88); utf8.RuneCountInString(got) != 88 || !strings.HasSuffix(got, "…") {
		t.Fatalf("unicode truncation = %q, want 88 runes ending in ellipsis", got)
	}
}

func TestRenderDoctorTTYLinesFitTerminalWidth(t *testing.T) {
	detail := strings.Repeat("/home/noam/.local/share/pi/extensions/launcher-wrapper/bin/pi, ", 4) + "/home/noam/.local/share/pi/extensions/launcher-wrapper/bin/pi"
	fix := strings.Repeat("/home/noam/.local/share/pi/extensions/launcher-wrapper/bin/hookyard, ", 4) + "/home/noam/.local/share/pi/extensions/launcher-wrapper/bin/hookyard"
	check := "launcher wrapper path"
	findings := []doctor.Finding{{Engine: vocab.Pi, Check: check, Status: doctor.Fail, Detail: detail, Fix: fix}}
	var out bytes.Buffer
	if _, err := renderDoctor(&out, "ignored", findings, true, false); err != nil {
		t.Fatal(err)
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	lines := strings.Split(strings.TrimSuffix(ansi.ReplaceAllString(out.String(), ""), "\n"), "\n")
	for _, line := range lines {
		if got := utf8.RuneCountInString(line); got > 88 {
			t.Fatalf("rendered line length = %d, want at most 88: %q", got, line)
		}
	}
	checkPrefix := fmt.Sprintf("  %s %-20s ", "✗", check)
	fixPrefix := fmt.Sprintf("    fix: %s", "")
	assertDoctorContinuationIndent(t, lines, checkPrefix, fixPrefix)
	assertDoctorContinuationIndent(t, lines, fixPrefix, "Affected:")
}

func assertDoctorContinuationIndent(t *testing.T, lines []string, prefix, nextPrefix string) {
	t.Helper()
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("row with prefix %q not found in %q", prefix, lines)
	}
	wantIndent := utf8.RuneCountInString(prefix)
	continuations := 0
	for _, line := range lines[start+1:] {
		if line == "" || strings.HasPrefix(line, nextPrefix) {
			break
		}
		gotIndent := utf8.RuneCountInString(line) - utf8.RuneCountInString(strings.TrimLeft(line, " "))
		if gotIndent != wantIndent {
			t.Fatalf("continuation indent = %d, want %d: %q", gotIndent, wantIndent, line)
		}
		continuations++
	}
	if continuations == 0 {
		t.Fatalf("row with prefix %q did not wrap", prefix)
	}
}
