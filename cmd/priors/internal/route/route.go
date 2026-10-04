// Package route classifies a session's repo by its git remotes and decides
// which stores it reads and where its facts are written.
package route

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

type Class string

const (
	ClassPersonal     Class = "personal"
	ClassWork         Class = "work"
	ClassUnresolvable Class = "unresolvable"
	ClassNoRepo       Class = "no-repo"
)

type StoreID string

const (
	StorePersonal StoreID = "personal"
	StoreWork     StoreID = "work"
)

// Session is the repo a session runs in. Repo is empty outside a repo; Dir is
// the git toplevel.
type Session struct {
	Class     Class
	Repo, Dir string
}

// Resolver maps an SSH host alias to the hostname it connects to. A nil
// SSHHost runs ssh -G.
type Resolver struct {
	SSHHost func(ctx context.Context, alias string) string
}

const (
	gitTimeout = 5 * time.Second
	sshTimeout = 2 * time.Second
)

// ParseURL splits a git remote URL into its lower-cased host, owner and repo.
// sshLike reports whether the host is an SSH host, and so possibly an alias.
func ParseURL(raw string) (host, owner, repo string, sshLike bool, err error) {
	var path string
	if scheme, _, ok := strings.Cut(raw, "://"); ok {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", "", false, perr
		}
		switch strings.ToLower(scheme) {
		case "https", "http", "git":
		case "ssh", "git+ssh", "ssh+git":
			sshLike = true
		default:
			return "", "", "", false, fmt.Errorf("unsupported remote scheme %q", scheme)
		}
		host, path = u.Hostname(), u.Path
	} else {
		// git reads host:path as scp-like only when the colon comes before any
		// slash; anything else is a local path.
		hostPart, rest, ok := strings.Cut(raw, ":")
		if !ok || strings.Contains(hostPart, "/") {
			return "", "", "", false, fmt.Errorf("not a network remote: %q", raw)
		}
		if i := strings.LastIndex(hostPart, "@"); i >= 0 {
			hostPart = hostPart[i+1:]
		}
		host, path, sshLike = hostPart, rest, true
	}
	// A leading dash would reach ssh -G as an option.
	if host == "" || strings.HasPrefix(host, "-") {
		return "", "", "", false, fmt.Errorf("remote %q has no usable host", raw)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := slices.DeleteFunc(strings.Split(path, "/"), func(s string) bool { return s == "" })
	if len(segs) < 2 {
		return "", "", "", false, errors.New("remote path needs owner/repo")
	}
	return strings.ToLower(host), strings.ToLower(segs[0]), strings.ToLower(segs[len(segs)-1]), sshLike, nil
}

type remote struct {
	hosts       []string
	owner, repo string
}

// matches reports whether r is one of entries ("host/owner"). An alias counts
// under its literal host as well as its resolved one: ssh -G may rewrite a real
// host (github.com → ssh.github.com), and that must not unmatch the org.
func (r remote) matches(entries []string) bool {
	for _, e := range entries {
		host, owner, _ := strings.Cut(e, "/")
		if owner == r.owner && slices.Contains(r.hosts, host) {
			return true
		}
	}
	return false
}

// Resolve classifies the repo containing cwd (spec §6).
func Resolve(ctx context.Context, cwd string, cfg config.Config, r Resolver) Session {
	dir, err := gitOut(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		// Only git's own verdict means no repo: a repo git cannot read (broken
		// config, safe.directory, timeout, no git) must not write as personal.
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "not a git repository") {
			return Session{Class: ClassNoRepo}
		}
		return Session{Class: ClassUnresolvable}
	}
	if dir == "" {
		return Session{Class: ClassUnresolvable}
	}
	sshHost := r.SSHHost
	if sshHost == nil {
		sshHost = func(ctx context.Context, alias string) string { return defaultSSHHost(ctx, cfg.SSHConfig, alias) }
	}
	resolved := map[string]string{}
	parse := func(raw string) (remote, error) {
		host, owner, repo, sshLike, err := ParseURL(raw)
		if err != nil {
			return remote{}, err
		}
		rem := remote{hosts: []string{host}, owner: owner, repo: repo}
		if sshLike {
			target, ok := resolved[host]
			if !ok {
				target = sshHost(ctx, host)
				resolved[host] = target
			}
			if target != host {
				rem.hosts = append(rem.hosts, target)
			}
		}
		return rem, nil
	}

	var all, originAll []remote
	var origin remote
	hasOrigin, opaque := false, false
	raws, err := rawURLs(ctx, dir)
	if err != nil {
		opaque = true
	}
	names, _ := gitOut(ctx, dir, "remote")
	for name := range strings.FieldsSeq(names) {
		fetch, err := gitOut(ctx, dir, "remote", "get-url", "--all", name)
		if err != nil {
			opaque = true
		}
		push, err := gitOut(ctx, dir, "remote", "get-url", "--push", "--all", name)
		if err != nil {
			opaque = true
		}
		// The first fetch URL is the one git fetches from; it alone decides the
		// repo name and whether the rewritten URL is unreadable.
		urls := strings.Split(fetch, "\n")
		urls = append(urls, strings.Split(push, "\n")...)
		rawStart := len(urls)
		urls = append(urls, raws[name]...)
		for i, raw := range urls {
			rem, err := parse(raw)
			if err != nil {
				// A raw value is what the repo's own config names, so one priors
				// cannot read could hide a work org behind an insteadOf rewrite.
				// A colon-less value is a local path to git. Other unparsable
				// rewritten URLs name no org they could be matched by.
				isRaw := i >= rawStart
				if (i == 0 || isRaw && strings.Contains(raw, ":")) && !isLocalPath(raw) {
					opaque = true
				}
				continue
			}
			if name == "origin" {
				originAll = append(originAll, rem)
				if i == 0 {
					origin, hasOrigin = rem, true
				}
			}
			all = append(all, rem)
		}
	}

	s := Session{Dir: dir}
	if hasOrigin {
		s.Repo = repoName(origin.repo)
	} else {
		s.Repo = repoName(commonDirName(ctx, dir))
	}
	switch {
	case slices.ContainsFunc(all, func(rem remote) bool { return rem.matches(cfg.WorkOrgs) }):
		s.Class = ClassWork
	case !hasOrigin, opaque:
		s.Class = ClassUnresolvable
	case slices.ContainsFunc(all, func(rem remote) bool {
		return !rem.matches(cfg.PersonalOrgs) && isWorkOwner(rem.owner, cfg.WorkOrgs)
	}):
		s.Class = ClassUnresolvable
	case !slices.ContainsFunc(originAll, func(rem remote) bool { return !rem.matches(cfg.PersonalOrgs) }):
		s.Class = ClassPersonal
	case cfg.Profile == "work":
		s.Class = ClassUnresolvable
	default:
		s.Class = ClassPersonal
	}
	return s
}

