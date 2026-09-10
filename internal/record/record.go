// Package record implements the always-on event stream that
// docs/design/hookyard.md §6 specifies: one JSON line per handled hook event,
// appended to an engine-agnostic file that any subscriber may read and
// hookyard never checks for a reader. hookyard writes the stream whether or
// not anything is watching it, and nothing here degrades or changes shape
// because a subscriber comes or goes.
//
// A subscriber gets an append-only, chronologically-appended, JSON-per-line
// history of every event hookyard handled inside the retention window, with
// the verdict and per-handler outcome for each. That is the entire contract.
// It must not assume:
//
//   - that hookyard knows it exists;
//   - that a record for one event implies one for any other;
//   - that all three engines appear;
//   - that the file is complete back to the beginning of time (retention
//     truncates it) or that it will grow again (a machine may simply stop
//     running agents);
//   - that the final line of a file is whole — a router killed mid-write can
//     leave a torn record, which a subscriber discards rather than retries;
//   - that unknown fields can be rejected, since v will grow additively and
//     readers must ignore what they do not recognize; or
//   - that it may write to the stream, which is hookyard's alone to append to.
package record

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/noamsto/hookyard/internal/vocab"
)

// Outcome vocabulary a handler can report, per §6. This is the transport's
// vocabulary, not a pre-emption of #9's verdict lattice (§4).
const (
	OutcomeAllow      = "allow"
	OutcomeDeny       = "deny"
	OutcomeAsk        = "ask"
	OutcomeAdvise     = "advise"
	OutcomeAbstain    = "abstain"
	OutcomeError      = "error"
	OutcomeTimeout    = "timeout"
	OutcomeSuppressed = "suppressed"
)

// Router status vocabulary, per §6.
const (
	RouterOK      = "ok"
	RouterError   = "error"
	RouterTimeout = "timeout"
)

// maxRecordBytes is §6's atomicity cap: one record is one write(2), and the
// cap is what makes a short write not a practical concern.
const maxRecordBytes = 65536

// maxReasonBytes is unconditional, independent of maxRecordBytes: a reason is
// truncated to this length before the record is even assembled.
const maxReasonBytes = 512

// maxFieldBytes bounds CWD and ToolName during the size-reduction cascade.
const maxFieldBytes = 256

// Event is the minimal input the router builds per handled hook event.
// SessionID is the correlation field — the one §7 confirms all three engines
// send — never a per-turn id (prompt_id/turn_id/generation_id).
type Event struct {
	Engine         vocab.Engine
	SessionID      string
	CanonicalEvent string
	NativeEvent    string
	CWD            string
	ToolName       string
	Verdict        string
	Enforced       bool
	Reason         string
	Router         string
	RouterElapsed  time.Duration
	Handlers       []HandlerOutcome
}

// HandlerOutcome is one handler's contribution to an Event. Delivered is nil
// unless Outcome == OutcomeAdvise, so a non-advise handler never emits a
// spurious delivered key on the wire.
type HandlerOutcome struct {
	Name      string
	Outcome   string
	Elapsed   time.Duration
	Advice    string
	Delivered *bool
}

// RecordHandler is the wire shape of one handlers[] entry. It is exported,
// not recordHandler, so Record.Handlers doesn't expose an unexported type
// through an exported field.
type RecordHandler struct {
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
	MS        int64  `json:"ms"`
	Advice    string `json:"advice,omitempty"`
	Delivered *bool  `json:"delivered,omitempty"`
}

