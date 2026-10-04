package gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// eventLine is the slice of a hookyard event-record line provenance needs.
// It is parsed here rather than imported: priors must not depend on
// hookyard's internal packages.
type eventLine struct {
	SessionID      string `json:"session_id"`
	CanonicalEvent string `json:"canonical_event"`
	ToolName       string `json:"tool_name"`
}

var webTools = []string{"webfetch", "websearch", "web_search", "web_fetch"}

// Provenance returns the gate-2 flag reasons for a fact written by session.
// A session is trusted only when the record positively covers it with tool
// events, the watcher saw it, and neither the record nor the watcher saw
// external content ingested.
func Provenance(recordDir, markerDir, session string, external bool) []string {
	var reasons []string
	add := func(r string) {
		if !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	if external {
		add("provenance:external")
	}
	if session == "" {
		add("provenance:unknown-session")
		return reasons
	}
	stream := filepath.Join(recordDir, "stream")
	entries, err := os.ReadDir(stream)
	if err != nil {
		add("provenance:no-record")
		return reasons
	}
	covered := false
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		scanRecordFile(filepath.Join(stream, e.Name()), session, func(l eventLine) {
			if l.CanonicalEvent == "pre_tool" || l.CanonicalEvent == "post_tool" {
				covered = true
			}
			switch {
			case slices.Contains(webTools, strings.ToLower(l.ToolName)):
				add("provenance:web")
			case strings.HasPrefix(l.ToolName, "mcp__"):
				add("provenance:mcp")
			}
		})
	}
	if !covered {
		add("provenance:no-tool-record")
	}
	seen, ingest, err := sessionMarkers(markerDir, session)
	if ingest {
		add("provenance:shell")
	}
	if err != nil || (covered && !seen) {
		add("provenance:no-ingest-record")
	}
	return reasons
}

// scanRecordFile calls fn for every parsable line of path belonging to
// session. Unreadable files and unparsable lines — including a torn final
// line from an interrupted append — are skipped.
func scanRecordFile(path, session string, fn func(eventLine)) {
	f, err := os.Open(path) //nolint:gosec // path is a *.jsonl entry listed from the configured record stream dir
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	needle := []byte(session)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, needle) {
			var l eventLine
			if json.Unmarshal(line, &l) == nil && l.SessionID == session {
				fn(l)
			}
		}
		if err != nil {
			return
		}
	}
}
