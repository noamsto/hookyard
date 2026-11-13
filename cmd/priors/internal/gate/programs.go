package gate

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

var (
	// flagFree only read their operands, and no flag in their manual writes a
	// file or runs a program, so every flag is allowed. printf -v sets a
	// variable, which the pre-pass reads; priors writes a store only through
	// its own gates; crew writes only its own crew dir.
	flagFree = []string{
		"cat", "head", "tail", "wc", "ls", "stat", "readlink", "realpath", "basename", "dirname",
		"cmp", "du", "df", "md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum",
		"b2sum", "cksum", "echo", "test", "[", "true", "false", ":", "pwd", "which", "whereis",
		"type", "nl", "od", "hexdump", "strings", "cut", "tr", "comm", "join", "paste", "fold", "rev",
		"tac", "id", "whoami", "uname", "column", "grep", "egrep", "fgrep", "jq", "cd", "pushd",
		"popd", "printf", "priors", "crew",
	}
	// builtins write no file: the variables they set are the pre-pass's, and
	// their redirects are targets like any call's.
	builtins = []string{
		"set", "shift", "exit", "return", "break", "continue", "export", "local", "declare", "typeset",
		"readonly", "unset", "read", "mapfile", "readarray", "getopts", "let", "shopt", "wait", "umask",
		"sleep",
	}
	// scriptShells run a script: their -c value, a file, or stdin.
	scriptShells = []string{"bash", "sh", "zsh", "dash", "ksh", "ash"}
	awks         = []string{"awk", "gawk", "mawk", "nawk"}
	// gitReaders leave the work tree as it is: they read, or write only git's
	// own index, objects and refs. commit is listed for its option checks but
	// classify treats it as a write in its repo.
	gitReaders = []string{
		"status", "log", "show", "diff", "ls-files", "ls-tree", "rev-parse", "cat-file", "grep",
		"blame", "shortlog", "describe", "rev-list", "show-ref", "for-each-ref", "merge-base",
		"name-rev", "add", "commit", "fetch", "push",
	}
	// gitValueOpts and gitFlagOpts are the options git takes before its
	// subcommand that set no config and run no program; gitGlueOpts take their
	// value after an =.
	gitValueOpts = []string{"-C", "--git-dir", "--work-tree", "--namespace"}
	gitGlueOpts  = []string{"--git-dir=", "--work-tree=", "--namespace="}
	gitFlagOpts  = []string{
		"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects", "--literal-pathspecs",
		"--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--bare",
		"--no-optional-locks", "--no-advice",
	}
	// gitRunOpts are the options after the subcommand that write a file or run a
	// program; git takes any unambiguous prefix of a long option.
	gitRunOpts = []string{"output", "exec", "upload-pack", "receive-pack", "open-files-in-pager", "ext-diff", "textconv"}
	// ghGroups and glabGroups are the forge groups and verbs that write no
	// local file; a nil list takes any verb, or none.
	ghGroups = map[string][]string{
		"pr":       {"list", "view", "create", "edit", "comment", "review", "close", "reopen", "diff", "checks", "status", "ready", "lock", "unlock", "update-branch"},
		"issue":    {"list", "view", "create", "edit", "comment", "close", "reopen", "status", "lock", "unlock", "pin", "unpin", "transfer", "delete"},
		"repo":     {"view", "list", "create", "edit", "archive", "unarchive", "delete", "deploy-key", "gitignore", "license"},
		"api":      nil,
		"run":      {"list", "view", "watch", "rerun", "cancel", "delete"},
		"release":  {"list", "view", "create", "edit", "delete", "delete-asset", "upload"},
		"search":   nil,
		"workflow": {"list", "view", "run", "enable", "disable"},
		"label":    nil,
		"status":   nil,
		"cache":    {"list", "delete"},
		"secret":   {"list", "set", "delete"},
		"variable": {"list", "get", "set", "delete"},
		"ruleset":  {"list", "view", "check"},
		"gist":     {"list", "view", "create", "delete"},
		"auth":     {"status"},
	}
	glabGroups = map[string][]string{
		"mr":      {"list", "view", "create", "note", "approve", "close", "reopen", "update", "diff"},
		"issue":   {"list", "view", "create", "note", "close", "reopen", "update"},
		"api":     nil,
		"repo":    {"view", "list"},
		"ci":      {"list", "view", "status"},
		"release": {"list", "view"},
	}
	findValue = []string{
		"-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-regex", "-iregex",
		"-regextype", "-type", "-xtype", "-size", "-mtime", "-mmin", "-atime", "-amin", "-ctime",
		"-cmin", "-newer", "-anewer", "-cnewer", "-perm", "-user", "-group", "-uid", "-gid",
		"-links", "-inum", "-samefile", "-fstype", "-lname", "-ilname", "-maxdepth", "-mindepth",
		"-printf",
	}
	findFlag = []string{
		"-nouser", "-nogroup", "-empty", "-readable", "-writable", "-executable", "-depth", "-d",
		"-mount", "-xdev", "-prune", "-quit", "-print", "-print0", "-ls", "-true", "-false", "-not",
		"-a", "-and", "-o", "-or", "-follow", "-noleaf", "-ignore_readdir_race",
		"-noignore_readdir_race", "-daystart", "-warn", "-nowarn", "!", "(", ")", ",",
	}
	findExec = []string{"-exec", "-execdir", "-ok", "-okdir"}
)

// flags describes a program's options (Appendix T): short letters that take
// no value, the rest of their cluster or the next word, or an optional glued
// rest; long names likewise; and the names it does not allow.
type flags struct {
	short, value, optional        string
	long, longValue, longOptional []string
	bad                           string
	badLong                       []string
	// any allows every letter and long name that is not bad, with no value.
	any bool
	// permute reads options anywhere before --, as GNU getopt does; else
	// the first operand ends them.
	permute bool
	// loose reads an unresolved word as an operand, for a writer whose every
	// operand is a target anyway.
	loose bool
	// operand reads a word starting with - as an operand (chmod's -x).
	operand func(string) bool
}

// hit is one option read: its short letter or full long name, and its value.
type hit struct {
	name string
	val  *arg
}

// read reads args from from as f describes, and returns the options, the
// indexes of the operands, and where a non-permuting read stopped; ok is
// false for an option f does not allow, or a word it cannot tell from one.
func (f *flags) read(args []arg, from int) (hits []hit, operands []int, next int, ok bool) {
	if f == nil {
		return nil, nil, 0, false
	}
	i := from
	stop := func() (bool, int) {
		if f.permute {
			operands = append(operands, i)
			return false, i + 1
		}
		return true, i
	}
	for i < len(args) {
		a := args[i]
		next := func() *arg {
			if i+1 < len(args) {
				i++
				return &args[i]
			}
			return nil
		}
		switch {
		case !a.ok:
			switch {
			case f.loose, a.text != "" && a.text[0] != '-', !f.permute && a.text == "":
				if done, at := stop(); done {
					return hits, operands, at, true
				}
				i++
				continue
			case strings.HasPrefix(a.text, "--") && strings.Contains(a.text, "="):
				name, _, _ := strings.Cut(a.text[2:], "=")
				full, kind, known := f.long1(name)
				if !known || kind == 0 {
					return nil, nil, 0, false
				}
				hits = append(hits, hit{full, &arg{word: a.word, src: a.src}})
			default:
				return nil, nil, 0, false
			}
		case a.text == "--":
			if !f.permute {
				return hits, operands, i + 1, true
			}
			for k := i + 1; k < len(args); k++ {
				operands = append(operands, k)
			}
			return hits, operands, len(args), true
		case len(a.text) < 2 || a.text[0] != '-' || f.operand != nil && f.operand(a.text):
			if done, at := stop(); done {
				return hits, operands, at, true
			}
			i++
			continue
		case strings.HasPrefix(a.text, "--"):
			name, v, glued := strings.Cut(a.text[2:], "=")
			full, kind, known := f.long1(name)
			switch {
			case !known:
				return nil, nil, 0, false
			case kind == 1 && glued:
				hits = append(hits, hit{full, &arg{text: v, ok: true, word: a.word, src: a.src}})
			case kind == 1:
				hits = append(hits, hit{full, next()})
			case kind == 2 && glued:
				hits = append(hits, hit{full, &arg{text: v, ok: true, word: a.word, src: a.src}})
			case glued && !f.any:
				return nil, nil, 0, false
			default:
				hits = append(hits, hit{name: full})
			}
		default:
			for k := 1; k < len(a.text); k++ {
				c := a.text[k]
				letter := string(c)
				rest := a.text[k+1:]
				switch {
				case strings.IndexByte(f.bad, c) >= 0:
					return nil, nil, 0, false
				case strings.IndexByte(f.value, c) >= 0:
					v := &arg{text: rest, ok: true, word: a.word, src: a.src}
					if rest == "" {
						v = next()
					}
					hits = append(hits, hit{letter, v})
					k = len(a.text)
				case strings.IndexByte(f.optional, c) >= 0:
					var v *arg
					if rest != "" {
						v = &arg{text: rest, ok: true, word: a.word, src: a.src}
					}
					hits = append(hits, hit{letter, v})
					k = len(a.text)
				case strings.IndexByte(f.short, c) >= 0, f.any:
					hits = append(hits, hit{name: letter})
				default:
					return nil, nil, 0, false
				}
			}
		}
		i++
	}
	return hits, operands, i, true
}

// long1 matches a long option name as getopt_long does: exactly, or by a
// prefix of exactly one listed name. kind is 0 for no value, 1 for a value,
// 2 for an optional glued one. A name not listed is known only under any.
func (f *flags) long1(name string) (full string, kind int, known bool) {
	if name == "" {
		return "", 0, false
	}
	lists := [][]string{f.long, f.longValue, f.longOptional, f.badLong}
	var matches []string
	kinds := []int{}
	for k, list := range lists {
		for _, l := range list {
			switch {
			case l == name:
				return l, k, k != 3
			case strings.HasPrefix(l, name):
				matches = append(matches, l)
				kinds = append(kinds, k)
			}
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], kinds[0], kinds[0] != 3
	case len(matches) == 0 && f.any:
		return name, 0, true
	}
	return "", 0, false
}

