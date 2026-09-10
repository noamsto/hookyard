// Package atomicfile lands file content through a single rename, so no
// reader ever observes a half-written file.
package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces path with content in one rename, at mode. The mode is a
// parameter rather than inferred here: whether to preserve an existing
// file's perm bits or fix one is a policy about files this package does not
// own, decided by each caller.
func Write(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hookyard-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// A no-op once the rename below succeeds; the point is to leave nothing
	// behind on any path that does not get that far.
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
