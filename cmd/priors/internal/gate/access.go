package gate

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Host is the environment ShellAccess renders a command's paths in. Gate
// 2's stand-ins keep a listed name's place; a path needs the real home and
// variables.
type Host struct {
	Home string
	// Env looks up a variable the command does not assign.
	Env func(string) (string, bool)
	// Users returns a user's home dir, for ~user.
	Users func(string) (string, bool)
}

// lookup reads a variable from the host's environment. PWD and OLDPWD are the
// hook's, not the command's, which may change dir before it reads them.
func (h *Host) lookup(name string) (string, bool) {
	if h.Env == nil || name == "PWD" || name == "OLDPWD" {
		return "", false
	}
	return h.Env(name)
}

// Access is what a shell command may touch.
type Access struct {
	Words []Touch
	// Writes reports a command that may write somewhere: one not known to only
	// read, or an output redirect to a file.
	Writes bool
	// Unreadable names why the command could not be read in full: a budget or
	// a panic.
	Unreadable string
}

// Touch is one path-like word a command names.
type Touch struct {
	// Text is the rendered word, or for an opaque one what precedes the part
	// that cannot be rendered.
	Text string
	// Write marks a word the command may write through: an operand of a
	// command not known to only read, or an output redirect's target.
	Write bool
	// Opaque marks a word whose value the text does not tell: a command
	// substitution, an unknown variable, a text the parser cannot read, or the
	// operands xargs takes from stdin.
	Opaque bool
	// Chdir marks a directory the command changes into.
	Chdir bool
}

const (
	maxTouches = 1 << 14
	// maxSubTokens bounds the path-like tokens taken from one field.
	maxSubTokens = 256
)

var (
	// readers only read their operands and run none of them; a command not
	// listed may write any operand. Each takes its operands through PATH, so
	// a command word with a slash is not one of them.
	readers = []string{
		"cat", "head", "tail", "wc", "ls", "stat", "readlink", "realpath", "basename", "dirname",
		"cmp", "diff", "du", "df", "md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum",
		"sha512sum", "b2sum", "cksum", "echo", "printf", "test", "[", "true", "false", ":", "pwd",
		"cd", "pushd", "popd", "which", "whereis", "type", "grep", "egrep", "fgrep", "jq", "nl",
		"od", "hexdump", "strings", "cut", "tr", "comm", "join", "paste", "fold", "rev", "tac",
		"id", "whoami", "date", "uname",
	}
	// gitReaders leave the work tree as it is: they read, or write only git's
	// own index, objects and refs.
	gitReaders = []string{
		"status", "log", "show", "diff", "ls-files", "ls-tree", "rev-parse", "cat-file", "grep",
		"blame", "shortlog", "describe", "rev-list", "show-ref", "for-each-ref", "merge-base",
		"name-rev", "add", "commit", "fetch", "push",
	}
	// findActions make find write or run a command.
	findActions = []string{"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"}
	// programVars change which program a command name, or a shell, runs; so
	// do the GIT_ and LD_ ones.
	programVars = []string{"PATH", "BASH_ENV", "ENV"}
	// shellVars are the variables a script can change without assigning them
	// (`let`, `read`, `mapfile`, `(( ))`, a nameref), which move what a name
	// runs, where cd looks, or how a word splits.
	shellVars = []string{"PATH", "BASH_ENV", "ENV", "CDPATH", "IFS"}
	// declarers take -n to declare a nameref.
	declarers = []string{"declare", "typeset", "local"}
	// chdirTools take -C as the dir they run in.
	chdirTools = []string{"git", "make", "tar"}
	// sinks are redirect targets that are not files.
	sinks = []string{"/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty"}
	// pathBreaks end a path-like token inside a word that is not itself a
	// path: code, a URL query, a key=value list.
	pathBreaks = " \t\n\r\v\f\"'`;&|()<>{}=,:$"
)