// call is one simple command seen through its wrappers (§4.5).
type call struct {
	argv []arg
	// at is the inner command word's index, len(argv) when there is none.
	at int
	// lost reports a wrapper the reader cannot read: an unrecognised flag, an
	// option word that does not render, or a path-invoked wrapper.
	lost bool
	// assigns are env's and sudo's NAME=value words.
	assigns []arg
	// dirs are env -C and sudo -D values.
	dirs []arg
	// split is env -S's value, which runs as a command line.
	split *arg
	// listing reports command -v or -V, which print.
	listing bool
	// xargs reports operands read from stdin; replace is its -I string.
	xargs   bool
	replace string
}

var wrapperFlags = map[string]*flags{
	"env": {
		short: "i0v", value: "uCS",
		long:         []string{"ignore-environment", "null", "debug", "list-signal-handling"},
		longValue:    []string{"unset", "chdir", "split-string"},
		longOptional: []string{"block-signal", "default-signal", "ignore-signal"},
		badLong:      []string{"help", "version"},
	},
	"sudo": {
		short: "nEHkPSABb", value: "ugpCThrtD",
		long:         []string{"non-interactive", "set-home", "reset-timestamp", "preserve-groups", "stdin", "askpass", "bell", "background"},
		longValue:    []string{"user", "group", "prompt", "close-from", "command-timeout", "host", "role", "type", "chdir"},
		longOptional: []string{"preserve-env"},
		badLong:      []string{"shell", "login", "edit", "list", "validate", "remove-timestamp", "chroot", "other-user", "help", "version"},
	},
	"doas":    {short: "n", value: "u"},
	"nice":    {value: "n", longValue: []string{"adjustment"}, badLong: []string{"help", "version"}},
	"nohup":   {badLong: []string{"help", "version"}},
	"timeout": {short: "v", value: "ks", long: []string{"preserve-status", "foreground", "verbose"}, longValue: []string{"kill-after", "signal"}, badLong: []string{"help", "version"}},
	"stdbuf":  {value: "ioe", longValue: []string{"input", "output", "error"}, badLong: []string{"help", "version"}},
	"time":    {short: "p"},
	"command": {short: "pvV"},
	"builtin": {},
	"exec":    {short: "cl", value: "a"},
	"xargs": {
		short: "0rtpxo", value: "nLPsdEaI", optional: "iel",
		long:         []string{"null", "no-run-if-empty", "verbose", "interactive", "exit", "open-tty"},
		longValue:    []string{"max-args", "max-procs", "max-chars", "delimiter", "arg-file"},
		longOptional: []string{"replace", "eof", "max-lines"},
		badLong:      []string{"process-slot-var", "show-limits", "help", "version"},
	},
}

// unwrap skips argv's wrappers with their closed flag tables.
func unwrap(argv []arg) call {
	c := call{argv: argv, at: len(argv)}
	for i := 0; i < len(argv); {
		a := argv[i]
		f, wrapper := wrapperFlags[a.text]
		if !a.ok || !wrapper {
			if a.ok && strings.Contains(a.text, "/") && wrapperFlags[wordName(a.text)] != nil {
				c.lost = true
				return c
			}
			c.at = i
			return c
		}
		i++
		if a.text == "env" && i < len(argv) && argv[i].ok && argv[i].text == "-" {
			i++
		}
		if a.text == "nice" && i < len(argv) && argv[i].ok && len(argv[i].text) > 1 && argv[i].text[0] == '-' && digits(argv[i].text[1:]) {
			i++
		}
		hits, _, next, ok := f.read(argv, i)
		if !ok {
			c.lost = true
			return c
		}
		i = next
		for _, h := range hits {
			switch {
			case a.text == "env" && (h.name == "C" || h.name == "chdir"), a.text == "sudo" && (h.name == "D" || h.name == "chdir"):
				if h.val != nil {
					c.dirs = append(c.dirs, *h.val)
				}
			case a.text == "env" && (h.name == "S" || h.name == "split-string"):
				if h.val == nil {
					c.lost = true
					return c
				}
				c.split = h.val
			case a.text == "command" && (h.name == "v" || h.name == "V"):
				c.listing = true
			case a.text == "xargs" && (h.name == "I" || h.name == "i" || h.name == "replace"):
				c.replace = "{}"
				if h.val != nil {
					if !h.val.ok {
						c.lost = true
						return c
					}
					c.replace = h.val.text
				}
			}
		}
		switch a.text {
		case "env", "sudo":
			for i < len(argv) && strings.Contains(argv[i].text, "=") {
				c.assigns = append(c.assigns, argv[i])
				i++
			}
			if c.split != nil {
				c.at = i
				return c
			}
		case "timeout":
			i++
		case "xargs":
			c.xargs = true
		}
		if c.listing {
			c.at = i
			return c
		}
	}
	return c
}

// verdict is a simple command's class (§4.2).
type verdict struct {
	kind kind
	// targets are a modeled writer's target words.
	targets []arg
	// why names an unresolved write the model finds.
	why string
}

type kind int

const (
	readKind kind = iota
	writeKind
	unknownKind
)

// classify classifies the command name with its arguments args.
func classify(name string, args []arg) verdict {
	unknown := verdict{kind: unknownKind}
	switch {
	case slices.Contains(flagFree, name), slices.Contains(builtins, name):
		return verdict{}
	case slices.Contains(awks, name):
		if awkReads(args) {
			return verdict{}
		}
		return unknown
	case writerFlags[name] != nil || name == "dd":
		return writer(name, args)
	}
	switch name {
	case "date", "diff", "rg":
		if _, _, _, ok := shapes[name].read(args, 0); ok {
			return verdict{}
		}
		return unknown
	case "sort":
		hits, _, _, ok := shapes["sort"].read(args, 0)
		if !ok {
			return unknown
		}
		var targets []arg
		for _, h := range hits {
			if (h.name == "o" || h.name == "output") && h.val != nil {
				targets = append(targets, *h.val)
			}
		}
		if targets != nil {
			return verdict{kind: writeKind, targets: targets}
		}
		return verdict{}
	case "uniq":
		_, operands, _, ok := shapes["uniq"].read(args, 0)
		switch {
		case !ok, len(operands) > 2:
			return unknown
		case len(operands) == 2:
			return verdict{kind: writeKind, targets: []arg{args[operands[1]]}}
		}
		return verdict{}
	case "sed":
		return sed(args)
	case "fd":
		if slices.ContainsFunc(args, func(a arg) bool { return fdRuns(text(a)) }) {
			return verdict{kind: unknownKind, why: "runs fd with --exec"}
		}
		if _, _, _, ok := shapes["fd"].read(args, 0); ok {
			return verdict{}
		}
		return unknown
	case "find":
		if why := findWhy(args); why != "" {
			return verdict{kind: unknownKind, why: why}
		}
		return verdict{}
	case "git":
		texts, heads := strs(args)
		if gitReads(texts, heads) {
			if sub, _ := gitSub(texts); sub < len(texts) && texts[sub] == "commit" {
				return verdict{kind: writeKind}
			}
			return verdict{}
		}
		if gitWorktree(texts) {
			return verdict{}
		}
		return unknown
	case "gh":
		return forge(args, ghGroups, true)
	case "glab":
		return forge(args, glabGroups, false)
	}
	return unknown
}

// text is a's rendered text, "" for one that does not render.
func text(a arg) string {
	if !a.ok {
		return ""
	}
	return a.text
}

// strs returns args' texts, "" for one that does not render, and their
// heads: the text, or an unrendered word's literal prefix.
func strs(args []arg) (texts, heads []string) {
	texts, heads = make([]string, len(args)), make([]string, len(args))
	for i, a := range args {
		texts[i], heads[i] = text(a), a.text
	}
	return texts, heads
}

// shapes are the read-only programs whose flags are a closed table (T2).
var shapes = map[string]*flags{
	"date": {any: true, permute: true, bad: "s", badLong: []string{"set"}, value: "dfr", optional: "I", longValue: []string{"date", "file", "reference", "rfc-3339"}, longOptional: []string{"iso-8601"}},
	"diff": {
		any: true, permute: true, bad: "l", badLong: []string{"paginate"}, value: "CUFIxXSDLW",
		longValue: []string{
			"show-function-line", "ignore-matching-lines", "exclude", "exclude-from", "starting-file", "ifdef", "label",
			"width", "line-format", "old-line-format", "new-line-format", "unchanged-line-format", "old-group-format",
			"new-group-format", "changed-group-format", "unchanged-group-format", "horizon-lines", "tabsize", "palette",
			"from-file", "to-file",
		},
		longOptional: []string{"context", "unified", "color"},
	},
	"sort": {
		permute: true, short: "bdfghiMnRrVcCmsuz", value: "ktSo", bad: "T",
		long: []string{
			"ignore-leading-blanks", "dictionary-order", "ignore-case", "general-numeric-sort", "human-numeric-sort",
			"ignore-nonprinting", "month-sort", "numeric-sort", "random-sort", "reverse", "version-sort", "merge",
			"stable", "unique", "zero-terminated", "debug",
		},
		longValue:    []string{"key", "field-separator", "buffer-size", "parallel", "random-source", "sort", "files0-from", "output"},
		longOptional: []string{"check"},
		badLong:      []string{"temporary-directory", "compress-program", "batch-size", "help", "version"},
	},
	"uniq": {
		permute: true, short: "cdDiuz", value: "fsw",
		long:         []string{"count", "repeated", "unique", "ignore-case", "zero-terminated"},
		longValue:    []string{"skip-fields", "skip-chars", "check-chars"},
		longOptional: []string{"all-repeated", "group"},
		badLong:      []string{"help", "version"},
	},
	"rg": {
		permute: true, short: "iSsFwxvnNHIlcouLpUPa0qb.", value: "efrmABCgtTdEjM", bad: "z",
		long: []string{
			"ignore-case", "smart-case", "case-sensitive", "fixed-strings", "word-regexp", "line-regexp",
			"invert-match", "line-number", "no-line-number", "with-filename", "no-filename", "files-with-matches",
			"files-without-match", "count", "count-matches", "only-matching", "glob-case-insensitive", "type-list",
			"unrestricted", "hidden", "no-hidden", "no-ignore", "no-ignore-vcs", "no-ignore-parent", "no-ignore-dot",
			"no-ignore-global", "no-ignore-exclude", "no-ignore-files", "follow", "files", "json", "column",
			"heading", "no-heading", "pretty", "multiline", "multiline-dotall", "pcre2", "text", "null",
			"null-data", "quiet", "trim", "max-columns-preview", "stats", "debug", "no-messages", "vimgrep",
			"byte-offset", "passthru", "line-buffered", "block-buffered", "no-config", "crlf", "mmap", "no-mmap",
			"one-file-system", "include-zero", "no-unicode",
		},
		longValue: []string{
			"regexp", "file", "replace", "max-count", "after-context", "before-context", "context", "glob", "iglob",
			"type", "type-not", "ignore-file", "max-depth", "max-filesize", "colors", "color", "encoding", "engine",
			"threads", "max-columns", "sort", "sortr", "path-separator",
		},
		badLong: []string{"pre", "pre-glob", "search-zip", "hostname-bin"},
	},
	"fd": {
		permute: true, short: "HIusigFaLp01q", value: "dEteSocj", bad: "xXl",
		long: []string{
			"hidden", "no-ignore", "no-ignore-vcs", "unrestricted", "case-sensitive", "ignore-case", "glob", "regex",
			"fixed-strings", "absolute-path", "follow", "full-path", "print0", "quiet", "show-errors",
			"strip-cwd-prefix", "one-file-system", "prune",
		},
		longValue: []string{
			"max-depth", "min-depth", "exact-depth", "exclude", "type", "extension", "size", "changed-within",
			"changed-before", "owner", "max-results", "base-directory", "path-separator", "color", "threads",
		},
		badLong: []string{"exec", "exec-batch", "batch-size", "list-details"},
	},
}

