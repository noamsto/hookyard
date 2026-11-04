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
	// From is the argument a derived subtoken was read from, empty for a
	// whole word.
	From string
}

const (
	maxTouches = 1 << 14
	// maxSubTokens bounds the path-like tokens taken from one field.
	maxSubTokens = 256
	// maxLoopRuns bounds the combinations of values finite loop variables are
	// read in, so per-value touches stay inside maxTouches.
	maxLoopRuns = 64
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
	// nonSetters set no shell variable, a closed list: a builtin that may set one
	// by a name the text does not spell (read ?, mapfile -C, wait -p, set -x
	// with PS4) is left out, as is test, and printf is checked on its own.
	nonSetters = []string{
		"cat", "head", "tail", "wc", "ls", "stat", "readlink", "realpath", "basename", "dirname",
		"cmp", "diff", "du", "df", "md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum",
		"sha512sum", "b2sum", "cksum", "echo", "true", "false", ":", "pwd", "cd", "pushd", "popd",
		"which", "whereis", "type", "grep", "egrep", "fgrep", "jq", "nl", "od", "hexdump", "strings",
		"cut", "tr", "comm", "join", "paste", "fold", "rev", "tac", "id", "whoami", "date", "uname",
		"sort", "uniq", "gh", "glab", "claude", "crew", "git", "sed", "rg", "fd", "find",
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
		j:         &judge{seen: map[string]bool{}, vars: map[string]string{}, host: host},
		tainted:   map[string]bool{},
		done:      map[*syntax.Word]bool{},
		touched:   map[Touch]bool{},
		piped:     map[*syntax.Stmt]bool{},
		loops:     map[string][]string{},
		assigns:   map[string]int{},
		mentioned: map[string]bool{},
		exempted:  map[string]bool{},
	}
	for _, text := range texts {
		a.script(text, 0)
	}
	// A loop variable's listed values hold every value it is read with only
	// if nothing else sets it, which the whole command must be read to tell:
	// a setter read before its loop is registered counts too.
	for name := range a.exempted {
		if a.wild || a.nameref || a.unsafe || a.assigns[name] != 1 || a.mentioned[name] || a.named(name) {
			a.redefines = true
		}
	}
	// A data position is an exemption only when nothing in the whole command
	// redefines what a name runs; a trigger seen anywhere withdraws every
	// deferred data word, which is then judged like any other operand.
	if a.redefines && a.acc.Unreadable == "" {
		for _, d := range a.deferred {
			a.word(d.w, d.text, d.depth, d.write, d.writer, false)
		}
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
	// redefines reports a command that can redefine what a name runs (shell
	// state, a function, a redefiner call, an opaque or manufactured name). It
	// withdraws every data position, so deferred data words are judged after the
	// whole command is read.
	redefines bool
	// deferred are the data words a clean, known program named, held back until
	// the walk finishes: judged then only if redefines was seen anywhere.
	deferred []deferral
	// loops are the for variables over listed plain values, with those values,
	// which a word opaque only through them is read once per value with.
	loops map[string][]string
	// assigns counts the assignments to each name; a loop's own is one.
	assigns map[string]int
	// mentioned are the loop names a rendered field, an arithmetic source or a
	// redirect's {varname} names, any of which may set them again.
	mentioned map[string]bool
	// mentions are every string mention saw, in or out of a loop's reach,
	// checked against the exempted names once the walk finishes.
	mentions []string
	// pins are the loop variables pinned to one value each, innermost last.
	pins []pinning
	// nameref reports a declared nameref, which may set any loop variable.
	nameref bool
	// wild reports an opaque word not opaque only through finite loop
	// variables, or anything bash evaluates as arithmetic, which reads a
	// variable's value as an expression: either may compute a loop variable's
	// name.
	wild bool
	// unsafe reports anything that may set a shell variable by a name the text
	// does not spell: only commands that cannot set one leave a loop's listed
	// values the only ones it holds.
	unsafe bool
	// exempted are the loop names a call's per-value check let through.
	exempted map[string]bool
	acc      Access
}

// deferral is a data word held back from the walk, with the arguments word
// needs to judge it in the post-pass.
type deferral struct {
	w             *syntax.Word
	text          string
	depth         int
	write, writer bool
}

func (a *accessor) script(text string, depth int) {
	if a.j.seen[text] {
		return
	}
	a.j.seen[text] = true
	// A script is read once whatever value renders it, so none of its loop
	// reads may take a pinned value as the only one.
	defer a.unpin()()
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
		// Text the parser cannot read at depth 0 runs calls it cannot name.
		if depth == 0 {
			a.unsafe = true
		}
		a.unparsed(text)
	}
}

