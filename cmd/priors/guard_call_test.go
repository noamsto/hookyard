package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// blocker is a channel closed when the test ends, for a stub that must stall.
func blocker(t *testing.T) chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	t.Cleanup(func() { close(ch) })
	return ch
}

func TestGuardCallRecordsAndReturnsTheVerdict(t *testing.T) {
	recorded := make(chan envelope, 1)
	check := func(envelope, error) string { return "verdict" }
	record := func(c envelope) { recorded <- c }

	got := guardCall(envelope{SessionID: "s"}, nil, time.Now(), check, record, &bytes.Buffer{})

	if got != "verdict" {
		t.Errorf("reason = %q, want the check's verdict", got)
	}
	select {
	case c := <-recorded:
		if c.SessionID != "s" {
			t.Errorf("recorded %+v, want the call", c)
		}
	case <-time.After(time.Second):
		t.Error("the call was not recorded")
	}
}

func TestGuardCallDeniesAPanicWithItsValue(t *testing.T) {
	var stderr bytes.Buffer
	check := func(envelope, error) string { panic("boom") }

	got := guardCall(envelope{}, nil, time.Now(), check, func(envelope) {}, &stderr)

	for what, text := range map[string]string{"reason": got, "stderr": stderr.String()} {
		if !strings.Contains(text, "the guard failed (boom), so the call is denied") {
			t.Errorf("%s = %q, want the panic value in the failure", what, text)
		}
	}
}

func TestGuardCallDeniesACheckPastTheDeadline(t *testing.T) {
	stall := blocker(t)
	check := func(envelope, error) string { <-stall; return "late" }

	got := guardCall(envelope{}, nil, time.Now().Add(-guardDeadline), check, func(envelope) {}, &bytes.Buffer{})

	if !strings.Contains(got, "ran out of time") {
		t.Errorf("reason = %q, want the out-of-time denial", got)
	}
}

func TestGuardCallAnswersWhileRecordStalls(t *testing.T) {
	stall := blocker(t)
	check := func(envelope, error) string { return "verdict" }
	record := func(envelope) { <-stall }

	begin := time.Now()
	got := guardCall(envelope{}, nil, begin, check, record, &bytes.Buffer{})

	if got != "verdict" {
		t.Errorf("reason = %q, want the check's verdict", got)
	}
	if elapsed := time.Since(begin); elapsed > guardDeadline+2*time.Second {
		t.Errorf("answered after %v, want about guardDeadline (%v)", elapsed, guardDeadline)
	}
}
