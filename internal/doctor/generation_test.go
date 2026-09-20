package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/installstate"
)

func generationFinding(t *testing.T, findings []Finding) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Check == "generation" {
			return f
		}
	}
	t.Fatal("no generation finding in Run's output")
	return Finding{}
}

// testPaths points every engine doctor inspects at an empty temp dir, so
// each case below only has to set up the witness/receipt fields it actually
// cares about. StateDir is always explicit: leaving it empty would send Run
// through recoverStateDir to record.DefaultStateDir(), i.e. this machine's
// real ~/.local/state/hookyard.
func testPaths(t *testing.T, witness, stateDir string) Paths {
	t.Helper()
	return Paths{
		ClaudeConfigDir:   t.TempDir(),
		CodexHome:         t.TempDir(),
		CursorHome:        t.TempDir(),
		PiAgentDir:        t.TempDir(),
		StateDir:          stateDir,
		GenerationWitness: witness,
	}
}

func writeWitness(t *testing.T, path string, w installstate.Witness) {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func baseIdentity(stateDir string) installstate.Identity {
	return installstate.Identity{
		Manifests:   []string{"manifest-a"},
		RouterPath:  "/bin/hookyard",
		StateDir:    stateDir,
		CodexConfig: "/codex/config.toml",
		CursorHooks: "/cursor/hooks.json",
		PiSettings:  []string{"/pi/settings.json"},
		Hookyard:    "/nix/store/aaa-hookyard/bin/hookyard",
	}
}

func assertNotStale(t *testing.T, detail string) {
	t.Helper()
	if strings.Contains(detail, "stale") {
		t.Errorf("detail = %q, must not call this drift \"stale\"", detail)
	}
}

func TestGenerationNoWitnessFile(t *testing.T) {
	root := t.TempDir()
	p := testPaths(t, filepath.Join(root, "generation.json"), filepath.Join(root, "state"))

	f := generationFinding(t, Run(p, root))
	if f.Status != Unknown {
		t.Errorf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

func TestGenerationMalformedWitness(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	if err := os.WriteFile(witness, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := testPaths(t, witness, filepath.Join(root, "state"))

	f := generationFinding(t, Run(p, root))
	if f.Status != Unknown {
		t.Errorf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
}

func TestGenerationWitnessNoReceipt(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	stateDir := filepath.Join(root, "state")
	writeWitness(t, witness, installstate.Witness{Schema: installstate.Schema, Identity: baseIdentity(stateDir)})
	p := testPaths(t, witness, stateDir)

	f := generationFinding(t, Run(p, root))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, stateDir) {
		t.Errorf("detail = %q, want it to name the state dir %q", f.Detail, stateDir)
	}
	assertNotStale(t, f.Detail)
}

func TestGenerationReceiptIncomplete(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	stateDir := filepath.Join(root, "state")
	identity := baseIdentity(stateDir)
	writeWitness(t, witness, installstate.Witness{Schema: installstate.Schema, Identity: identity})
	if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: false, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	p := testPaths(t, witness, stateDir)

	f := generationFinding(t, Run(p, root))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	assertNotStale(t, f.Detail)
}

// TestGenerationStaleWitness is the headline case #59 exists for:
// activation aborted somewhere between linkGeneration and hookyard install
// reaching this generation's manifests, so the witness names a manifest set
// install never saw. The receipt is fully finalised (Complete == true) for
// the *previous* generation — a self-consistent machine on its own terms
// that must still fail this check, because the current generation no longer
// matches what was installed.
func TestGenerationStaleWitness(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	stateDir := filepath.Join(root, "state")

	witnessIdentity := baseIdentity(stateDir)
	witnessIdentity.Manifests = []string{"manifest-new"}
	writeWitness(t, witness, installstate.Witness{Schema: installstate.Schema, Identity: witnessIdentity})

	receiptIdentity := baseIdentity(stateDir)
	receiptIdentity.Manifests = []string{"manifest-old"}
	if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: true, Identity: receiptIdentity}); err != nil {
		t.Fatal(err)
	}
	p := testPaths(t, witness, stateDir)

	f := generationFinding(t, Run(p, root))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "manifest set") {
		t.Errorf("detail = %q, want it to name the manifest set diff", f.Detail)
	}
	assertNotStale(t, f.Detail)
}

func TestGenerationHookyardBinaryDiffers(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	stateDir := filepath.Join(root, "state")

	witnessIdentity := baseIdentity(stateDir)
	witnessIdentity.Hookyard = "/nix/store/aaa-hookyard/bin/hookyard"
	writeWitness(t, witness, installstate.Witness{Schema: installstate.Schema, Identity: witnessIdentity})

	receiptIdentity := baseIdentity(stateDir)
	receiptIdentity.Hookyard = "/nix/store/bbb-hookyard/bin/hookyard"
	if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: true, Identity: receiptIdentity}); err != nil {
		t.Fatal(err)
	}
	p := testPaths(t, witness, stateDir)

	f := generationFinding(t, Run(p, root))
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "hookyard binary") {
		t.Errorf("detail = %q, want it to name the hookyard binary diff", f.Detail)
	}
	assertNotStale(t, f.Detail)
}

func TestGenerationAgreeAndComplete(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "generation.json")
	stateDir := filepath.Join(root, "state")
	identity := baseIdentity(stateDir)
	writeWitness(t, witness, installstate.Witness{Schema: installstate.Schema, Identity: identity})
	if err := installstate.WriteReceipt(stateDir, installstate.Receipt{Complete: true, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	p := testPaths(t, witness, stateDir)

	f := generationFinding(t, Run(p, root))
	if f.Status != Pass {
		t.Errorf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}
