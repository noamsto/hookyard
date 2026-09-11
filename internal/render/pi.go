package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/hookyard/internal/atomicfile"
)

// piBridgeTemplate is the extension hookyard installs for Pi, byte-identical on
// every install. It is embedded rather than generated because it is executable
// code pi loads in-process: source spliced together per install would be
// reviewable nowhere and testable in no CI, and a value that terminated a
// string literal early would execute rather than merely corrupt a config (§8).
//
//go:embed pi_bridge.ts
var piBridgeTemplate string

// piBridgeSplice is the template's one and only splice point, and its
// replacement is json.Marshal output and nothing else. That is the whole of
// what keeps every install-specific value data rather than code.
const piBridgeSplice = "__HOOKYARD_DATA__"

// piBridgeData is that spliced value. It carries the timeout because Pi's
// config has no timeout field and Pi imposes none itself — it awaits the
// handler's promise — so the bridge has to hold the clock.
type piBridgeData struct {
	TimeoutMS int `json:"timeout_ms"`
	// PiVersion is resolved at install time. Pi exposes no version accessor to
	// an extension, and this key is envelope.Detect's only Pi discriminator, so
	// a runtime lookup would yield undefined, JSON.stringify would drop the key
	// and every Pi hook would become a no-op no Go test can see.
	PiVersion string          `json:"pi_version"`
	Entries   []piBridgeEntry `json:"entries"`
}

// piBridgeEntry mirrors Entry, except that Command stays a single string rather
// than a pre-split argv array: doctor recovers the state directory from the
// bridge's raw bytes with a flag-then-whitespace regex an array would break.
// The bridge splits it back on single spaces, which checkShellSafe makes exact.
type piBridgeEntry struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher"`
	Command string `json:"command"`
}

// PiBridgePath is where WritePi puts the bridge: bin/hookyard-bridge.ts beside
// Pi's settings.json. Both halves of that path are load-bearing.
//
// The bin/ segment makes the path contain Marker ("/bin/hookyard"), so Pi's
// registered value — a file path, where the other three engines register a
// command string — is found again by exactly the same strings.Contains strip
// the other writers use, and by doctor's raw-bytes grep, with no Pi-only
// constant and no basename rule anywhere.
//
// It sits outside <dir>/extensions/, and the reason is uninstall rather than
// double-firing: pi de-duplicates, but a bridge under extensions/ would also
// be auto-discovered, so stripping the extensions[] entry would leave it
// running and the config strip would no longer be the uninstall it is on the
// other three engines.
func PiBridgePath(settingsPath string) string {
	return filepath.Join(filepath.Dir(settingsPath), "bin", "hookyard-bridge.ts")
}

// WritePi renders entries into Pi's settings.json and the bridge it names.
//
// Pi has no subprocess hook protocol, so hookyard registers a file rather than
// a command and the hook itself is the bridge's JavaScript (§8). That makes
// this the one writer that lands two artifacts, and their order is the whole of
// what keeps a half-finished install harmless: the bridge is written before the
// entry naming it, and on removal the entry goes before the file it named, so
// pi never reads an extensions[] entry pointing at something this writer is
// still creating.
func WritePi(settingsPath string, entries []Entry, piVersion string) error {
	raw, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	root, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON, refusing to overwrite it: %w", settingsPath, err)
	}

	var extensions []string
	if existing, ok := root.get("extensions"); ok {
		if err := json.Unmarshal(existing, &extensions); err != nil {
			return fmt.Errorf("%s has an extensions key hookyard cannot read, refusing to overwrite it: %w",
				settingsPath, err)
		}
	}
	// Contains and nothing more. A basename or exact-path clause would either
	// strip a foreign extension that merely happens to be called
	// hookyard-bridge.ts, or fail to strip a stale entry left under bin/ by an
	// older bridge filename — which Contains already catches.
	kept := extensions[:0]
	for _, entry := range extensions {
		if !strings.Contains(entry, Marker) {
			kept = append(kept, entry)
		}
	}
	extensions = kept

	bridge := PiBridgePath(settingsPath)
	if len(entries) > 0 {
		if err := writePiBridge(bridge, entries, piVersion); err != nil {
			return err
		}
		extensions = append(extensions, bridge)
	}
	if len(extensions) == 0 {
		root.delete("extensions")
	} else if err := root.set("extensions", extensions); err != nil {
		return err
	}
	out, err := root.marshalIndent()
	if err != nil {
		return err
	}
	if err := atomicfile.Write(settingsPath, out, fileMode(settingsPath)); err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	// An empty plan is --allow-empty, which exists so the home-manager module
	// can render the removed state. Leaving the bridge behind would leave
	// executable code on disk that removal did not remove — so the file goes
	// too, after the entry that named it.
	if err := os.Remove(bridge); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writePiBridge(path string, entries []Entry, piVersion string) error {
	data := piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		PiVersion: piVersion,
		Entries:   make([]piBridgeEntry, 0, len(entries)),
	}
	for _, e := range entries {
		data.Entries = append(data.Entries, piBridgeEntry(e))
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	// One Replace, count 1, of a token the template carries exactly once, with
	// json.Marshal output. This is the only way any install-specific value
	// enters a file pi executes: nothing is ever concatenated into that source.
	source := strings.Replace(piBridgeTemplate, piBridgeSplice, string(encoded), 1)
	return atomicfile.Write(path, []byte(source), fileMode(path))
}

// fileMode is the perm policy both Pi artifacts share with the other writers:
// these are files render does not own, so a pre-existing one keeps its bits
// and one hookyard creates starts at 0600.
func fileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}
