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
	default:
		return fmt.Errorf("hookyard build does not support %s yet; only %s is implemented", engine, vocab.ClaudeCode)
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
