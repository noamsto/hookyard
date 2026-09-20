// Package build generates a native plugin that bundles the hookyard binary
// itself, for engines whose distribution model is build mode rather than yard
// mode (design doc §3.1).
package build

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/internal/atomicfile"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

//go:embed launcher.sh
var launcher []byte

// Options configures Build. Out must already exist: the handlers' execs are
// resolved against it, and it may already hold the author's own plugin
// content (skills, handler scripts, an existing plugin.json).
type Options struct {
	Manifests []string
	Out       string
	Name      string
	Binary    string
}

// Build writes engine's plugin layout into opts.Out.
func Build(engine vocab.Engine, opts Options) error {
	switch engine {
	case vocab.ClaudeCode:
		return claudeCode(opts)
	case vocab.Pi:
		return pi(opts)
	default:
		return fmt.Errorf("hookyard build does not support %s yet; only %s and %s are implemented", engine, vocab.ClaudeCode, vocab.Pi)
	}
}

func claudeCode(opts Options) error {
	// Checked up front, before any manifest or handler work, because the
	// error naming --out should be the first thing an author sees for a
	// typo'd path rather than surfacing as a confusing exec-resolution
	// failure further down.
	info, err := os.Stat(opts.Out)
	if err != nil {
		return fmt.Errorf("--out %s: %w", opts.Out, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--out %s: not a directory", opts.Out)
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return err
	}

	var manifests []*manifest.Manifest
	for _, path := range opts.Manifests {
		m, err := manifest.LoadPlugin(path)
		if err != nil {
			return err
		}
		manifests = append(manifests, m)
	}
	// Commands render only through PiPluginBridge into pi.registerCommand;
	// accepting them here would silently drop them from this engine's
	// build rather than telling the author they picked the wrong engine.
	for _, m := range manifests {
		if len(m.Commands) > 0 {
			return fmt.Errorf("%s: commands are supported only for %s, not %s", m.Source, vocab.Pi, vocab.ClaudeCode)
		}
	}
	merged, err := manifest.Merge(manifests)
	if err != nil {
		return err
	}

	var handlers []manifest.Handler
	for _, h := range merged {
		if slices.Contains(h.Engines, string(vocab.ClaudeCode)) {
			handlers = append(handlers, h)
		}
	}
	if len(handlers) == 0 {
		return fmt.Errorf("no handler in the manifests claims claude-code")
	}

	if err := manifest.CheckPluginExecs(out, handlers); err != nil {
		return err
	}

	entries, err := render.PluginPlan(handlers, vocab.ClaudeCode)
	if err != nil {
		return err
	}

	hooksPath := filepath.Join(out, "hooks", "hooks.json")
	tablePath := filepath.Join(out, manifest.PluginTablePath)
	launcherPath := filepath.Join(out, render.PluginLauncher)
	binaryPath := filepath.Join(out, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
	pluginJSONPath := filepath.Join(out, ".claude-plugin", "plugin.json")

	if err := checkGeneratedDirs(out, hooksPath, tablePath, launcherPath, binaryPath, pluginJSONPath); err != nil {
		return err
	}
	if err := render.CheckDestinations(
		render.Destination{Flag: "--out", Path: hooksPath},
		render.Destination{Flag: "--out", Path: tablePath},
		render.Destination{Flag: "--out", Path: launcherPath},
		render.Destination{Flag: "--out", Path: binaryPath},
		render.Destination{Flag: "--out", Path: pluginJSONPath},
	); err != nil {
		return err
	}

	base, err := os.ReadFile(hooksPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		base = nil
	}
	hooksDoc, err := render.ClaudeSettings(base, entries)
	if err != nil {
		return fmt.Errorf("%s: %w", hooksPath, err)
	}

	// Everything below is read/validation, completed before the first write
	// so a failure here never leaves a partial build on disk.
	var pluginJSON []byte
	writePluginJSON := false
	if _, err := os.Lstat(pluginJSONPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if opts.Name == "" {
			return fmt.Errorf("--name is required when %s does not exist", pluginJSONPath)
		}
		pluginJSON, err = json.MarshalIndent(struct {
			Name string `json:"name"`
		}{Name: opts.Name}, "", "  ")
		if err != nil {
			return err
		}
		pluginJSON = append(pluginJSON, '\n')
		writePluginJSON = true
	}

	binary, err := os.ReadFile(opts.Binary)
	if err != nil {
		return err
	}

	if err := manifest.WritePluginTable(tablePath, handlers); err != nil {
		return err
	}
	if err := atomicfile.Write(hooksPath, hooksDoc, 0o644); err != nil {
		return err
	}
	if writePluginJSON {
		if err := atomicfile.Write(pluginJSONPath, pluginJSON, 0o644); err != nil {
			return err
		}
	}
	if err := atomicfile.Write(launcherPath, launcher, 0o755); err != nil {
		return err
	}
	return atomicfile.Write(binaryPath, binary, 0o755)
}

// pi writes the Pi build package (decomposition "Pi build package on disk"):
// package.json, extensions/hookyard.ts, the launcher and bundled binary under
// bin/, and the baked handler table. Unlike claudeCode's hooks.json, pi's
// package.json is merged even when it already exists, because pi.extensions
// is one entry among a plugin author's own package.json keys rather than a
// file hookyard owns outright the way .claude-plugin/plugin.json is.
func pi(opts Options) error {
	// Checked up front, before any manifest or handler work, because the
	// error naming --out should be the first thing an author sees for a
	// typo'd path rather than surfacing as a confusing exec-resolution
	// failure further down.
	info, err := os.Stat(opts.Out)
	if err != nil {
		return fmt.Errorf("--out %s: %w", opts.Out, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--out %s: not a directory", opts.Out)
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return err
	}

	var manifests []*manifest.Manifest
	for _, path := range opts.Manifests {
		m, err := manifest.LoadPlugin(path)
		if err != nil {
			return err
		}
		manifests = append(manifests, m)
	}
	merged, err := manifest.Merge(manifests)
	if err != nil {
		return err
	}

	var handlers []manifest.Handler
	for _, h := range merged {
		if slices.Contains(h.Engines, string(vocab.Pi)) {
			handlers = append(handlers, h)
		}
	}
	if len(handlers) == 0 {
		return fmt.Errorf("no handler in the manifests claims pi")
	}

	// Merge does not return commands: they are not part of the handler table
	// it dedupes, only cross-manifest name uniqueness, which it already
	// checked above.
	var commands []manifest.Command
	for _, m := range manifests {
		commands = append(commands, m.Commands...)
	}

	if err := manifest.CheckPluginExecs(out, handlers); err != nil {
		return err
	}
	if err := manifest.CheckCommandExecs(out, commands); err != nil {
		return err
	}

	bridge, err := render.PiPluginBridge(handlers, commands)
	if err != nil {
		return err
	}

	packageJSONPath := filepath.Join(out, "package.json")
	extensionPath := filepath.Join(out, "extensions", "hookyard.ts")
	tablePath := filepath.Join(out, manifest.PluginTablePath)
	launcherPath := filepath.Join(out, render.PluginLauncher)
	binaryPath := filepath.Join(out, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))

	// package.json's directory is --out itself, which checkGeneratedDirs does
	// not guard: --out is the caller's own path, not a directory hookyard
	// creates, and a symlinked --out is ordinary (a symlinked checkout, a
	// symlinked plugin dir). extensions/, bin/ and hookyard/ are the
	// directories build-pi creates, so those are the ones a link could
	// redirect.
	if err := checkGeneratedDirs(out, extensionPath, launcherPath, binaryPath, tablePath); err != nil {
		return err
	}
	if err := render.CheckDestinations(
		render.Destination{Flag: "--out", Path: packageJSONPath},
		render.Destination{Flag: "--out", Path: extensionPath},
		render.Destination{Flag: "--out", Path: launcherPath},
		render.Destination{Flag: "--out", Path: binaryPath},
		render.Destination{Flag: "--out", Path: tablePath},
	); err != nil {
		return err
	}

	// Everything below is read/validation, completed before the first write
	// so a failure here never leaves a partial build on disk.
	existing, err := os.ReadFile(packageJSONPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		existing = nil
		if opts.Name == "" {
			return fmt.Errorf("--name is required when %s does not exist", packageJSONPath)
		}
	}
	packageJSON, err := mergePackageJSON(packageJSONPath, existing, opts.Name)
	if err != nil {
		return err
	}

	binary, err := os.ReadFile(opts.Binary)
	if err != nil {
		return err
	}

	if err := manifest.WritePluginTable(tablePath, handlers); err != nil {
		return err
	}
	// The extension is written before package.json names it, the same order
	// WritePi keeps between the yard bridge and the settings.json entry
	// naming it (pi.go:93-97): so a crash mid-build never leaves pi.extensions
	// pointing at a file that does not exist yet.
	if err := atomicfile.Write(extensionPath, bridge, 0o644); err != nil {
		return err
	}
	if err := atomicfile.Write(launcherPath, launcher, 0o755); err != nil {
		return err
	}
	if err := atomicfile.Write(binaryPath, binary, 0o755); err != nil {
		return err
	}
	return atomicfile.Write(packageJSONPath, packageJSON, 0o644)
}

// piExtensionEntry is the value build-pi adds to package.json's pi.extensions
// (decomposition "Pi build package on disk"): the bridge's path relative to
// the package root, exactly as pi.extensions resolves entries.
const piExtensionEntry = "./extensions/hookyard.ts"

// mergePackageJSON returns package.json's bytes. Absent input creates
// {"name": name, "pi": {"extensions": [piExtensionEntry]}}, the plugin.json
// rule's name-required-only-when-absent counterpart. Present input keeps
// every other top-level and pi.* key and appends the entry to pi.extensions
// only if it is not already there, so a rebuild is idempotent and a plugin
// author's own package.json content survives the build.
func mergePackageJSON(path string, existing []byte, name string) ([]byte, error) {
	if existing == nil {
		doc := struct {
			Name string `json:"name"`
			Pi   struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}{Name: name}
		doc.Pi.Extensions = []string{piExtensionEntry}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(out, '\n'), nil
	}

	// A touched or truncated package.json arrives here as a non-nil empty
	// slice, not fs.ErrNotExist, so it never takes the existing==nil branch
	// above. render.ParseObject treats empty input as an empty object — the
	// right leniency for a merge that starts with no file at all — but here
	// the file does exist, so that leniency would silently swallow the
	// --name guard above and write a nameless package.json. Refused before
	// ParseObject sees it, since "empty file" and "no file" want different
	// remedies.
	if len(strings.TrimSpace(string(existing))) == 0 {
		return nil, fmt.Errorf("%s exists but is empty, refusing to overwrite it: remove the file so "+
			"hookyard can create it, or give it real JSON content", path)
	}

	// render.Object, not map[string]json.RawMessage: package.json is
	// hand-edited like Claude's settings.json, and a map re-sorts every
	// top-level and pi.* key on marshal, turning a two-line hook change into
	// a whole-file diff for the plugin author (orderedjson.go's own reason
	// for existing).
	root, err := render.ParseObject(existing)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, refusing to overwrite it: %w", path, err)
	}
	rawPi, _ := root.Get("pi")
	pi, err := render.ParseObject(rawPi)
	if err != nil {
		return nil, fmt.Errorf(`%s has a "pi" key hookyard cannot read, refusing to overwrite it: %w`, path, err)
	}
	var extensions []string
	if rawExtensions, ok := pi.Get("extensions"); ok {
		if err := json.Unmarshal(rawExtensions, &extensions); err != nil {
			return nil, fmt.Errorf(`%s has a "pi.extensions" key hookyard cannot read, refusing to overwrite it: %w`, path, err)
		}
	}
	if !slices.Contains(extensions, piExtensionEntry) {
		extensions = append(extensions, piExtensionEntry)
	}
	if err := pi.Set("extensions", extensions); err != nil {
		return nil, err
	}
	nestedPi, err := pi.MarshalCompact()
	if err != nil {
		return nil, err
	}
	if err := root.Set("pi", json.RawMessage(nestedPi)); err != nil {
		return nil, err
	}
	return root.MarshalIndent()
}

// checkGeneratedDirs refuses to reach a generated file through a directory
// that leads outside the plugin root.
//
// render.CheckDestinations Lstats the final path, but atomicfile.Write reaches
// it through MkdirAll, CreateTemp and Rename, all of which follow a symlinked
// parent transparently — so a link at bin/ would land the bundled binary
// wherever it points. hookyard creates these directories itself, so a link at
// one is refused outright rather than judged by where it happens to point.
func checkGeneratedDirs(out string, paths ...string) error {
	root, err := filepath.EvalSymlinks(out)
	if err != nil {
		return fmt.Errorf("--out %s: %w", out, err)
	}
	for _, path := range paths {
		dir := filepath.Dir(path)
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				return err
			}
			return fmt.Errorf("%s is a symlink to %s, refusing to write through it: hookyard creates that directory "+
				"itself and puts generated plugin files in it, so a link there redirects them outside --out %s; "+
				"remove the link", dir, target, out)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s resolves to %s, outside --out %s, refusing to write through it", dir, resolved, out)
		}
	}
	return nil
}
