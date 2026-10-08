// Package guard is priors' pre_tool write guard: a coarse tripwire over what
// only priors writes. It matches text and path prefixes and models nothing,
// so it over-denies and misses indirection (§4.4, "The write guard").
package guard

import (
	"encoding/json"
	"os"
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
	if h, err := os.UserHomeDir(); err == nil && within(out[0], h) {
		if rel, err := filepath.Rel(h, out[0]); err == nil {
			out = append(out, "~/"+rel)
		}
	}
	if r, err := config.ResolveDir(dir); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// reviewedLine matches a claim line; a plain line match is the whole rule.
// A bare `reviewed` word also counts: an Edit that turns proposed into
// reviewed carries no confidence: key in its new text.
var reviewedLine = regexp.MustCompile(`\breviewed\b`)

// stderrRedirect is a redirect that cannot write a protected file.
var stderrRedirect = regexp.MustCompile(`[0-9]*>&[0-9-]|[0-9]*>>?[ \t]*/dev/null`)

// writeToken matches a shell token that can write. Plain text matching: a
// word-boundary scan, not a parse.
var writeToken = regexp.MustCompile(
	`>|(^|[^\w.-])(tee|mv|cp|rm|install|dd|truncate|eval)($|[^\w.-])` +
		`|(^|[^\w.-])sed\b[^|;&\n]*[ \t](-[A-Za-z]*i|--in-place)` +
		`|(^|[^\w.-])perl\b[^|;&\n]*[ \t]-[A-Za-z]*i` +
		`|(^|[^\w.-])(bash|sh)\b[^|;&\n]*[ \t]-[A-Za-z]*c`)

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
			Path      string `json:"path"`
			Content   string `json:"content"`
			NewString string `json:"new_string"`
			Edits     []struct {
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if len(input) == 0 || json.Unmarshal(input, &in) != nil {
			return "priors: an unreadable write call is denied"
		}
		if in.FilePath == "" {
			in.FilePath = in.Path
		}
		if in.FilePath == "" {
			// A patch body names its paths inline (Codex apply_patch).
			return s.checkText(string(input))
		}
		texts := []string{in.Content, in.NewString}
		for _, e := range in.Edits {
			texts = append(texts, e.NewString)
		}
		return s.checkWrite(cwd, in.FilePath, strings.Join(texts, "\n"))
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
	if !writeToken.MatchString(stderrRedirect.ReplaceAllString(cmd, "")) {
		return ""
	}
	return s.checkText(cmd)
}

// checkText denies text that names a protected path.
func (s Set) checkText(cmd string) string {
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