// ShellAccess reads the texts a shell call runs, as one command, for every
// word that may name a path and whether anything in it may write. Words are
// rendered as gate 2 renders them, in two environments: every variable unset
// but HOME, and every variable the command does not assign read from host. A
// word with a part neither tells is opaque. Each word is also searched for
// path-like tokens, with printf escapes decoded, as code in an interpreter's
// argument names paths that way. A writer's word that still holds a shell
// metacharacter is read again as a script one level deeper, as `bash -c` and
// `eval` run it, and so is a heredoc a writer reads. What the parser cannot
// read adds an opaque write and its path-like tokens.
func ShellAccess(host *Host, texts ...string) (acc Access) {
	defer func() {
		if recover() != nil {
			acc = Access{Unreadable: "fallback"}
		}
	}()
	if host == nil {
		host = &Host{}
	}
	a := &accessor{
		j:       &judge{seen: map[string]bool{}, vars: map[string]string{}, host: host},
		tainted: map[string]bool{},
		done:    map[*syntax.Word]bool{},
		touched: map[Touch]bool{},
		piped:   map[*syntax.Stmt]bool{},
	}
	for _, text := range texts {
		a.script(text, 0)
	}
	return a.acc
}

// accessor collects one command's Access, sharing gate 2's budget.
type accessor struct {
	j *judge
	// tainted are variables whose rendered value may not be the one a word
	// reads: assigned more than once, from an opaque word, or to a list.
	tainted map[string]bool
	// done are the words a command or redirect has already read.
	done    map[*syntax.Word]bool
	touched map[Touch]bool
	// piped are the statements that read a pipe.
	piped map[*syntax.Stmt]bool
	// cdpath and ifs report a script that sets CDPATH or IFS, or declares a
	// nameref that may name either, by a means the assignments do not show.
	// While ifs holds a word's split cannot be told, so an unquoted expansion
	// is opaque.
	cdpath, ifs bool
	acc         Access
}

func (a *accessor) script(text string, depth int) {
	if a.j.seen[text] {
		return
	}
	a.j.seen[text] = true
	a.j.parsed += parseBase + parseCost*len(text)
	if depth > maxDepth || a.j.parsed > maxParsed || len(text) > maxText || nesting(text) > maxNesting {
		a.acc.Unreadable = "budget"
		return
	}
	var stmts []*syntax.Stmt
	failed := false
	for stmt, err := range syntax.NewParser(syntax.Variant(syntax.LangBash)).StmtsSeq(strings.NewReader(text)) {
		if err != nil {
			failed = true
			break
		}
		stmts = append(stmts, stmt)
	}
	a.scan(text, stmts)
	for _, stmt := range stmts {
		if a.acc.Unreadable != "" {
			break
		}
		a.stmt(stmt, text, depth)
	}
	if failed && a.acc.Unreadable == "" {
		a.unparsed(text)
	}
}

// scan reads a script before any of it is read as commands, since a later
// statement can change what an earlier function body runs: a script that names
// one of shellVars other than as an expansion, or declares a nameref, writes.
func (a *accessor) scan(text string, stmts []*syntax.Stmt) {
	for _, name := range shellVars {
		if namesVar(text, name) {
			a.changes(name == "CDPATH", name == "IFS")
		}
	}
	for _, s := range stmts {
		walkBounded(s, func(n syntax.Node) bool {
			if declaresNameref(n) {
				a.changes(true, true)
			}
			return !arithmetic(n) && a.j.parsed <= maxParsed
		})
	}
}

func (a *accessor) changes(cdpath, ifs bool) {
	a.acc.Writes = true
	a.cdpath = a.cdpath || cdpath
	a.ifs = a.ifs || ifs
}

