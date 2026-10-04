package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const scanTimeout = 30 * time.Second

// Scanner runs an external secret scanner. betterleaks and gitleaks share the
// flags used here, so either binary works. Bin is an absolute path.
type Scanner struct {
	Bin string
}

// PinnedScanner returns the scanner fixed at build time.
func PinnedScanner() (Scanner, error) {
	if err := tools.Check(tools.Scanner); err != nil {
		return Scanner{}, fmt.Errorf("secret scanner: %w", err)
	}
	return Scanner{Bin: tools.Scanner}, nil
}

type finding struct {
	RuleID string `json:"RuleID"`
	File   string `json:"File"`
}

var reportFlags = []string{"--no-banner", "--report-format", "json", "--report-path", "-", "--redact", "--log-level", "error", "--exit-code", "0"}

// pinnedConfig is the only config a scan uses: the scanner would otherwise
// take one from the scanned directory or the environment, both of which the
// content's author may control.
const pinnedConfig = "[extend]\nuseDefault = true\n"

var configEnv = []string{"GITLEAKS_CONFIG", "BETTERLEAKS_CONFIG", "GITLEAKS_CONFIG_TOML", "BETTERLEAKS_CONFIG_TOML"}

// ScanText scans text and returns the distinct rule IDs it trips. Any failure
// to get a clean, parsable report is an error so callers fail closed.
func (s Scanner) ScanText(ctx context.Context, text string) ([]string, error) {
	sb, err := newSandbox()
	if err != nil {
		return nil, err
	}
	defer sb.remove()
	found, err := s.run(ctx, sb, strings.NewReader(text), "stdin")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, f := range found {
		if !slices.Contains(ids, f.RuleID) {
			ids = append(ids, f.RuleID)
		}
	}
	return ids, nil
}

// ScanDir scans dir and returns rule IDs keyed by file, relative to dir when
// the file lies under it. It scans a private copy: the scanner always honours
// an ignore file at the root of what it scans, and no flag turns that off.
func (s Scanner) ScanDir(ctx context.Context, dir string) (map[string][]string, error) {
	sb, err := newSandbox()
	if err != nil {
		return nil, err
	}
	defer sb.remove()
	target := filepath.Join(sb.dir, "target")
	if err := copyTree(dir, target); err != nil {
		return nil, fmt.Errorf("copying %s to scan: %w", dir, err)
	}
	found, err := s.run(ctx, sb, nil, "dir", target)
	if err != nil {
		return nil, err
	}
	byFile := map[string][]string{}
	for _, f := range found {
		file := f.File
		if rel, err := filepath.Rel(target, file); err == nil && filepath.IsLocal(rel) {
			file = rel
		}
		if !slices.Contains(byFile[file], f.RuleID) {
			byFile[file] = append(byFile[file], f.RuleID)
		}
	}
	return byFile, nil
}

// sandbox is a scan's private working directory: the pinned config and an
// empty ignore directory.
type sandbox struct{ dir string }

func newSandbox() (sandbox, error) {
	dir, err := os.MkdirTemp("", "priors-scan-")
	if err != nil {
		return sandbox{}, fmt.Errorf("secret scanner: %w", err)
	}
	sb := sandbox{dir: dir}
	if err := os.WriteFile(sb.config(), []byte(pinnedConfig), 0o600); err != nil {
		sb.remove()
		return sandbox{}, fmt.Errorf("secret scanner: %w", err)
	}
	if err := os.Mkdir(sb.ignore(), 0o700); err != nil {
		sb.remove()
		return sandbox{}, fmt.Errorf("secret scanner: %w", err)
	}
	return sb, nil
}

func (sb sandbox) config() string { return filepath.Join(sb.dir, "config.toml") }
func (sb sandbox) ignore() string { return filepath.Join(sb.dir, "ignore") }
func (sb sandbox) remove()        { _ = os.RemoveAll(sb.dir) }

// copyTree copies src's regular files to dst, leaving out .git, the ignore
// files the scanner would honour, and anything that is not a regular file.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir() && d.Name() == ".git" && path != src:
			return filepath.SkipDir
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		case !d.Type().IsRegular(), rel == ".gitleaksignore", rel == ".betterleaksignore":
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // path comes from walking the directory being scanned
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // rel is relative to the walked src, so the copy stays under dst
	})
}

func (s Scanner) run(ctx context.Context, sb sandbox, stdin io.Reader, args ...string) ([]finding, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, scanTimeout)
		defer cancel()
	}
	name := filepath.Base(s.Bin)
	args = append(args, reportFlags...)
	args = append(args, "--config", sb.config(), "--ignore-gitleaks-allow", "--gitleaks-ignore-path", sb.ignore())
	cmd := tools.Command(ctx, s.Bin, args...)
	cmd.Dir = sb.dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(configEnv, k)
	})
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	// stderr is dropped rather than surfaced: a scanner's diagnostics could
	// echo the content being scanned.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// A null report is the scanner's own empty result.
	var found []finding
	if err := json.Unmarshal(out.Bytes(), &found); err != nil {
		return nil, fmt.Errorf("%s: unparsable report: %w", name, err)
	}
	return found, nil
}