// Record is the wire shape written to disk. Field order matches §6's example
// so a hand-inspected line reads the same way the spec documents it.
type Record struct {
	V              int             `json:"v"`
	TS             string          `json:"ts"`
	Key            string          `json:"key"`
	Engine         string          `json:"engine"`
	SessionID      string          `json:"session_id"`
	CanonicalEvent string          `json:"canonical_event"`
	NativeEvent    string          `json:"native_event"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	Verdict        string          `json:"verdict"`
	Enforced       bool            `json:"enforced"`
	Reason         string          `json:"reason,omitempty"`
	Router         string          `json:"router"`
	RouterMS       int64           `json:"router_ms"`
	Handlers       []RecordHandler `json:"handlers"`
	Truncated      bool            `json:"truncated,omitempty"`
}

// minimalRecord is buildLine's provably-bounded fallback. It is its own type,
// not a zeroed-out Record, because zeroing Record would still marshal
// "canonical_event":"", "cwd":"", "handlers":null and the rest of Record's
// fields — the fallback must emit only these fields, not every field at its
// zero value.
type minimalRecord struct {
	V         int    `json:"v"`
	TS        string `json:"ts"`
	Key       string `json:"key"`
	Engine    string `json:"engine"`
	SessionID string `json:"session_id"`
	Verdict   string `json:"verdict"`
	Router    string `json:"router"`
	Truncated bool   `json:"truncated"`
}

// StreamPath is the single source of truth for where a day's file lives.
// Append, the retention sweep's cutoff computation, and doctor's "today's
// file" lookup all compute the path through this function, never re-deriving
// the format string elsewhere, so the three can never drift against each
// other on timezone.
func StreamPath(stateDir string, t time.Time) string {
	return filepath.Join(stateDir, "stream", t.UTC().Format("2006-01-02")+".jsonl")
}

// computeKey is hookyard's own correlation-key rule (§6): pane if non-empty,
// else engine-scoped session id. Engine-scoped because session identifiers
// are only unique within an engine.
func computeKey(pane string, engine vocab.Engine, sessionID string) string {
	if pane != "" {
		return pane
	}
	return string(engine) + "/" + sessionID
}

func toRecord(e Event, now time.Time, key string) Record {
	handlers := make([]RecordHandler, 0, len(e.Handlers))
	for _, h := range e.Handlers {
		handlers = append(handlers, RecordHandler{
			Name:      h.Name,
			Outcome:   h.Outcome,
			MS:        h.Elapsed.Milliseconds(),
			Advice:    h.Advice,
			Delivered: h.Delivered,
		})
	}
	return Record{
		V:              1,
		TS:             now.UTC().Format("2006-01-02T15:04:05.000000Z"),
		Key:            key,
		Engine:         string(e.Engine),
		SessionID:      e.SessionID,
		CanonicalEvent: e.CanonicalEvent,
		NativeEvent:    e.NativeEvent,
		CWD:            e.CWD,
		ToolName:       e.ToolName,
		Verdict:        e.Verdict,
		Enforced:       e.Enforced,
		Reason:         truncateUTF8(e.Reason, maxReasonBytes),
		Router:         e.Router,
		RouterMS:       e.RouterElapsed.Milliseconds(),
		Handlers:       handlers,
	}
}

// buildLine marshals rec plus a trailing newline. A record that would exceed
// maxRecordBytes is truncated, not split, per §6's atomicity contract: a
// deliberately torn line would violate the same "whole or discarded" rule
// that governs a line torn by a crash.
func buildLine(rec Record) ([]byte, error) {
	line, err := marshalLine(rec)
	if err != nil {
		return nil, err
	}
	if len(line) <= maxRecordBytes {
		return line, nil
	}

	rec.Truncated = true
	for len(rec.Handlers) > 0 {
		rec.Handlers = rec.Handlers[:len(rec.Handlers)-1]
		line, err = marshalLine(rec)
		if err != nil {
			return nil, err
		}
		if len(line) <= maxRecordBytes {
			return line, nil
		}
	}

	rec.Reason = ""
	rec.CWD = truncateUTF8(rec.CWD, maxFieldBytes)
	rec.ToolName = truncateUTF8(rec.ToolName, maxFieldBytes)
	line, err = marshalLine(rec)
	if err != nil {
		return nil, err
	}
	if len(line) <= maxRecordBytes {
		return line, nil
	}

	// Bounded fallback: Key and SessionID are the only two fields nothing
	// else in the cascade constrains, so bounding them here — rather than
	// clipping the marshaled JSON bytes — keeps this a valid JSON object with
	// a static, provable upper bound regardless of input.
	min := minimalRecord{
		V:         rec.V,
		TS:        rec.TS,
		Key:       truncateUTF8(rec.Key, maxFieldBytes),
		Engine:    rec.Engine,
		SessionID: truncateUTF8(rec.SessionID, maxFieldBytes),
		Verdict:   rec.Verdict,
		Router:    rec.Router,
		Truncated: true,
	}
	line, err = marshalLine(min)
	if err != nil {
		return nil, err
	}
	if len(line) <= maxRecordBytes {
		return line, nil
	}
	return nil, fmt.Errorf("record: minimal fallback still exceeds %d bytes", maxRecordBytes)
}

func marshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("record: marshal: %w", err)
	}
	return append(b, '\n'), nil
}

// truncateUTF8 truncates s to at most n bytes without splitting a multi-byte
// rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := s[:n]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