// writerFlags are the modeled writers' flag tables (T3); valueTargets are the
// writers whose option values are targets too.
var (
	writerFlags = map[string]*flags{
		"rm":    {permute: true, loose: true, short: "fiIrRdv", long: []string{"force", "one-file-system", "no-preserve-root", "recursive", "dir", "verbose"}, longOptional: []string{"interactive", "preserve-root"}},
		"rmdir": {permute: true, loose: true, short: "pv", long: []string{"parents", "verbose", "ignore-fail-on-non-empty"}},
		"mkdir": {permute: true, loose: true, short: "pvZ", value: "m", long: []string{"parents", "verbose"}, longValue: []string{"mode"}, longOptional: []string{"context"}},
		"touch": {permute: true, loose: true, short: "achm", value: "drt", long: []string{"no-create", "no-dereference"}, longValue: []string{"date", "reference", "time"}},
		"tee":   {permute: true, loose: true, short: "aip", long: []string{"append", "ignore-interrupts"}, longOptional: []string{"output-error"}},
		"truncate": {
			permute: true, loose: true, short: "co", value: "rs",
			long: []string{"no-create", "io-blocks"}, longValue: []string{"reference", "size"},
		},
		"cp": {
			permute: true, loose: true, short: "abdfiHlLnPpRrsTuvxZ", value: "St",
			long: []string{
				"archive", "force", "interactive", "link", "dereference", "no-clobber", "no-dereference", "recursive",
				"symbolic-link", "no-target-directory", "verbose", "one-file-system", "attributes-only",
				"copy-contents", "debug", "parents", "remove-destination", "strip-trailing-slashes",
				"keep-directory-symlink",
			},
			longValue:    []string{"suffix", "target-directory"},
			longOptional: []string{"backup", "preserve", "no-preserve", "reflink", "sparse", "update", "context"},
		},
		"mv": {
			permute: true, loose: true, short: "bfinTuvZ", value: "St",
			long:         []string{"force", "interactive", "no-clobber", "no-target-directory", "verbose", "no-copy", "strip-trailing-slashes", "exchange"},
			longValue:    []string{"suffix", "target-directory"},
			longOptional: []string{"backup", "update", "context"},
		},
		"ln": {
			permute: true, loose: true, short: "bdFfiLnPrsTv", value: "St",
			long:         []string{"directory", "force", "interactive", "logical", "no-dereference", "physical", "relative", "symbolic", "no-target-directory", "verbose"},
			longValue:    []string{"suffix", "target-directory"},
			longOptional: []string{"backup"},
		},
		"install": {
			permute: true, loose: true, short: "bCcDdpsTvZ", value: "gmoSt",
			long:         []string{"compare", "directory", "preserve-timestamps", "strip", "no-target-directory", "verbose", "preserve-context"},
			longValue:    []string{"group", "mode", "owner", "suffix", "target-directory"},
			longOptional: []string{"backup", "context"},
			badLong:      []string{"strip-program"},
		},
		"chmod": {
			permute: true, loose: true, short: "cfvR",
			long:      []string{"changes", "silent", "quiet", "verbose", "recursive", "no-preserve-root", "preserve-root"},
			longValue: []string{"reference"},
			operand:   symbolicMode,
		},
		"chown": chownFlags,
		"chgrp": chownFlags,
	}
	chownFlags = &flags{
		permute: true, loose: true, short: "cfvhHLPR",
		long:      []string{"changes", "silent", "quiet", "verbose", "dereference", "no-dereference", "preserve-root", "no-preserve-root", "recursive"},
		longValue: []string{"from", "reference"},
	}
	valueTargets = []string{"touch", "truncate", "cp", "mv", "ln", "install", "chmod", "chown", "chgrp"}
)

// symbolicMode reports a chmod operand that starts with - (-x, -rwx).
func symbolicMode(w string) bool {
	return len(w) > 1 && w[0] == '-' && strings.Trim(w[1:], "rwxXstugoa+-=,01234567") == "" && strings.ContainsAny(w[1:], "rwxXst")
}

// writer reads a modeled writer's targets: its operands, and its option
// values where valueTargets lists it; dd's if= and of= values.
func writer(name string, args []arg) verdict {
	if name == "dd" {
		v := verdict{kind: writeKind}
		for _, a := range args {
			key, value, ok := strings.Cut(a.text, "=")
			switch {
			case !a.ok:
				a.text = ""
				if ok && (key == "if" || key == "of") {
					a.text = value
				}
				v.targets = append(v.targets, a)
			case !ok:
				return verdict{kind: unknownKind}
			case key == "if" || key == "of":
				a.text = value
				v.targets = append(v.targets, a)
			}
		}
		return v
	}
	hits, operands, _, ok := writerFlags[name].read(args, 0)
	if !ok {
		return verdict{kind: unknownKind}
	}
	v := verdict{kind: writeKind}
	for _, i := range operands {
		v.targets = append(v.targets, args[i])
	}
	if slices.Contains(valueTargets, name) {
		for _, h := range hits {
			if h.val != nil {
				v.targets = append(v.targets, *h.val)
			}
		}
	}
	return v
}

// awkReads reports an awk call given only -F, -v and -- and an inline
// program with no output redirect or pipe, no gawk directive, and no call
// that runs a program or reads past its operands.
func awkReads(args []arg) bool {
	i := 0
loop:
	for ; i < len(args); i++ {
		a := args[i]
		if !a.ok {
			return false
		}
		switch w := a.text; {
		case w == "--":
			i++
			break loop
		case w == "-F", w == "-v", w == "--field-separator", w == "--assign":
			i++
		case strings.HasPrefix(w, "-F"), strings.HasPrefix(w, "-v"),
			strings.HasPrefix(w, "--field-separator="), strings.HasPrefix(w, "--assign="):
		case strings.HasPrefix(w, "-") && w != "-":
			return false
		default:
			break loop
		}
	}
	if i >= len(args) {
		return true
	}
	p := args[i]
	if !p.ok || strings.ContainsAny(p.text, ">|@") {
		return false
	}
	for _, fn := range []string{"system", "getline", "close", "fflush"} {
		if strings.Contains(p.text, fn) {
			return false
		}
	}
	return true
}

// findWhy names why a find call may write or run something: an action or
// other primary not on the closed list, or a word that does not render where
// it may be one; "" when it only reads.
func findWhy(args []arg) string {
	i := 0
leading:
	for i < len(args) && args[i].ok {
		switch w := args[i].text; {
		case w == "-H", w == "-L", w == "-P":
			i++
		case w == "-D":
			i += 2
		case strings.HasPrefix(w, "-O") && digits(w[2:]):
			i++
		default:
			break leading
		}
	}
	for ; i < len(args); i++ {
		a := args[i]
		if !a.ok {
			if a.text == "" || strings.ContainsAny(a.text[:1], "-(!),") {
				return "runs find with an expression built at run time"
			}
			continue
		}
		if strings.HasPrefix(a.text, "-") || slices.Contains([]string{"(", "!", ")", ","}, a.text) {
			break
		}
	}
	for i < len(args) {
		a := args[i]
		if !a.ok {
			return "runs find with an expression built at run time"
		}
		switch w := a.text; {
		case slices.Contains(findValue, w), newerXY(w):
			i += 2
		case slices.Contains(findFlag, w):
			i++
		default:
			return "runs find with " + w
		}
	}
	return ""
}

// newerXY reports find's -newerXY test.
func newerXY(w string) bool {
	return len(w) == 8 && strings.HasPrefix(w, "-newer") && strings.IndexByte("aBcmt", w[6]) >= 0 && strings.IndexByte("aBcmt", w[7]) >= 0
}