// scan reads a script before any of it is read as commands, since a later
// statement can change what an earlier function body runs: a script that names
// one of shellVars other than as an expansion, or declares a nameref, writes.
// A script that names a state variable or declares a function can also redefine
// what a name runs, which withdraws every data position.
func (a *accessor) scan(text string, stmts []*syntax.Stmt) {
	for _, name := range shellVars {
		if namesVar(text, name) {
			a.changes(name == "CDPATH", name == "IFS")
		}
	}
	for _, name := range stateNames {
		if namesVar(text, name) {
			a.redefines = true
		}
	}
	for _, s := range stmts {
		walkBounded(s, func(n syntax.Node) bool {
			if declares(n, "n") {
				a.changes(true, true)
				a.nameref = true
			}
			// An integer variable evaluates every value assigned to it as
			// arithmetic, which writes nothing by itself.
			if declares(n, "i") {
				a.wild = true
			}
			if _, ok := n.(*syntax.FuncDecl); ok {
				a.redefines = true
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

// countName counts the occurrences of name in s, a rendered field, that may
// set it: a whole name that is not an expansion reading it ($name, or ${name
// not followed by = or :=, after an optional index), not escaped (a variable
// name holds no backslash, so \n is text), not a flag spelling (-n) and not
// inside a longer name, unless it ends a short-option cluster (-vn).
func countName(s, name string) int {
	count := 0
	for from := 0; ; {
		k := strings.Index(s[from:], name)
		if k < 0 {
			return count
		}
		start := from + k
		end := start + len(name)
		from = start + 1
		if end < len(s) && nameByte(s[end]) {
			continue
		}
		switch {
		case start > 0 && (s[start-1] == '$' || s[start-1] == '\\'):
			continue
		case start > 1 && s[start-2:start] == "${":
			rest := s[end:]
			if strings.HasPrefix(rest, "[") {
				if i := strings.IndexByte(rest, ']'); i >= 0 {
					rest = rest[i+1:]
				}
			}
			if !strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, ":=") {
				continue
			}
		case start > 0 && nameByte(s[start-1]):
			run := start - 1
			for run > 0 && nameByte(s[run-1]) {
				run--
			}
			if !loneDash(s, run-1) {
				continue
			}
		case loneDash(s, start-1):
			continue
		}
		count++
	}
}

// loneDash reports a - at i that is not part of -- or of a word: at the start
// of s, or after a byte that is neither - nor a name byte.
func loneDash(s string, i int) bool {
	return i >= 0 && s[i] == '-' && (i == 0 || s[i-1] != '-' && !nameByte(s[i-1]))
}

// declares reports a declare, typeset or local with an option cluster holding
// flag; a word that is not literal may be one.
func declares(n syntax.Node, flag string) bool {
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
		return w == "" || len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.Contains(w, flag)
	})
}

// unparsed reads text the parser cannot: whatever it runs is unknown, so it
// is an opaque write, and its path-like tokens, quotes deleted, are writes.
func (a *accessor) unparsed(text string) {
	a.acc.Writes = true
	a.touch(Touch{Write: true, Opaque: true})
	a.field(overflagQuotes.Replace(text), true, false)
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
		case *syntax.ForClause:
			if wi, ok := n.Loop.(*syntax.WordIter); ok && !n.Select {
				a.loop(wi, text)
			}
		case *syntax.WordIter:
			a.assign(n.Name.Value, n.Items...)
			// assign keeps a lone item's text, but a brace, a glob or a split
			// makes it values the text does not show.
			if len(n.Items) == 1 && !glob(n.Items[0]) && expansions(n.Items) == 1 {
				fields, r := a.j.fields(real, text, n.Items...)
				if r != "" {
					a.acc.Unreadable = r
					return false
				}
				if len(fields) == 1 {
					break
				}
			}
			a.tainted[n.Name.Value] = true
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
				a.word(n, text, depth, false, false, false)
			}
		case *syntax.DeclClause:
			a.unsafe = true
			// A quoted operand (`declare 'a[x]=1'`) parses with no index.
			if strings.Contains(source(text, n), "[") {
				a.wild = true
			}
		case *syntax.Assign, *syntax.LetClause, *syntax.ArithmCmd, *syntax.TestClause, *syntax.CoprocClause, *syntax.FuncDecl:
			a.unsafe = true
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				a.unsafe = true
			}
		case *syntax.ForClause:
			if _, ok := n.Loop.(*syntax.WordIter); !ok || n.Select {
				a.unsafe = true
			}
		}
		if subscripts(n) {
			a.wild = true
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
	a.assigns[name]++
	if slices.Contains(programVars, name) || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "LD_") {
		a.acc.Writes = true
	}
	a.ifs = a.ifs || name == "IFS"
	if _, ok := a.j.vars[name]; ok || len(values) != 1 || values[0] == nil || a.opaque(values[0]) {
		a.tainted[name] = true
	}
	a.j.assign(name, values...)
}