// namesVar reports text holding name as a whole word that is not an
// expansion: not after a $ or {, nor inside a longer name.
func namesVar(text, name string) bool {
	for from := 0; ; {
		k := strings.Index(text[from:], name)
		if k < 0 {
			return false
		}
		start := from + k
		end := start + len(name)
		from = end
		if start > 0 && (text[start-1] == '$' || text[start-1] == '{' || nameByte(text[start-1])) {
			continue
		}
		if end < len(text) && nameByte(text[end]) {
			continue
		}
		return true
	}
}

// declaresNameref reports a declare, typeset or local with an option cluster
// holding n; a word that is not literal may be one.
func declaresNameref(n syntax.Node) bool {
	var opts []string
	switch n := n.(type) {
	case *syntax.DeclClause:
		if !slices.Contains(declarers, n.Variant.Value) {
			return false
		}
		for _, o := range n.Args {
			if o.Name == nil && o.Value != nil {
				opts = append(opts, literal(o.Value))
			}
		}
	case *syntax.CallExpr:
		words := make([]string, len(n.Args))
		for i, w := range n.Args {
			words[i] = literal(w)
		}
		idx, _ := commandWord(words)
		if idx == len(words) || !slices.Contains(declarers, words[idx]) {
			return false
		}
		opts = words[idx+1:]
	}
	return slices.ContainsFunc(opts, func(w string) bool {
		return w == "" || len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.Contains(w, "n")
	})
}

// unparsed reads text the parser cannot: whatever it runs is unknown, so it
// is an opaque write, and its path-like tokens, quotes deleted, are writes.
func (a *accessor) unparsed(text string) {
	a.acc.Writes = true
	a.touch(Touch{Write: true, Opaque: true})
	a.field(overflagQuotes.Replace(text), true)
}

func (a *accessor) stmt(s *syntax.Stmt, text string, depth int) {
	walkBounded(s, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Assign:
			switch {
			case n.Name == nil:
			case n.Array != nil:
				a.assign(n.Name.Value, elemValues(n.Array)...)
			default:
				a.assign(n.Name.Value, n.Value)
			}
		case *syntax.ParamExp:
			if n.Param != nil && n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				a.assign(n.Param.Value, n.Exp.Word)
			}
		case *syntax.WordIter:
			a.assign(n.Name.Value, n.Items...)
		}
		return !arithmetic(n) && a.j.parsed <= maxParsed
	})
	walkBounded(s, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				a.piped[n.Y] = true
			}
		case *syntax.Stmt:
			a.command(n, text, depth)
		case *syntax.Word:
			if !a.done[n] {
				a.done[n] = true
				a.word(n, text, depth, false, false)
			}
		}
		if arithmetic(n) {
			a.arithmetic(source(text, n))
			return false
		}
		return a.acc.Unreadable == "" && a.j.parsed <= maxParsed
	})
}

// assign records name's value as gate 2 does, and taints it when a word may
// read another value than the one recorded. Setting a variable that picks the
// program a name runs writes, whatever the command that follows.
func (a *accessor) assign(name string, values ...*syntax.Word) {
	if slices.Contains(programVars, name) || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "LD_") {
		a.acc.Writes = true
	}
	a.ifs = a.ifs || name == "IFS"
	if _, ok := a.j.vars[name]; ok || len(values) != 1 || values[0] == nil || a.opaque(values[0]) {
		a.tainted[name] = true
	}
	a.j.assign(name, values...)
}

// arithmetic reads an arithmetic expression, which is never walked: a
// substitution in it runs a command that cannot be seen.
func (a *accessor) arithmetic(src string) {
	runs := strings.Contains(src, "$(") || strings.Contains(src, "`") || strings.Contains(src, "<(") || strings.Contains(src, ">(")
	if runs {
		a.acc.Writes = true
		a.touch(Touch{Write: true, Opaque: true})
	}
	a.field(src, runs)
}

