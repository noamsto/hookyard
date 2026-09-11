package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

// Read straight from the source tree rather than t.TempDir(), unlike every
// other test in this repo: these fixtures are the ground truth this package
// is graded against (docs/design/fixtures/hook-payloads/README.md), so a
// copy would drift from the thing it's supposed to pin.
const fixtureDir = "../../docs/design/fixtures/hook-payloads"

type want struct {
	engine         vocab.Engine
	canonicalEvent string
	nativeEvent    string
	protocol       string
	toolName       string
	toolInputJSON  string // "" means ToolInput must be nil
	cwd            string
}

func TestDecodeEveryFixture(t *testing.T) {
	tests := map[string]want{
		"claude-PreToolUse.json": {
			engine: vocab.ClaudeCode, canonicalEvent: vocab.PreTool, nativeEvent: "PreToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","description":"Echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/claude-probe",
		},
		"claude-PostToolUse.json": {
			engine: vocab.ClaudeCode, canonicalEvent: vocab.PostTool, nativeEvent: "PostToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","description":"Echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/claude-probe",
		},
		"claude-PreToolUse-DENY.json": {
			engine: vocab.ClaudeCode, canonicalEvent: vocab.PreTool, nativeEvent: "PreToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","description":"Echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/claude-probe",
		},
		"codex-pre_tool_use.json": {
			engine: vocab.Codex, canonicalEvent: vocab.PreTool, nativeEvent: "PreToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/probe",
		},
		"codex-pre_tool_use-DENY.json": {
			engine: vocab.Codex, canonicalEvent: vocab.PreTool, nativeEvent: "PreToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/probe",
		},
		"codex-user_prompt_submit-TICK.json": {
			engine: vocab.Codex, canonicalEvent: vocab.PromptSubmit, nativeEvent: "UserPromptSubmit",
			toolName: "", toolInputJSON: "",
			cwd: "<PROBE>/capture/probe",
		},
		"cursor-preToolUse.json": {
			engine: vocab.Cursor, canonicalEvent: vocab.PreTool, nativeEvent: "preToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","cwd":"","timeout":30000}`,
			cwd: "<PROBE>/capture/cursor-probe",
		},
		"cursor-preToolUse-DENY.json": {
			engine: vocab.Cursor, canonicalEvent: vocab.PreTool, nativeEvent: "preToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","cwd":"","timeout":30000}`,
			cwd: "<PROBE>/capture/cursor-probe",
		},
		"cursor-postToolUse.json": {
			engine: vocab.Cursor, canonicalEvent: vocab.PostTool, nativeEvent: "postToolUse",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe","cwd":"","timeout":30000}`,
			cwd: "<PROBE>/capture/cursor-probe",
		},
		"cursor-beforeShellExecution.json": {
			engine: vocab.Cursor, canonicalEvent: "", nativeEvent: "beforeShellExecution", protocol: "shell",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe"}`,
			cwd: "<PROBE>/capture/cursor-probe",
		},
		"pi-session_start.json": {
			engine: vocab.Pi, canonicalEvent: vocab.SessionStart, nativeEvent: "session_start",
			toolName: "", toolInputJSON: "",
			cwd: "<PROBE>/work",
		},
		"pi-input.json": {
			engine: vocab.Pi, canonicalEvent: vocab.PromptSubmit, nativeEvent: "input",
			toolName: "", toolInputJSON: "",
			cwd: "<PROBE>/work",
		},
		"pi-tool_call.json": {
			engine: vocab.Pi, canonicalEvent: vocab.PreTool, nativeEvent: "tool_call",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe"}`,
			cwd: "<PROBE>/work",
		},
		"pi-tool_call-DENY.json": {
			engine: vocab.Pi, canonicalEvent: vocab.PreTool, nativeEvent: "tool_call",
			toolName: "Bash", toolInputJSON: `{"command":"touch SIDE-EFFECT.txt"}`,
			cwd: "<PROBE>/work",
		},
		"pi-tool_result.json": {
			engine: vocab.Pi, canonicalEvent: vocab.PostTool, nativeEvent: "tool_result",
			toolName: "Bash", toolInputJSON: `{"command":"echo hookyard-probe"}`,
			cwd: "<PROBE>/work",
		},
		"pi-turn_end.json": {
			engine: vocab.Pi, canonicalEvent: vocab.TurnEnd, nativeEvent: "turn_end",
			toolName: "", toolInputJSON: "",
			cwd: "<PROBE>/work",
		},
	}

	for name, w := range tests {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join(fixtureDir, name))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()

			env, err := Decode(f)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			if env.Engine != w.engine {
				t.Errorf("Engine = %q, want %q", env.Engine, w.engine)
			}
			if env.CanonicalEvent != w.canonicalEvent {
				t.Errorf("CanonicalEvent = %q, want %q", env.CanonicalEvent, w.canonicalEvent)
			}
			if env.NativeEvent != w.nativeEvent {
				t.Errorf("NativeEvent = %q, want %q", env.NativeEvent, w.nativeEvent)
			}
			if env.Protocol != w.protocol {
				t.Errorf("Protocol = %q, want %q", env.Protocol, w.protocol)
			}
			if env.ToolName != w.toolName {
				t.Errorf("ToolName = %q, want %q", env.ToolName, w.toolName)
			}
			if env.Cwd != w.cwd {
				t.Errorf("Cwd = %q, want %q", env.Cwd, w.cwd)
			}
			if w.toolInputJSON == "" {
				if env.ToolInput != nil {
					t.Errorf("ToolInput = %s, want nil", env.ToolInput)
				}
			} else if !jsonEqual(t, env.ToolInput, []byte(w.toolInputJSON)) {
				t.Errorf("ToolInput = %s, want %s", env.ToolInput, w.toolInputJSON)
			}
		})
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not valid JSON: %v", err)
	}
	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)
	return string(gj) == string(wj)
}

