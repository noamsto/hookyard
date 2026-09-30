package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const scanTimeout = 30 * time.Second

// Scanner runs an external secret scanner. betterleaks and gitleaks share the
// flags used here, so either binary works.
type Scanner struct {
	Bin string
}

// FindScanner resolves v to a scanner binary: "" tries betterleaks then
// gitleaks on PATH; a value containing '/' must itself be executable; any
// other value is looked up on PATH.
func FindScanner(v string) (Scanner, error) {
	if v != "" {
		bin, err := exec.LookPath(v)
		if err != nil {
			return Scanner{}, fmt.Errorf("secret scanner: %w", err)
		}
		return Scanner{Bin: bin}, nil
	}
	for _, name := range []string{"betterleaks", "gitleaks"} {
		if bin, err := exec.LookPath(name); err == nil {
			return Scanner{Bin: bin}, nil
		}
	}
	return Scanner{}, errors.New("secret scanner: neither betterleaks nor gitleaks is on PATH")
}

type finding struct {
	RuleID string `json:"RuleID"`
	File   string `json:"File"`
}

var reportFlags = []string{"--no-banner", "--report-format", "json", "--report-path", "-", "--redact", "--log-level", "error", "--exit-code", "0"}

// ScanText scans text and returns the distinct rule IDs it trips. Any failure
// to get a clean, parsable report is an error so callers fail closed.
func (s Scanner) ScanText(ctx context.Context, text string) ([]string, error) {
	fs, err := s.run(ctx, strings.NewReader(text), append([]string{"stdin"}, reportFlags...))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, f := range fs {
		if !slices.Contains(ids, f.RuleID) {
			ids = append(ids, f.RuleID)
		}
	}
	return ids, nil
}

// ScanDir scans dir and returns rule IDs keyed by file, relative to dir when
// the file lies under it.
func (s Scanner) ScanDir(ctx context.Context, dir string) (map[string][]string, error) {
	fs, err := s.run(ctx, nil, append([]string{"dir", dir}, reportFlags...))
	if err != nil {
		return nil, err
	}
	byFile := map[string][]string{}
	for _, f := range fs {
		file := f.File
		if rel, err := filepath.Rel(dir, file); err == nil && filepath.IsLocal(rel) {
			file = rel
		}
		if !slices.Contains(byFile[file], f.RuleID) {
			byFile[file] = append(byFile[file], f.RuleID)
		}
	}
	return byFile, nil
}

func (s Scanner) run(ctx context.Context, stdin io.Reader, args []string) ([]finding, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, scanTimeout)
		defer cancel()
	}
	name := filepath.Base(s.Bin)
	cmd := exec.CommandContext(ctx, s.Bin, args...) //nolint:gosec // Bin is the scanner resolved from config/PATH by FindScanner, run by design
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
	var fs []finding
	if err := json.Unmarshal(out.Bytes(), &fs); err != nil {
		return nil, fmt.Errorf("%s: unparsable report: %w", name, err)
	}
	return fs, nil
}