// loop registers a for variable whose values the text tells: bash sets no
// lowercase name on its own, so a lowercase one changes only where the
// command names it. Its items must be plain and render to at most
// maxLoopRuns values, none empty: an item's expansion may read a value an
// untracked read set, a glob names files, and an empty value is the one a
// default assignment acts on.
func (a *accessor) loop(wi *syntax.WordIter, text string) {
	name := wi.Name.Value
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return
	}
	if slices.ContainsFunc(wi.Items, func(w *syntax.Word) bool { return !plain(w) || a.opaque(w) }) {
		return
	}
	n := expansions(wi.Items)
	if n == 0 || n > maxLoopRuns {
		return
	}
	values, r := a.j.fields(real, text, wi.Items...)
	if r != "" {
		a.acc.Unreadable = r
		return
	}
	// Each plain alternative renders to one field unless it is empty. A quoted
	// glob character is a glob again where the variable is read unquoted.
	if len(values) != n || slices.ContainsFunc(values, func(v string) bool { return strings.ContainsAny(v, "*?[") }) {
		return
	}
	a.loops[name] = values
}

// plain reports a word of literal and quoted parts only, with no glob.
func plain(w *syntax.Word) bool {
	if glob(w) {
		return false
	}
	return !slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		switch p := p.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
			return false
		case *syntax.DblQuoted:
			return slices.ContainsFunc(p.Parts, func(q syntax.WordPart) bool {
				_, ok := q.(*syntax.Lit)
				return !ok
			})
		}
		return true
	})
}

// glob reports a word holding an unquoted glob character or an extglob.
func glob(w *syntax.Word) bool {
	return slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		switch p := p.(type) {
		case *syntax.Lit:
			return strings.ContainsAny(p.Value, "*?[")
		case *syntax.ExtGlob:
			return true
		}
		return false
	})
}

// expansions counts the words brace expansion makes of words, saturating at
// maxParsed.
func expansions(words []*syntax.Word) int {
	n := 0
	for _, w := range words {
		split := *w
		syntax.SplitBraces(&split)
		n = min(n+braceCount(split.Parts), maxParsed)
	}
	return n
}

// arithmetic reads an arithmetic expression, which is never walked: a
// substitution in it runs a command that cannot be seen. Any name in it may
// hold an expression that sets a loop variable (`x='n=7'; (( x ))`).
func (a *accessor) arithmetic(src string) {
	runs := strings.Contains(src, "$(") || strings.Contains(src, "`") || strings.Contains(src, "<(") || strings.Contains(src, ">(")
	if runs {
		a.acc.Writes = true
		a.touch(Touch{Write: true, Opaque: true})
	}
	a.wild = true
	a.field(src, runs, false)
}

