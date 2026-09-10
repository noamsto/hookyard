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
	StateDir        string
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
	stateDir, err := record.DefaultStateDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{ClaudeConfigDir: claude, CodexHome: codex, CursorHome: filepath.Join(home, ".cursor"), StateDir: stateDir}, nil
}

// Run reports on each engine for one working directory.
func Run(p Paths, dir string) []Finding {
	var findings []Finding
	findings = append(findings, claudeFindings(p, dir)...)
	findings = append(findings, codexFindings(p, dir)...)
	findings = append(findings, cursorFindings(p, dir)...)
	findings = append(findings, streamFindings(p.StateDir, time.Now())...)
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

	return []Finding{trust, gate, registration(vocab.ClaudeCode, settings)}
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

	return []Finding{trust, hookTrust, registration(vocab.Codex, config)}
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

	return []Finding{trust, registration(vocab.Cursor, hooks)}
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
// is always Pass; only the count is informational. Engine is left at its zero
// value since the finding isn't per-engine (it renders as a blank column in
// hookyard doctor's %-12s output).
func streamFindings(stateDir string, now time.Time) []Finding {
	f := Finding{Check: "enforcement"}

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