// The marshalled shape is what every handler actually reads; a typo'd struct
// tag passes every field assertion above and breaks every guard on every
// engine.
func TestMarshalledShapeIsReadableJSON(t *testing.T) {
	env := decodeFixture(t, "cursor-preToolUse.json")
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", doc["tool_name"])
	}
	toolInput, ok := doc["tool_input"].(map[string]any)
	if !ok {
		t.Fatalf("tool_input is not an object: %v", doc["tool_input"])
	}
	if toolInput["command"] != "echo hookyard-probe" {
		t.Errorf("tool_input.command = %v, want echo hookyard-probe", toolInput["command"])
	}

	shell := decodeFixture(t, "cursor-beforeShellExecution.json")
	raw, err = json.Marshal(shell)
	if err != nil {
		t.Fatal(err)
	}
	doc = nil
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["tool_name"] != "Bash" {
		t.Errorf("synthesized tool_name = %v, want Bash", doc["tool_name"])
	}
	toolInput, ok = doc["tool_input"].(map[string]any)
	if !ok {
		t.Fatalf("synthesized tool_input is not an object: %v", doc["tool_input"])
	}
	if toolInput["command"] != "echo hookyard-probe" {
		t.Errorf("synthesized tool_input.command = %v, want echo hookyard-probe", toolInput["command"])
	}
}

// omitempty on tool_input only: canonical_event/protocol/tool_name stay
// present, even empty, because the envelope sketch (§7) shows "" as a legal
// value for each and a field reliably present beats one that sometimes
// vanishes.
func TestMarshalledShapeOmitsOnlyToolInputWhenEmpty(t *testing.T) {
	env := decodeFixture(t, "codex-user_prompt_submit-TICK.json")
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["tool_input"]; present {
		t.Errorf("tool_input should be omitted, got %v", doc["tool_input"])
	}
	toolName, present := doc["tool_name"]
	if !present {
		t.Fatal("tool_name should be present even when empty")
	}
	if toolName != "" {
		t.Errorf("tool_name = %v, want \"\"", toolName)
	}
}

