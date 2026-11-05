package gate

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var (
	// bashSpecial are the variables whose assignment changes what a name
	// runs, how a word splits, or where cd looks.
	bashSpecial = []string{
		"PATH", "HOME", "IFS", "CDPATH", "PS4", "ENV", "BASH_ENV", "BASH_CMDS", "BASH_ALIASES",
		"GLOBIGNORE", "EXECIGNORE", "BASH_XTRACEFD", "SHELLOPTS", "BASHOPTS", "POSIXLY_CORRECT",
		"BASH_COMPAT", "BASH_LOADABLES_PATH", "FUNCNEST",
	}
	// safeEnv are the variables a command may export without withdrawing
	// the allowlist: no allowlisted program reads them as a path or program.
	safeEnv = []string{"LANG", "LANGUAGE", "TZ", "NO_COLOR", "CLICOLOR", "FORCE_COLOR", "TERM", "COLUMNS", "LINES"}
	// declarers are the declare family run as a call (`builtin declare`).
	declarers = []string{"declare", "typeset", "local", "export", "readonly"}
	// shellBuiltins run in the shell itself, where a prefix assignment may
	// outlast the call or reach code it runs.
	shellBuiltins = []string{
		":", ".", "[", "alias", "bg", "bind", "break", "builtin", "caller", "cd", "command", "compgen",
		"complete", "compopt", "continue", "declare", "dirs", "disown", "echo", "enable", "eval", "exec",
		"exit", "export", "false", "fc", "fg", "getopts", "hash", "help", "history", "jobs", "kill", "let",
		"local", "logout", "mapfile", "popd", "printf", "pushd", "pwd", "read", "readarray", "readonly",
		"return", "set", "shift", "shopt", "source", "suspend", "test", "times", "trap", "true", "type",
		"typeset", "ulimit", "umask", "unalias", "unset", "wait",
	}
)

// positional is the pseudo-name of the positional parameters.
const positional = "@"

// bound is a value a setter binds to name, word in the script text, spelled
// src; a nil word is one the reader cannot tell: read at run time or
// appended to another. An operand's word renders NAME=value and so names
// the variable itself: a declare-family operand, or env's or sudo's. A
// prefix one reaches only the call it is a prefix of.
type bound struct {
	name, text, src string
	word            *syntax.Word
	operand, prefix bool
}

// prepass is what the setter pre-pass (§4.6, §4.7, §5) finds over every
// parsed statement before any call is judged.
type prepass struct {
	// set are the names a setter sets by literal name, and assigned those of
	// them set other than by a prefix assignment to an external program.
	set, assigned map[string]bool
	// scoped are the external program calls a prefix assignment exports to,
	// each withdrawn on its own; scopedAssigns holds every prefix
	// assignment, true for those.
	scoped        map[*syntax.CallExpr]bool
	scopedAssigns map[*syntax.Assign]bool
	// computed reports a setter whose name is not literal, a nameref, or
	// eval, source or `.`, any of which may set any name.
	computed bool
	// runtime reports a value reaching a parameter that is not a literal
	// free of [, $ and backtick: the command is then not tame.
	runtime bool
	// untame are the names some non-inert value may reach.
	untame map[string]bool
	// loops are the for variables iterating the positional parameters.
	loops []string
	funcs map[string]bool
	// exported are the names exported to a command.
	exported  map[string]bool
	allexport bool
	// bound are the values every setter binds.
	bound []bound
	// redefines reports a function, alias, hash, enable, source or `.`.
	redefines bool
	// refs are the names arithmetic reads; substituted reports a command
	// substitution an arithmetic context evaluates, indirect an indirection.
	refs                  map[string]bool
	substituted, indirect bool
	// sinks are the value-as-code sinks, opaque code unless tame.
	sinks []string
	// arithDone are the arithmetic nodes already read from an outer one.
	arithDone map[syntax.Node]bool
}

func (p *prepass) init() {
	p.set, p.untame, p.funcs, p.exported, p.refs = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	p.arithDone = map[syntax.Node]bool{}
	p.assigned, p.scoped, p.scopedAssigns = map[string]bool{}, map[*syntax.CallExpr]bool{}, map[*syntax.Assign]bool{}
}

// settable reports a name the command may set.
func (r *reader) settable(name string) bool { return r.pre.computed || r.pre.set[name] }

