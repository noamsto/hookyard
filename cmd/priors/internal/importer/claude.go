package importer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
)

// maxSource bounds one native file; a memory topic file is a few KB.
const maxSource = 1 << 20

// Claude reads the topic files of every project's memory dir under
// claudeDir/projects, each routed by the project dir's decoded path (§4.2).
// MEMORY.md is Claude's pointer index, which the store generates for itself,
// so it is not imported: its links are checked, and the lines its topic files
// do not cover are reported. Notes are what was read and not imported. Every
// path is read without following a symlink, so a link cannot pull a file from
// outside the memory dir.
func Claude(ctx context.Context, claudeDir string, cfg config.Config, r route.Resolver) (items []Item, notes []Report, err error) {
	projects := filepath.Join(claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		memDir := filepath.Join(projects, d.Name(), "memory")
		info, err := os.Lstat(memDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			notes = append(notes, Report{Outcome: Skipped, Source: sourcePath(memDir), Detail: err.Error()})
			continue
		}
		if !info.IsDir() {
			notes = append(notes, Report{Outcome: Skipped, Source: sourcePath(memDir), Detail: "not a directory"})
			continue
		}
		got, more := claudeProject(memDir)
		notes = append(notes, more...)
		if len(got) == 0 {
			continue
		}
		sess := route.Session{Class: route.ClassUnresolvable}
		if dir, ok := decodeProjectDir(d.Name()); ok {
			sess = route.Resolve(ctx, dir, cfg, r)
		}
		for i := range got {
			got[i].Session = sess
		}
		items = append(items, got...)
	}
	return items, notes, nil
}

// claudeProject reads one memory dir's topic files and checks its MEMORY.md.
func claudeProject(memDir string) (items []Item, notes []Report) {
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil, []Report{{Outcome: Skipped, Source: sourcePath(memDir), Detail: err.Error()}}
	}
	var topics []string
	var index []byte
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(memDir, name)
		src := sourcePath(path)
		raw, err := readRegular(path)
		if err != nil {
			notes = append(notes, Report{Outcome: Skipped, Source: src, Detail: err.Error()})
			continue
		}
		if name == "MEMORY.md" {
			index = raw
			continue
		}
		topics = append(topics, name)
		f, err := fact.Parse(raw)
		if err != nil {
			notes = append(notes, Report{Outcome: Skipped, Source: src, Detail: "not a fact file: " + err.Error()})
			continue
		}
		items = append(items, Item{Engine: "claude", Source: src, Raw: raw, Native: f})
	}
	if index != nil {
		notes = append(notes, checkIndex(sourcePath(filepath.Join(memDir, "MEMORY.md")), index, topics)...)
	}
	return items, notes
}

// readRegular reads path only if it is a regular file: O_NOFOLLOW refuses a
// symlink, and O_NONBLOCK keeps a FIFO from hanging the open.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // path is an entry of the memory dir being imported
	if errors.Is(err, syscall.ELOOP) {
		return nil, errors.New("a symlink, not a regular file")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSource+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSource {
		return nil, fmt.Errorf("larger than %d bytes", maxSource)
	}
	return raw, nil
}

var indexLinkRE = regexp.MustCompile(`^- \[[^\]]*\]\(([^)]*)\)`)

// checkIndex reports each MEMORY.md line that is not a link to one of topics:
// a link that leaves the memory dir, a link to no topic file, and free text.
func checkIndex(src string, raw []byte, topics []string) []Report {
	var notes []Report
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		at := fmt.Sprintf("%s:%d", src, n)
		m := indexLinkRE.FindStringSubmatch(line)
		switch {
		case m == nil:
			notes = append(notes, Report{Outcome: Skipped, Source: at, Detail: "index text is not imported"})
		case !filepath.IsLocal(m[1]):
			notes = append(notes, Report{Outcome: Skipped, Source: at, Detail: "index link leaves the memory dir"})
		case !slices.Contains(topics, m[1]):
			notes = append(notes, Report{Outcome: Skipped, Source: at, Detail: "index link names no topic file"})
		}
	}
	return notes
}

// sourcePath is path as a source block records it: under the home dir it
// starts ~/, so the same native file names itself alike on every host.
func sourcePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(rel) {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// encodeProjectDir is how Claude names a project dir after its path: every
// character but a letter or digit becomes '-'.
func encodeProjectDir(path string) string {
	return nonAlnum.ReplaceAllString(path, "-")
}

// decodeProjectDir finds the existing directory whose path encodes to name.
// The encoding is lossy, so the name is matched one path segment at a time
// against the directories that exist; ok is false for no match, and for more
// than one, since a repo is never guessed.
func decodeProjectDir(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "-")
	if !ok {
		return "", false
	}
	found := map[string]bool{}
	decodeUnder("/", rest, found)
	if len(found) != 1 {
		return "", false
	}
	for dir := range found {
		return dir, true
	}
	return "", false
}

// decodeUnder adds to found every directory under dir whose path below dir
// encodes to rest, keyed by its resolved path so two spellings of one
// directory count once. It stops once a second match makes the decode
// ambiguous.
func decodeUnder(dir, rest string, found map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(found) > 1 {
			return
		}
		enc := encodeProjectDir(e.Name())
		tail, ok := strings.CutPrefix(rest, enc)
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		if tail == "" {
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				found[resolved] = true
			}
			continue
		}
		if next, ok := strings.CutPrefix(tail, "-"); ok {
			decodeUnder(path, next, found)
		}
	}
}