// command reads one statement's simple command and its redirects.
func (a *accessor) command(st *syntax.Stmt, text string, depth int) {
	writer := false
	if call, ok := st.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 {
		writer = a.call(call, text, depth)
	}
	// A writer reading a pipe or a file may take the paths it writes, or a
	// script (`printf 'rm %s' … | sh`), from what it reads.
	if writer && (a.piped[st] || slices.ContainsFunc(st.Redirs, func(r *syntax.Redirect) bool { return r.Op == syntax.RdrIn })) {
		a.touch(Touch{Write: true, Opaque: true})
	}
	for _, r := range st.Redirs {
		a.redirect(r, writer, text, depth)
	}
}

// call reads a simple command's words and reports whether it may write.
func (a *accessor) call(c *syntax.CallExpr, text string, depth int) bool {
	argv := a.argv(c.Args, text)
	idx, afterFlag := commandWord(argv)
	name := ""
	if idx < len(argv) {
		name = argv[idx]
	}
	assigned := len(c.Assigns) > 0 || slices.ContainsFunc(argv[:idx], assignment)
	// A word renders to any number of fields, so the command word is found
	// again among the syntax words to leave it out of the writes.
	cmdArg := commandArg(c.Args, argv, idx)
	args := argv[min(idx+1, len(argv)):]
	heads := args
	if cmdArg >= 0 {
		heads = a.heads(c.Args[idx+1:], args)
	}
	// A command word found after a flag may be that flag's value, not the
	// command that runs; none found after one may be a wrapper's split string
	// (`env -S 'rm -rf x'`), which runs.
	writer := afterFlag || idx < len(argv) && (assigned || !readOnly(name, args, heads))
	if writer {
		a.acc.Writes = true
		// xargs writes through operands it reads from stdin.
		if slices.ContainsFunc(argv[:idx], func(w string) bool { return wordName(w) == "xargs" }) {
			a.touch(Touch{Write: true, Opaque: true})
		}
	}
	var messages []int
	if cmdArg >= 0 && (name == "git" || name == "gh" || name == "glab") {
		messages = messageArgs(name, args, heads)
	}
	for i, w := range c.Args {
		a.done[w] = true
		if slices.Contains(messages, i-idx-1) {
			continue
		}
		a.word(w, text, depth, writer && i != cmdArg, writer)
	}
	a.chdirs(name, c.Args, argv, idx, afterFlag, cmdArg, text)
	return writer
}

// argv renders a command's words in the host's environment, one field per
// word part the shell would split, with "" standing for an opaque word.
func (a *accessor) argv(words []*syntax.Word, text string) []string {
	argv := make([]string, 0, len(words))
	for _, w := range words {
		if a.opaque(w) {
			argv = append(argv, "")
			continue
		}
		fields, r := a.j.fields(real, text, w)
		if r != "" {
			a.acc.Unreadable = r
			return []string{""}
		}
		argv = append(argv, fields...)
	}
	return argv
}

// commandArg maps argv's command word back to the index of its syntax word,
// or -1 when the words render to fields one-to-one nowhere before it.
func commandArg(words []*syntax.Word, argv []string, idx int) int {
	if idx >= len(argv) || len(words) != len(argv) {
		return -1
	}
	return idx
}

// readOnly reports a command that only reads its operands. An empty word in
// args is opaque, and may be any flag.
func readOnly(name string, args, heads []string) bool {
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	switch name {
	case "priors":
		// priors writes a store only through its own gates.
		return true
	case "git":
		return gitReads(args, heads)
	case "sed":
		return sedReads(args)
	case "printf":
		return !printfAssigns(args)
	case "rg":
		return !slices.ContainsFunc(args, func(w string) bool { return w == "" || strings.HasPrefix(w, "--pre") })
	case "find":
		return !slices.ContainsFunc(args, func(w string) bool { return w == "" || slices.Contains(findActions, w) })
	case "fd":
		return !slices.ContainsFunc(args, fdRuns)
	}
	return slices.Contains(readers, name)
}

