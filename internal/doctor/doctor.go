// Package doctor answers the question hookyard's own event record cannot: is
// this machine actually going to run the handlers it looks like it registered?
//
// Every engine skips hooks entirely in a directory the user has not trusted,
// and a handler that was never invoked cannot abstain, error or time out — so a
// silently disabled guard is indistinguishable, from inside the record, from a
// session where nothing dangerous was attempted (§8).
package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

// Status is deliberately three-valued: hookyard can read some of what each
// engine decides but not all of it, and reporting an unknown as a pass would
// be worse than reporting it as unknown.
type Status int

const (
	Unknown Status = iota
	Pass
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "ok"
	case Fail:
		return "PROBLEM"
	default:
		return "unknown"
	}
}

type Finding struct {
	Engine vocab.Engine
	Check  string
	Status Status
	Detail string
}

// Paths locates each engine's configuration. Tests set it; the CLI derives it
// from the environment.
type Paths struct {
	ClaudeConfigDir string
	CodexHome       string
	CursorHome      string
	// StateDir is the operator's explicit --state-dir. Empty means none was
	// given: Run recovers it from the --state-dir the three engines' emitted
	// configs already name, rather than treating empty as shorthand for
	// record.DefaultStateDir().
	StateDir string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return Paths{ClaudeConfigDir: claude, CodexHome: codex, CursorHome: filepath.Join(home, ".cursor")}, nil
}

// Run reports on each engine for one working directory.
func Run(p Paths, dir string) []Finding {
	var findings []Finding
	findings = append(findings, claudeFindings(p, dir)...)
	findings = append(findings, codexFindings(p, dir)...)
	findings = append(findings, cursorFindings(p, dir)...)

	stateDir := p.StateDir
	var disagreement []string
	if stateDir == "" {
		switch recovered := recoverStateDir(p); len(recovered) {
		case 1:
			stateDir = recovered[0]
		case 0:
			// Run cannot propagate DefaultStateDir's error; falling through to
			// an empty directory leaves streamFindings to report Unknown,
			// which is already its answer for a missing stream.
			stateDir, _ = record.DefaultStateDir()
		default:
			disagreement = recovered
		}
	}
	findings = append(findings, streamFindings(stateDir, disagreement, time.Now())...)
	return findings
}

func claudeFindings(p Paths, dir string) []Finding {
	settings := filepath.Join(p.ClaudeConfigDir, "settings.json")
	state := claudeStatePath(p.ClaudeConfigDir)

	trust := Finding{Engine: vocab.ClaudeCode, Check: "workspace trust", Detail: state}
	var claudeState struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := readJSON(state, &claudeState); os.IsNotExist(err) {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("no trust record at %s, so hooks are skipped entirely", state)
	} else if err != nil {
		trust.Detail = fmt.Sprintf("cannot read %s: %v", state, err)
	} else if claudeState.Projects[dir].HasTrustDialogAccepted {
		trust.Status = Pass
		trust.Detail = "trusted in " + state
	} else {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("%s is not trusted in %s, so hooks are skipped entirely", dir, state)
	}

	gate := Finding{Engine: vocab.ClaudeCode, Check: "hooks enabled", Detail: settings}
	var claudeSettings struct {
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	if err := readJSON(settings, &claudeSettings); err != nil && !os.IsNotExist(err) {
		gate.Detail = fmt.Sprintf("%s: %v", settings, err)
	} else if claudeSettings.DisableAllHooks {
		gate.Status = Fail
		gate.Detail = "disableAllHooks is set in " + settings
	} else {
		gate.Status = Pass
		// The --settings overlay is a separate source hookyard cannot see from
		// here, and it can disable hooks on its own.
		gate.Detail = "not disabled in " + settings + " (a --settings overlay is not visible here)"
	}

	return []Finding{trust, gate, registration(vocab.ClaudeCode, settings), routerPath(vocab.ClaudeCode, settings)}
}

// Claude Code keeps per-directory trust in .claude.json, one entry per
// project. It sits inside the config dir when CLAUDE_CONFIG_DIR is set and
// beside it otherwise.
func claudeStatePath(configDir string) string {
	inside := filepath.Join(configDir, ".claude.json")
	if _, err := os.Stat(inside); err == nil {
		return inside
	}
	return filepath.Join(filepath.Dir(configDir), ".claude.json")
}

func codexFindings(p Paths, dir string) []Finding {
	config := filepath.Join(p.CodexHome, "config.toml")

	var codexConfig struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
		Hooks map[string]any `toml:"hooks"`
	}
	trust := Finding{Engine: vocab.Codex, Check: "workspace trust", Detail: config}
	hookTrust := Finding{Engine: vocab.Codex, Check: "hook trust", Detail: config}

	if _, err := toml.DecodeFile(config, &codexConfig); err != nil {
		trust.Detail = fmt.Sprintf("%s: %v", config, err)
		hookTrust.Detail = trust.Detail
	} else {
		if codexConfig.Projects[dir].TrustLevel == "trusted" {
			trust.Status = Pass
			trust.Detail = "trusted in " + config
		} else {
			trust.Status = Fail
			trust.Detail = fmt.Sprintf("%s is not trusted in %s, so hooks are not loaded", dir, config)
		}
		// Codex reviews each hook entry separately from trusting the directory,
		// and an untrusted entry is simply not run.
		if state, ok := codexConfig.Hooks["state"].(map[string]any); ok && len(state) > 0 {
			hookTrust.Status = Pass
			hookTrust.Detail = fmt.Sprintf("%d reviewed hook entries in %s", len(state), config)
		} else {
			hookTrust.Status = Fail
			hookTrust.Detail = "no reviewed hook entries in " + config + "; run codex and accept /hooks"
		}
	}

	return []Finding{trust, hookTrust, registration(vocab.Codex, config), routerPath(vocab.Codex, config)}
}