// prepass reads every parsed statement for what it sets, then decides
// tameness, the opaque code that hangs on it, and withdrawal.
func (r *reader) prepass() {
	p := &r.pre
	for _, s := range r.scripts {
		for _, st := range s.stmts {
			walkBounded(st, func(n syntax.Node) bool {
				if f, ok := n.(*syntax.FuncDecl); ok {
					p.funcs[f.Name.Value] = true
					p.redefines = true
				}
				return r.spend(1)
			})
		}
	}
	for _, s := range r.scripts {
		r.walk(s, r.preNode)
		if r.over() {
			return
		}
	}
	r.decide()
}

func (r *reader) preNode(n syntax.Node, f feed) {
	p := &r.pre
	switch n := n.(type) {
	case *syntax.Assign:
		if n.Name == nil {
			return
		}
		name := n.Name.Value
		scoped, prefix := p.scopedAssigns[n]
		if scoped {
			r.setScoped(name)
		} else {
			r.setName(name)
		}
		if n.Value != nil {
			r.value(name, n.Value)
			b := bound{name: name, src: source(r.text, n), word: n.Value, prefix: prefix}
			if n.Append {
				b.word = nil
			}
			r.bind(b)
		}
		if n.Array != nil {
			for _, e := range n.Array.Elems {
				if e.Value != nil {
					r.value(name, e.Value)
					r.bind(bound{name: name, src: source(r.text, e), word: e.Value, prefix: prefix})
				}
			}
		}
		r.arith(n.Index)
	case *syntax.ArrayElem:
		r.arith(n.Index)
	case *syntax.DeclClause:
		var words []arg
		for _, a := range n.Args {
			switch {
			case a.Naked && a.Name == nil && a.Value != nil:
				words = append(words, r.args([]*syntax.Word{a.Value}, false)...)
			case a.Name != nil:
				words = append(words, arg{text: a.Name.Value, ok: true})
			}
		}
		r.declare(n.Variant.Value, words)
	case *syntax.CallExpr:
		r.preCall(n, f)
	case *syntax.ForClause:
		if n.Select {
			r.setRuntime("REPLY")
		}
	case *syntax.WordIter:
		name := n.Name.Value
		r.setName(name)
		if !n.InPos.IsValid() {
			p.loops = append(p.loops, name)
		}
		for _, w := range n.Items {
			r.value(name, w)
			r.bind(bound{name: name, src: source(r.text, w), word: w})
			if globbed(w) {
				p.runtime, p.untame[name] = true, true
			}
		}
	case *syntax.CStyleLoop:
		r.arith(n.Init)
		r.arith(n.Cond)
		r.arith(n.Post)
	case *syntax.ArithmCmd:
		r.arith(n.X)
	case *syntax.ArithmExp:
		if !p.arithDone[n] {
			r.arith(n.X)
		}
	case *syntax.LetClause:
		for _, e := range n.Exprs {
			r.arith(e)
		}
	case *syntax.ParamExp:
		r.param(n)
	case *syntax.Redirect:
		if n.N != nil && !digits(n.N.Value) {
			r.setterName(strings.Trim(n.N.Value, "{}"))
		}
	case *syntax.BinaryTest:
		switch n.Op {
		case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
			r.arith(n.X)
			r.arith(n.Y)
		default:
		}
	case *syntax.UnaryTest:
		if w, ok := n.X.(*syntax.Word); ok && n.Op == syntax.TsVarSet {
			r.varTest(r.args([]*syntax.Word{w}, false))
		}
	case *syntax.CoprocClause:
		name := "COPROC"
		if n.Name != nil {
			name = literal(n.Name)
		}
		r.setterName(name)
	}
}

// param reads a parameter expansion: its subscript and offsets are
// arithmetic, a default assignment sets its name, and indirection and the @P
// transformation run a value as code.
func (r *reader) param(n *syntax.ParamExp) {
	p := &r.pre
	if !p.arithDone[n] {
		if n.Index != nil && !allIndex(n.Index) {
			r.arith(n.Index)
		}
		if n.Slice != nil {
			r.arith(n.Slice.Offset)
			r.arith(n.Slice.Length)
		}
	}
	if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
		switch {
		case n.Excl, n.Param == nil:
			p.computed = true
		default:
			r.setName(n.Param.Value)
			if n.Exp.Word != nil {
				r.value(n.Param.Value, n.Exp.Word)
				r.bind(bound{name: n.Param.Value, src: source(r.text, n), word: n.Exp.Word})
			}
		}
	}
	if n.Excl && n.Names == 0 && (n.Index == nil || !allIndex(n.Index)) {
		p.sinks = append(p.sinks, "indirection over a value set at run time")
	}
	if n.Exp != nil && n.Exp.Op == syntax.OtherParamOps && n.Exp.Word != nil && literal(n.Exp.Word) == "P" {
		p.sinks = append(p.sinks, "a prompt expansion of a value set at run time")
	}
}

