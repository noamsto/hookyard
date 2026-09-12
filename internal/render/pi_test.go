package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

// The bridge authors Pi's inbound payload — pi sends none — so the committed
// captures are the contract rather than a sample of one, and this test is what
// keeps them from drifting apart in the direction nothing else would notice.
const piFixtureDir = "../../docs/design/fixtures/hook-payloads"

func TestPiBridgeTemplateMentionsEveryFixturePayloadKey(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(piFixtureDir, "pi-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no pi-*.json fixtures under %s; the payload contract has nothing pinning it", piFixtureDir)
	}

	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixture map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for key, value := range fixture {
			assertTemplateMentions(t, path, key)
			// The recursion stops one level in, and only under tool_response:
			// its content/is_error spelling is hookyard's, while tool_input and
			// the content entries themselves are copied verbatim out of pi and
			// are not this file's shape to keep.
			if key != "tool_response" {
				continue
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(value, &response); err != nil {
				t.Fatalf("%s: tool_response: %v", path, err)
			}
			for inner := range response {
				assertTemplateMentions(t, path, inner)
			}
		}
	}
}

func assertTemplateMentions(t *testing.T, path, key string) {
	t.Helper()
	if !strings.Contains(piBridgeTemplate, key) {
		t.Errorf("%s carries payload key %q that the bridge template never mentions", path, key)
	}
}

// doctor recovers the router path and the state directory by scanning the
// bridge's raw bytes, so a marker-bearing string anywhere in the template —
// including in a comment — would be reported as a second, non-runnable router,
// and a bare state-dir flag followed by whitespace would hand doctor a state
// directory nothing writes to. Both are silent failures in the tool nominated
// to catch silent failures.
func TestPiBridgeTemplateCarriesNoStringDoctorWouldRecover(t *testing.T) {
	if strings.Contains(piBridgeTemplate, Marker) {
		t.Errorf("bridge template contains %q; doctor would report it as a second router", Marker)
	}
	if strings.Contains(piBridgeTemplate, "--state-dir") {
		t.Error("bridge template spells the state-dir flag; doctor would recover whatever text follows it")
	}
}

// The one splice point is a property of the template, not of the writer that
// fills it: a second one would mean some other value reaching the file by
// concatenation.
func TestPiBridgeTemplateHasExactlyOneSplicePoint(t *testing.T) {
	if got := strings.Count(piBridgeTemplate, piBridgeSplice); got != 1 {
		t.Errorf("bridge template has %d splice points, want exactly 1", got)
	}
}