// rawURLs returns every remote.<name>.url and .pushurl value as configured,
// before insteadOf rewriting, keyed by remote name.
func rawURLs(ctx context.Context, dir string) (map[string][]string, error) {
	out, err := gitOut(ctx, dir, "config", "-z", "--get-regexp", `^remote\..*\.(url|pushurl)$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	raws := map[string][]string{}
	for rec := range strings.SplitSeq(out, "\x00") {
		key, value, _ := strings.Cut(rec, "\n")
		rest, ok := strings.CutPrefix(key, "remote.")
		if !ok {
			continue
		}
		// A remote name may contain dots; the variable follows the last one.
		name := rest[:strings.LastIndex(rest, ".")]
		raws[name] = append(raws[name], value)
	}
	return raws, nil
}

// isLocalPath reports whether an unparsable remote URL is plainly a path on
// this host, which says nothing about whose code the repo holds.
func isLocalPath(raw string) bool {
	for _, p := range []string{"/", "./", "../", "file://"} {
		if strings.HasPrefix(raw, p) {
			return true
		}
	}
	return false
}

func isWorkOwner(owner string, workOrgs []string) bool {
	for _, e := range workOrgs {
		if _, o, _ := strings.Cut(e, "/"); o == owner {
			return true
		}
	}
	return false
}

// commonDirName names the repo after the directory holding its common git dir,
// so a linked worktree names the main repo rather than itself.
func commonDirName(ctx context.Context, dir string) string {
	common, err := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return filepath.Base(dir)
	}
	return filepath.Base(filepath.Dir(common))
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; only the directory and a remote name vary
	// Resolve matches git's English "not a git repository".
	cmd.Env = RepoEnv("LC_ALL=C")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// RepoEnv is os.Environ plus extra, minus RepoLocatingEnv: left in place, the
// variables git exports into a hook's environment would point every
// `git -C dir` at that hook's repository instead of dir.
func RepoEnv(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(RepoLocatingEnv, k)
	})
	return append(env, extra...)
}

// RepoLocatingEnv mirrors `git rev-parse --local-env-vars`, plus the
// namespace and quarantine variables a hook can also inherit.
var RepoLocatingEnv = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_QUARANTINE_PATH",
}

func defaultSSHHost(ctx context.Context, sshConfig, alias string) string {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	args := []string{"-G"}
	if sshConfig != "" {
		args = append(args, "-F", sshConfig)
	}
	out, err := exec.CommandContext(ctx, "ssh", append(args, alias)...).Output() //nolint:gosec // ssh -G only prints config; ParseURL rejects a host that would parse as an option
	if err != nil {
		return alias
	}
	for line := range strings.Lines(string(out)) {
		if h, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.ToLower(strings.TrimSpace(h))
		}
	}
	return alias
}

// ReadStores is the ordered list of stores a session reads (spec §6).
func ReadStores(s Session, cfg config.Config) []StoreID {
	if s.Class == ClassWork && cfg.WorkPresent() {
		return []StoreID{StoreWork, StorePersonal}
	}
	return []StoreID{StorePersonal}
}

// Dest is where a session's fact is written: a store, or the host-local
// quarantine with the reason why.
type Dest struct {
	Store      StoreID
	Quarantine bool
	Why        string
}

// WriteDest applies the §4.2 write table.
func WriteDest(s Session, cfg config.Config) Dest {
	if s.Class == ClassPersonal {
		return Dest{Store: StorePersonal}
	}
	if cfg.Profile == "work" {
		if cfg.WorkPresent() {
			return Dest{Store: StoreWork}
		}
		return Dest{Quarantine: true, Why: "work clone missing"}
	}
	switch s.Class {
	case ClassWork:
		return Dest{Quarantine: true, Why: "work repo on a personal host"}
	case ClassUnresolvable:
		return Dest{Quarantine: true, Why: "unresolvable repo on a personal host"}
	default:
		return Dest{Store: StorePersonal}
	}
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// repoName folds a repo name into the fact-name alphabet, since it becomes a
// repos: entry and a store directory name.
func repoName(raw string) string {
	name := strings.Trim(nonName.ReplaceAllString(strings.ToLower(raw), "-"), "-")
	if len(name) > 81 {
		name = strings.TrimRight(name[:81], "-")
	}
	if name == "" {
		return "repo"
	}
	return name
}