// printfAssigns reports a printf whose -v sets a variable.
func printfAssigns(args []string) bool {
	for _, w := range args {
		if !strings.HasPrefix(w, "-") {
			return false
		}
		if strings.HasPrefix(w, "-v") {
			return true
		}
	}
	return false
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

var (
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
	// forgeMessageOpts take a message or body as their value, but only in a
	// command of forgeMessageGroups and forgeMessageVerbs.
	forgeMessageOpts   = []string{"-b", "--body", "-t", "--title", "-m", "--message", "--notes", "--description"}
	forgeGlueOpts      = []string{"--body=", "--title=", "--message=", "--notes=", "--description="}
	forgeMessageGroups = []string{"pr", "issue", "mr", "release"}
	forgeMessageVerbs  = []string{"create", "edit", "comment", "review", "merge", "close", "reopen", "note"}
)

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
	skip := messageArgs("git", args, heads)
	for i := sub + 1; i < len(args); i++ {
		if !slices.Contains(skip, i) && gitRuns(args[i]) {
			return false
		}
	}
	return true
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

// messageArgs returns the indexes in args of the words that are a git
// commit's or a forge command's message, which is text, not a path. heads
// holds each word's text, or the start of an opaque one.
func messageArgs(name string, args, heads []string) []int {
	switch name {
	case "git":
		sub, ok := gitSub(args)
		if !ok || sub == len(args) || args[sub] != "commit" {
			return nil
		}
		return messageWords(args, heads, sub+1, []string{"-m", "--message"}, []string{"--message="}, true)
	case "gh", "glab":
		if !forgeMessages(args) {
			return nil
		}
		return messageWords(args, heads, 0, forgeMessageOpts, forgeGlueOpts, false)
	}
	return nil
}

// forgeMessages reports a forge command whose first two words that are not
// flags are a group and a verb that take a message.
func forgeMessages(args []string) bool {
	var words []string
	for _, w := range args {
		if len(words) < 2 && !strings.HasPrefix(w, "-") {
			words = append(words, w)
		}
	}
	return len(words) == 2 && slices.Contains(forgeMessageGroups, words[0]) && slices.Contains(forgeMessageVerbs, words[1])
}

// messageWords scans args from i for an option in opts, or with cluster a
// short cluster of commit flags ending in m, and returns the index of its
// value, and for a word starting with one of glue its own. An opaque word
// (an empty arg) whose head is such a cluster, glued to its message, is one.
func messageWords(args, heads []string, from int, opts, glue []string, cluster bool) []int {
	var out []int
	for i := from; i < len(args); i++ {
		w, head := args[i], heads[i]
		glued, short := false, false
		if cluster {
			glued, short = shortMessage(head)
		}
		switch {
		case slices.Contains(opts, w), short && !glued && w != "":
			if i++; i < len(args) {
				out = append(out, i)
			}
		case short, hasAnyPrefix(head, glue):
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

// heads returns the text of each word in args, or for an opaque one what
// precedes its first opaque part.
func (a *accessor) heads(words []*syntax.Word, args []string) []string {
	heads := slices.Clone(args)
	for i, w := range words {
		if prefix, ok := a.opaquePrefix(w); ok {
			heads[i] = prefix
		}
	}
	return heads
}

// sedLong are the long options of sed that take no value and read or write
// nothing.
var sedLong = []string{
	"quiet", "silent", "debug", "posix", "regexp-extended", "sandbox", "separate", "unbuffered",
	"null-data", "zero-terminated", "follow-symlinks", "help", "version",
}

// sedReads reports a sed command that edits nothing in place, reads no script
// file, and whose scripts hold no w, W or e, which write a file or run one.
// The letters are refused anywhere in a script, regexes included. Options are
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

func sedScript(s string) bool { return s != "" && !strings.ContainsAny(s, "wWe") }

// chdirs records the dirs a command changes into: cd's and pushd's operand
// (home without one), the -C dir of git, make and tar, and the chdir option of
// an env or sudo word before the command word. One whose value cannot be told
// (cd -, popd, pushd +N, an opaque word) is an opaque chdir.
func (a *accessor) chdirs(name string, words []*syntax.Word, argv []string, idx int, afterFlag bool, cmdArg int, text string) {
	syntaxAt := func(i int) *syntax.Word {
		if cmdArg < 0 || i >= len(words) {
			return nil
		}
		return words[i]
	}
	// After a flag the command word may be a flag's value, so the wrapper's own
	// options run to the end of the words.
	if afterFlag {
		for i, w := range argv {
			switch wordName(w) {
			case "env":
				a.chdirOpts(argv, i+1, syntaxAt, text, "-C", "--chdir")
			case "sudo":
				a.chdirOpts(argv, i+1, syntaxAt, text, "-D", "--chdir")
			}
		}
	}
	switch {
	case name == "cd" || name == "pushd":
		for i := idx + 1; i < len(argv); i++ {
			switch w := argv[i]; {
			case w == "-", name == "pushd" && stackIndex(w):
				a.touch(Touch{Opaque: true, Chdir: true})
			case strings.HasPrefix(w, "-") && w != "":
				continue
			default:
				a.chdir(syntaxAt(i), w, text)
				if a.searchesCDPATH(w) {
					a.touch(Touch{Opaque: true, Chdir: true})
				}
			}
			return
		}
		if name == "cd" {
			a.touch(Touch{Text: a.j.host.Home, Chdir: true})
			return
		}
		a.touch(Touch{Opaque: true, Chdir: true})
	case name == "popd":
		a.touch(Touch{Opaque: true, Chdir: true})
	case slices.Contains(chdirTools, name):
		a.chdirOpts(argv, idx+1, syntaxAt, text, "-C", "--directory", "--work-tree", "--git-dir")
	}
}

// stackIndex reports a +N or -N operand, which names a dir on the stack.
func stackIndex(w string) bool {
	return len(w) > 1 && (w[0] == '+' || w[0] == '-') && digits(w[1:])
}

// chdirOpts records the dir each option in argv[from:] sets: short as `-C dir`
// or `-Cdir`, a long one as `--opt dir` or `--opt=dir`.
func (a *accessor) chdirOpts(argv []string, from int, syntaxAt func(int) *syntax.Word, text, short string, long ...string) {
	for i := from; i < len(argv); i++ {
		w := argv[i]
		switch {
		case w == short || slices.Contains(long, w):
			if i+1 < len(argv) {
				a.chdir(syntaxAt(i+1), argv[i+1], text)
			}
		case strings.HasPrefix(w, short) && len(w) > len(short):
			a.touch(Touch{Text: w[len(short):], Chdir: true})
		case strings.HasPrefix(w, "--"):
			if opt, v, ok := strings.Cut(w, "="); ok && slices.Contains(long, opt) {
				a.touch(Touch{Text: v, Chdir: true})
			}
		}
	}
}

// searchesCDPATH reports a cd operand that may resolve under a CDPATH dir:
// relative, and not starting with . or .., which skip it, while CDPATH is
// assigned in the command or set on the host.
func (a *accessor) searchesCDPATH(operand string) bool {
	first, _, _ := strings.Cut(operand, "/")
	if operand == "" || first == "" || first == "." || first == ".." {
		return false
	}
	if _, ok := a.j.vars["CDPATH"]; ok || a.tainted["CDPATH"] || a.cdpath {
		return true
	}
	v, ok := a.j.host.lookup("CDPATH")
	return ok && v != ""
}

// chdir records one dir operand: its syntax word when the fields map back to
// one, else its rendered field, which is "" for an opaque word.
func (a *accessor) chdir(w *syntax.Word, field, text string) {
	if w == nil {
		a.touch(Touch{Text: field, Opaque: field == "", Chdir: true})
		return
	}
	if prefix, ok := a.opaquePrefix(w); ok {
		a.touch(Touch{Text: prefix, Opaque: true, Chdir: true})
		return
	}
	for _, e := range a.envs(w) {
		fields, r := a.j.fields(e, text, w)
		if r != "" {
			a.acc.Unreadable = r
			return
		}
		for _, f := range fields {
			a.touch(Touch{Text: f, Chdir: true})
		}
	}
}

// redirect reads one redirect. Output to a file is a write; a heredoc or
// here-string is read as a script when a writer reads it, and as data
// otherwise.
func (a *accessor) redirect(r *syntax.Redirect, writer bool, text string, depth int) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if r.Word != nil {
			a.done[r.Word] = true
		}
		if r.Hdoc != nil {
			a.done[r.Hdoc] = true
			a.body(r.Hdoc, text, depth, writer)
		}
		return
	case syntax.WordHdoc:
		if r.Word != nil {
			a.done[r.Word] = true
			a.body(r.Word, text, depth, writer)
		}
		return
	default:
	}
	if r.Word == nil {
		return
	}
	a.done[r.Word] = true
	write := outputOp(r.Op) && !a.sink(r)
	if write {
		a.acc.Writes = true
	}
	a.word(r.Word, text, depth, write, false)
}

// outputOp reports an operator that may write its word; one the parser adds
// later counts as writing.
func outputOp(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrIn, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return false
	default:
		return true
	}
}

// sink reports an output redirect that writes no file: a dup to an fd, or a
// device.
func (a *accessor) sink(r *syntax.Redirect) bool {
	l := literal(r.Word)
	if r.Op == syntax.DplOut && (l == "-" || digits(l)) {
		return true
	}
	return slices.Contains(sinks, l) || strings.HasPrefix(l, "/dev/fd/")
}

// body reads a heredoc or here-string: as a script when a writer reads it,
// since a shell runs it, else as data.
func (a *accessor) body(w *syntax.Word, text string, depth int, writer bool) {
	fields, r := a.j.fields(real, text, w)
	if r != "" {
		a.acc.Unreadable = r
		return
	}
	body := strings.Join(fields, " ")
	a.field(body, writer)
	if writer {
		a.script(body, depth+1)
	}
}

// word records w's fields in each environment, or the prefix of an opaque
// one, and reads a writer's field that still holds a shell metacharacter as
// a script one level deeper.
func (a *accessor) word(w *syntax.Word, text string, depth int, write, writer bool) {
	if prefix, ok := a.opaquePrefix(w); ok {
		a.touch(Touch{Text: prefix, Write: write, Opaque: true})
	}
	for _, e := range a.envs(w) {
		fields, r := a.j.fields(e, text, w)
		if r != "" {
			a.acc.Unreadable = r
			return
		}
		for _, f := range fields {
			a.field(f, write)
			if writer && strings.ContainsAny(f, scriptBytes) {
				a.script(f, depth+1)
			}
		}
	}
}

func (a *accessor) envs(w *syntax.Word) []env {
	if readsVar(w) {
		return []env{unset, real}
	}
	return []env{unset}
}

// field records f and the path-like tokens in it and its decodings.
func (a *accessor) field(f string, write bool) {
	a.touch(Touch{Text: f, Write: write})
	for _, d := range Decodings(f) {
		for _, t := range a.subTokens(d) {
			a.touch(Touch{Text: t, Write: write})
		}
	}
}

// subTokens returns the path-like tokens in s: runs between pathBreaks that
// hold a slash, start with ~ or name .attest, each also from its first slash
// or ~ on, as `-o/path` glues one to a flag. $HOME and ~ are expanded.
func (a *accessor) subTokens(s string) []string {
	if a.j.parsed += len(s); a.j.parsed > maxParsed {
		a.acc.Unreadable = "budget"
		return nil
	}
	home := a.j.host.Home
	s = strings.NewReplacer("${HOME}", home, "$HOME", home).Replace(s)
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(pathBreaks, r) }) {
		if len(out) >= maxSubTokens {
			break
		}
		if !strings.ContainsAny(t, "/~") && !strings.Contains(t, ".attest") {
			continue
		}
		if k := strings.IndexAny(t, "/~"); k > 0 {
			out = append(out, a.tildeToken(t[k:]))
		}
		out = append(out, a.tildeToken(t))
	}
	return out
}