// allIndex reports a [@] or [*] subscript, which lists every element.
func allIndex(x syntax.ArithmExpr) bool {
	w, ok := x.(*syntax.Word)
	l := ""
	if ok {
		l = literal(w)
	}
	return l == "@" || l == "*"
}

// globbed reports a word with an unquoted glob byte, which expands at run
// time.
func globbed(w *syntax.Word) bool {
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

func (r *reader) setName(name string) {
	r.pre.assigned[name] = true
	r.setScoped(name)
}

// external reports a call whose command, after its wrappers, is a program the
// shell execs: no builtin, function or shell, which runs in the shell or runs
// shell code a prefix assignment reaches.
func (r *reader) external(c call) bool {
	if c.lost || c.split != nil || c.listing || c.at >= len(c.argv) {
		return false
	}
	cmd := c.argv[c.at]
	if !cmd.ok || globWord(cmd.word) || strings.Contains(cmd.text, "=") {
		return false
	}
	name := cmd.text
	return !slices.Contains(shellBuiltins, name) && !r.pre.funcs[name] && !slices.Contains(scriptShells, wordName(name))
}

// setScoped records a name a prefix assignment to an external program sets.
func (r *reader) setScoped(name string) {
	r.pre.set[name] = true
	if name == "PS4" {
		r.pre.sinks = append(r.pre.sinks, "a PS4 prompt set by the command")
	}
}

// setRuntime records a setter that reads a value at run time into name.
func (r *reader) setRuntime(name string) {
	r.setName(name)
	r.bind(bound{name: name, src: "$" + name})
	r.pre.runtime, r.pre.untame[name] = true, true
}

// bind records a value bound in the text being walked.
func (r *reader) bind(b bound) {
	b.text = r.text
	r.pre.bound = append(r.pre.bound, b)
}

// value records a value word that reaches name.
func (r *reader) value(name string, w *syntax.Word) {
	lit, inert := valueKinds(w.Parts, false)
	r.pre.runtime = r.pre.runtime || !lit
	if !inert {
		r.pre.untame[name] = true
	}
}

// valueString records a value text that reaches name.
func (r *reader) valueString(name, v string) {
	r.pre.runtime = r.pre.runtime || strings.ContainsAny(v, "[$`")
	if !inertText(v) {
		r.pre.untame[name] = true
	}
}

// valueKinds reports parts that are a literal free of [, $ and backtick,
// and parts that are inert: literal text holding no letter, _, [, $ or
// backtick, and $((…)) expansions, which make a number.
func valueKinds(parts []syntax.WordPart, quoted bool) (lit, inert bool) {
	lit, inert = true, true
	check := func(s string) {
		if strings.ContainsAny(s, "[$`") {
			lit = false
		}
		if !inertText(s) {
			inert = false
		}
	}
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			check(unescape(p.Value, quoted))
		case *syntax.SglQuoted:
			check(ansi(p))
		case *syntax.DblQuoted:
			l, i := valueKinds(p.Parts, true)
			lit, inert = lit && l, inert && i
		case *syntax.ArithmExp:
			lit = false
		default:
			lit, inert = false, false
		}
	}
	return lit, inert
}

