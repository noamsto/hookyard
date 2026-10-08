// Package guard is priors' pre_tool write guard: a coarse tripwire over what
// only priors writes. It matches text and path prefixes and models nothing,
// so it over-denies and misses indirection (§4.4, "The write guard").
package guard

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const attestDir = ".attest"

// Set is what the guard protects, each root in its given and resolved form.
type Set struct {
	// stores are the checkouts: their fact files may be written, but not with
	// a reviewed claim, and their indexes and .attest entries not at all.
	stores []string
	// state are priors' state dirs, denied outright.
	state []string
}

// ForConfig builds the set. A config that failed to load leaves the
// config-independent entries: the default state dir and the .attest rule.
func ForConfig(cfg config.Config, cfgErr error) Set {
	var s Set
	if cfgErr != nil {
		s.state = both(config.DefaultState())
		return s
	}
	s.state = both(cfg.State())
	s.stores = both(cfg.PersonalStore)
	if cfg.WorkStore != "" {
		s.stores = append(s.stores, both(cfg.WorkStore)...)
	}
	return s
}

// both returns dir and its resolved form, deduplicated; empty dir yields none.
func both(dir string) []string {
	if dir == "" {
		return nil
	}
	out := []string{filepath.Clean(dir)}
	if r, err := config.ResolveDir(dir); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// reviewedLine matches a claim line; a plain line match is the whole rule.
var reviewedLine = regexp.MustCompile(`(?m)^[ \t]*confidence:[ \t]*reviewed[ \t]*\r?$`)

// writeToken matches a shell token that can write. Plain text matching: a
// word-boundary scan, not a parse.
var writeToken = regexp.MustCompile(
	`>|(^|[^\w.-])(tee|mv|cp|rm|install|dd|truncate|eval)($|[^\w.-])` +
		`|(^|[^\w.-])sed[ \t]+(-[A-Za-z]*i|--in-place)` +
		`|(^|[^\w.-])perl[ \t]+-[A-Za-z]*i` +
		`|(^|[^\w.-])(bash|sh)[ \t]+-[A-Za-z]*c`)

// Check returns why the call must be denied, or "".
func Check(s Set, cwd, tool string, input json.RawMessage) string {
	switch strings.ToLower(tool) {
	case "bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) != nil {
			return ""
		}
		return s.checkCommand(in.Command)
	case "write", "edit", "multiedit":
		var in struct {
			FilePath  string `json:"file_path"`
			Content   string `json:"content"`
			NewString string `json:"new_string"`
			Edits     []struct {
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return ""
		}
		text := in.Content + "\n" + in.NewString
		for _, e := range in.Edits {
			text += "\n" + e.NewString
		}
		return s.checkWrite(cwd, in.FilePath, text)
	}
	return ""
}

func (s Set) checkWrite(cwd, path, text string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	paths := []string{path}
	if r, err := config.ResolveDir(filepath.Dir(path)); err == nil {
		paths = append(paths, filepath.Join(r, filepath.Base(path)))
	}
	for _, p := range paths {
		if hasElem(p, attestDir) {
			return "priors: writes to .attest entries are denied"
		}
		if slices.ContainsFunc(s.state, func(d string) bool { return within(p, d) }) {
			return "priors: writes to priors' state directory are denied"
		}
		for _, d := range s.stores {
			if p == filepath.Join(d, store.IndexFile) {
				return "priors: writes to a generated MEMORY.md index are denied"
			}
		}
	}
	if slices.ContainsFunc(paths, func(p string) bool {
		return slices.ContainsFunc(s.stores, func(d string) bool { return within(p, d) })
	}) && reviewedLine.MatchString(text) {
		return "priors: a confidence: reviewed claim is written by attestation, not by an agent"
	}
	return ""
}

func (s Set) checkCommand(cmd string) string {
	if !writeToken.MatchString(cmd) {
		return ""
	}
	protected := append(slices.Clone(s.state), s.stores...)
	protected = append(protected, attestDir)
	if slices.ContainsFunc(protected, func(p string) bool { return strings.Contains(cmd, p) }) {
		return "priors: a command that writes and names a protected path is denied"
	}
	return ""
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}

func hasElem(p, elem string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(p), "/"), elem)
}