// subscripts reports a node bash evaluates as arithmetic outside an
// arithmetic node: an index, a C-style loop, an integer test, or a -v or -R
// test, whose operand may be subscripted.
func subscripts(n syntax.Node) bool {
	switch n := n.(type) {
	case *syntax.Assign:
		return n.Index != nil
	case *syntax.ArrayElem:
		return n.Index != nil
	case *syntax.CStyleLoop:
		return true
	case *syntax.BinaryTest:
		return n.Op >= syntax.TsEql && n.Op <= syntax.TsGtr
	case *syntax.UnaryTest:
		return n.Op == syntax.TsVarSet || n.Op == syntax.TsRefVar
	}
	return false
}

// nameSubscript reports a builtin whose variable-name operand may be
// subscripted, which bash evaluates as arithmetic.
func nameSubscript(name string, args []string) bool {
	bracket := slices.ContainsFunc(args, func(w string) bool { return strings.Contains(w, "[") })
	switch name {
	case "read", "unset", "declare", "typeset", "local", "readonly", "export":
		return bracket
	case "printf":
		return bracket && printfAssigns(args)
	case "test", "[":
		return slices.Contains(args, "-v") || slices.Contains(args, "-R")
	}
	return false
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
	v := a.view(c, text)
	argv, idx, name := v.argv, v.idx, v.name
	assigned := len(c.Assigns) > 0 || slices.ContainsFunc(argv[:idx], assignment)
	// A command word found after a flag may be that flag's value, not the
	// command that runs; none found after one may be a wrapper's split string
	// (`env -S 'rm -rf x'`), which runs.
	writer := v.afterFlag || idx < len(argv) && (assigned || !readOnly(name, v.args, v.heads))
	if writer {
		a.acc.Writes = true
		// xargs writes through operands it reads from stdin.
		if slices.ContainsFunc(argv[:idx], func(w string) bool { return wordName(w) == "xargs" }) {
			a.touch(Touch{Write: true, Opaque: true})
		}
	}
	// A command that can redefine what a name runs is never exempted: an opaque
	// command word, or a call to a builtin that installs an alias, a hash, a
	// function or a trap, forfeits every data position.
	if idx < len(argv) && name == "" || slices.Contains(redefiners, name) {
		a.redefines = true
	}
	if nameSubscript(name, v.args) {
		a.wild = true
	}
	// A wrapper before the command word (command read, builtin mapfile) is
	// outside nonSetters too. A nested script runs, if at all, in a child of a
	// call at depth 0, which sets this shell's variables only when it is
	// outside nonSetters itself; a writer's data word read as one names no call
	// that runs here.
	if depth == 0 && (idx > 0 || !a.setsNoVar(c, v, text)) {
		a.unsafe = true
	}
	var loops []string
	for i, w := range c.Args {
		a.done[w] = true
		write := writer && i != v.cmdArg
		if slices.Contains(v.data, i) {
			if a.opaque(w) {
				a.wild = true
			}
			a.deferred = append(a.deferred, deferral{w, text, depth, write, writer})
			continue
		}
		if write && a.opaque(w) {
			if names, ok := a.finiteOnly(w); ok {
				loops = append(loops, names...)
			} else {
				a.redefines = true
			}
		}
		a.word(w, text, depth, write, writer, i == v.cmdArg)
	}
	if len(loops) > 0 {
		a.perValue(c, text, v.data, loops)
	}
	a.chdirs(name, c.Args, argv, idx, v.afterFlag, v.cmdArg, text)
	return writer
}

// setsNoVar reports a call to one of nonSetters, or a printf whose first
// argument is a format and not a flag: one word, not opaque, with no glob, that
// renders to one field not starting with -.
func (a *accessor) setsNoVar(c *syntax.CallExpr, v callView, text string) bool {
	if v.name != "printf" {
		return slices.Contains(nonSetters, v.name)
	}
	if v.cmdArg < 0 || v.cmdArg+1 >= len(c.Args) {
		return false
	}
	format := c.Args[v.cmdArg+1]
	if a.opaque(format) || glob(format) {
		return false
	}
	fields, r := a.j.fields(real, text, format)
	return r == "" && len(fields) == 1 && !strings.HasPrefix(fields[0], "-")
}