func inertText(s string) bool {
	for i := range len(s) {
		c := s[i]
		if c == '_' || c == '[' || c == '$' || c == '`' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
}

// setterName records a setter's name operand: its base name is set, and a
// subscript is arithmetic over a value set at run time, or code hidden in a
// literal when it holds $ or a backtick. A glob byte may name any variable.
func (r *reader) setterName(s string) {
	if r.subscript(s) {
		s = s[:strings.IndexByte(s, '[')]
	}
	switch {
	case strings.ContainsAny(s, "*?"):
		r.pre.computed = true
	case identifier(s):
		r.setName(s)
	}
}

// subscript applies a subscripted name's rules and reports one.
func (r *reader) subscript(s string) bool {
	k := strings.IndexByte(s, '[')
	if k < 0 {
		return false
	}
	if strings.ContainsAny(s[k:], "$`") {
		r.codeFirst("code hidden in a subscripted name")
	}
	r.pre.sinks = append(r.pre.sinks, "a subscripted name set at run time")
	return true
}

// runtimeSetter records a setter's name operands, each read at run time; one
// that does not render may name any variable.
func (r *reader) runtimeSetter(names ...arg) {
	for _, a := range names {
		if !a.ok {
			r.pre.computed = true
			continue
		}
		r.setterName(a.text)
		base, _, _ := strings.Cut(a.text, "[")
		if identifier(base) {
			r.setRuntime(base)
		}
	}
}

// varTest reads the operand of a -v test.
func (r *reader) varTest(ops []arg) {
	for _, a := range ops {
		if !a.ok {
			r.pre.sinks = append(r.pre.sinks, "a -v test of a name set at run time")
			continue
		}
		r.subscript(a.text)
	}
}

// preCall reads a simple command for what it sets, exports and feeds to
// positional parameters.
func (r *reader) preCall(ce *syntax.CallExpr, f feed) {
	p := &r.pre
	argv := r.args(ce.Args, false)
	c := unwrap(argv)
	// A prefix assignment to an external program exports to it alone, so
	// it withdraws the allowlist for that call only.
	scoped := r.external(c)
	export := func(name string) {
		switch {
		case !scoped:
			p.exported[name] = true
		case !safe(name):
			p.scoped[ce] = true
		}
	}
	if len(ce.Args) > 0 {
		for _, a := range ce.Assigns {
			if a.Name != nil {
				export(a.Name.Value)
				p.scopedAssigns[a] = scoped
			}
		}
	}
	for _, a := range c.assigns {
		name, v, _ := strings.Cut(a.text, "=")
		if !identifier(name) {
			continue
		}
		if scoped {
			r.setScoped(name)
		} else {
			r.setName(name)
		}
		export(name)
		if a.ok {
			r.valueString(name, v)
		} else {
			p.runtime, p.untame[name] = true, true
		}
		if a.word != nil {
			r.bind(bound{src: a.src, word: a.word, operand: true, prefix: true})
		}
	}
	if c.split != nil {
		r.positionals(argv[c.at:])
	}
	if c.lost || c.split != nil || c.at >= len(argv) || !argv[c.at].ok {
		return
	}
	name, args := argv[c.at].text, argv[c.at+1:]
	if slices.Contains(scriptShells, wordName(name)) {
		s := readShell(args)
		p.allexport = p.allexport || s.allexport
		if c.xargs || s.stdin && f.kind == pipeFeed {
			p.runtime, p.untame[positional] = true, true
		}
		r.positionals(s.extra)
		return
	}
	if p.funcs[name] {
		r.positionals(args)
	}
	if slices.Contains(declarers, name) {
		r.declare(name, args)
		return
	}
	switch name {
	case "eval":
		p.computed = true
	case "source", ".":
		p.computed, p.redefines = true, true
		if len(args) > 1 {
			r.positionals(args[1:])
		}
	case "alias", "hash", "enable":
		p.redefines = true
	case "read":
		r.readNames(args, "Nnptuadi", "REPLY", "a", true)
	case "mapfile", "readarray":
		r.readNames(args, "dnOsuCc", "MAPFILE", "", true)
	case "printf":
		r.printfName(args)
	case "getopts":
		if len(args) > 1 {
			r.runtimeSetter(args[1])
		}
		r.setRuntime("OPTARG")
		r.setRuntime("OPTIND")
	case "wait":
		r.readNames(args, "p", "", "p", false)
	case "unset":
		for _, a := range args {
			switch {
			case !a.ok:
				p.computed = true
			case !strings.HasPrefix(a.text, "-"):
				r.setterName(a.text)
			}
		}
	case "set":
		r.setCall(args)
	case "test", "[":
		for i := 0; i+1 < len(args); i++ {
			if args[i].ok && args[i].text == "-v" {
				r.varTest(args[i+1 : i+2])
			}
		}
	case "find", "fd":
		for _, inner := range execs(name, args) {
			if ic := unwrap(inner); !ic.lost && ic.at < len(inner) && slices.Contains(scriptShells, wordName(inner[ic.at].text)) {
				p.runtime, p.untame[positional] = true, true
			}
		}
	}
}

// positionals records values fed to the positional parameters.
func (r *reader) positionals(args []arg) {
	for _, a := range args {
		if !a.ok {
			r.pre.runtime, r.pre.untame[positional] = true, true
			continue
		}
		r.valueString(positional, a.text)
	}
}

// readNames records the names a reading setter sets: the value of nameFlag,
// its operands after options when operands are names (values lists the
// letters taking a value), and fallback when it names none. All are read at
// run time, and an option word that does not render may name any variable.
func (r *reader) readNames(args []arg, values, fallback, nameFlag string, operands bool) {
	named := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !a.ok && (a.text == "" && operands || strings.HasPrefix(a.text, "-")):
			r.pre.computed = true
			return
		case !operands && (!a.ok || !strings.HasPrefix(a.text, "-")):
		case a.ok && a.text == "--":
			if operands {
				r.runtimeSetter(args[i+1:]...)
				named = named || i+1 < len(args)
			}
			i = len(args)
		case a.ok && len(a.text) > 1 && a.text[0] == '-':
			for k := 1; k < len(a.text); k++ {
				if strings.IndexByte(values, a.text[k]) < 0 {
					continue
				}
				v := []arg{{text: a.text[k+1:], ok: true}}
				if k+1 == len(a.text) {
					v = nil
					if i+1 < len(args) {
						i++
						v = args[i : i+1]
					}
				}
				if nameFlag != "" && a.text[k] == nameFlag[0] {
					r.runtimeSetter(v...)
					named = true
				}
				break
			}
		default:
			r.runtimeSetter(a)
			named = true
		}
	}
	if !named && fallback != "" {
		r.setRuntime(fallback)
	}
}