func (a *accessor) tildeToken(t string) string {
	if t == "~" || strings.HasPrefix(t, "~/") {
		return a.j.host.Home + t[1:]
	}
	return t
}

func (a *accessor) touch(t Touch) {
	if a.touched[t] {
		return
	}
	if len(a.touched) >= maxTouches {
		a.acc.Unreadable = "budget"
		return
	}
	a.touched[t] = true
	a.acc.Words = append(a.acc.Words, t)
}

// opaque reports a word holding a part whose value the text does not tell.
func (a *accessor) opaque(w *syntax.Word) bool {
	_, ok := a.cut(w.Parts, false)
	return ok
}

// opaquePrefix renders what precedes an opaque word's first opaque part.
func (a *accessor) opaquePrefix(w *syntax.Word) (string, bool) {
	prefix, ok := a.cut(w.Parts, false)
	if !ok {
		return "", false
	}
	r := &render{env: real, vars: a.j.vars, host: a.j.host, whole: true, spent: &a.j.parsed}
	r.parts(prefix, false)
	return r.cur.String(), true
}

// cut returns parts up to the first opaque one, cutting inside a
// double-quoted part, and whether there is one.
func (a *accessor) cut(parts []syntax.WordPart, quoted bool) ([]syntax.WordPart, bool) {
	for i, p := range parts {
		if dq, ok := p.(*syntax.DblQuoted); ok {
			if inner, cut := a.cut(dq.Parts, true); cut {
				return append(slices.Clone(parts[:i]), &syntax.DblQuoted{Parts: inner}), true
			}
			continue
		}
		if a.opaquePart(p, i, quoted) {
			return parts[:i], true
		}
	}
	return nil, false
}

