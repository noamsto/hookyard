package gate

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeScanner writes an executable that ignores its arguments, drains stdin,
// prints out and exits with code.
func fakeScanner(t *testing.T, dir, name, out string, code int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n" + out + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanText(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		code    int
		want    []string
		wantErr bool
	}{
		{"findings", `[{"RuleID":"github-pat","Match":"REDACTED"},{"RuleID":"aws-access-token"},{"RuleID":"github-pat"}]`, 0, []string{"github-pat", "aws-access-token"}, false},
		{"no findings", `[]`, 0, nil, false},
		{"non-zero exit", `[]`, 2, nil, true},
		{"junk output", `this is not json`, 0, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Scanner{Bin: fakeScanner(t, t.TempDir(), "scanner", tt.out, tt.code)}
			got, err := s.ScanText(context.Background(), "some fact text")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ScanText = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ScanText = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanTextCancelledContextFails(t *testing.T) {
	s := Scanner{Bin: fakeScanner(t, t.TempDir(), "scanner", `[]`, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ScanText(ctx, "text"); err == nil {
		t.Fatal("ScanText with cancelled ctx succeeded, want error")
	}
}

func TestScanDirMapsFileToIDs(t *testing.T) {
	dir := t.TempDir()
	out := `[{"RuleID":"r1","File":"` + filepath.Join(dir, "sub", "a.md") + `"},` +
		`{"RuleID":"r2","File":"` + filepath.Join(dir, "sub", "a.md") + `"},` +
		`{"RuleID":"r3","File":"/elsewhere/b.md"}]`
	s := Scanner{Bin: fakeScanner(t, t.TempDir(), "scanner", out, 0)}

	got, err := s.ScanDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		filepath.Join("sub", "a.md"): {"r1", "r2"},
		"/elsewhere/b.md":            {"r3"},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("ScanDir = %v, want %v", got, want)
	}
}

func TestScanDirFailsClosed(t *testing.T) {
	s := Scanner{Bin: fakeScanner(t, t.TempDir(), "scanner", `junk`, 0)}
	if _, err := s.ScanDir(context.Background(), t.TempDir()); err == nil {
		t.Fatal("ScanDir on junk output succeeded, want error")
	}
}

func TestFindScanner(t *testing.T) {
	t.Run("empty PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if s, err := FindScanner(""); err == nil {
			t.Fatalf("FindScanner = %+v, want error", s)
		}
	})

	t.Run("prefers betterleaks over gitleaks", func(t *testing.T) {
		dir := t.TempDir()
		fakeScanner(t, dir, "gitleaks", `[]`, 0)
		fakeScanner(t, dir, "betterleaks", `[]`, 0)
		t.Setenv("PATH", dir)
		s, err := FindScanner("")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(s.Bin) != "betterleaks" {
			t.Errorf("Bin = %s, want betterleaks", s.Bin)
		}
	})

	t.Run("falls back to gitleaks", func(t *testing.T) {
		dir := t.TempDir()
		fakeScanner(t, dir, "gitleaks", `[]`, 0)
		t.Setenv("PATH", dir)
		s, err := FindScanner("")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(s.Bin) != "gitleaks" {
			t.Errorf("Bin = %s, want gitleaks", s.Bin)
		}
	})

	t.Run("explicit path", func(t *testing.T) {
		dir := t.TempDir()
		exe := fakeScanner(t, dir, "custom", `[]`, 0)
		notExe := filepath.Join(dir, "plain")
		if err := os.WriteFile(notExe, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := FindScanner(exe); err != nil || s.Bin != exe {
			t.Errorf("FindScanner(%s) = %+v, %v", exe, s, err)
		}
		if s, err := FindScanner(notExe); err == nil {
			t.Errorf("FindScanner(non-executable) = %+v, want error", s)
		}
	})

	t.Run("name on PATH", func(t *testing.T) {
		dir := t.TempDir()
		fakeScanner(t, dir, "myscan", `[]`, 0)
		t.Setenv("PATH", dir)
		if s, err := FindScanner("myscan"); err != nil || filepath.Base(s.Bin) != "myscan" {
			t.Errorf("FindScanner(myscan) = %+v, %v", s, err)
		}
		if s, err := FindScanner("absent"); err == nil {
			t.Errorf("FindScanner(absent) = %+v, want error", s)
		}
	})
}

func TestRealBetterleaks(t *testing.T) {
	bin, err := exec.LookPath("betterleaks")
	if err != nil {
		t.Skip("betterleaks not on PATH")
	}
	s := Scanner{Bin: bin}
	// betterleaks' github-pat rule has an entropy floor, so a repeated
	// pattern would not trip it; this body is varied on purpose.
	token := "gh" + "p_" + "Zq8Kd3LmX0pR7tVb2Nw9" + "Yc4Hf6Js1GaEuQiO"

	got, err := s.ScanText(context.Background(), "the token is "+token+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("ScanText found nothing in a ghp_-shaped token")
	}
	for _, id := range got {
		if strings.Contains(id, token) {
			t.Errorf("finding leaked the token: %q", id)
		}
	}

	clean, err := s.ScanText(context.Background(), "a plain descriptive fact\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(clean) != 0 {
		t.Errorf("ScanText(clean) = %v, want none", clean)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leak.md"), []byte("the token is "+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	byFile, err := s.ScanDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(byFile["leak.md"]) == 0 {
		t.Errorf("ScanDir = %v, want a finding for leak.md", byFile)
	}
}