// printfName records printf -v's name.
func (r *reader) printfName(args []arg) {
	for i, a := range args {
		switch {
		case !a.ok && (a.text == "" || a.text[0] == '-'):
			r.pre.computed = true
			return
		case !a.ok, !strings.HasPrefix(a.text, "-"), a.text == "--":
			return
		case a.text == "-v":
			if i+1 < len(args) {
				r.runtimeSetter(args[i+1])
			}
			return
		case strings.HasPrefix(a.text, "-v"):
			r.runtimeSetter(arg{text: a.text[2:], ok: true})
			return
		}
	}
}

// declare reads the declare family's words: flags, names and name=value
// operands. A word with no syntax word is a DeclClause's named assignment,
// whose value the Assign case reads.
func (r *reader) declare(variant string, words []arg) {
	p := &r.pre
	export, unexport := variant == "export", false
	if variant == "nameref" {
		p.computed = true
	}
	var names []string
	for _, a := range words {
		if !a.ok {
			p.computed = true
			if a.word != nil {
				r.bind(bound{src: a.src, word: a.word, operand: true})
			}
			continue
		}
		t := a.text
		if t != "" && (t[0] == '-' || t[0] == '+') {
			// export -n un-exports; the rest of the family declares a nameref.
			if t[0] == '-' && strings.Contains(t, "n") {
				if variant == "export" {
					unexport = true
				} else {
					p.computed = true
				}
			}
			if t[0] == '-' && strings.Contains(t, "i") {
				p.sinks = append(p.sinks, "an integer variable assigned at run time")
			}
			export = export || strings.Contains(t, "x")
			continue
		}
		name, v, assigned := strings.Cut(t, "=")
		name = strings.TrimSuffix(name, "+")
		r.setterName(name)
		base, _, _ := strings.Cut(name, "[")
		names = append(names, base)
		if assigned && a.word != nil {
			r.valueString(base, v)
			r.bind(bound{src: a.src, word: a.word, operand: true})
		}
	}
	if export && !unexport {
		for _, n := range names {
			p.exported[n] = true
		}
	}
}

// setCall reads set: -a and -o allexport export every later assignment, and
// its operands are positional parameters.
func (r *reader) setCall(args []arg) {
	p := &r.pre
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !a.ok && (a.text == "" || a.text[0] == '-' || a.text[0] == '+'):
			p.allexport = true
			r.positionals(args[i:])
			return
		case a.ok && a.text == "--":
			r.positionals(args[i+1:])
			return
		case a.ok && len(a.text) > 1 && (a.text[0] == '-' || a.text[0] == '+'):
			if a.text[0] == '-' && strings.Contains(a.text, "a") {
				p.allexport = true
			}
			if strings.Contains(a.text, "o") && i+1 < len(args) {
				i++
				if o := args[i]; !o.ok || o.text == "allexport" {
					p.allexport = true
				}
			}
		default:
			r.positionals(args[i:])
			return
		}
	}
}

