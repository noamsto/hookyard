// Command hookyard registers agent hooks once and routes them to every engine.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/hookyard/internal/doctor"
	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/render"
	"github.com/noamsto/hookyard/internal/vocab"
)

const usage = `hookyard — register agent hooks once, route them to every coding agent

  hookyard install   render every manifest into all three engines' native config
  hookyard validate  check manifests without writing anything
  hookyard doctor    report whether each engine will actually run the hooks
  hookyard route     dispatch one hook event (not yet implemented)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = install(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "route":
		err = route(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hookyard: %v\n", err)
		os.Exit(1)
	}
}

// manifestPaths collects a repeatable --manifest flag. hookyard is invoked
// once per activation with every manifest, never once per repo: the strip is
// keyed on a marker that does not record which manifest produced a row, so a
// second per-repo invocation would strip the first repo's rows (§8).
type manifestPaths []string

func (m *manifestPaths) String() string { return strings.Join(*m, ",") }

func (m *manifestPaths) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type targets struct {
	claude string
	codex  string
	cursor string
}

func defaultTargets() (targets, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return targets{}, err
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return targets{
		claude: filepath.Join(claudeDir, "settings.json"),
		codex:  filepath.Join(codexHome, "config.toml"),
		cursor: filepath.Join(home, ".cursor", "hooks.json"),
	}, nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	defaults, err := defaultTargets()
	if err != nil {
		return err
	}
	routerPath := fs.String("router-path", "", "absolute path hookyard is invoked by (defaults to this binary)")
	claude := fs.String("claude-settings", defaults.claude, "Claude Code settings.json to write")
	codex := fs.String("codex-config", defaults.codex, "Codex config.toml to write")
	cursor := fs.String("cursor-hooks", defaults.cursor, "Cursor hooks.json to write")
	dryRun := fs.Bool("dry-run", false, "print what would be written and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no --manifest given")
	}

	handlers, err := loadAll(paths)
	if err != nil {
		return err
	}
	router := *routerPath
	if router == "" {
		router, err = os.Executable()
		if err != nil {
			return err
		}
	}
	plan, err := render.BuildPlan(handlers, router)
	if err != nil {
		return err
	}

	if *dryRun {
		printPlan(plan)
		return nil
	}
	// Each writer is independent, so a later failure does not undo an earlier
	// write. They are ordered least to most consequential: Codex last, because
	// its file also holds the trust stores.
	if err := render.WriteCursor(*cursor, plan[vocab.Cursor]); err != nil {
		return err
	}
	if err := render.WriteClaude(*claude, plan[vocab.ClaudeCode]); err != nil {
		return err
	}
	if err := render.WriteCodex(*codex, plan[vocab.Codex]); err != nil {
		return err
	}
	for _, engine := range vocab.Engines {
		fmt.Printf("%-12s %d entries\n", engine, len(plan[engine]))
	}
	return nil
}

func validate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var paths manifestPaths
	fs.Var(&paths, "manifest", "path to a handler manifest (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no --manifest given")
	}
	handlers, err := loadAll(paths)
	if err != nil {
		return err
	}
	fmt.Printf("%d manifests, %d handlers, no problems found\n", len(paths), len(handlers))
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to report on (defaults to the working directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		target, _ = os.Getwd()
	}
	paths, err := doctor.DefaultPaths()
	if err != nil {
		return err
	}

	fmt.Printf("hookyard doctor — %s\n\n", target)
	problems := 0
	for _, f := range doctor.Run(paths, target) {
		if f.Status == doctor.Fail {
			problems++
		}
		fmt.Printf("%-12s %-20s %-8s %s\n", f.Engine, f.Check, f.Status, f.Detail)
	}
	if problems == 1 {
		return fmt.Errorf("1 problem found")
	}
	if problems > 1 {
		return fmt.Errorf("%d problems found", problems)
	}
	return nil
}

// route is the event path, which lands in its own change. It exits 0 rather
// than erroring so an installed-but-incomplete hookyard fails open, matching
// what §5 commits the router itself to doing.
func route(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	fs.String("registered-for", "", "engine this invocation was registered for")
	fs.String("event", "", "canonical or engine-scoped event name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "hookyard: the router is not implemented yet; allowing this call")
	return nil
}

func loadAll(paths []string) ([]manifest.Handler, error) {
	manifests := make([]*manifest.Manifest, 0, len(paths))
	for _, p := range paths {
		m, err := manifest.Load(p)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifest.Merge(manifests)
}

func printPlan(plan render.Plan) {
	for _, engine := range vocab.Engines {
		fmt.Printf("%s\n", engine)
		if len(plan[engine]) == 0 {
			fmt.Println("  (nothing)")
			continue
		}
		for _, e := range plan[engine] {
			matcher := e.Matcher
			if matcher == "" {
				matcher = "(every tool)"
			}
			fmt.Printf("  %-22s %-24s %s\n", e.Event, matcher, e.Command)
		}
	}
}