// forge classifies a gh or glab call by its group and verb (T2). Any -O,
// --output, -D or --dir may write a file.
func forge(args []arg, groups map[string][]string, gh bool) verdict {
	unknown := verdict{kind: unknownKind}
	if len(args) == 0 {
		return verdict{}
	}
	g := args[0]
	verbs, known := groups[g.text]
	if !g.ok || !known {
		return unknown
	}
	rest := args[1:]
	verb := ""
	if verbs != nil {
		if len(rest) == 0 {
			return verdict{}
		}
		v := rest[0]
		if !v.ok || !slices.Contains(verbs, v.text) {
			return unknown
		}
		verb, rest = v.text, rest[1:]
	}
	create := gh && g.text == "repo" && verb == "create"
	for _, a := range rest {
		if !a.ok {
			if create {
				return unknown
			}
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a.text, "--"), "=")
		cluster := len(a.text) > 1 && a.text[0] == '-' && a.text[1] != '-' && !digits(a.text[1:])
		switch {
		case strings.HasPrefix(a.text, "--") && (name == "output" || name == "dir"),
			cluster && strings.ContainsAny(a.text[1:], "OD"):
			return unknown
		case create && (strings.HasPrefix(a.text, "--") && slices.Contains([]string{"clone", "source", "remote", "push"}, name) ||
			cluster && strings.ContainsAny(a.text[1:], "csr")):
			return unknown
		}
	}
	return verdict{}
}

// sed classifies a sed call: read-only by sedReads; else a modeled writer on
// its file operands when it edits in place, with an unresolved write for a
// script the program reader does not prove read-only, a script file, or a
// backup suffix that may name a path; else an unknown program.
func sed(args []arg) verdict {
	texts, _ := strs(args)
	if sedReads(texts) {
		return verdict{}
	}
	files, why, inPlace, ok := sedEdits(args)
	switch {
	case !ok:
		return verdict{kind: unknownKind, why: "runs sed with options the guard cannot read"}
	case !inPlace:
		return verdict{kind: unknownKind, why: why}
	}
	return verdict{kind: writeKind, targets: files, why: why}
}

// sedLongAll are every long option of GNU sed, for matching an abbreviation.
var sedLongAll = append([]string{"expression", "file", "in-place", "line-length", "binary"}, sedLong...)

// sedEdits reads a sed call that sedReads does not prove read-only, as GNU
// sed reads it: its file operands, why it may write a path it does not name,
// whether it edits in place, and ok false for an option it cannot read.
func sedEdits(args []arg) (files []arg, why string, inPlace, ok bool) {
	script, opts := false, true
	scriptReads := func(v *arg) {
		script = true
		if why == "" && (v == nil || !v.ok || !sedScript(v.text)) {
			why = "runs sed with a script that may write"
		}
	}
	suffix := func(s string) {
		inPlace = true
		if why == "" && strings.ContainsAny(s, "/*") {
			why = "runs sed -i with a backup suffix that may name a path"
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() *arg {
			if i+1 < len(args) {
				i++
				return &args[i]
			}
			return nil
		}
		switch {
		case !a.ok:
			if !script {
				scriptReads(&a)
				continue
			}
			files = append(files, a)
		case opts && a.text == "--":
			opts = false
		case opts && strings.HasPrefix(a.text, "--"):
			name, v, glued := strings.Cut(a.text[2:], "=")
			full := sedLongMatch(name)
			value := func() *arg {
				if glued {
					return &arg{text: v, ok: true}
				}
				return next()
			}
			switch full {
			case "expression":
				scriptReads(value())
			case "file":
				value()
				script = true
				if why == "" {
					why = "reads a sed script from a file"
				}
			case "in-place":
				suffix(v)
			case "line-length":
				value()
			case "":
				return nil, "", false, false
			default:
				if glued {
					return nil, "", false, false
				}
			}
		case opts && len(a.text) > 1 && a.text[0] == '-':
			cluster := a.text[1:]
			for k := 0; k < len(cluster); k++ {
				rest := cluster[k+1:]
				value := func() *arg {
					if rest != "" {
						return &arg{text: rest, ok: true}
					}
					return next()
				}
				switch cluster[k] {
				case 'e':
					scriptReads(value())
					k = len(cluster)
				case 'f':
					value()
					script = true
					if why == "" {
						why = "reads a sed script from a file"
					}
					k = len(cluster)
				case 'l':
					value()
					k = len(cluster)
				case 'i':
					suffix(rest)
					k = len(cluster)
				case 'n', 'E', 'r', 's', 'u', 'z':
				default:
					return nil, "", false, false
				}
			}
		case !script:
			scriptReads(&a)
		default:
			files = append(files, a)
		}
	}
	return files, why, inPlace, true
}

// sedLongMatch returns the long option name abbreviates, "" for none or more
// than one.
func sedLongMatch(name string) string {
	if slices.Contains(sedLongAll, name) {
		return name
	}
	full := ""
	for _, l := range sedLongAll {
		if name != "" && strings.HasPrefix(l, name) {
			if full != "" {
				return ""
			}
			full = l
		}
	}
	return full
}

// sedLong are the long options of sed that take no value and read or write
// nothing.
var sedLong = []string{
	"quiet", "silent", "debug", "posix", "regexp-extended", "sandbox", "separate", "unbuffered",
	"null-data", "zero-terminated", "follow-symlinks", "help", "version",
}

// sedReads reports a sed command that edits nothing in place, reads no script
// file, and whose scripts write no file and run nothing. A script without w, W
// or e reads only; one holding them must parse as a program with no w, W, e,
// r, R, a, i, c, y or other unknown command and no s flag w or e. Options are
// read as GNU sed does: anywhere before --, a long one by any prefix of
// --expression, a short cluster letter by letter, where e takes the rest of
// the cluster as its script and l as a number.
func sedReads(args []string) bool {
	script, opts := false, true
	for i := 0; i < len(args); i++ {
		w := args[i]
		// next takes the following word as an option's value; a missing one is
		// "", which no script passes.
		next := func() string {
			if i++; i < len(args) {
				return args[i]
			}
			return ""
		}
		switch {
		case w == "":
			return false
		case w == "--" && opts:
			opts = false
		case opts && strings.HasPrefix(w, "--"):
			name, v, glued := strings.Cut(w[2:], "=")
			switch {
			case name != "" && strings.HasPrefix("expression", name):
				if !glued {
					v = next()
				}
				if !sedScript(v) {
					return false
				}
				script = true
			case glued, !slices.Contains(sedLong, name):
				return false
			}
		case opts && len(w) > 1 && w[0] == '-':
			isScript, ok := sedCluster(w[1:], next)
			if !ok {
				return false
			}
			script = script || isScript
		case !script:
			if !sedScript(w) {
				return false
			}
			script = true
		}
	}
	return true
}

// sedCluster reads a short-option cluster, and reports whether it gave a
// script and whether every letter is harmless. e takes the rest of the cluster
// as its script, else next's; l takes a number the same way.
func sedCluster(cluster string, next func() string) (script, ok bool) {
	for k, c := range cluster {
		switch c {
		case 'e':
			rest := cluster[k+1:]
			if rest == "" {
				rest = next()
			}
			return true, sedScript(rest)
		case 'l':
			if k+1 == len(cluster) {
				next()
			}
			return false, true
		case 'n', 'E', 'r', 's', 'u', 'z':
		default:
			return false, false
		}
	}
	return false, true
}

func sedScript(s string) bool {
	return s != "" && (!strings.ContainsAny(s, "wWe") || sedProgramReads(s))
}

// sedProgramReads reports a script it fully parses whose commands neither
// write a file nor run one. Anything it does not recognise counts as a writer,
// and so do the forms GNU and BSD sed read differently (a backslash-newline, a
// delimiter or backslash inside a bracket, escapes such as \x that can spell a
// delimiter), so both seds see the commands it saw.
func sedProgramReads(s string) bool {
	for _, esc := range []string{"\\\n", "\\c", "\\d", "\\o", "\\x"} {
		if strings.Contains(s, esc) {
			return false
		}
	}
	p := &sedProgram{s: s}
	for {
		p.skip(" \t\n;")
		if p.done() {
			return true
		}
		if !p.command() {
			return false
		}
	}
}

// sedProgram is a cursor over a sed script.
type sedProgram struct {
	s string
	i int
}

func (p *sedProgram) done() bool { return p.i >= len(p.s) }

// peek is the next byte, or 0 at the end.
func (p *sedProgram) peek() byte {
	if p.done() {
		return 0
	}
	return p.s[p.i]
}

func (p *sedProgram) next() byte {
	c := p.s[p.i]
	p.i++
	return c
}

func (p *sedProgram) skip(set string) {
	for !p.done() && strings.IndexByte(set, p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *sedProgram) digits() bool {
	start := p.i
	p.skip("0123456789")
	return p.i > start
}

// command reads one command with its addresses, and reports whether it reads
// only.
func (p *sedProgram) command() bool {
	addressed, ok := p.addresses()
	if !ok || p.done() {
		return false
	}
	switch c := p.next(); {
	case c == '#' && !addressed:
		if k := strings.IndexByte(p.s[p.i:], '\n'); k >= 0 {
			p.i += k
		} else {
			p.i = len(p.s)
		}
		return true
	case c == '{':
		return true
	case strings.IndexByte("}=dDgGhHnNpPxzF", c) >= 0:
		return p.end()
	case strings.IndexByte("lqQ", c) >= 0:
		p.skip(" \t")
		p.digits()
		return p.end()
	case strings.IndexByte("btT", c) >= 0:
		p.label()
		return true
	case c == ':':
		return !addressed && p.label() != ""
	case c == 's':
		return p.substitute()
	}
	return false
}

// label reads a label as GNU sed does, up to whitespace, ; or }, so what
// follows on the line is parsed as further commands.
func (p *sedProgram) label() string {
	p.skip(" \t")
	start := p.i
	for !p.done() && strings.IndexByte(" \t\n;}", p.s[p.i]) < 0 {
		p.i++
	}
	return p.s[start:p.i]
}

// end reports a command followed only by blanks before its terminator.
func (p *sedProgram) end() bool {
	p.skip(" \t")
	return p.done() || strings.IndexByte(";\n}#", p.s[p.i]) >= 0
}

// addresses reads a command's addresses and the ! and blanks after them.
func (p *sedProgram) addresses() (addressed, ok bool) {
	found, ok := p.address()
	if !found || !ok {
		return found, ok
	}
	if p.peek() == ',' {
		p.i++
		if c := p.peek(); c == '+' || c == '~' {
			p.i++
			ok = p.digits()
		} else {
			found, ok = p.address()
			ok = found && ok
		}
		if !ok {
			return true, false
		}
	}
	p.skip("! \t")
	return true, true
}

// address reads one address, if one starts here.
func (p *sedProgram) address() (found, ok bool) {
	switch c := p.peek(); {
	case c == '$':
		p.i++
		return true, true
	case c >= '0' && c <= '9':
		p.digits()
		if p.peek() == '~' {
			p.i++
			return true, p.digits()
		}
		return true, true
	case c == '/' || c == '\\':
		p.i++
		if c == '\\' {
			c = p.peek()
			p.i++
		}
		if !sedDelim(c) || !p.regex(c, true) {
			return true, false
		}
		p.skip("IM")
		return true, true
	}
	return false, true
}

// substitute reads an s command after its s.
func (p *sedProgram) substitute() bool {
	delim := p.peek()
	p.i++
	if !sedDelim(delim) || !p.regex(delim, true) || !p.regex(delim, false) {
		return false
	}
	p.skip("gpiImM0123456789")
	return p.end()
}

// regex reads up to and past an unescaped delim. Only a regex has bracket
// expressions; a replacement does not.
func (p *sedProgram) regex(delim byte, brackets bool) bool {
	for !p.done() {
		switch c := p.next(); {
		case c == delim:
			return true
		case c == '\n':
			return false
		case c == '\\':
			if p.done() {
				return false
			}
			p.i++
		case c == '[' && brackets:
			if !p.bracket(delim) {
				return false
			}
		}
	}
	return false
}

// bracket reads a bracket expression after its [. One holding the delimiter
// or a backslash is refused: GNU and BSD sed disagree on where it ends.
func (p *sedProgram) bracket(delim byte) bool {
	if p.peek() == '^' {
		p.i++
	}
	if p.peek() == ']' {
		p.i++
	}
	refused := "\\\n" + string(delim)
	for !p.done() {
		c := p.next()
		switch {
		case c == ']':
			return true
		case strings.IndexByte(refused, c) >= 0:
			return false
		case c == '[' && strings.IndexByte(":.=", p.peek()) >= 0:
			closer := string(p.next()) + "]"
			k := strings.Index(p.s[p.i:], closer)
			if k < 0 || strings.ContainsAny(p.s[p.i:p.i+k], refused) {
				return false
			}
			p.i += k + len(closer)
		}
	}
	return false
}

// sedDelim reports a delimiter both seds read alike and that cannot be taken
// for part of a command.
func sedDelim(c byte) bool {
	alnum := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	return c > ' ' && c < 0x7f && !alnum && strings.IndexByte("\\[]", c) < 0
}

// fdRuns reports an option that runs a command: --exec or --exec-batch, or
// an unambiguous prefix of one, or x or X in a short cluster before a letter
// that takes the rest of it as its value.
func fdRuns(w string) bool {
	switch {
	case w == "":
		return true
	case strings.HasPrefix(w, "--"):
		name, _, _ := strings.Cut(w[2:], "=")
		return name != "" && (strings.HasPrefix("exec-batch", name) || strings.HasPrefix(name, "exec"))
	case strings.HasPrefix(w, "-"):
		for _, c := range w[1:] {
			switch c {
			case 'x', 'X':
				return true
			case 'e', 't', 'd', 'E', 'S', 'j', 'c', 'o':
				return false
			}
		}
	}
	return false
}

// gitSub returns the index of git's subcommand in args, len(args) when there
// is none, and false when an option before it is not a known harmless one.
func gitSub(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case slices.Contains(gitValueOpts, w):
			i++
		case slices.Contains(gitFlagOpts, w), hasAnyPrefix(w, gitGlueOpts):
		case w == "", strings.HasPrefix(w, "-"):
			return 0, false
		default:
			return i, true
		}
	}
	return len(args), true
}

