// Package installstate holds the identity contract shared by three parties
// that must otherwise agree without a compiler to check them: nix/hm-module.nix
// writes a witness describing what the current home-manager generation
// expects `hookyard install` to have produced; cmd/hookyard's install writes a
// receipt recording what it actually did; internal/doctor compares the two to
// catch drift between them (#59). It is a package of its own because
// internal/record's doc scopes that package to the §6 event stream ("That is
// the entire contract"), internal/manifest owns handler manifests rather than
// activation state, and doctor must not own the state install writes — a
// checker that also owned the thing it checks could never disagree with it.
package installstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/noamsto/hookyard/internal/atomicfile"
)

// Schema is the identity contract's version. Diff short-circuits on a
// mismatch: a witness and receipt written by two schema versions have nothing
// meaningful to compare field-by-field.
const Schema = 1

// Identity is the set of paths and inputs a home-manager generation and an
// install run must agree on. Field names are a cross-language contract with
// nix/hm-module.nix's JSON output — do not rename any of them.
type Identity struct {
	Manifests   []string `json:"manifests"`
	RouterPath  string   `json:"routerPath"`
	StateDir    string   `json:"stateDir"`
	CodexConfig string   `json:"codexConfig"`
	CursorHooks string   `json:"cursorHooks"`
	PiSettings  string   `json:"piSettings"`
	Hookyard    string   `json:"hookyard"`
}

// Receipt is what `hookyard install` writes after a run: the identity it
// acted on, and whether it ran to completion. Identity is embedded so its
// fields marshal inline at the top level, alongside schema and complete.
type Receipt struct {
	Schema   int  `json:"schema"`
	Complete bool `json:"complete"`
	Identity
}

// Witness is what nix/hm-module.nix writes: the identity the current
// home-manager generation expects install to have produced.
type Witness struct {
	Schema int `json:"schema"`
	Identity
}

// ReceiptPath is where install's receipt lives within stateDir.
func ReceiptPath(stateDir string) string {
	return filepath.Join(stateDir, "install.json")
}

// WriteReceipt writes r to ReceiptPath(stateDir) at a fixed 0600: the receipt
// is hookyard's own file, so a pre-existing one left looser by something else
// is tightened rather than honoured, the same reasoning manifest.WriteTable
// applies to the handler table.
func WriteReceipt(stateDir string, r Receipt) error {
	r.Schema = Schema
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicfile.Write(ReceiptPath(stateDir), raw, 0o600)
}

// ReadReceipt reads the receipt WriteReceipt produces. A missing file's error
// is returned unwrapped so a caller can tell errors.Is(err, fs.ErrNotExist)
// apart from a corrupt file: "never installed" is a different answer from
// "install ran and left something unreadable", the same distinction
// internal/doctor's tableHandlers already draws for the handler table.
func ReadReceipt(stateDir string) (Receipt, error) {
	path := ReceiptPath(stateDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// ReadWitness reads the witness nix/hm-module.nix writes at path. Unlike the
// receipt, a witness is not in stateDir — it names its generation, not
// hookyard's install output — so the caller supplies the full path.
func ReadWitness(path string) (Witness, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Witness{}, err
	}
	var w Witness
	if err := json.Unmarshal(raw, &w); err != nil {
		return Witness{}, fmt.Errorf("%s: %w", path, err)
	}
	return w, nil
}

// Diff reports, in a fixed order, the labels of every field where w and r
// disagree. A schema mismatch short-circuits to ["schema"] alone: comparing
// the rest across schema versions is meaningless, since a later schema may
// have changed what a field means.
func Diff(w Witness, r Receipt) []string {
	if w.Schema != r.Schema {
		return []string{"schema"}
	}
	var diffs []string
	if !slices.Equal(w.Manifests, r.Manifests) {
		diffs = append(diffs, "manifest set")
	}
	if w.RouterPath != r.RouterPath {
		diffs = append(diffs, "router path")
	}
	if w.StateDir != r.StateDir {
		diffs = append(diffs, "state dir")
	}
	if w.CodexConfig != r.CodexConfig {
		diffs = append(diffs, "codex config")
	}
	if w.CursorHooks != r.CursorHooks {
		diffs = append(diffs, "cursor hooks")
	}
	if w.PiSettings != r.PiSettings {
		diffs = append(diffs, "pi settings")
	}
	if w.Hookyard != r.Hookyard {
		diffs = append(diffs, "hookyard binary")
	}
	return diffs
}
