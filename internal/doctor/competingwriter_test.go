package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/render"
)

// aeyeScript returns the handler shape every non-empty-table fixture here uses:
// fully valid, so WriteTable→ReadTable round-trips without validateStatic
// refusing it (empty events/engines would make ReadTable return an error and
// silently hide the check under test).
func aeyeScript(id, exec string) manifest.Handler {
	return manifest.Handler{
		ID:      id,
		Exec:    exec,
		Events:  []string{"post_tool"},
		Engines: []string{"cursor"},
	}
}

func writeTable(t *testing.T, stateDir string, handlers []manifest.Handler) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manifest.WriteTable(filepath.Join(stateDir, "table.json"), handlers); err != nil {
		t.Fatal(err)
	}
}

// hookyardRow is one of hookyard's own emitted rows, built from render.Marker
// so the fixture stays correct if the marker string ever moves.
var hookyardRow = `"/opt` + render.Marker + ` route --registered-for cursor --event post_tool --state-dir /state"`

func writeCursorHooks(t *testing.T, path string, hookRows ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	objects := make([]string, 0, len(hookRows))
	for _, r := range hookRows {
		objects = append(objects, `{"command":`+r+`}`)
	}
	raw := `{"version":1,"hooks":{"postToolUse":[` + strings.Join(objects, ",") + `]}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompetingWriterFailsOnForeignRowNamingAHandler(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	foreign := "/nix/store/y/adapters/cursor/scripts/images.sh"
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("aeye/images", "/nix/store/x/adapters/cursor/scripts/images.sh")})

	hooks := filepath.Join(root, "cursor", "hooks.json")
	writeCursorHooks(t, hooks, hookyardRow, `"`+foreign+`"`)

	f := competingWriter(stateDir, hooks)
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "aeye/images") {
		t.Errorf("detail = %q, want it to name the handler id", f.Detail)
	}
	if !strings.Contains(f.Detail, foreign) {
		t.Errorf("detail = %q, want it to name the foreign command", f.Detail)
	}
}

func TestCompetingWriterPassesOnDisjointForeignRows(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("aeye/images", "/nix/store/x/adapters/cursor/scripts/images.sh")})

	hooks := filepath.Join(root, "cursor", "hooks.json")
	// A foreign writer's row for a different handler — lazytmux's marker — must
	// not be read as a double against hookyard's images.sh.
	writeCursorHooks(t, hooks, hookyardRow, `"/bin/cursor-status-hook --tick"`)

	f := competingWriter(stateDir, hooks)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
}

func TestCompetingWriterPassesOnEmptyTable(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	writeTable(t, stateDir, nil)

	hooks := filepath.Join(root, "cursor", "hooks.json")
	writeCursorHooks(t, hooks, hookyardRow, `"/nix/store/y/adapters/cursor/scripts/images.sh"`)

	f := competingWriter(stateDir, hooks)
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "no handlers in the table") {
		t.Errorf("detail = %q, want the zero-handler wording", f.Detail)
	}
}

func TestCompetingWriterUnknownWithoutHookyardRow(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	writeTable(t, stateDir, []manifest.Handler{aeyeScript("aeye/images", "/nix/store/x/adapters/cursor/scripts/images.sh")})

	// No hookyard-marker row: a foreign row naming hookyard's script is not a
	// double until hookyard is also registered, which registration() reports.
	hooks := filepath.Join(root, "cursor", "hooks.json")
	writeCursorHooks(t, hooks, `"/nix/store/y/adapters/cursor/scripts/images.sh"`)

	f := competingWriter(stateDir, hooks)
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "no hookyard entry") {
		t.Errorf("detail = %q, want the defer wording", f.Detail)
	}
}

func TestCompetingWriterUnknownWithoutStateDir(t *testing.T) {
	root := t.TempDir()
	hooks := filepath.Join(root, "cursor", "hooks.json")
	writeCursorHooks(t, hooks, hookyardRow)

	// stateDir is empty, so the guard fires before the file is even read; a
	// missing hooks file is fine here because the check never reaches it.
	f := competingWriter("", filepath.Join(root, "never-exists.json"))
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "no --state-dir") {
		t.Errorf("detail = %q, want the state-dir wording", f.Detail)
	}
}

func TestCompetingWriterUnknownOnMissingTable(t *testing.T) {
	root := t.TempDir()
	// stateDir is set but table.json was never written: a missing table is not
	// an empty one, and the check must not report Pass as if hookyard owned
	// nothing.
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	hooks := filepath.Join(root, "cursor", "hooks.json")
	writeCursorHooks(t, hooks, hookyardRow)

	f := competingWriter(stateDir, hooks)
	if f.Status != Unknown {
		t.Fatalf("status = %v, want Unknown; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "cannot read the handler table") {
		t.Errorf("detail = %q, want the unreadable-table wording", f.Detail)
	}
}