func hasAnyPrefix(w string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(w, p) })
}

// gitReads reports a git command that leaves the work tree as it is: one of
// gitReaders, with no option that sets config, runs a program or writes a
// file. A commit's message is text, not an option.
func gitReads(args, heads []string) bool {
	sub, ok := gitSub(args)
	if !ok {
		return false
	}
	if sub == len(args) {
		return true
	}
	if !slices.Contains(gitReaders, args[sub]) {
		return false
	}
	var skip []int
	if args[sub] == "commit" {
		skip = commitMessages(args, heads, sub+1)
	}
	for i := sub + 1; i < len(args); i++ {
		if !slices.Contains(skip, i) && gitRuns(args[i]) {
			return false
		}
	}
	return true
}

// gitWorktree reports git worktree list, repair or prune, with no option
// that writes a file or runs a program.
func gitWorktree(args []string) bool {
	sub, ok := gitSub(args)
	if !ok || sub+1 >= len(args) || args[sub] != "worktree" || !slices.Contains([]string{"list", "repair", "prune"}, args[sub+1]) {
		return false
	}
	return !slices.ContainsFunc(args[sub+2:], gitRuns)
}

// gitRuns reports an option that writes a file or runs a program.
func gitRuns(w string) bool {
	switch {
	case w == "":
		return true
	case strings.HasPrefix(w, "--"):
		name, _, _ := strings.Cut(w[2:], "=")
		return name != "" && slices.ContainsFunc(gitRunOpts, func(o string) bool {
			return strings.HasPrefix(o, name) || strings.HasPrefix(name, o)
		})
	}
	return strings.HasPrefix(w, "-") && strings.Contains(w, "O")
}

// commitMessages returns the indexes in args, from from, of a git commit's
// message words: the value of -m or --message, of a short cluster of commit
// flags ending in m, and a --message= word. heads holds each word's text, or
// an unrendered word's literal prefix, whose cluster glued to its message
// is one.
func commitMessages(args, heads []string, from int) []int {
	var out []int
	for i := from; i < len(args); i++ {
		w, head := args[i], heads[i]
		glued, short := shortMessage(head)
		switch {
		case w == "-m", w == "--message", short && !glued && w != "":
			if i++; i < len(args) {
				out = append(out, i)
			}
		case short, strings.HasPrefix(head, "--message="):
			out = append(out, i)
		}
	}
	return out
}

// shortMessage reads w as a git commit's short options up to an m, which only
// valueless flags may precede, as in -am: glued reports that the message
// follows the m in the same word.
func shortMessage(w string) (glued, ok bool) {
	if len(w) < 2 || w[0] != '-' || w[1] == '-' {
		return false, false
	}
	rest := strings.TrimLeft(w[1:], "asnvqeiop")
	if rest == "" || rest[0] != 'm' {
		return false, false
	}
	return len(rest) > 1, true
}

// shellRun is how a shell call reads its script.
type shellRun struct {
	// c reports -c, whose first operand, script, is the script.
	c      bool
	script *arg
	// extra are the positional parameters, $0 first.
	extra []arg
	// file reports a script file: an operand without -c, or -i, --rcfile,
	// --init-file.
	file bool
	// stdin reports a script read from stdin.
	stdin bool
	// lost reports an option word that does not render.
	lost bool
	// allexport reports -a or -o allexport, read as set reads them.
	allexport bool
}

func readShell(args []arg) shellRun {
	var s shellRun
	fromStdin := false
	i := 0
options:
	for ; i < len(args); i++ {
		a := args[i]
		if !a.ok {
			if a.text == "" || a.text[0] == '-' || a.text[0] == '+' {
				s.lost = true
				return s
			}
			break
		}
		switch w := a.text; {
		case w == "--", w == "-":
			i++
			break options
		case w == "--rcfile", w == "--init-file":
			s.file = true
			i++
		case strings.HasPrefix(w, "--"):
		case len(w) > 1 && (w[0] == '-' || w[0] == '+'):
			for _, c := range w[1:] {
				switch c {
				case 'c':
					s.c = true
				case 'i':
					s.file = true
				case 's':
					fromStdin = true
				case 'a':
					s.allexport = s.allexport || w[0] == '-'
				case 'o', 'O':
					i++
					if c == 'o' && i < len(args) && (!args[i].ok || args[i].text == "allexport") {
						s.allexport = true
					}
				}
			}
		default:
			break options
		}
	}
	operands := args[min(i, len(args)):]
	switch {
	case s.c:
		if len(operands) > 0 {
			s.script, s.extra = &operands[0], operands[1:]
		}
	case len(operands) > 0 && !fromStdin:
		s.file = true
		s.script, s.extra = &operands[0], operands[1:]
	default:
		s.stdin, s.extra = true, operands
	}
	return s
}

// evalScript returns what eval runs when every argument is literal.
func evalScript(args []arg) (string, bool) {
	texts := make([]string, len(args))
	for i, a := range args {
		if !isLiteral(a.word) {
			return "", false
		}
		texts[i] = a.text
	}
	return strings.Join(texts, " "), len(args) > 0
}

// trapAction returns trap's action, when it sets one.
func trapAction(args []arg) (arg, bool) {
	i := 0
	for i < len(args) && args[i].ok && (args[i].text == "-l" || args[i].text == "-p" || args[i].text == "--") {
		i++
	}
	if len(args)-i < 2 || args[i].ok && (args[i].text == "-" || args[i].text == "") {
		return arg{}, false
	}
	return args[i], true
}

// mapfileCallback reports a mapfile call that may be given -C, whose value
// bash runs as code. A word that does not render may be that flag.
func mapfileCallback(args []arg) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !a.ok:
			return true
		case a.text == "--", len(a.text) < 2 || a.text[0] != '-':
			return false
		}
		for k := 1; k < len(a.text); k++ {
			if a.text[k] == 'C' {
				return true
			}
			if strings.IndexByte("dnOsuc", a.text[k]) >= 0 {
				if k == len(a.text)-1 {
					i++
				}
				break
			}
		}
	}
	return false
}

