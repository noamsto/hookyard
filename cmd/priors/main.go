// Command priors is the memory-layer CLI: scoped, gated stores of facts
// that agents read at session start and write through a reviewed path.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
)

const usage = `usage: priors <command> [flags]

commands:
  add      write one fact through redaction, routing and gates
  list     list facts visible to this session
  show     print one fact, fenced
  search   search facts visible to this session
  lint     check a store against the lint rules and gates
  index    tier-1 session_start output; --write regenerates every MEMORY.md

With no command and an event envelope on stdin, priors runs as a hookyard
exec handler.
`

type streams struct {
	in       *os.File
	out, err io.Writer
}

func (s streams) errln(a ...any)          { _, _ = fmt.Fprintln(s.err, a...) }
func (s streams) errf(f string, a ...any) { _, _ = fmt.Fprintf(s.err, f, a...) }
func (s streams) errText(t string)        { _, _ = io.WriteString(s.err, t) }
func (s streams) outln(a ...any)          { _, _ = fmt.Fprintln(s.out, a...) }
func (s streams) outText(t string)        { _, _ = io.WriteString(s.out, t) }

func main() {
	os.Exit(run(os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr}))
}

func run(args []string, s streams) int {
	if len(args) == 0 {
		return cmdBare(s)
	}
	rest := args[1:]
	switch args[0] {
	case "add":
		return cmdAdd(rest, s)
	case "list":
		return cmdList(rest, s)
	case "show":
		return cmdShow(rest, s)
	case "search":
		return cmdSearch(rest, s)
	case "lint":
		return cmdLint(rest, s)
	case "index":
		return cmdIndex(rest, s)
	default:
		s.errText(usage)
		return 1
	}
}

func newFlagSet(name string, s streams) *flag.FlagSet {
	fs := flag.NewFlagSet("priors "+name, flag.ContinueOnError)
	fs.SetOutput(s.err)
	return fs
}

// parseFlags parses args with flags and positional arguments interleaved. A
// false ok means the command must return code: a usage error, or -h.
func parseFlags(fs *flag.FlagSet, args []string) (positional []string, code int, ok bool) {
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, 1, false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, 0, true
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func loadConfig(path string) (config.Config, error) {
	if path == "" {
		path = config.DefaultPath()
	}
	return config.Load(path)
}

// identity is who is running a command (spec §3).
type identity struct {
	cwd, engine, session, host string
}

func resolveIdentity(cwd, engine, session string) (identity, error) {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return identity{}, err
		}
		cwd = wd
	}
	claude := os.Getenv("CLAUDECODE") == "1"
	if engine == "" {
		engine = os.Getenv("PRIORS_ENGINE")
	}
	if engine == "" {
		engine = "unknown"
		if claude {
			engine = "claude"
		}
	}
	if session == "" {
		session = os.Getenv("PRIORS_SESSION")
	}
	if session == "" && claude && engine == "claude" {
		session = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return identity{cwd: cwd, engine: engine, session: session, host: host}, nil
}

func sessionAt(ctx context.Context, cwd string, cfg config.Config) route.Session {
	return route.Resolve(ctx, cwd, cfg, route.Resolver{})
}
