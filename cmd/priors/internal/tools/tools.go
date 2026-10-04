// Package tools holds the absolute paths of the programs priors runs. They are
// fixed at build time with -ldflags -X, so neither config nor PATH can swap them.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Set by -ldflags -X; empty in an unpinned build.
var (
	Git     string
	SSH     string
	Rg      string
	Scanner string
)

// ErrUnpinned reports a tool whose path was not fixed at build time.
var ErrUnpinned = errors.New("not pinned at build time")

// Unpinned names the tools whose path is not absolute.
func Unpinned() []string {
	var names []string
	for _, t := range []struct{ name, path string }{
		{"git", Git}, {"ssh", SSH}, {"rg", Rg}, {"scanner", Scanner},
	} {
		if !filepath.IsAbs(t.path) {
			names = append(names, t.name)
		}
	}
	return names
}

// Check reports whether path is a pinned, executable regular file.
func Check(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q: %w", path, ErrUnpinned)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s: not an executable file", path)
	}
	return nil
}

// Command is exec.CommandContext for a pinned path. A path that is not
// absolute fails at Start, so nothing is looked up on PATH.
func Command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // path is fixed at build time; a non-absolute one is refused below
	if !filepath.IsAbs(path) {
		cmd.Err = fmt.Errorf("%q: %w", path, ErrUnpinned)
	}
	return cmd
}