// perValue checks a call whose opaque write words are opaque only through the
// finite loop variables names, which word reads once per value: each
// combination of values must also leave every data word a data word, as a
// value may shift what the program parses (`gh $s view`).
func (a *accessor) perValue(c *syntax.CallExpr, text string, data []int, names []string) {
	slices.Sort(names)
	names = slices.Compact(names)
	ok := a.runs(names, func() {
		pinned := a.view(c, text).data
		if slices.ContainsFunc(data, func(i int) bool { return !slices.Contains(pinned, i) }) {
			a.redefines = true
		}
	})
	if !ok {
		a.redefines = true
		return
	}
	for _, name := range names {
		a.exempted[name] = true
	}
}

// callView is what a call's rendered words make of it.
type callView struct {
	argv      []string
	idx       int
	afterFlag bool
	name      string
	// cmdArg is the command word's syntax index, -1 when the words render to
	// fields one-to-one nowhere before it.
	cmdArg      int
	args, heads []string
	// data are the syntax indexes of the data words of a clean, known program.
	data []int
}

func (a *accessor) view(c *syntax.CallExpr, text string) callView {
	v := callView{argv: a.argv(c.Args, text)}
	v.idx, v.afterFlag = commandWord(v.argv)
	if v.idx < len(v.argv) {
		v.name = v.argv[v.idx]
	}
	// A word renders to any number of fields, so the command word is found
	// again among the syntax words to leave it out of the writes.
	v.cmdArg = commandArg(c.Args, v.argv, v.idx)
	v.args = v.argv[min(v.idx+1, len(v.argv)):]
	v.heads = v.args
	if v.cmdArg >= 0 {
		v.heads = a.heads(c.Args[v.idx+1:], v.args)
		if a.cleanWrap(v.argv, v.idx) {
			for _, k := range messageArgs(v.name, v.args, v.heads) {
				v.data = append(v.data, v.idx+1+k)
			}
		}
	}
	return v
}

// cleanWrap reports that every word commandWord skipped to reach the command at
// idx is on a closed whitelist, so its identity can be trusted for a data
// position. A leading NAME=value is a quoted program name the shell execs
// literally, not an assignment (an assignment is parsed out of argv), so it is
// unclean; an assignment is clean only right after a bare env or sudo, a number
// only right after a bare timeout, and env's -u/-C flag+value pairs only after a
// bare env.
func (a *accessor) cleanWrap(argv []string, idx int) bool {
	last := ""
	awaitValue := false
	for i := range idx {
		w := argv[i]
		switch {
		case awaitValue:
			awaitValue = false
		case w != "" && !strings.Contains(w, "/") && slices.Contains(wrappers, wordName(w)):
			last = wordName(w)
		case (last == "env" || last == "sudo") && assignment(w):
		case last == "env" && envValueFlag(w):
			awaitValue = envSeparateFlag(w)
		case last == "timeout" && number(w):
		default:
			return false
		}
	}
	return true
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
	// forgeJSONSubs are the gh/glab subcommands that build JSON output, whose
	// --jq, --template and (gh) -q values are a filter, not a path.
	forgeJSONSubs = []string{
		"api", "pr", "issue", "mr", "repo", "run", "release", "search", "workflow", "gist", "cache",
		"label", "project", "ruleset", "secret", "variable", "status", "org", "ssh-key", "gpg-key", "codespace",
	}
	// dataTakers are the programs with a data position; a word, redirect target
	// or path subtoken naming one (other than a call's own command word)
	// manufactures a data-taker name, which withdraws every data position.
	dataTakers = []string{"claude", "gh", "glab", "crew"}
	// redefiners install an alias, a hash, a function, a trap or run text as
	// code, so a call to one withdraws every data position.
	redefiners = []string{"alias", "hash", "enable", "source", ".", "eval", "trap"}
	// stateNames are the variables whose assignment moves what a name runs; a
	// command naming one, even after rendering, withdraws every data position.
	stateNames = []string{"BASH_CMDS", "BASH_ALIASES", "PATH", "BASH_ENV", "ENV"}
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

// messageArgs returns the indexes in args of the words that are a known data
// position of a known program (a git commit's or forge message, a gh/glab JSON
// filter or template, the claude prompt, a crew message body), which is text,
// not a path. heads holds each word's text, or the start of an opaque one.
func messageArgs(name string, args, heads []string) []int {
	switch name {
	case "git":
		sub, ok := gitSub(args)
		if !ok || sub == len(args) || args[sub] != "commit" {
			return nil
		}
		return messageWords(args, heads, sub+1, []string{"-m", "--message"}, []string{"--message="}, true)
	case "gh", "glab":
		var out []int
		if forgeMessages(args) {
			out = messageWords(args, heads, 0, forgeMessageOpts, forgeGlueOpts, false)
		}
		return append(out, forgeJSON(name, args)...)
	case "claude":
		return claudePrompt(args)
	case "crew":
		return crewBody(args)
	}
	return nil
}

// forgeJSON returns the indexes of the gh/glab --jq, --template (and their =
// forms) values, plus gh's -q, when the subcommand is a known JSON-output
// builtin. The scan stops at the first --, past which the value runs another
// program. glab's -q is not a jq short, so only gh's is taken.
func forgeJSON(name string, args []string) []int {
	sub := firstNonFlag(args)
	if sub < 0 || !slices.Contains(forgeJSONSubs, args[sub]) {
		return nil
	}
	var out []int
	for i := sub + 1; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "--":
			return out
		case w == "--jq", w == "--template", name == "gh" && w == "-q":
			if i+1 < len(args) {
				out = append(out, i+1)
				i++
			}
		case strings.HasPrefix(w, "--jq="), strings.HasPrefix(w, "--template="):
			out = append(out, i)
		}
	}
	return out
}