// execs returns the commands find's -exec family or fd's -x and -X run,
// each up to its ; or +.
func execs(name string, args []arg) [][]arg {
	var out [][]arg
	for i := 0; i < len(args); i++ {
		a := args[i]
		starts := name == "find" && a.ok && slices.Contains(findExec, a.text) ||
			name == "fd" && a.ok && (a.text == "-x" || a.text == "-X" || a.text == "--exec" || a.text == "--exec-batch")
		if !starts {
			continue
		}
		end := i + 1
		for end < len(args) && (!args[end].ok || args[end].text != ";" && args[end].text != "+") {
			end++
		}
		out = append(out, args[i+1:end])
		i = end
	}
	return out
}

// splitScript is env -S's string as the script it runs: env splits it into
// its own arguments, so its flags (-C) are env's.
func splitScript(a arg) arg {
	a.text = "env " + a.text
	return a
}

// scriptsOf returns the literal scripts a call runs: a shell's -c script,
// heredoc or here-string, eval's words, trap's action, env -S's string, and
// those of a shell run by find -exec or fd -x.
func scriptsOf(c call, f feed) []string {
	var out []string
	if c.split != nil && isLiteral(c.split.word) && c.split.ok {
		out = append(out, splitScript(*c.split).text)
	}
	if c.lost || c.at >= len(c.argv) || !c.argv[c.at].ok {
		return out
	}
	name, args := c.argv[c.at].text, c.argv[c.at+1:]
	if slices.Contains(scriptShells, wordName(name)) {
		s := readShell(args)
		switch {
		case s.c && s.script != nil && s.script.ok && isLiteral(s.script.word):
			out = append(out, s.script.text)
		case s.stdin && f.kind == docFeed:
			if t, ok := docScript(f.doc); ok {
				out = append(out, t)
			}
		}
		return out
	}
	switch name {
	case "eval":
		if t, ok := evalScript(args); ok {
			out = append(out, t)
		}
	case "trap":
		if a, ok := trapAction(args); ok && a.ok && isLiteral(a.word) {
			out = append(out, a.text)
		}
	case "find", "fd":
		for _, inner := range execs(name, args) {
			out = append(out, scriptsOf(unwrap(inner), feed{})...)
		}
	}
	return out
}

// call classifies one simple command and records what it writes; when it is
// an unresolved write, the names its own words hold.
func (r *reader) call(ce *syntax.CallExpr, f feed) {
	before, calls := r.out.writesUnresolved, len(r.out.options)
	r.classifyCall(ce, f)
	call := 0
	if len(r.out.options) > calls {
		call = calls + 1
	}
	if !r.over() {
		r.environment(ce, call)
	}
	if r.out.writesUnresolved > before && !r.over() {
		r.ownNames(ce, f)
	}
}

func (r *reader) classifyCall(ce *syntax.CallExpr, f feed) {
	argv := r.args(ce.Args, true)
	if r.over() || len(argv) == 0 {
		return
	}
	c := unwrap(argv)
	for _, d := range c.dirs {
		r.dir(d)
	}
	for _, a := range c.assigns {
		if name, _, _ := strings.Cut(a.text, "="); !identifier(name) {
			r.codeOut("an environment entry whose name is not a variable")
		}
	}
	if c.lost {
		r.dirLost()
		r.unknown(argv[0].text, argv, f)
		return
	}
	if c.split != nil {
		r.nested(splitScript(*c.split), "env runs a split string built at run time")
		if c.at < len(argv) {
			r.unknown("env", argv[c.at:], f)
		}
		return
	}
	if c.at == len(argv) {
		return
	}
	if c.xargs {
		// xargs reads stdin for operands, and its command's own stdin is
		// /dev/null.
		f = feed{}
		argv = slices.Clone(argv)
		if c.replace != "" {
			for i := c.at; i < len(argv); i++ {
				if k := strings.Index(argv[i].text, c.replace); argv[i].ok && k >= 0 {
					argv[i].text, argv[i].ok = argv[i].text[:k], false
				}
			}
		} else {
			argv = append(argv, arg{src: "operands from stdin"})
		}
	}
	cmd, args := argv[c.at], argv[c.at+1:]
	// A glob expands at run time too, and may name a builtin (`e?al`).
	if !cmd.ok || globWord(cmd.word) {
		if strings.Contains(cmd.text, "/") || hasLiteralSlash(cmd.word) {
			r.unknown(cmd.src, args, f)
			return
		}
		r.codeOut("a command name built at run time")
		return
	}
	name := cmd.text
	switch {
	case strings.Contains(name, "="):
		r.dirLost()
		r.unresolved(fmt.Sprintf("runs a command named %q", name))
		r.unknown(name, args, f)
		return
	case c.listing:
		return
	case slices.Contains(scriptShells, wordName(name)):
		r.shell(name, args, f)
		return
	case strings.ContainsAny(name, scriptBytes), strings.Contains(name, "/"):
		r.unknown(name, args, f)
		return
	}
	r.dirsOf(name, args)
	switch name {
	case "eval":
		if t, ok := evalScript(args); ok {
			r.nestedText(t)
		} else if len(args) > 0 {
			r.codeOut("eval of a run-time string")
		}
		r.redefined(name, args, f)
		return
	case "trap":
		if a, ok := trapAction(args); ok {
			r.nested(a, "trap text built at run time")
		}
		r.redefined(name, args, f)
		return
	case "alias":
		if slices.ContainsFunc(args, func(a arg) bool { return !isLiteral(a.word) }) {
			r.codeOut("alias text built at run time")
		}
		r.unknown(name, args, f)
		return
	case "source", ".":
		if len(args) > 0 && args[0].ok && pipePath(args[0].text) {
			r.codeOut("a script piped into a shell")
		}
		r.unresolved(fmt.Sprintf("runs %s on a script file", name))
		r.unknown(name, args, f)
		return
	case "mapfile", "readarray":
		if mapfileCallback(args) {
			r.codeOut("mapfile runs a callback as code")
		}
	}
	v := classify(name, args)
	if (r.withdrawn || r.pre.scoped[ce]) && v.kind != unknownKind {
		r.redefines(name)
		v = verdict{kind: unknownKind}
	}
	if c.xargs && v.kind != readKind {
		r.unresolved(fmt.Sprintf("takes %s's operands from stdin", name))
		r.out.replaces = true
	}
	if v.why != "" {
		r.unresolved(v.why)
	}
	switch v.kind {
	case readKind:
	case writeKind:
		r.out.writes = true
		for _, t := range v.targets {
			if t.ok {
				r.target(Target{Text: t.text})
			} else {
				r.opaque(t)
			}
		}
	case unknownKind:
		r.unknown(name, args, f)
	}
}

// redefined judges a script runner (eval, trap) that runs no program of its
// own: under withdrawal it is an unknown program like any other call.
func (r *reader) redefined(name string, args []arg, f feed) {
	if r.withdrawn {
		r.redefines(name)
		r.unknown(name, args, f)
	}
}

// redefines records a call that withdrawal made an unknown program, an
// unresolved write named only when no other is.
func (r *reader) redefines(name string) {
	r.out.writes = true
	r.out.writesUnresolved++
	if r.out.redefined == "" {
		r.out.redefined = fmt.Sprintf("runs %s, which the command may have redefined", name)
	}
}

// nested checks a script a call runs: one that is not literal is opaque
// code, why; a literal one was parsed with the rest.
func (r *reader) nested(a arg, why string) {
	if !a.ok || !isLiteral(a.word) {
		r.codeOut(why)
		return
	}
	r.nestedText(a.text)
}

func (r *reader) nestedText(t string) {
	if !r.seen[t] {
		r.codeOut("a script the guard did not read")
	}
}

// shell judges a shell call by how it reads its script (§5, §4.4).
func (r *reader) shell(name string, args []arg, f feed) {
	s := readShell(args)
	switch {
	case s.lost:
		r.codeOut("a shell runs a script built at run time")
	case s.c && s.script != nil:
		r.nested(*s.script, "a shell runs a script built at run time")
	case s.stdin:
		switch f.kind {
		case pipeFeed:
			r.codeOut("a script piped into a shell")
		case docFeed:
			if t, ok := docScript(f.doc); ok {
				r.nestedText(t)
			} else {
				r.codeOut("a shell runs a script built at run time")
			}
		case fileFeed:
			r.unresolved(fmt.Sprintf("runs %s on a script file", name))
		case noFeed:
		}
	}
	// A script read from stdin is judged above, not as input naming paths.
	if s.stdin {
		f = feed{}
	}
	switch {
	case s.file:
		if s.script != nil && s.script.ok && pipePath(s.script.text) {
			r.codeOut("a script piped into a shell")
		}
		r.unresolved(fmt.Sprintf("runs %s on a script file", name))
		r.unknown(name, args, f)
	case r.withdrawn:
		r.redefines(name)
		r.unknown(name, args, f)
	}
}

// pipePath reports a path that reads a pipe, as a process substitution
// renders: a script read from one is code the text does not show.
func pipePath(p string) bool {
	return strings.HasPrefix(p, "/dev/fd/") || p == "/dev/stdin" || strings.HasPrefix(p, "/proc/self/fd/")
}

// globWord reports a word whose unquoted text is a glob: a * or ?, a
// bracket expression with its closing ], or an extglob.
func globWord(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.ExtGlob:
			return true
		}
	}
	lit := b.String()
	k := strings.IndexByte(lit, '[')
	return strings.ContainsAny(lit, "*?") || k >= 0 && strings.IndexByte(lit[k:], ']') > 0
}

// unknown records an unknown program over words (§4.2): it writes its cwd,
// every rendered operand without a script byte with the paths packed in it,
// and every operand it cannot resolve; a script-byte operand and a fed stdin
// are unresolved writes.
func (r *reader) unknown(name string, words []arg, f feed) {
	r.out.writes = true
	r.out.ran = true
	call := 0
	if opts := r.optionValues(words); len(opts) > 0 {
		r.out.options = append(r.out.options, opts)
		call = len(r.out.options)
	}
	for _, a := range words {
		switch {
		case !a.ok:
			r.opaqueOperand(a, call, false)
		case strings.ContainsAny(a.text, scriptBytes):
			r.unresolved(fmt.Sprintf("runs %s on inline code", name))
		default:
			r.operandTargets(a.text, call)
		}
	}
	if f.kind != noFeed {
		r.unresolved(fmt.Sprintf("feeds %s input that may name paths", name))
	}
}

