package installstate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func sampleIdentity() Identity {
	return Identity{
		Manifests:   []string{"/a/hookyard.json", "/b/hookyard.json"},
		RouterPath:  "/nix/store/abc/bin/hookyard-router",
		StateDir:    "/home/u/.local/state/hookyard",
		CodexConfig: "/home/u/.codex/config.toml",
		CursorHooks: "/home/u/.cursor/hooks.json",
		PiSettings:  []string{"/home/u/.pi/settings.json"},
		Hookyard:    "/nix/store/abc/bin/hookyard",
	}
}

func TestWriteReceiptReadReceiptRoundTrip(t *testing.T) {
	for _, complete := range []bool{false, true} {
		stateDir := t.TempDir()
		want := Receipt{Complete: complete, Identity: sampleIdentity()}
		if err := WriteReceipt(stateDir, want); err != nil {
			t.Fatal(err)
		}
		got, err := ReadReceipt(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		want.Schema = Schema
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestWriteReceiptModeIs0600(t *testing.T) {
	stateDir := t.TempDir()
	if err := WriteReceipt(stateDir, Receipt{Identity: sampleIdentity()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ReceiptPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("got mode %o, want 0600", got)
	}
}

func TestReadReceiptMissingFileIsErrNotExist(t *testing.T) {
	stateDir := t.TempDir()
	_, err := ReadReceipt(stateDir)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist", err)
	}
}

func TestReadReceiptMalformedJSONIsNotErrNotExist(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(ReceiptPath(stateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadReceipt(stateDir)
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got fs.ErrNotExist for malformed JSON: %v", err)
	}
}

func TestReadWitnessMalformedJSONIsNotErrNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witness.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadWitness(path)
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got fs.ErrNotExist for malformed JSON: %v", err)
	}
}

func TestReadWitnessMissingFileIsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadWitness(filepath.Join(dir, "witness.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist", err)
	}
}

func TestDiffAgreeingPairIsEmpty(t *testing.T) {
	id := sampleIdentity()
	w := Witness{Schema: Schema, Identity: id}
	r := Receipt{Schema: Schema, Complete: true, Identity: id}
	if got := Diff(w, r); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestDiffSchemaMismatchShortCircuits(t *testing.T) {
	id := sampleIdentity()
	w := Witness{Schema: Schema, Identity: id}
	r := Receipt{Schema: Schema + 1, Identity: id}
	got := Diff(w, r)
	want := []string{"schema"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiffPerField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(id *Identity)
		want   string
	}{
		{"manifests", func(id *Identity) { id.Manifests = append(id.Manifests, "/c/hookyard.json") }, "manifest set"},
		{"routerPath", func(id *Identity) { id.RouterPath = "/other/router" }, "router path"},
		{"stateDir", func(id *Identity) { id.StateDir = "/other/state" }, "state dir"},
		{"codexConfig", func(id *Identity) { id.CodexConfig = "/other/codex.toml" }, "codex config"},
		{"cursorHooks", func(id *Identity) { id.CursorHooks = "/other/hooks.json" }, "cursor hooks"},
		{"piSettings", func(id *Identity) { id.PiSettings = []string{"/other/pi.json"} }, "pi settings"},
		{"hookyard", func(id *Identity) { id.Hookyard = "/other/hookyard" }, "hookyard binary"},
		{"claudeSettings", func(id *Identity) { id.ClaudeSettings = "/home/u/.claude/settings.json" }, "claude settings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := Witness{Schema: Schema, Identity: sampleIdentity()}
			rID := sampleIdentity()
			tc.mutate(&rID)
			r := Receipt{Schema: Schema, Identity: rID}
			got := Diff(w, r)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %v, want [%q]", got, tc.want)
			}
		})
	}
}

// A receipt from an install that wrote no Claude settings must marshal
// without the key at all, so a Nix witness — which has never heard of it —
// and the receipt stay the same schema-2 document they were before it existed.
func TestReceiptOmitsUnsetClaudeSettings(t *testing.T) {
	raw, err := json.Marshal(Receipt{Schema: Schema, Identity: sampleIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["claudeSettings"]; ok {
		t.Fatalf("receipt carries claudeSettings with no Claude settings written: %s", raw)
	}
}

func TestDiffReorderedManifestsReportsManifestSet(t *testing.T) {
	id := sampleIdentity()
	w := Witness{Schema: Schema, Identity: id}
	rID := sampleIdentity()
	rID.Manifests = []string{rID.Manifests[1], rID.Manifests[0]}
	r := Receipt{Schema: Schema, Identity: rID}
	got := Diff(w, r)
	if len(got) != 1 || got[0] != "manifest set" {
		t.Fatalf("got %v, want [\"manifest set\"]", got)
	}
}

// TestReceiptFieldsMarshalInline pins the embedding contract: Identity's
// fields sit alongside schema/complete at the top level of the JSON object,
// not nested under an "Identity" key, because nix/hm-module.nix and doctor
// both read them as flat top-level fields.
func TestReceiptFieldsMarshalInline(t *testing.T) {
	raw, err := json.Marshal(Receipt{Schema: Schema, Complete: true, Identity: sampleIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "complete", "manifests", "routerPath", "stateDir", "codexConfig", "cursorHooks", "piSettings", "hookyard"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("marshaled receipt missing top-level key %q: %s", key, raw)
		}
	}
}
