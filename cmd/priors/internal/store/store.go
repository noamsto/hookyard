// Package store is priors' view of the directories facts live in: where a
// fact goes, how a root is walked, and how a path read back from an index is
// confined to its root.
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
)

// Kind says which of the three layers a root is.
type Kind string

const (
	KindCheckout   Kind = "checkout"
	KindLocal      Kind = "local"
	KindQuarantine Kind = "quarantine"
)

const (
	archiveDir = "_archive"
	globalDir  = "_global"
	noRepoDir  = "_norepo"
)

// Root is one directory tree of facts. Store is empty for the quarantine.
type Root struct {
	Store route.StoreID
	Kind  Kind
	Path  string
}

// Entry is one parsed fact file. Rel uses '/' separators.
type Entry struct {
	Root     Root
	Rel      string
	Fact     fact.Fact
	Size     int64
	Archived bool
}

// WalkErr is a file Walk could not read or parse.
type WalkErr struct {
	Rel string
	Err error
}

func CheckoutRoot(cfg config.Config, id route.StoreID) Root {
	path := cfg.PersonalStore
	if id == route.StoreWork {
		path = cfg.WorkStore
	}
	return Root{Store: id, Kind: KindCheckout, Path: path}
}

func LocalRoot(cfg config.Config, id route.StoreID) Root {
	return Root{Store: id, Kind: KindLocal, Path: filepath.Join(cfg.State(), "local", string(id))}
}

func QuarantineRoot(cfg config.Config) Root {
	return Root{Kind: KindQuarantine, Path: filepath.Join(cfg.State(), "quarantine")}
}

// ReadRoots lists, per store in order, its checkout and then its local layer.
func ReadRoots(cfg config.Config, stores []route.StoreID) []Root {
	roots := make([]Root, 0, 2*len(stores))
	for _, id := range stores {
		roots = append(roots, CheckoutRoot(cfg, id), LocalRoot(cfg, id))
	}
	return roots
}

// Walk parses every fact under the root, sorted by Rel. Root-level files and
// dot directories are not facts, and a symlinked directory is not followed
// (a symlinked file is listed; Confine is how a caller checks where it
// leads). A missing root is an empty one.
func (r Root) Walk() ([]Entry, []WalkErr) {
	if r.Path == "" {
		return nil, nil
	}
	var entries []Entry
	var errs []WalkErr
	_ = filepath.WalkDir(r.Path, func(path string, d fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(r.Path, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			if rel != "." || !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, WalkErr{Rel: rel, Err: err})
			}
			return nil
		}
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(rel, "/") || !strings.HasSuffix(rel, ".md") {
			return nil
		}
		e, err := readEntry(r, path, rel)
		if err != nil {
			errs = append(errs, WalkErr{Rel: rel, Err: err})
			return nil //nolint:nilerr // recorded as a WalkErr; one bad file must not stop the walk
		}
		entries = append(entries, e)
		return nil
	})
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Rel, b.Rel) })
	slices.SortFunc(errs, func(a, b WalkErr) int { return strings.Compare(a.Rel, b.Rel) })
	return entries, errs
}

// readEntry stats before reading so a FIFO posing as a fact cannot hang the
// walk.
func readEntry(r Root, path, rel string) (Entry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Entry{}, err
	}
	if !info.Mode().IsRegular() {
		return Entry{}, errors.New("not a regular file")
	}
	b, err := os.ReadFile(path) //nolint:gosec // path comes from walking the store root
	if err != nil {
		return Entry{}, err
	}
	f, err := fact.Parse(b)
	if err != nil {
		return Entry{}, err
	}
	top, _, _ := strings.Cut(rel, "/")
	return Entry{Root: r, Rel: rel, Fact: f, Size: int64(len(b)), Archived: top == archiveDir}, nil
}

// FindByName is the first fact called name; a live one wins over an archived
// one.
func (r Root) FindByName(name string) (Entry, bool) {
	entries, _ := r.Walk()
	var archived *Entry
	for i, e := range entries {
		if e.Fact.Name != name {
			continue
		}
		if !e.Archived {
			return e, true
		}
		if archived == nil {
			archived = &entries[i]
		}
	}
	if archived == nil {
		return Entry{}, false
	}
	return *archived, true
}

// Confine resolves rel against the root and returns the absolute path, or an
// error if rel could leave the root lexically or through a symlink. A file
// that does not exist passes the lexical checks and returns its path with an
// error wrapping fs.ErrNotExist; the caller decides what that means.
func (r Root) Confine(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) {
		return "", fmt.Errorf("path %q is not a relative store path", rel)
	}
	if slices.Contains(strings.Split(rel, "/"), "..") {
		return "", fmt.Errorf("path %q climbs out of the store", rel)
	}
	joined := filepath.Join(r.Path, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(joined)
	if errors.Is(err, fs.ErrNotExist) {
		return joined, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(r.Path)
	if err != nil {
		return "", err
	}
	inside, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || !filepath.IsLocal(inside) {
		return "", fmt.Errorf("path %q resolves outside the store", rel)
	}
	return joined, nil
}

// PathFor is where a fact belongs. A checkout files by the fact's own scope
// and repo; the local layer and the quarantine file by the repo the session
// learned it in.
func (r Root) PathFor(f fact.Fact, learnedRepo string) string {
	dir := noRepoDir
	switch {
	case r.Kind != KindCheckout:
		if learnedRepo != "" {
			dir = learnedRepo
		}
	case f.Metadata.Scope == "global":
		dir = globalDir
	case len(f.Metadata.Repos) > 0:
		dir = f.Metadata.Repos[0]
	}
	return filepath.Join(r.Path, dir, f.Name+".md")
}