// environment judges the values a call puts in the environment of what it
// runs: its prefix assignments and env's or sudo's NAME=value words, each
// belonging to the call whose option values call indexes. boundEnv judges
// the values the command's other setters bind.
func (r *reader) environment(ce *syntax.CallExpr, call int) {
	argv := r.args(ce.Args, true)
	childless := len(argv) > 0 && r.childless(argv[0])
	for _, as := range ce.Assigns {
		if as.Name == nil || as.Value == nil || as.Index != nil {
			continue
		}
		if len(ce.Args) > 0 && !safe(as.Name.Value) && !childless {
			r.assignValue(as, call)
		}
	}
	c := unwrap(argv)
	for _, a := range c.assigns {
		if name, v, _ := strings.Cut(a.text, "="); identifier(name) && !safe(name) {
			r.envValue(arg{text: v, ok: a.ok, word: a.word, src: a.src}, call)
		}
	}
}

// childlessBuiltins run no program and no code, and none is a POSIX special
// builtin, whose prefix assignments outlast it in POSIX mode, so a prefix
// assignment's value reaches nothing past them.
var childlessBuiltins = []string{
	"true", "false", "cd", "pushd", "popd", "dirs", "pwd", "echo", "printf", "read", "test", "[", "let",
	"getopts", "umask", "wait",
}

// childless reports a command word naming a childless builtin the command
// cannot have redefined.
func (r *reader) childless(cmd arg) bool {
	return cmd.ok && slices.Contains(childlessBuiltins, cmd.text) && !r.pre.redefines
}

// assignValue judges an assignment's value as an environment value. An
// appended one continues a value the reader cannot tell.
func (r *reader) assignValue(as *syntax.Assign, call int) {
	src := source(r.text, as)
	if as.Append {
		r.envValue(arg{word: as.Value, src: src}, call)
		return
	}
	rw := r.render(as.Value, true)
	for _, f := range rw.fields {
		r.envValue(arg{text: f, ok: true, word: as.Value, src: src}, call)
	}
	for _, p := range rw.opaque {
		r.envValue(arg{text: p, word: as.Value, src: src}, call)
	}
}

// envValue judges a value a program finds in its environment like an
// unknown program's operand, as any of them may read it as a path: the whole
// value and each field between pathBreaks, from the assignment spelling it.
// call indexes the option values of the call it belongs to. A value the
// reader cannot tell is an unresolved write. A value naming a path in a dir
// may name a file a program writes and a later call runs, so one that may
// name an allowlisted program withdraws the allowlist; a bare name names no
// dir, and an allowlisted program reads it as no program it runs.
func (r *reader) envValue(v arg, call int) {
	r.out.writes = true
	if !v.ok {
		r.unresolved(fmt.Sprintf("puts %q in a program's environment", v.src))
		r.opaqueOperand(v, call, true)
		return
	}
	replaces := r.out.replaces
	fields := append([]string{v.text}, r.tildeFields(v.text)...)
	for _, f := range fields {
		r.target(Target{Text: f, From: v.src, Call: call})
	}
	r.out.replaces = replaces || slices.ContainsFunc(fields, func(f string) bool {
		return strings.Contains(f, "/") && replacesProgram(f)
	})
}

// opaqueOperand records an operand or environment value that does not
// render, from its literal prefix a.text: each complete path field as a
// target, the trailing partial one as an unresolved write, each also expanded
// where it starts with ~. An environment value's prefixes replace a program
// only when they name a dir.
func (r *reader) opaqueOperand(a arg, call int, env bool) {
	fields := PathFields(a.text)
	partial := ""
	if a.text != "" && !strings.ContainsRune(pathBreaks, rune(a.text[len(a.text)-1])) {
		partial, fields = fields[len(fields)-1], fields[:len(fields)-1]
	}
	replaces, named := r.out.replaces, false
	for _, f := range fields {
		for _, t := range r.tildeFields(f) {
			r.target(Target{Text: t, From: a.src, Call: call})
			named = named || strings.Contains(t, "/") && replacesProgram(t)
		}
	}
	if !env {
		r.opaque(a)
	} else {
		r.out.replaces = replaces || named
		if a.text != "" {
			r.opaqueAt(a, strings.Contains(a.text, "/"))
		}
	}
	if len(a.text) > 2 && a.text[0] == '-' && a.text[1] != '-' {
		for k := 2; k < len(a.text) && !r.over(); k++ {
			if !r.spend(len(a.text) - k) {
				break
			}
			s := arg{text: a.text[k:], src: a.src}
			r.opaqueAt(s, env && strings.Contains(s.text, "/"))
			if s.text[0] != '~' {
				continue
			}
			if home, ok := r.tildeHome(PathFields(s.text)[0]); ok {
				r.opaqueAt(arg{text: home, src: a.src}, env && strings.Contains(home, "/"))
			}
		}
	}
	if partial == "" {
		return
	}
	texts := []string{partial}
	if strings.HasPrefix(partial, "~") {
		if home, ok := r.tildeHome(partial); ok {
			texts = append(texts, home)
		}
	}
	for _, t := range texts {
		r.opaqueAt(arg{text: t, src: a.src}, env && strings.Contains(t, "/"))
	}
}

// boundEnv judges every value the command binds to a name outside the safe
// set, but for a prefix one, as an environment value of the whole command,
// whichever names it exports: a program the reader does not model may find
// any of them.
func (r *reader) boundEnv() {
	for _, b := range r.pre.bound {
		if b.prefix {
			continue
		}
		for _, v := range r.boundValues(b) {
			if !safe(v.name) {
				r.envValue(arg{text: v.text, ok: v.ok, word: b.word, src: b.src}, 0)
			}
		}
	}
}

// boundValue is one rendering of a bound value: its text, or the literal
// prefix of one that does not render, and the name it binds, "" for any.
type boundValue struct {
	name, text string
	ok         bool
}

// boundValues renders b's values. A declare-family operand renders
// NAME=value: one without = binds nothing, and one appended to or whose name
// does not render binds a value the reader cannot tell.
func (r *reader) boundValues(b bound) []boundValue {
	if b.word == nil {
		return []boundValue{{name: b.name}}
	}
	text := r.text
	r.text = b.text
	rw := r.render(b.word, true)
	r.text = text
	var out []boundValue
	add := func(s string, ok bool) {
		if !b.operand {
			out = append(out, boundValue{name: b.name, text: s, ok: ok})
			return
		}
		name, v, assigned := strings.Cut(s, "=")
		base, _, _ := strings.Cut(strings.TrimSuffix(name, "+"), "[")
		switch {
		case !assigned && ok:
		case !assigned:
			out = append(out, boundValue{})
		case strings.HasSuffix(name, "+"):
			out = append(out, boundValue{name: base})
		default:
			out = append(out, boundValue{name: base, text: v, ok: ok})
		}
	}
	for _, f := range rw.fields {
		add(f, true)
	}
	for _, p := range rw.opaque {
		add(p, false)
	}
	return out
}

// tildeHome is f, which starts with ~, with its tilde-prefix expanded as a
// program may, and false when none is: ~ under a HOME the command may set is
// an unresolved write, and ~+, ~- and ~N name dirs no program expands.
func (r *reader) tildeHome(f string) (string, bool) {
	user, rest := f[1:], ""
	if k := strings.IndexByte(user, '/'); k >= 0 {
		user, rest = user[:k], user[k:]
	}
	switch {
	case user == "":
		if r.settable("HOME") {
			r.unresolved(fmt.Sprintf("writes through %q", f))
			return "", false
		}
		return r.host.Home + rest, true
	case strings.HasPrefix(user, "+"), strings.HasPrefix(user, "-"), digits(user):
		return "", false
	}
	home, ok := r.userHome(user)
	return home + rest, ok
}

// userHome is user's home dir by the host, looked up once per reader. A name
// holding a byte no account name can (`:`, `~`, whitespace, a control byte) is
// not looked up.
func (r *reader) userHome(user string) (string, bool) {
	if r.host.Users == nil || strings.IndexFunc(user, func(c rune) bool {
		return c == ':' || c == '~' || unicode.IsSpace(c) || unicode.IsControl(c)
	}) >= 0 {
		return "", false
	}
	if h, ok := r.homes[user]; ok {
		return h.dir, h.ok
	}
	if r.homes == nil {
		r.homes = map[string]userDir{}
	}
	dir, ok := r.host.Users(user)
	r.homes[user] = userDir{dir, ok}
	return dir, ok
}

type userDir struct {
	dir string
	ok  bool
}

// tildeFields are s's fields between pathBreaks, each that starts with ~
// also with its tilde-prefix expanded, as a program may expand it there.
func (r *reader) tildeFields(s string) []string {
	fields := PathFields(s)
	for _, f := range fields {
		if !strings.HasPrefix(f, "~") {
			continue
		}
		if home, ok := r.tildeHome(f); ok {
			fields = append(fields, home)
		}
	}
	return fields
}

// optionValues are the rendered values an unknown program's options may
// take: a long option's after =, each suffix of a short-option cluster from
// its third byte, and the word after an option, each that starts with ~ also
// expanded.
func (r *reader) optionValues(words []arg) []string {
	var out []string
	for i, a := range words {
		w := a.text
		if !a.ok || len(w) < 2 || w[0] != '-' {
			continue
		}
		if w == "--" {
			break
		}
		if name, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(name, "--") {
			out = append(out, v)
		} else if w[1] != '-' {
			for k := 2; k < len(w); k++ {
				out = append(out, w[k:])
			}
		}
		if i+1 < len(words) && words[i+1].ok && !strings.HasPrefix(words[i+1].text, "-") {
			out = append(out, words[i+1].text)
		}
	}
	for _, v := range out {
		if !strings.HasPrefix(v, "~") {
			continue
		}
		if !r.spend(len(v)) {
			return nil
		}
		if home, ok := r.tildeHome(v); ok {
			out = append(out, home)
		}
	}
	return slices.DeleteFunc(out, func(v string) bool { return v == "" })
}