// arith reads an arithmetic context: every name it reads, a substitution it
// evaluates, and what it assigns.
func (r *reader) arith(x syntax.Node) {
	if x == nil {
		return
	}
	p := &r.pre
	walkBounded(x, func(n syntax.Node) bool {
		if !r.spend(1) {
			return false
		}
		switch n := n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			p.substituted = true
			return false
		case *syntax.ParamExp:
			p.arithDone[n] = true
			switch {
			case n.Excl:
				p.indirect = true
			case n.Param != nil:
				p.refs[n.Param.Value] = true
			}
		case *syntax.ArithmExp:
			p.arithDone[n] = true
		case *syntax.Lit:
			r.arithText(n.Value)
		case *syntax.SglQuoted:
			r.arithText(n.Value)
		case *syntax.BinaryArithm:
			switch n.Op {
			case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn, syntax.QuoAssgn, syntax.RemAssgn,
				syntax.AndAssgn, syntax.OrAssgn, syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn:
				r.arithTarget(n.X)
			default:
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				r.arithTarget(n.X)
			}
		case *syntax.Word:
			r.arithWord(n)
		}
		return !r.over()
	})
}

// arithText records the names a text in arithmetic reads: runs of name
// bytes not starting with a digit, which start a number (0x1f, 16#ff).
func (r *reader) arithText(s string) {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			for i < len(s) && (nameByte(s[i]) || s[i] == '#' || s[i] == '@') {
				i++
			}
		case nameByte(c):
			start := i
			for i < len(s) && nameByte(s[i]) {
				i++
			}
			r.pre.refs[s[start:i]] = true
		default:
			i++
		}
	}
}

// arithTarget records what an arithmetic assignment sets: a bare name or a
// bare subscripted one; anything else may name any variable.
func (r *reader) arithTarget(x syntax.ArithmExpr) {
	if w, ok := x.(*syntax.Word); ok && len(w.Parts) == 1 {
		switch p := w.Parts[0].(type) {
		case *syntax.Lit:
			if identifier(p.Value) {
				r.setName(p.Value)
				return
			}
		case *syntax.ParamExp:
			if !p.Dollar.IsValid() && p.Param != nil {
				r.setName(p.Param.Value)
				return
			}
		}
	}
	r.pre.computed = true
}

// arithWord reads a quoted word arithmetic evaluates as an expression (`let
// "x=1"`): the names it spells may be assigned, and one that does not render
// may assign any.
func (r *reader) arithWord(w *syntax.Word) {
	quoted := slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		switch p.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		}
		return false
	})
	if !quoted {
		return
	}
	src := source(r.text, w)
	if !strings.ContainsAny(src, "=+-") {
		return
	}
	if !isLiteral(w) {
		r.pre.computed = true
		return
	}
	t := literalText(w)
	for i := 0; i < len(t); {
		if !nameByte(t[i]) || t[i] >= '0' && t[i] <= '9' {
			i++
			continue
		}
		start := i
		for i < len(t) && nameByte(t[i]) {
			i++
		}
		r.setName(t[start:i])
	}
}

// decide settles tameness, the opaque code that hangs on it and on taint,
// and withdrawal.
func (r *reader) decide() {
	p := &r.pre
	tame := !p.runtime && !p.computed
	for _, l := range p.loops {
		if p.untame[positional] {
			p.untame[l] = true
		}
	}
	untame := func(name string) bool {
		switch {
		case name == "0", name == "_":
			return true
		case name == "-":
			return !tame
		case name == "#", name == "?", name == "$", name == "!":
			return false
		case digits(name), name == "@", name == "*":
			return p.computed || p.untame[positional]
		}
		return p.computed || p.untame[name]
	}
	opaque := p.substituted || p.indirect && !tame
	for name := range p.refs {
		opaque = opaque || untame(name)
	}
	if opaque {
		r.codeFirst("arithmetic over a value set at run time")
	}
	if !tame && len(p.sinks) > 0 {
		r.codeFirst(p.sinks[0])
	}
	withdraw := p.redefines || p.computed || p.allexport
	for name := range p.exported {
		withdraw = withdraw || !safe(name)
	}
	for name := range p.assigned {
		withdraw = withdraw || slices.Contains(bashSpecial, name) || !safe(name) && r.hostHas(name)
	}
	r.withdrawn = withdraw
}

func safe(name string) bool {
	return slices.Contains(safeEnv, name) || strings.HasPrefix(name, "LC_")
}

// hostHas reports a name the host environment exports, which stays exported
// when the command assigns it.
func (r *reader) hostHas(name string) bool {
	if name == "HOME" {
		return true
	}
	if r.host.Env == nil {
		return false
	}
	_, ok := r.host.Env(name)
	return ok
}