// The drift pin: every fixture's hook_event_name must resolve either through
// InboundEvent (fifteen of sixteen) or as a known engine-only protocol-split
// spelling (beforeShellExecution). A spelling in neither set means the
// fixture and vocab's tables have drifted apart, and must fail loudly rather
// than being patched by inventing a fake canonical mapping (§7, §8's
// double-fire rule) for beforeShellExecution.
func TestEveryFixtureEventIsKnownToVocab(t *testing.T) {
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			env := decodeFixture(t, name)
			_, resolvesCanonically := vocab.InboundEvent(env.Engine, env.NativeEvent)
			isKnownSplit := vocab.Protocol(env.NativeEvent) != ""
			if !resolvesCanonically && !isKnownSplit {
				t.Errorf("native_event %q for %s resolves to neither a canonical event nor a known "+
					"protocol-split spelling", env.NativeEvent, env.Engine)
			}
		})
	}
}

// Each fixture must match exactly one of Detect's four rules, not merely
// the first one checked. The likeliest future overlap is Codex growing an
// effort field of its own; this turns that into a red test here rather than
// a silent misdetection.
func TestEachFixtureMatchesExactlyOneDetectionRule(t *testing.T) {
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			native := readNative(t, name)
			matches := 0
			if present(native, "cursor_version") {
				matches++
			}
			if present(native, "prompt_id") || present(native, "effort") {
				matches++
			}
			if present(native, "turn_id") {
				matches++
			}
			if present(native, "pi_version") {
				matches++
			}
			if matches != 1 {
				t.Errorf("%s matched %d of Detect's four rules, want exactly 1", name, matches)
			}
		})
	}
}

func TestDetectReturnsErrUnknownEngineForAnUndetectablePayload(t *testing.T) {
	native := map[string]json.RawMessage{"session_id": json.RawMessage(`"x"`)}
	_, err := Detect(native)
	if !errors.Is(err, ErrUnknownEngine) {
		t.Errorf("Detect: got %v, want ErrUnknownEngine", err)
	}
}

// A null discriminator is an absent value, not evidence of an engine.
func TestDetectTreatsNullDiscriminatorAsAbsent(t *testing.T) {
	native := map[string]json.RawMessage{
		"cursor_version": json.RawMessage(`null`),
		"turn_id":        json.RawMessage(`"t1"`),
	}
	engine, err := Detect(native)
	if err != nil {
		t.Fatal(err)
	}
	if engine != vocab.Codex {
		t.Errorf("Detect with null cursor_version = %q, want falling through to codex", engine)
	}
}

func TestCwdFallback(t *testing.T) {
	tests := map[string]struct {
		native map[string]json.RawMessage
		want   string
	}{
		"payload cwd wins even with workspace_roots present": {
			native: map[string]json.RawMessage{
				"cwd":             json.RawMessage(`"/payload"`),
				"workspace_roots": json.RawMessage(`["/root"]`),
			},
			want: "/payload",
		},
		"one root": {
			native: map[string]json.RawMessage{
				"cwd":             json.RawMessage(`""`),
				"workspace_roots": json.RawMessage(`["/root"]`),
			},
			want: "/root",
		},
		"absent workspace_roots": {
			native: map[string]json.RawMessage{"cwd": json.RawMessage(`""`)},
			want:   "",
		},
		"empty workspace_roots": {
			native: map[string]json.RawMessage{
				"cwd":             json.RawMessage(`""`),
				"workspace_roots": json.RawMessage(`[]`),
			},
			want: "",
		},
		"multi-root still index 0": {
			native: map[string]json.RawMessage{
				"cwd":             json.RawMessage(`""`),
				"workspace_roots": json.RawMessage(`["/first","/second"]`),
			},
			want: "/first",
		},
		"workspace_roots not an array of strings": {
			native: map[string]json.RawMessage{
				"cwd":             json.RawMessage(`""`),
				"workspace_roots": json.RawMessage(`"not-an-array"`),
			},
			want: "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := cwd(tt.native); got != tt.want {
				t.Errorf("cwd() = %q, want %q", got, tt.want)
			}
		})
	}
}