// The JSON tags and the template's accessors are the two halves of one
// contract, written in two languages that no compiler checks against each
// other. Renaming either half alone turns this red instead of turning every Pi
// hook into a silent no-op.
func TestPiBridgeDataMatchesWhatTheTemplateReads(t *testing.T) {
	raw, err := json.Marshal(piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		PiVersion: "0.85.1",
		Entries:   []piBridgeEntry{{Event: "tool_call", Matcher: "bash", Command: "hookyard route"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"timeout_ms"`, `"pi_version"`, `"entries"`, `"event"`, `"matcher"`, `"command"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the data constant marshals without %s", key)
		}
	}
	for _, access := range []string{"DATA.timeout_ms", "DATA.pi_version", "DATA.entries", "entry.event", "entry.matcher", "entry.command"} {
		if !strings.Contains(piBridgeTemplate, access) {
			t.Errorf("bridge template never reads %s", access)
		}
	}
}

// The bin/ segment is not decoration: it is what makes Pi's registered value —
// a file path, where the other three engines register a command string —
// findable by the same marker every other writer and doctor already use. Lose
// it and the strip needs a Pi-only constant nothing else would keep in sync.
func TestPiBridgePathCarriesTheMarker(t *testing.T) {
	got := PiBridgePath("/home/noams/.pi/agent/settings.json")
	if !strings.Contains(got, Marker) {
		t.Errorf("PiBridgePath = %q, which does not contain the marker %q", got, Marker)
	}
}

// Inside extensions/ the bridge would also be auto-discovered, so stripping the
// registration would leave it running: the config strip would stop being the
// uninstall it is on the other three engines.
func TestPiBridgePathIsOutsideTheExtensionsDirectory(t *testing.T) {
	dir := "/home/noams/.pi/agent"
	got := PiBridgePath(filepath.Join(dir, "settings.json"))
	if strings.HasPrefix(got, filepath.Join(dir, "extensions")+string(filepath.Separator)) {
		t.Errorf("PiBridgePath = %q, which is under %s/extensions", got, dir)
	}
}

func piSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// piExtensions is what pi would read back: the registered list, in order.
func piExtensions(t *testing.T, settingsPath string) []string {
	t.Helper()
	var settings struct {
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(readFile(t, settingsPath)), &settings); err != nil {
		t.Fatalf("%s: %v", settingsPath, err)
	}
	return settings.Extensions
}

// The strip is strings.Contains(entry, Marker) and nothing more. Both rows
// below are what that buys, and each would be broken by an "obvious" fix: a
// basename rule would eat the foreign entry, and an exact-path rule would leave
// the stale one firing forever, owned by nobody.
func TestWritePiStripsByTheMarkerAndNothingElse(t *testing.T) {
	// The foreign path is spelled out because the point is its shape: the same
	// basename as hookyard's own bridge, at a path with no /bin/hookyard in it.
	const foreign = "/home/noams/.pi/agent/extensions/hookyard-bridge.ts"
	const stale = "/home/noams/.pi/agent/bin/hookyard-bridge-v1.ts"
	path := piSettings(t, `{"extensions":["`+foreign+`","`+stale+`"]}`)

	if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err != nil {
		t.Fatal(err)
	}

	got := piExtensions(t, path)
	want := []string{foreign, PiBridgePath(path)}
	if len(got) != len(want) {
		t.Fatalf("extensions = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extensions[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// An extensions value this writer cannot read is a file it does not understand,
// and §8's rule for those is to refuse rather than clobber — here doubly so,
// since rewriting it would silently unregister whatever pi was loading.
func TestWritePiRefusesAnExtensionsValueItCannotRead(t *testing.T) {
	for name, content := range map[string]string{
		"an object":           `{"extensions":{"a":"b"}}`,
		"an array of objects": `{"extensions":[{"path":"/x.ts"}]}`,
		"an array of numbers": `{"extensions":[1,2]}`,
		"a bare string":       `{"extensions":"/x.ts"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := piSettings(t, content)
			before := readFile(t, path)
			if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if got := readFile(t, path); got != before {
				t.Error("the refusal still modified the file")
			}
			if _, err := os.Stat(PiBridgePath(path)); !os.IsNotExist(err) {
				t.Errorf("want no bridge written by the refused install, got stat err: %v", err)
			}
		})
	}
}

// --allow-empty exists so the home-manager module can render the removed state.
// A surviving bridge would mean removal did not remove: the entry would be gone
// while executable code stayed on disk, ready for anything that re-registers it.
func TestWritePiOnAnEmptyPlanStripsTheEntryAndDeletesTheBridge(t *testing.T) {
	path := piSettings(t, `{"model":"kimi-k2"}`)
	if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	bridge := PiBridgePath(path)
	if _, err := os.Stat(bridge); err != nil {
		t.Fatalf("setup: the first install wrote no bridge: %v", err)
	}

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, path)
	if strings.Contains(got, "hookyard") {
		t.Errorf("a hookyard registration survived the empty plan\n--- got ---\n%s", got)
	}
	// Dropped rather than left as [], the way WriteCursor drops an emptied
	// hooks key: hookyard adding a key pi never had is a diff it does not own.
	if strings.Contains(got, `"extensions"`) {
		t.Errorf("the emptied extensions key survived\n--- got ---\n%s", got)
	}
	if !strings.Contains(got, `"model"`) {
		t.Errorf("the empty plan dropped an inherited key\n--- got ---\n%s", got)
	}
	if _, err := os.Stat(bridge); !os.IsNotExist(err) {
		t.Errorf("want the bridge deleted, got stat err: %v", err)
	}
}

// An empty plan that leaves another writer's extension behind must keep the
// key: dropping it is only correct when the strip is what emptied it.
func TestWritePiOnAnEmptyPlanKeepsForeignExtensions(t *testing.T) {
	const foreign = "/home/noams/.pi/agent/extensions/foreign.ts"
	path := piSettings(t, `{"extensions":["`+foreign+`"]}`)

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	if got := piExtensions(t, path); len(got) != 1 || got[0] != foreign {
		t.Errorf("extensions = %q, want [%q]", got, foreign)
	}
}

// settings.json is Pi's own file: an empty plan with nothing to strip must
// take no rename over it at all, not even one that reproduces the same JSON
// with different formatting.
func TestWritePiOnAnEmptyPlanWithNoMarkerLeavesTheFileByteIdentical(t *testing.T) {
	const content = `{"extensions":  ["/home/noams/.pi/agent/extensions/foreign.ts"],   "model":"kimi-k2"}`
	path := piSettings(t, content)
	before := readFile(t, path)

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("file was rewritten with nothing to add or strip\n--- before ---\n%s\n--- got ---\n%s", before, got)
	}
}

// The first --allow-empty install a machine ever runs has no settings.json at
// all yet. That must not conjure one into existence just to hold an
// extensions key with nothing in it.
func TestWritePiOnAnEmptyPlanWithNoSettingsFileWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("want no settings.json created, got stat err: %v", err)
	}
}

// The skip requires *both* halves of the condition: zero entries is not
// enough on its own when a prior hookyard marker row is still registered,
// since leaving it behind would keep firing against a router that no longer
// wants it.
func TestWritePiOnAnEmptyPlanStillStripsAStaleMarkerEvenWithNoNewEntries(t *testing.T) {
	const stale = "/home/noams/.pi/agent/bin/hookyard-bridge-v1.ts"
	path := piSettings(t, `{"extensions":["`+stale+`"]}`)

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	if got := piExtensions(t, path); len(got) != 0 {
		t.Errorf("extensions = %q, want the stale marker row stripped", got)
	}
}

// A prior install can be interrupted between the settings.json write (marker
// already gone from extensions) and the os.Remove that follows it, leaving an
// orphaned bridge with no entry pointing at it. Nothing in the extensions
// array says so, but the file on disk does, and the skip must not let that
// leftover survive forever.
func TestWritePiOnAnEmptyPlanRemovesAnOrphanedBridgeEvenWithNoMarkerToStrip(t *testing.T) {
	path := piSettings(t, `{"model":"kimi-k2"}`)
	bridge := PiBridgePath(path)
	if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, []byte("// orphaned by a crashed install\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WritePi(path, nil, "0.85.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bridge); !os.IsNotExist(err) {
		t.Errorf("want the orphaned bridge removed, got stat err: %v", err)
	}
}

// Nothing is concatenated into source (§8): the one splice is json.Marshal
// output, so a value that would terminate a string literal, close a comment or
// break a line lands as data no matter how it is spelled. It is asserted on the
// bytes pi would actually load.
func TestWritePiSplicesAHostileValueAsDataNotCode(t *testing.T) {
	hostile := "/x/bin/hookyard route --reason \"quoted\" `backtick` */ end\nconsole.log('pwned')"
	path := piSettings(t, "{}")
	if err := WritePi(path, []Entry{{Event: "tool_call", Matcher: hostile, Command: hostile}}, hostile); err != nil {
		t.Fatal(err)
	}
	source := readFile(t, PiBridgePath(path))

	// Every trace of the value is confined to the one spliced line, and the
	// rest of the file is the reviewed template byte for byte. A value that
	// escaped its string literal would necessarily show up on some other line.
	if got, want := strings.Count(source, "\n"), strings.Count(piBridgeTemplate, "\n"); got != want {
		t.Errorf("the bridge has %d newlines, the template %d: the value broke out of its line", got, want)
	}
	for i, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, "const DATA = ") {
			continue
		}
		if strings.Contains(line, "pwned") || strings.Contains(line, "backtick") {
			t.Errorf("line %d carries the hostile value outside the data constant: %s", i+1, line)
		}
	}
	// It survives as data, exactly: escaping that mangled the value would be a
	// different bug with the same green test.
	var data struct {
		PiVersion string `json:"pi_version"`
		Entries   []struct {
			Matcher string `json:"matcher"`
			Command string `json:"command"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(spliced(t, source)), &data); err != nil {
		t.Fatalf("the spliced constant is not JSON: %v", err)
	}
	if len(data.Entries) != 1 {
		t.Fatalf("want one entry in the constant, got %d", len(data.Entries))
	}
	for name, got := range map[string]string{"pi_version": data.PiVersion, "matcher": data.Entries[0].Matcher, "command": data.Entries[0].Command} {
		if got != hostile {
			t.Errorf("%s = %q, want the value verbatim %q", name, got, hostile)
		}
	}
}

// spliced recovers the data constant from a written bridge by reading the one
// line the splice point sits on in the template, so the assertions above run
// against what pi parses rather than a substring search.
func spliced(t *testing.T, source string) string {
	t.Helper()
	const prefix = "const DATA = "
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), ";")
		}
	}
	t.Fatalf("no %q line in the written bridge:\n%s", prefix, source)
	return ""
}