func (a *accessor) opaquePart(p syntax.WordPart, i int, quoted bool) bool {
	switch p := p.(type) {
	case *syntax.Lit:
		// ~+ and ~- are the command's PWD and OLDPWD.
		return i == 0 && (strings.HasPrefix(p.Value, "~+") || strings.HasPrefix(p.Value, "~-"))
	case *syntax.CmdSubst:
		return true
	case *syntax.ParamExp:
		return a.ifs && !quoted || a.opaqueParam(p)
	}
	return false
}

// opaqueParam reports an expansion the renderer reads approximately (an
// operator past the default ones, an index, a slice, indirection) or one of a
// variable it does not know.
func (a *accessor) opaqueParam(p *syntax.ParamExp) bool {
	switch {
	case p.Length:
		return false
	case p.Param == nil, p.Excl, p.Names != 0, p.Index != nil, p.Slice != nil, p.Repl != nil, p.Width:
		return true
	case p.Exp != nil && (p.Exp.Op > syntax.AssignUnsetOrNull || p.Exp.Word != nil && a.opaque(p.Exp.Word)):
		return true
	}
	return !a.known(p.Param.Value)
}

// known reports a variable whose value the renderer has: assigned once in the
// command, a special parameter that is no path, or set in the host's
// environment. IFS is not once the script may have changed it.
func (a *accessor) known(name string) bool {
	if a.tainted[name] || name == "IFS" && a.ifs {
		return false
	}
	if _, ok := a.j.vars[name]; ok {
		return true
	}
	if name == "HOME" || name == "IFS" || name == "0" || len(name) == 1 && strings.Contains("#?$!-", name) {
		return true
	}
	_, ok := a.j.host.lookup(name)
	return ok
}
