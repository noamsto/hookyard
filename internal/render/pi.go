package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/hookyard/internal/atomicfile"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
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
	// PiVersion is envelope.Detect's only Pi discriminator, and Pi exposes no
	// version accessor to an extension. Yard mode resolves it at install time;
	// a built package has no install to ask, so it ships this empty and the
	// bridge falls back to reading Pi's own package.json, then to "unknown".
	PiVersion string `json:"pi_version"`
	// Root is the package root as a path relative to the bridge file, which the
	// bridge resolves against import.meta.url at load. nil in yard mode. It is
	// how a built package carries no install path at all in its bytes.
	Root    *string         `json:"root"`
	Entries []piBridgeEntry `json:"entries"`
	// Commands is always an array: the bridge iterates it, and a nil slice
	// would marshal to null and throw there.
	Commands []piBridgeCommand `json:"commands"`
}

// piBridgeEntry mirrors Entry, except that the invocation arrives pre-split
// into the argv execFile takes. Nothing downstream of here parses a path: the
// bridge splits nothing, so a built package may live under a path containing
// spaces even though yard mode's own paths may not.
type piBridgeEntry struct {
	Event   string   `json:"event"`
	Matcher string   `json:"matcher"`
	Bin     string   `json:"bin"`
	Args    []string `json:"args"`
}

// piBridgeCommand is one pi.registerCommand registration. Args is empty because
// a command's only argument is the string Pi hands the handler when a user
// invokes it, which exists only at invocation time.
type piBridgeCommand struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Bin         string   `json:"bin"`
	Args        []string `json:"args"`
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
//
// settings.json is Pi's own file — Pi rewrites it itself at runtime, so
// nothing else may take an atomic rename over it without a reason. A zero-entry
// plan with no marker rows to strip has nothing to add and nothing to remove,
// so it takes no rename at all and leaves the file exactly as Pi last wrote it.
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
	stripped := false
	for _, entry := range extensions {
		if strings.Contains(entry, Marker) {
			stripped = true
			continue
		}
		kept = append(kept, entry)
	}
	extensions = kept

	bridge := PiBridgePath(settingsPath)
	// A prior install can be interrupted between stripping the marker from
	// extensions and removing the bridge it named (crash, disk full, kill
	// -9), leaving an orphaned bridge with no matching entry — which
	// !stripped alone would not see.
	if len(entries) == 0 && !stripped {
		if _, err := os.Stat(bridge); os.IsNotExist(err) {
			return nil
		}
	}
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
	source, err := piBridgeSource(piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		PiVersion: piVersion,
		Entries:   piBridgeEntries(entries),
		Commands:  []piBridgeCommand{},
	})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, source, fileMode(path))
}

// PiPluginBridge renders the extension a built Pi package ships. It is the
// sibling of PluginPlan rather than a row in pluginRootVar because Pi needs no
// plugin-root variable to interpolate: its invocation travels as argv, and the
// bridge resolves its own root and appends --plugin-root itself, so a package's
// bytes name no install.
func PiPluginBridge(handlers []manifest.Handler, commands []manifest.Command) ([]byte, error) {
	plan, err := buildPlan(handlers, func(engine vocab.Engine, event string) string {
		return fmt.Sprintf("%s route --registered-for %s --event %s", PluginLauncher, engine, event)
	})
	if err != nil {
		return nil, err
	}
	// The bridge is written to <root>/extensions/hookyard.ts, so the root is one
	// level up from the directory import.meta.url resolves to.
	root := ".."
	rendered := make([]piBridgeCommand, 0, len(commands))
	for _, c := range commands {
		// Exec is already plugin-root-relative, so the bridge resolves it
		// against the same base it resolves an entry's Bin against.
		rendered = append(rendered, piBridgeCommand{
			Name:        c.Name,
			Description: c.Description,
			Bin:         c.Exec,
			Args:        []string{},
		})
	}
	return piBridgeSource(piBridgeData{
		TimeoutMS: EmittedTimeoutSeconds * 1000,
		Root:      &root,
		Entries:   piBridgeEntries(plan[vocab.Pi]),
		Commands:  rendered,
	})
}

// piBridgeEntries splits each rendered command back into argv. command() joins
// its tokens with single spaces, and checkRouterPath and checkStateDir (both
// in this package) keep whitespace out of the two tokens that could otherwise
// be split wrong, so this is that join's exact inverse for a BuildPlan-produced
// entry. checkShellSafe is a separate, cmd/hookyard CLI-layer guard that
// additionally screens shell metacharacters before a value ever reaches this
// package — it still exists, just not as the guard this invariant leans on.
// Doing the split here rather than in the bridge is what leaves the bridge
// with no path parsing at all, since build-mode entries never pass through
// either guard.
func piBridgeEntries(entries []Entry) []piBridgeEntry {
	out := make([]piBridgeEntry, 0, len(entries))
	for _, e := range entries {
		argv := strings.Split(e.Command, " ")
		out = append(out, piBridgeEntry{
			Event:   e.Event,
			Matcher: e.Matcher,
			Bin:     argv[0],
			Args:    argv[1:],
		})
	}
	return out
}

// piBridgeSource fills the template's one splice point. One Replace, count 1,
// of a token the template carries exactly once, with json.Marshal output. This
// is the only way any install-specific value enters a file pi executes:
// nothing is ever concatenated into that source.
func piBridgeSource(data piBridgeData) ([]byte, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Replace(piBridgeTemplate, piBridgeSplice, string(encoded), 1)), nil
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