func cursorFindings(p Paths, dir string) []Finding {
	hooks := filepath.Join(p.CursorHome, "hooks.json")
	marker := filepath.Join(p.CursorHome, "projects", cursorProjectSlug(dir), ".workspace-trusted")

	trust := Finding{Engine: vocab.Cursor, Check: "workspace trust", Detail: marker}
	if _, err := os.Stat(marker); err == nil {
		trust.Status = Pass
		trust.Detail = "trusted, per " + marker
	} else {
		trust.Status = Fail
		trust.Detail = fmt.Sprintf("no trust marker at %s, so hooks are skipped entirely", marker)
	}

	return []Finding{trust, registration(vocab.Cursor, hooks), routerPath(vocab.Cursor, hooks)}
}

// cursorProjectSlug mirrors how Cursor names a project directory under
// ~/.cursor/projects: path separators become dashes, runs collapse, and the
// edges are trimmed.
var slugSeparators = regexp.MustCompile(`[^A-Za-z0-9]+`)

func cursorProjectSlug(dir string) string {
	return strings.Trim(slugSeparators.ReplaceAllString(dir, "-"), "-")
}

func registration(engine vocab.Engine, path string) Finding {
	f := Finding{Engine: engine, Check: "hookyard registered", Detail: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		f.Status = Fail
		f.Detail = fmt.Sprintf("%s: %v", path, err)
		return f
	}
	if strings.Contains(string(raw), render.Marker) {
		f.Status = Pass
		f.Detail = "present in " + path
		return f
	}
	f.Status = Fail
	f.Detail = "no hookyard entry in " + path + "; run hookyard install"
	return f
}

// routerPathPattern recovers the absolute path hookyard's own emitted
// command names, by scanning the raw config bytes rather than parsing three
// different config formats: render.command puts the identical string in all
// three verbatim (§9). The character class excludes both quote characters
// as well as whitespace: every encoder wraps the whole command in "…", so a
// quote is the delimiter, never part of the path, and checkShellSafe
// (cmd/hookyard/main.go) already guarantees a legitimate router path
// contains neither — the exclusion is exact, not heuristic. A whitespace-only
// boundary would recover the opening quote along with the path and fail the
// check on every engine, always (§9).
var routerPathPattern = regexp.MustCompile(`/[^\s"']*` + regexp.QuoteMeta(render.Marker))

// stateDirPattern recovers --state-dir from the same command line, in the
// same character class and for the same reason.
var stateDirPattern = regexp.MustCompile(`--state-dir\s+([^\s"']+)`)

// routerPath reports whether the absolute path this
// engine's emitted command names is actually there to exec. §9's closing
// argument is why doctor, not the event record, has to catch this: a broken
// profile symlink fails at exec, before any hookyard code runs, so there is
// nothing running to write a record for streamFindings to read.
func routerPath(engine vocab.Engine, configPath string) Finding {
	f := Finding{Engine: engine, Check: "router path", Detail: configPath}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", configPath, err)
		return f
	}
	paths := distinctStrings(routerPathPattern.FindAllString(string(raw), -1))
	if len(paths) == 0 {
		// registration() above already reports a missing hookyard entry as
		// Fail; a second Fail here for the same cause would be duplicate
		// noise.
		f.Status = Unknown
		f.Detail = "no hookyard entry in " + configPath + " to check"
		return f
	}

	// Every writer strips each row carrying the marker regardless of its
	// path, so hookyard's own re-render always leaves one form; a second
	// path can only come from a hand-edit or a foreign writer, and is
	// reported rather than collapsed.
	var bad []string
	for _, p := range paths {
		if reason := notExecutable(p); reason != "" {
			bad = append(bad, fmt.Sprintf("%s (%s)", p, reason))
		}
	}
	if len(bad) > 0 {
		f.Status = Fail
		f.Detail = "not runnable: " + strings.Join(bad, "; ")
		return f
	}
	f.Status = Pass
	f.Detail = strings.Join(paths, ", ") + ": executable"
	return f
}

