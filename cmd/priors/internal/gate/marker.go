package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// markerPath hashes the session id so an engine-supplied id can never escape
// dir or collide with a path separator.
func markerPath(dir, session, ext string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+ext)
}

// MarkSession records that the watcher saw session make a tool call, and that
// the call ingested content when ingest is set. The ingest marker lands before
// the seen one, so a failed ingest write never leaves a fresh session looking
// watched and clean. Markers are created once and never rewritten, so
// concurrent handlers cannot downgrade one.
func MarkSession(dir, session string, ingest bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if ingest {
		if err := touch(markerPath(dir, session, ".ingest")); err != nil {
			return err
		}
	}
	return touch(markerPath(dir, session, ".seen"))
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // path is a hash-named file inside the configured marker dir
	if err != nil {
		return err
	}
	return f.Close()
}

// sessionMarkers reports which markers exist for session. An error means a
// marker's existence could not be established, which callers must not read as
// absence.
func sessionMarkers(dir, session string) (seen, ingest bool, err error) {
	seen, err = exists(markerPath(dir, session, ".seen"))
	if err != nil {
		return false, false, err
	}
	ingest, err = exists(markerPath(dir, session, ".ingest"))
	if err != nil {
		return false, false, err
	}
	return seen, ingest, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