// operandTargets records an unknown program's operand: the whole word, each
// field between pathBreaks, a long option's value, and each suffix of a
// short-option cluster from its third byte, with the first field of one
// that starts with ~ expanded. call indexes the call's option values.
func (r *reader) operandTargets(w string, call int) {
	r.target(Target{Text: w, Call: call})
	for _, f := range r.tildeFields(w) {
		r.target(Target{Text: f, From: w, Call: call})
	}
	if name, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(name, "--") {
		r.target(Target{Text: v, From: w, Call: call})
	}
	if len(w) > 2 && w[0] == '-' && w[1] != '-' {
		for k := 2; k < len(w) && !r.over(); k++ {
			if r.spend(len(w) - k) {
				r.target(Target{Text: w[k:], From: w, Call: call})
				if w[k] != '~' {
					continue
				}
				if home, ok := r.tildeHome(PathFields(w[k:])[0]); ok {
					r.target(Target{Text: home, From: w, Call: call})
				}
			}
		}
	}
}

// dirsOf records the dirs cd, pushd, popd, git, make and tar change into.
func (r *reader) dirsOf(name string, args []arg) {
	switch name {
	case "cd", "pushd":
		r.cd(name, args)
	case "popd":
		r.dirLost()
	case "git":
		r.gitDirs(args)
	case "make":
		r.clusterDirs(args, "fIoWE", "directory", 2)
	case "tar":
		if len(args) > 0 && args[0].ok && !strings.HasPrefix(args[0].text, "-") && strings.Contains(args[0].text, "C") {
			r.dirLost()
		}
		r.clusterDirs(args, "fbTXKNgVHL", "directory", 3)
	}
}

// cd records cd's or pushd's dir: its operand, home without one. cd -,
// pushd with no operand or ±N, an operand that does not render, and a
// relative one CDPATH may resolve are dirs the reader cannot tell; one
// naming a variable may also be its value.
func (r *reader) cd(name string, args []arg) {
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case !a.ok && (a.text == "" || a.text[0] == '-'):
			r.dirLost()
			return
		case a.ok && a.text == "--":
			i++
		case a.ok && len(a.text) > 1 && a.text[0] == '-' && strings.Trim(a.text[1:], "LPe@") == "":
			continue
		}
		break
	}
	if i >= len(args) {
		if name == "pushd" || r.settable("HOME") {
			r.dirLost()
			return
		}
		r.dir(arg{text: r.host.Home, ok: true})
		return
	}
	a := args[i]
	first, _, _ := strings.Cut(a.text, "/")
	switch {
	case !a.ok, a.text == "-", name == "pushd" && len(a.text) > 1 && (a.text[0] == '+' || a.text[0] == '-') && digits(a.text[1:]):
		r.dirLost()
	case a.text == "":
	case first != "" && first != "." && first != ".." && r.cdpath():
		r.dirLost()
	default:
		if identifier(a.text) {
			r.cdable(a.text)
		}
		r.dir(a)
	}
}

// cdable records the dirs a cd operand naming a variable may change into
// besides itself: cdable_vars, which a shopt, a shell's options or the host
// may set however they spell it, takes it to the variable's value. That is
// the host's value and each value the command binds to the name; one the
// reader cannot tell loses the dir, as does a name bash assigns itself or,
// unset on the host, a default it assigns. A setter that may set any name
// makes CDPATH settable, which loses the dir before this.
func (r *reader) cdable(name string) {
	if r.out.cdable[name] {
		return
	}
	r.out.cdable[name] = true
	if bashAssigns(name) {
		r.dirLost()
		return
	}
	switch {
	case name == "HOME":
		r.dir(arg{text: r.host.Home, ok: true})
	case r.host.Env != nil:
		v, ok := r.host.Env(name)
		if !ok && slices.Contains(bashDefaults, name) {
			r.dirLost()
		}
		if ok && v != "" {
			r.dir(arg{text: v, ok: true})
		}
	case slices.Contains(bashDefaults, name):
		r.dirLost()
	}
	if r.named == nil {
		r.named = map[string][]boundValue{}
		for _, b := range r.pre.bound {
			for _, v := range r.boundValues(b) {
				r.named[v.name] = append(r.named[v.name], v)
			}
		}
	}
	for _, v := range r.named[name] {
		r.dir(arg{text: v.text, ok: v.ok})
	}
}

// cdpath reports a relative cd operand bash may look up elsewhere: CDPATH
// set on the host or settable by the command.
func (r *reader) cdpath() bool {
	if r.settable("CDPATH") {
		return true
	}
	if r.host.Env == nil {
		return false
	}
	v, ok := r.host.Env("CDPATH")
	return ok && v != ""
}

// gitDirs records git's -C, --git-dir and --work-tree before its
// subcommand.
func (r *reader) gitDirs(args []arg) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.ok {
			if a.text == "" || a.text[0] == '-' {
				r.dirLost()
			}
			return
		}
		w := a.text
		value := func() {
			if i+1 < len(args) {
				i++
				r.dir(args[i])
			}
		}
		switch {
		case w == "-C", w == "--git-dir", w == "--work-tree":
			value()
		case w == "-c", w == "--namespace", w == "--config-env", w == "--attr-source":
			i++
		case strings.HasPrefix(w, "--git-dir="), strings.HasPrefix(w, "--work-tree="):
			_, v, _ := strings.Cut(w, "=")
			r.dir(arg{text: v, ok: true})
		case strings.HasPrefix(w, "-C"):
			r.dir(arg{text: w[2:], ok: true})
		case strings.HasPrefix(w, "-"):
		default:
			return
		}
	}
}

// clusterDirs records make's or tar's -C dir, glued or not, in a cluster
// after letters that take none (values lists those that take one), and its
// long form by a prefix of at least min bytes.
func (r *reader) clusterDirs(args []arg, values, long string, minLen int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(rest string) {
			switch {
			case rest != "":
				r.dir(arg{text: rest, ok: true})
			case i+1 < len(args):
				i++
				r.dir(args[i])
			}
		}
		switch {
		case !a.ok:
			if strings.HasPrefix(a.text, "-") && !strings.HasPrefix(a.text, "--") && strings.Contains(a.text, "C") {
				r.dirLost()
			}
		case a.text == "--":
			return
		case strings.HasPrefix(a.text, "--"):
			name, v, glued := strings.Cut(a.text[2:], "=")
			if len(name) >= minLen && strings.HasPrefix(long, name) {
				if glued {
					value(v)
				} else {
					value("")
				}
			}
		case len(a.text) > 1 && a.text[0] == '-':
			for k := 1; k < len(a.text); k++ {
				c := a.text[k]
				if c == 'C' {
					value(a.text[k+1:])
					break
				}
				if strings.IndexByte(values, c) >= 0 {
					if k+1 == len(a.text) {
						i++
					}
					break
				}
			}
		}
	}
}

// allowlisted are the names a write may replace to change what a call the
// reader trusts runs.
var allowlisted = func() []string {
	names := slices.Concat(flagFree, builtins, awks, scriptShells, []string{"date", "diff", "sort", "uniq", "sed", "rg", "fd", "find", "git", "gh", "glab", "dd"})
	for w := range wrapperFlags {
		names = append(names, w)
	}
	for w := range writerFlags {
		names = append(names, w)
	}
	return names
}()

// replacesProgram reports a target whose basename is, or as a glob may
// match, an allowlisted command name.
func replacesProgram(target string) bool {
	base := target[strings.LastIndex(target, "/")+1:]
	if base == "" {
		return false
	}
	if slices.Contains(allowlisted, base) {
		return true
	}
	if !strings.ContainsAny(base, "*?[(") {
		return false
	}
	pattern := globTokens(base)
	return slices.ContainsFunc(allowlisted, func(name string) bool { return globMatch(pattern, name) })
}

// startsProgram reports an unresolved target's literal prefix whose last
// element, the name the rest continues, may start an allowlisted command
// name; an empty one may start any.
func startsProgram(prefix string) bool {
	partial := prefix[strings.LastIndex(prefix, "/")+1:]
	pattern := append(globTokens(partial), globToken{star: true})
	return slices.ContainsFunc(allowlisted, func(name string) bool { return globMatch(pattern, name) })
}

// globToken is one element of a glob: a byte, any one byte, or any run.
type globToken struct {
	b         byte
	one, star bool
}

// globTokens reads a bash glob loosely, matching at least what bash does: an
// extglob group is a run, a bracket expression any one byte.
func globTokens(g string) []globToken {
	var out []globToken
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '\\' && i+1 < len(g):
			i++
			out = append(out, globToken{b: g[i]})
		case i+1 < len(g) && g[i+1] == '(' && strings.IndexByte("@!?*+", c) >= 0:
			out = append(out, globToken{star: true})
			depth := 0
			for i++; i < len(g); i++ {
				if g[i] == '(' {
					depth++
				} else if g[i] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
		case c == '*':
			out = append(out, globToken{star: true})
		case c == '?':
			out = append(out, globToken{one: true})
		case c == '[':
			end := i + 1
			if end < len(g) && (g[end] == '!' || g[end] == '^') {
				end++
			}
			if end < len(g) && g[end] == ']' {
				end++
			}
			if k := strings.IndexByte(g[end:], ']'); k >= 0 {
				out = append(out, globToken{one: true})
				i = end + k
				continue
			}
			out = append(out, globToken{b: c})
		default:
			out = append(out, globToken{b: c})
		}
	}
	return out
}

// globMatch matches name against tokens.
func globMatch(tokens []globToken, name string) bool {
	// at[j] reports name[:j] matched by the tokens so far.
	at := make([]bool, len(name)+1)
	at[0] = true
	for _, t := range tokens {
		next := make([]bool, len(name)+1)
		for j := range at {
			switch {
			case !at[j]:
			case t.star:
				for k := j; k <= len(name); k++ {
					next[k] = true
				}
			case j < len(name) && (t.one || name[j] == t.b):
				next[j+1] = true
			}
		}
		at = next
	}
	return at[len(name)]
}