// claudePrompt returns the index of the claude prompt: the word after a
// first-position -p/--print that does not start with -. Pinning it to position
// keeps a -p that is another option's value, a config flag's JSON, a mcp
// subcommand or anything past a -- judged, since none of those is first.
func claudePrompt(args []string) []int {
	if len(args) >= 2 && (args[0] == "-p" || args[0] == "--print") && !strings.HasPrefix(args[1], "-") {
		return []int{1}
	}
	return nil
}

// crewBody returns the index of a crew reply or msg body, the positional after
// the recipient (reply) or after the sender and recipient (msg), with any
// --crew <id> / --crew=<id> removed. The body is always data, a bare . or /
// included.
func crewBody(args []string) []int {
	want, n := -1, 0
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--crew":
			i++
			continue
		case strings.HasPrefix(args[i], "-"):
			continue
		}
		switch n {
		case 0:
			switch args[i] {
			case "reply":
				want = 2
			case "msg":
				want = 3
			default:
				return nil
			}
		case want:
			return []int{i}
		}
		n++
	}
	return nil
}

// firstNonFlag returns the index in args of the first word that is neither a
// flag nor opaque, or -1.
func firstNonFlag(args []string) int {
	for i, w := range args {
		if w != "" && !strings.HasPrefix(w, "-") {
			return i
		}
	}
	return -1
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
	// A bare env's -C/--chdir is a flag+value unit before the command word, so
	// it is scanned up to it. After any other wrapper flag the command word may
	// be that flag's value, so the wrapper's own options run to the end of the
	// words.
	end := idx
	if afterFlag {
		end = len(argv)
	}
	for i := 0; i < end; i++ {
		switch wordName(argv[i]) {
		case "env":
			a.chdirOpts(argv, i+1, end, syntaxAt, text, "-C", "--chdir")
		case "sudo":
			if afterFlag {
				a.chdirOpts(argv, i+1, end, syntaxAt, text, "-D", "--chdir")
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
		a.chdirOpts(argv, idx+1, len(argv), syntaxAt, text, "-C", "--directory", "--work-tree", "--git-dir")
	}
}

// stackIndex reports a +N or -N operand, which names a dir on the stack.
func stackIndex(w string) bool {
	return len(w) > 1 && (w[0] == '+' || w[0] == '-') && digits(w[1:])
}

// chdirOpts records the dir each option in argv[from:until] sets: short as
// `-C dir` or `-Cdir`, a long one as `--opt dir` or `--opt=dir`.
func (a *accessor) chdirOpts(argv []string, from, until int, syntaxAt func(int) *syntax.Word, text, short string, long ...string) {
	for i := from; i < until; i++ {
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
	// A {varname} sets that variable to the fd it opens; a subscript in it is
	// arithmetic.
	if r.N != nil && !digits(r.N.Value) {
		a.unsafe = true
		a.mention(r.N.Value)
		if strings.Contains(r.N.Value, "[") {
			a.wild = true
		}
	}
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
	a.word(r.Word, text, depth, write, false, false)
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
	if a.opaque(w) {
		a.wild = true
	}
	fields, r := a.j.fields(real, text, w)
	if r != "" {
		a.acc.Unreadable = r
		return
	}
	body := strings.Join(fields, " ")
	a.field(body, writer, false)
	if writer {
		a.script(body, depth+1)
	}
}

// word records w's fields in each environment, or the prefix of an opaque
// one, and reads a writer's field that still holds a shell metacharacter as
// a script one level deeper. cmd marks the call's own command word, which is
// exempt from the manufactured-name trigger. An opaque word read only through
// finite loop variables is also read once per combination of their values.
func (a *accessor) word(w *syntax.Word, text string, depth int, write, writer, cmd bool) {
	if prefix, ok := a.opaquePrefix(w); ok {
		a.touch(Touch{Text: prefix, Write: write, Opaque: true})
		names, finite := a.finiteOnly(w)
		if !finite || !a.runs(names, func() {
			if a.opaque(w) {
				a.wild = true
				return
			}
			a.word(w, text, depth, write, writer, cmd)
		}) {
			a.wild = true
		}
	}
	for _, e := range a.envs(w) {
		fields, r := a.j.fields(e, text, w)
		if r != "" {
			a.acc.Unreadable = r
			return
		}
		for _, f := range fields {
			a.field(f, write, cmd)
			if writer && strings.ContainsAny(f, scriptBytes) {
				// A command word that is itself a script is a split string a
				// wrapper runs (`env -S 'crew reply x /'`): the program is
				// resolved through splitting, not cleanly, so its re-read gets no
				// data position.
				if cmd {
					a.redefines = true
				}
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

// field records f and the path-like tokens in it and its decodings, and flags
// a word that redefines resolution: one naming a state variable (over the
// rendered field and its decodings, so a quote-split name is caught), or, unless
// it is a call's own command word, one whose basename is a data-taker.
func (a *accessor) field(f string, write, cmd bool) {
	a.touch(Touch{Text: f, Write: write})
	a.redefiner(f, cmd)
	for _, d := range Decodings(f) {
		a.redefiner(d, cmd)
		for _, t := range a.subTokens(d) {
			a.touch(Touch{Text: t, Write: write, From: f})
			if !cmd && slices.Contains(dataTakers, wordName(t)) {
				a.redefines = true
			}
		}
	}
}

// redefiner sets redefines when s names a state variable, or when it is not a
// command word and its basename is a data-taker. It also records the loop
// variables s mentions.
func (a *accessor) redefiner(s string, cmd bool) {
	a.mention(s)
	if !cmd && slices.Contains(dataTakers, wordName(s)) {
		a.redefines = true
	}
	for _, name := range stateNames {
		if namesVar(s, name) {
			a.redefines = true
			return
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

// mention records s and the registered loop variables it names in a way that
// may set them, charging both to the judge's budget.
func (a *accessor) mention(s string) {
	if a.j.parsed += len(s); a.j.parsed > maxParsed {
		a.acc.Unreadable = "budget"
		return
	}
	a.mentions = append(a.mentions, s)
	for name := range a.loops {
		if a.j.parsed += len(s); a.j.parsed > maxParsed {
			a.acc.Unreadable = "budget"
			return
		}
		if countName(s, name) > 0 {
			a.mentioned[name] = true
		}
	}
}

// named reports name in a string mention saw, charging the scan to the
// judge's budget.
func (a *accessor) named(name string) bool {
	for _, s := range a.mentions {
		if a.j.parsed += len(s); a.j.parsed > maxParsed {
			a.acc.Unreadable = "budget"
			return true
		}
		if countName(s, name) > 0 {
			return true
		}
	}
	return false
}

// loopReads returns the sorted registered loop variables w expands outside a
// substitution, whose insides are opaque whatever the loop holds.
func (a *accessor) loopReads(w *syntax.Word) []string {
	var names []string
	walkBounded(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		case *syntax.ParamExp:
			if n.Param != nil {
				if _, ok := a.loops[n.Param.Value]; ok {
					names = append(names, n.Param.Value)
				}
			}
		}
		return !arithmetic(n)
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// finite reports a loop variable that holds only its listed values so far:
// assigned only by its loop and not mentioned.
func (a *accessor) finite(name string) bool {
	_, ok := a.loops[name]
	return ok && a.assigns[name] == 1 && !a.mentioned[name]
}

// finiteOnly returns the loop variables w reads, when they are all finite and
// w is opaque only through them.
func (a *accessor) finiteOnly(w *syntax.Word) ([]string, bool) {
	names := a.loopReads(w)
	if len(names) == 0 || slices.ContainsFunc(names, func(n string) bool { return !a.finite(n) }) {
		return nil, false
	}
	first := make([]string, len(names))
	for i, name := range names {
		vals := a.loops[name]
		if len(vals) == 0 {
			return nil, false
		}
		first[i] = vals[0]
	}
	restore := a.pin(names, first)
	defer restore()
	return names, !a.opaque(w)
}

// pinning is one pin: the names it sets, their values, and what they held
// before.
type pinning struct {
	names, values []string
	old           []saved
}

type saved struct {
	value        string
	set, tainted bool
}

// pin sets each of names to its value in values, known, and returns what
// restores both as they were.
func (a *accessor) pin(names, values []string) (restore func()) {
	p := pinning{names: names, values: values, old: make([]saved, len(names))}
	for i, name := range names {
		p.old[i].value, p.old[i].set = a.j.vars[name]
		p.old[i].tainted = a.tainted[name]
	}
	a.apply(p)
	a.pins = append(a.pins, p)
	return func() {
		a.pins = a.pins[:len(a.pins)-1]
		a.lift(p)
	}
}

// unpin lifts every active pin, innermost first, and returns what applies
// them again.
func (a *accessor) unpin() (repin func()) {
	pins := a.pins
	for _, p := range slices.Backward(pins) {
		a.lift(p)
	}
	a.pins = nil
	return func() {
		for _, p := range pins {
			a.apply(p)
		}
		a.pins = pins
	}
}

func (a *accessor) apply(p pinning) {
	for i, name := range p.names {
		a.j.vars[name] = p.values[i]
		delete(a.tainted, name)
	}
}

func (a *accessor) lift(p pinning) {
	for i, name := range p.names {
		delete(a.j.vars, name)
		if p.old[i].set {
			a.j.vars[name] = p.old[i].value
		}
		delete(a.tainted, name)
		if p.old[i].tainted {
			a.tainted[name] = true
		}
	}
}

// runs calls f once per combination of the values of the loop variables
// names, in order, with them pinned; it reports false, calling none, when
// there are more than maxLoopRuns combinations.
func (a *accessor) runs(names []string, f func()) bool {
	lists := make([][]string, len(names))
	total := 1
	for i, name := range names {
		lists[i] = a.loops[name]
		if total *= len(lists[i]); total > maxLoopRuns {
			return false
		}
	}
	if total == 0 {
		return false
	}
	at := make([]int, len(names))
	values := make([]string, len(names))
	for {
		for i, list := range lists {
			values[i] = list[at[i]]
		}
		restore := a.pin(names, values)
		f()
		restore()
		i := len(names) - 1
		for ; i >= 0; i-- {
			if at[i]++; at[i] < len(lists[i]) {
				break
			}
			at[i] = 0
		}
		if i < 0 {
			return true
		}
	}
}
