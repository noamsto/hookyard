package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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