// synthesizeShell must not fire when the top-level command is absent or
// empty: a synthesized tool_name with nothing to inspect can make a guard
// ALLOW, which is worse than an empty tool_name making it abstain.
func TestNoShellSynthesisWithoutACommand(t *testing.T) {
	tests := map[string]map[string]json.RawMessage{
		"absent command": {
			"hook_event_name": json.RawMessage(`"beforeShellExecution"`),
		},
		"empty command": {
			"hook_event_name": json.RawMessage(`"beforeShellExecution"`),
			"command":         json.RawMessage(`""`),
		},
	}
	for name, native := range tests {
		t.Run(name, func(t *testing.T) {
			env := From(vocab.Cursor, native)
			if env.ToolName != "" {
				t.Errorf("ToolName = %q, want \"\"", env.ToolName)
			}
			if env.ToolInput != nil {
				t.Errorf("ToolInput = %s, want nil", env.ToolInput)
			}
		})
	}
}

// native retains the whole raw payload, including fields no top-level field
// surfaces, so a handler that needs one can still read it.
func TestNativeRetainsEngineOnlyFields(t *testing.T) {
	env := decodeFixture(t, "cursor-preToolUse.json")
	if _, ok := env.Native["workspace_roots"]; !ok {
		t.Error("Native should retain workspace_roots")
	}

	env = decodeFixture(t, "codex-pre_tool_use.json")
	if _, ok := env.Native["model"]; !ok {
		t.Error("Native should retain model")
	}

	env = decodeFixture(t, "claude-PreToolUse.json")
	if _, ok := env.Native["transcript_path"]; !ok {
		t.Error("Native should retain transcript_path")
	}
}

// TestMaxPayloadIsOneMebibyte pins maxPayload to the literal value §4 fixes
// the inbound cap at. TestDecodeAcceptsExactlyAtCap and
// TestDecodeRejectsOneByteOverCap both derive their input size from
// maxPayload itself, so they only pin "there is a consistent boundary
// somewhere" — they'd pass unchanged even if maxPayload silently drifted
// from 1 MiB.
func TestMaxPayloadIsOneMebibyte(t *testing.T) {
	if maxPayload != 1<<20 {
		t.Errorf("maxPayload = %d, want %d (1 MiB, per §4)", maxPayload, 1<<20)
	}
}

func TestDecodeAcceptsExactlyAtCap(t *testing.T) {
	payload := paddedPayload(t, maxPayload)
	env, err := Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Decode at exactly the cap: %v", err)
	}
	if env.Engine != vocab.ClaudeCode {
		t.Errorf("Engine = %q, want claude-code", env.Engine)
	}
}

func TestDecodeRejectsOneByteOverCap(t *testing.T) {
	payload := paddedPayload(t, maxPayload+1)
	_, err := Decode(bytes.NewReader(payload))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Decode one byte over the cap: got %v, want ErrTooLarge", err)
	}
}

// paddedPayload reads a real fixture and pads it with trailing whitespace to
// exactly n bytes. Padding is built in memory rather than committed as a
// testdata file: the repo's trim-trailing-whitespace pre-commit hook would
// strip trailing whitespace from a committed file and silently move the
// boundary. Whitespace keeps the payload valid JSON, so the test exercises
// the cap itself rather than failing on unrelated malformed JSON.
func paddedPayload(t *testing.T, n int) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "claude-PreToolUse.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > n {
		t.Fatalf("fixture is %d bytes, longer than the requested %d", len(raw), n)
	}
	padded := make([]byte, n)
	copy(padded, raw)
	for i := len(raw); i < n; i++ {
		padded[i] = ' '
	}
	return padded
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	_, err := Decode(bytes.NewReader([]byte(`{not valid`)))
	if err == nil {
		t.Fatal("want an error for malformed JSON")
	}
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrUnknownEngine) {
		t.Errorf("malformed JSON should match neither sentinel, got %v", err)
	}
}

func TestDecodeRejectsNonObjectJSON(t *testing.T) {
	for _, input := range []string{"[]", `"x"`, "null", "42", "true"} {
		t.Run(input, func(t *testing.T) {
			_, err := Decode(bytes.NewReader([]byte(input)))
			if err == nil {
				t.Fatalf("want an error for %s", input)
			}
			if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrUnknownEngine) {
				t.Errorf("%s should match neither sentinel, got %v", input, err)
			}
		})
	}
}

func decodeFixture(t *testing.T, name string) *Envelope {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	env, err := Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func readNative(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]json.RawMessage
	if err := json.Unmarshal(raw, &native); err != nil {
		t.Fatal(err)
	}
	return native
}
