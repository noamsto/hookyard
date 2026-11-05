package loadercheck

import (
	"debug/elf"
	"debug/macho"
	"os"
	"runtime"
	"testing"
)

func TestLoaderHardening(t *testing.T) {
	bin := os.Getenv("PRIORS_BIN")
	if bin == "" {
		t.Skip("PRIORS_BIN not set")
	}

	switch runtime.GOOS {
	case "linux":
		checkELF(t, bin)
	case "darwin":
		checkMachO(t, bin)
	default:
		t.Skipf("no loader check for %s", runtime.GOOS)
	}
}

func checkELF(t *testing.T, bin string) {
	t.Helper()
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatalf("elf.Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatal("binary has a PT_INTERP header: it is dynamically linked")
		}
	}
	// A static binary has no dynamic section, which ImportedLibraries reports as an error.
	libs, _ := f.ImportedLibraries()
	if len(libs) > 0 {
		t.Fatalf("binary imports shared libraries: %v", libs)
	}
}

func checkMachO(t *testing.T, bin string) {
	t.Helper()
	f, err := macho.Open(bin)
	if err != nil {
		t.Fatalf("macho.Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if f.Segment("__RESTRICT") == nil {
		t.Fatal("binary has no __RESTRICT segment")
	}
	s := f.Section("__restrict")
	if s == nil || s.Seg != "__RESTRICT" {
		t.Fatal("binary has no __restrict section in the __RESTRICT segment")
	}
}