// notExecutable reports why path cannot be run, or "" when it can. "Can be
// run" means path stats, following symlinks, to a regular file with any
// execute bit set — not identical to test -x, which asks about the calling
// user's effective access, but the two agree for the 0555 store targets §9
// enumerates, and the difference is not worth a unix.Access call that would
// cost portability.
func notExecutable(path string) string {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "missing"
	case info.IsDir():
		return "a directory"
	case !info.Mode().IsRegular() || info.Mode()&0o111 == 0:
		return "not executable"
	default:
		return ""
	}
}

// distinctStrings deduplicates a slice while preserving first-seen order.
func distinctStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// recoverStateDir scans all three engines' config files for --state-dir and
// returns the distinct values found, in first-seen order across Claude, then
// Codex, then Cursor.
//
// doctor's own state directory otherwise resolves from
// HOOKYARD_STATE_DIR/XDG_STATE_HOME/$HOME — the same untrusted chain the module
// refuses for install — so an overridden state directory makes the
// enforcement finding read a directory nothing writes to and report "no
// events recorded yet today" forever on a healthy machine: a silent false
// negative in the tool nominated to catch silent failures. The emitted
// config is ground truth for what the router actually uses, and it is
// already open in front of the router-path scan above.
func recoverStateDir(p Paths) []string {
	var found []string
	for _, path := range []string{
		filepath.Join(p.ClaudeConfigDir, "settings.json"),
		filepath.Join(p.CodexHome, "config.toml"),
		filepath.Join(p.CursorHome, "hooks.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, m := range stateDirPattern.FindAllStringSubmatch(string(raw), -1) {
			found = append(found, m[1])
		}
	}
	return distinctStrings(found)
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// streamFindings counts, in today's event record, how many verdicts hookyard
// computed but the engine had no way to enforce (§12) — an ask rendered to an
// engine that only accepts deny is expected, not a misconfiguration, so this
// is Pass whenever stateDir could be resolved unambiguously; only the count
// is informational. It is Fail when disagreement names more than one
// --state-dir recovered from the three engines' configs: runInstall writes
// Cursor, then Claude, then Codex, and a later failure does not undo an
// earlier write, so an activation aborted partway through a --state-dir
// change genuinely leaves two configs naming different tables.
// Silently picking one would reproduce exactly the false negative that
// recovery exists to remove. Engine is left at its zero value since the
// finding isn't per-engine (it renders as a blank column in hookyard
// doctor's %-12s output).
func streamFindings(stateDir string, disagreement []string, now time.Time) []Finding {
	f := Finding{Check: "enforcement"}

	if len(disagreement) > 0 {
		f.Status = Fail
		f.Detail = "engines disagree on --state-dir: " + strings.Join(disagreement, ", ")
		return []Finding{f}
	}

	path := record.StreamPath(stateDir, now)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		f.Status = Unknown
		f.Detail = "no events recorded yet today"
		return []Finding{f}
	}
	if err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("cannot read %s: %v", path, err)
		return []Finding{f}
	}
	defer func() { _ = file.Close() }()

	var total, enforcedFalse int
	scanner := bufio.NewScanner(file)
	// The default MaxScanTokenSize (64KiB) equals record's own maxRecordBytes
	// cap, so a max-size record would fail Scan with ErrTooLong; raise it well
	// above that cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var line struct {
			Enforced bool `json:"enforced"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		total++
		if !line.Enforced {
			enforcedFalse++
		}
	}
	if err := scanner.Err(); err != nil {
		f.Status = Unknown
		f.Detail = fmt.Sprintf("%s: %v (counted %d/%d before the error)", path, err, enforcedFalse, total)
		return []Finding{f}
	}

	f.Status = Pass
	f.Detail = fmt.Sprintf("%d/%d events today had a computed verdict the engine could not enforce", enforcedFalse, total)
	return []Finding{f}
}
