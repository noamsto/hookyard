package gate

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

func TestMain(m *testing.M) {
	toolstest.Pin()
	os.Exit(m.Run())
}

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
		{"null report", `null`, 0, nil, false},
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
	// The scanner reports paths under the directory it was given ("$2").
	bin := filepath.Join(t.TempDir(), "scanner")
	script := `#!/bin/sh
printf '[{"RuleID":"r1","File":"%s/sub/a.md"},{"RuleID":"r2","File":"%s/sub/a.md"},{"RuleID":"r3","File":"/elsewhere/b.md"}]' "$2" "$2"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Scanner{Bin: bin}

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

// TestScannerRunsPinned checks, without a real scanner, that every run gets a
// pinned config, ignores inline allows, reads no ignore file of the caller's,
// and sees none of the config environment variables.
func TestScannerRunsPinned(t *testing.T) {
	for _, k := range []string{"GITLEAKS_CONFIG", "BETTERLEAKS_CONFIG", "GITLEAKS_CONFIG_TOML", "BETTERLEAKS_CONFIG_TOML"} {
		t.Setenv(k, "/attacker/config.toml")
	}
	bin := filepath.Join(t.TempDir(), "scanner")
	script := `#!/bin/sh
cat >/dev/null
for v in "$GITLEAKS_CONFIG" "$BETTERLEAKS_CONFIG" "$GITLEAKS_CONFIG_TOML" "$BETTERLEAKS_CONFIG_TOML"; do
	[ -z "$v" ] || exit 7
done
cfg= allow= ign=
while [ $# -gt 0 ]; do
	case "$1" in
	--config) cfg=$2 ;;
	--ignore-gitleaks-allow) allow=1 ;;
	--gitleaks-ignore-path) ign=$2 ;;
	esac
	shift
done
[ -n "$allow" ] && [ -f "$cfg" ] && [ -d "$ign" ] || exit 8
[ -z "$(ls -A "$ign")" ] || exit 9
[ "$(cd "$(dirname "$cfg")" && pwd -P)" = "$(pwd -P)" ] || exit 10
grep -q useDefault "$cfg" || exit 11
echo '[]'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Scanner{Bin: bin}
	if _, err := s.ScanText(context.Background(), "text"); err != nil {
		t.Errorf("ScanText: %v", err)
	}
	if _, err := s.ScanDir(context.Background(), t.TempDir()); err != nil {
		t.Errorf("ScanDir: %v", err)
	}
}

func TestPinnedScanner(t *testing.T) {
	saved := tools.Scanner
	t.Cleanup(func() { tools.Scanner = saved })

	tools.Scanner = ""
	if s, err := PinnedScanner(); err == nil {
		t.Errorf("unpinned: PinnedScanner = %+v, want error", s)
	}

	tools.Scanner = filepath.Join(t.TempDir(), "absent")
	if s, err := PinnedScanner(); err == nil {
		t.Errorf("missing: PinnedScanner = %+v, want error", s)
	}

	exe := fakeScanner(t, t.TempDir(), "custom", `[]`, 0)
	tools.Scanner = exe
	if s, err := PinnedScanner(); err != nil || s.Bin != exe {
		t.Errorf("PinnedScanner = %+v, %v; want Bin %s", s, err, exe)
	}
}

// realBetterleaks is the pinned scanner when it is betterleaks.
func realBetterleaks(t *testing.T) Scanner {
	t.Helper()
	if filepath.Base(tools.Scanner) != "betterleaks" {
		t.Skip("betterleaks is not the pinned scanner")
	}
	return Scanner{Bin: tools.Scanner}
}

func TestRealBetterleaks(t *testing.T) {
	s := realBetterleaks(t)
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

// TestRealBetterleaksCannotBeSuppressed plants each way the scanned content or
// the environment could tell the scanner to look away.
func TestRealBetterleaksCannotBeSuppressed(t *testing.T) {
	s := realBetterleaks(t)
	token := "gh" + "p_" + "Zq8Kd3LmX0pR7tVb2Nw9" + "Yc4Hf6Js1GaEuQiO"
	line := "the token is " + token + "\n"
	allowAll := "[allowlist]\npaths = [\".*\"]\nregexes = [\".*\"]\n"

	scanDir := func(t *testing.T, content string, extra map[string]string) {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{"leak.md": content}
		maps.Copy(files, extra)
		for name, body := range files {
			body = strings.ReplaceAll(body, "{dir}", dir)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		byFile, err := s.ScanDir(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(byFile["leak.md"]) == 0 {
			t.Errorf("ScanDir = %v, want a finding for leak.md", byFile)
		}
	}
	scanText := func(t *testing.T, text string) {
		t.Helper()
		got, err := s.ScanText(context.Background(), text)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Error("ScanText found nothing")
		}
	}

	t.Run("allow-all config in the scanned dir", func(t *testing.T) {
		scanDir(t, line, map[string]string{".gitleaks.toml": allowAll, ".betterleaks.toml": allowAll})
	})
	t.Run("ignore files in the scanned dir", func(t *testing.T) {
		ignore := "{dir}/leak.md:github-pat:1\nleak.md:github-pat:1\n"
		scanDir(t, line, map[string]string{".gitleaksignore": ignore, ".betterleaksignore": ignore})
	})
	t.Run("inline allow comment", func(t *testing.T) {
		allowed := "the token is " + token + " # gitleaks:allow betterleaks:allow\n"
		scanDir(t, allowed, nil)
		scanText(t, allowed)
	})
	t.Run("config environment variables", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "allow.toml")
		if err := os.WriteFile(cfg, []byte(allowAll), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITLEAKS_CONFIG", cfg)
		t.Setenv("BETTERLEAKS_CONFIG", cfg)
		t.Setenv("GITLEAKS_CONFIG_TOML", allowAll)
		t.Setenv("BETTERLEAKS_CONFIG_TOML", allowAll)
		scanDir(t, line, nil)
		scanText(t, line)
	})
}