// Pi's tool column is lowercase (read/write/bash/grep/find), and the bridge
// filters on event.toolName before it spawns anything. A capitalised matcher
// would therefore match nothing, every Pi handler would silently never fire,
// and a deny handler that never fires is a fail-open.
func TestWritePiRendersPiSpellingOfTheMatcher(t *testing.T) {
	plan, err := BuildPlan([]manifest.Handler{
		{ID: "a", Events: []string{vocab.PreTool}, Engines: []string{"pi"}, Match: []string{"Bash"}},
	}, "/nix/store/x/bin/hookyard", "/var/state")
	if err != nil {
		t.Fatal(err)
	}
	path := piSettings(t, "{}")
	if err := WritePi(path, plan[vocab.Pi], "0.85.1"); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, PiBridgePath(path)); !strings.Contains(got, `"matcher":"bash"`) {
		t.Errorf(`the bridge does not carry "matcher":"bash"; a capitalised matcher matches no Pi tool name`+"\n--- got ---\n%s", got)
	}
}

// The bridge is written before the entry that names it, so a bridge write that
// fails leaves no registration at all — rather than a settings file pointing at
// a file pi would load if anything ever created it.
func TestWritePiWritesNoEntryWhenTheBridgeCannotBeWritten(t *testing.T) {
	path := piSettings(t, "{}")
	// bin/ as a regular file: the bridge's directory cannot be created.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "bin"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WritePi(path, []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}, "0.85.1"); err == nil {
		t.Fatal("want an error from the unwritable bridge, got nil")
	}
	if got := readFile(t, path); strings.Contains(got, "hookyard") {
		t.Errorf("the failed bridge write still left a registration behind\n--- got ---\n%s", got)
	}
}

// Both artifacts follow the perm policy of files hookyard does not own;
// mode_test.go pins the settings file, this pins the bridge.
func TestWritePiBridgeMode(t *testing.T) {
	entries := []Entry{{Event: "tool_call", Command: "/x/bin/hookyard route"}}

	t.Run("lands 0600 on a bridge it creates", func(t *testing.T) {
		path := piSettings(t, "{}")
		if err := WritePi(path, entries, "0.85.1"); err != nil {
			t.Fatal(err)
		}
		if mode := mustMode(t, PiBridgePath(path)); mode != 0o600 {
			t.Errorf("mode = %v, want 0600", mode)
		}
	})

	t.Run("preserves an existing bridge's mode", func(t *testing.T) {
		path := piSettings(t, "{}")
		bridge := PiBridgePath(path)
		if err := os.MkdirAll(filepath.Dir(bridge), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bridge, []byte("// stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WritePi(path, entries, "0.85.1"); err != nil {
			t.Fatal(err)
		}
		if mode := mustMode(t, bridge); mode != 0o644 {
			t.Errorf("mode = %v, want 0644 (the pre-existing bridge's mode)", mode)
		}
	})
}
