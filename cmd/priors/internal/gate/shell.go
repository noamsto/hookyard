package gate

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

var (
	shellTools = []string{"bash", "shell", "exec_command", "local_shell", "run_terminal_cmd"}
	// wrappers run their arguments as another command; they are skipped to find
	// the command word, so an inert command directly behind one stays clean.
	wrappers = []string{"env", "sudo", "doas", "command", "exec", "time", "nice", "nohup", "xargs", "timeout", "stdbuf", "builtin"}
	fetchers = []string{"curl", "wget", "xh", "http", "https", "aria2c", "lynx", "w3m", "links", "elinks"}
	// inert commands search or print their arguments and never run one as a command.
	inert      = []string{"grep", "egrep", "fgrep", "rg", "which", "whereis", "type"}
	forgesAny  = []string{"hub", "tea", "jira"}
	forgeLocal = []string{"auth", "config", "alias", "completion", "help", "version", "secret", "variable", "ssh-key", "gpg-key"}
	// forgeWrite verbs print only a URL or status the agent itself caused.
	forgeWrite = []string{"create", "close", "reopen", "edit", "update", "comment", "note", "merge", "approve", "ready", "lock", "unlock", "delete", "transfer", "pin", "unpin", "review", "subscribe", "unsubscribe"}
	schemes    = []string{"http://", "https://", "ftp://"}
	// shellSet are the variables bash always sets, so they hold a value even
	// in the unset environment.
	shellSet = []string{"PATH", "PWD", "OLDPWD", "SHELL", "BASH", "BASH_VERSION", "OSTYPE", "HOSTTYPE", "MACHTYPE", "UID", "EUID", "PPID", "HOSTNAME", "0"}
	// maxListed is the length of the longest listed name.
	maxListed = len(slices.MaxFunc(slices.Concat(fetchers, forgesAny), func(a, b string) int { return len(a) - len(b) }))

	// echo -e reads an octal escape as \0NNN where printf reads \NNN.
	echoOctal = regexp.MustCompile(`\\0([0-7]{1,3})`)
	// paramOps are the operators that may end a parameter expansion's
	// opening, longest first.
	paramOps = []string{":-", ":+", ":=", ":?", "-", "+", "=", "?", "##", "#", "%%", "%", "//", "/", "^^", "^", ",,", ",", "@"}
	// overflagQuotes are deleted by rule (b), as bash joins the words around them.
	overflagQuotes = strings.NewReplacer(`"`, "", "'", "", `\`, "")
)

const (
	maxDepth  = 8
	maxParsed = 1 << 20
	// maxText and maxNesting keep the parser's recursion far from the end of
	// the goroutine stack, whose overflow is fatal, not a panic. The deepest
	// unbracketed nesting maxText allows (`[[ ! ! …`, `if if …`) needs under
	// 64 MiB of stack.
	maxText    = 1 << 17
	maxNesting = 256
	// maxWalk is how many levels of a syntax tree one Walk descends at once.
	maxWalk = 512
	// parseCost is what each parsed byte charges the budget, and parseBase
	// what each parse does: the parser allocates up to about 300 bytes per
	// byte of its costliest text (an unclosed `if if …` chain) and about
	// 6 KiB on its own, so the budget holds a command's parsing to about
	// 75 MiB.
	parseCost = 4
	parseBase = 1 << 10
	// seqDigits is the most bytes one element of a brace sequence renders to,
	// as -9223372036854775808 does.
	seqDigits = 20
	// scriptBytes in an expanded word mean it may still be a script of its own.
	scriptBytes = " \t\n\r\v\f\"'\\$`;&|()<>{}"
)

// IngestsCall reports whether a tool call looks like it pulled issue, PR,
// comment or other fetched content into the session. hookyard's event record
// carries no tool input, so shell commands are judged by their text here. A
// shell call whose command is missing or unreadable counts as ingestion: gate
// 2 fails closed on what it cannot see.
func IngestsCall(toolName string, toolInput json.RawMessage) bool {
	isShell := slices.Contains(shellTools, strings.ToLower(toolName))
	var in struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(toolInput, &in) != nil || in.Command == nil || string(in.Command) == "null" {
		return isShell
	}
	var s string
	if json.Unmarshal(in.Command, &s) == nil {
		return ingestReason(s) != ""
	}
	var elems []any
	if json.Unmarshal(in.Command, &elems) != nil {
		return true
	}
	parts := make([]string, len(elems))
	for i, e := range elems {
		p, ok := e.(string)
		if !ok {
			return true
		}
		parts[i] = p
	}
	// Codex puts the script in one element (["bash", "-lc", "<script>"]),
	// whose joined form starts with "bash" and would hide it. The joined form
	// and every element share one budget.
	return ingestReason(append([]string{strings.Join(parts, " ")}, parts...)...) != ""
}

// ingestReason names why text ingests, or returns "" when it does not. The
// text is parsed as bash, statement by statement, and every simple command's
// words are rendered as the shell would split them, without evaluating
// anything (see render), in three stand-in environments: every variable
// unset, every one set to a placeholder, and each the command assigns set to
// its first assigned value. The fields are judged by segmentIngests, again
// from each value glued to an option or assignment, and again as a script one
// level deeper when they still hold a shell metacharacter, as `eval`,
// `ssh host` and `echo … | sh` join their words into one. Every word is also
// judged as a script one level deeper, in full when it still holds a shell
// metacharacter, from after its first `=`, and with printf escapes decoded,
// since such a word may reach a shell (`sh -c`, `ssh -o ProxyCommand=`,
// `printf … | sh`). Any URL flags. What the parser cannot see falls to the
// over-flagging overflagReason: the whole text on a parse error, after the
// statements before it are judged, and the source of every arithmetic
// expression, which is never walked. A text past the depth, byte or nesting
// budget flags, so the gate fails closed. Several texts are judged as one
// command, sharing one budget.
func ingestReason(texts ...string) (reason string) {
	// render panics on a word part it does not know; a panic must flag, not
	// fail open.
	defer func() {
		if recover() != nil {
			reason = "fallback"
		}
	}()
	j := &judge{seen: map[string]bool{}, vars: map[string]string{}}
	for _, text := range texts {
		if reason = j.script(text, 0); reason != "" {
			return reason
		}
	}
	return ""
}

// judge holds the budget shared by every script judged for one command, and
// the first value the command assigns to each variable.
type judge struct {
	parsed int
	seen   map[string]bool
	vars   map[string]string
}

// env is a stand-in for the unknown environment a command runs in.
type env int

const (
	// unset leaves every variable unset but HOME, IFS, PS4, the special
	// parameters and the ones bash always sets.
	unset env = iota
	// placeholder sets every variable to a neutral word, so an argument taken
	// from one still holds its place (`gh "$@"`).
	placeholder
	// assigned sets each variable the command assigns to its first assigned
	// value, and the rest as unset does.
	assigned
)

var envs = []env{unset, placeholder, assigned}

// envsFor returns every stand-in environment when a word reads a variable,
// and only the first otherwise, as the words then render the same in each.
func envsFor(reads bool) []env {
	if reads {
		return envs
	}
	return envs[:1]
}

// readsVar reports that w holds a parameter expansion, unquoted or in double
// quotes.
func readsVar(w *syntax.Word) bool {
	return w != nil && slices.ContainsFunc(w.Parts, partReadsVar)
}

func partReadsVar(p syntax.WordPart) bool {
	switch p := p.(type) {
	case *syntax.ParamExp:
		return true
	case *syntax.DblQuoted:
		return slices.ContainsFunc(p.Parts, partReadsVar)
	}
	return false
}

func (j *judge) script(text string, depth int) string {
	if hasURL(text) {
		return "url"
	}
	if j.seen[text] {
		return ""
	}
	j.seen[text] = true
	j.parsed += parseBase + parseCost*len(text)
	if depth > maxDepth || j.parsed > maxParsed || len(text) > maxText || nesting(text) > maxNesting {
		return "budget"
	}
	var reason string
	// The parser yields each statement as it ends, so one after a syntax error
	// is still judged. A loop body that returns early would make the parser
	// yield its error again, so a found reason only skips the rest.
	for stmt, err := range syntax.NewParser(syntax.Variant(syntax.LangBash)).StmtsSeq(strings.NewReader(text)) {
		switch {
		case reason != "":
		case err != nil:
			reason = fallback(text)
		default:
			reason = j.stmt(stmt, text, depth)
		}
	}
	return reason
}

// stmt records the statement's assignments, then judges its commands and
// words.
func (j *judge) stmt(s *syntax.Stmt, text string, depth int) string {
	walkBounded(s, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Assign:
			switch {
			case n.Name == nil:
			case n.Array != nil:
				j.assign(n.Name.Value, elemValues(n.Array)...)
			default:
				j.assign(n.Name.Value, n.Value)
			}
		case *syntax.ParamExp:
			if n.Param != nil && n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				j.assign(n.Param.Value, n.Exp.Word)
			}
		case *syntax.WordIter:
			if len(n.Items) > 0 {
				j.assign(n.Name.Value, n.Items[0])
			}
		}
		return !arithmetic(n) && j.parsed <= maxParsed
	})
	var reason string
	walkBounded(s, func(n syntax.Node) bool {
		if reason != "" {
			return false
		}
		switch n := n.(type) {
		case *syntax.CallExpr:
			reason = j.call(n.Args, n.Assigns, text, depth)
		case *syntax.ArrayExpr:
			reason = j.call(elemValues(n), nil, text, depth)
		case *syntax.WordIter:
			reason = j.names(text, n.Items...)
		case *syntax.Redirect:
			if n.Op == syntax.WordHdoc {
				reason = j.names(text, n.Word)
			}
		case *syntax.Word:
			reason = j.word(n, text, depth)
		}
		if arithmetic(n) {
			reason = fallback(source(text, n))
			return false
		}
		return reason == ""
	})
	return reason
}

// arithmetic reports an arithmetic expression or command, which is never
// walked: its tree nests one level per operator, and evaluating it can loop
// or allocate without bound.
func arithmetic(n syntax.Node) bool {
	switch n.(type) {
	case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.LetClause, *syntax.BinaryArithm, *syntax.UnaryArithm, *syntax.ParenArithm:
		return true
	}
	return false
}

// walkBounded walks root as syntax.Walk does, but sets aside every node
// maxWalk levels below where a walk began and walks it later from its own
// root, so at most maxWalk Walk frames are on the stack whatever the tree's
// depth: a pipe or && chain, or a `[[ ! ! … ]]` test, nests one level per
// operator.
func walkBounded(root syntax.Node, f func(syntax.Node) bool) {
	pending := []syntax.Node{root}
	for len(pending) > 0 {
		top := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		level := 0
		syntax.Walk(top, func(n syntax.Node) bool {
			switch {
			case n == nil:
				level--
				return true
			case level == maxWalk:
				pending = append(pending, n)
				return false
			case !f(n):
				return false
			}
			level++
			return true
		})
	}
}

// assign records values, joined by spaces, as name's, unless name already has
// one. The values are rendered in the unset environment, so one assignment
// never feeds another.
func (j *judge) assign(name string, values ...*syntax.Word) {
	if _, ok := j.vars[name]; ok || len(values) == 0 || values[0] == nil {
		return
	}
	r := &render{spent: &j.parsed}
	rendered := make([]string, len(values))
	for i, v := range values {
		rendered[i] = r.operand(v, false)
	}
	j.vars[name] = strings.Join(rendered, " ")
}

// elemValues returns an array's element words.
func elemValues(a *syntax.ArrayExpr) []*syntax.Word {
	var words []*syntax.Word
	for _, e := range a.Elems {
		if e.Value != nil {
			words = append(words, e.Value)
		}
	}
	return words
}

// call judges a simple command's fields, or an array's, which `"${a[@]}"`
// runs as one. Its prefix assignments lead the glued judgement, as
// `GIT_PAGER=curl git log` runs the value.
func (j *judge) call(args []*syntax.Word, assigns []*syntax.Assign, text string, depth int) string {
	reads := slices.ContainsFunc(args, readsVar) || slices.ContainsFunc(assigns, func(a *syntax.Assign) bool { return readsVar(a.Value) })
	for _, e := range envsFor(reads) {
		fields, r := j.fields(e, text, args...)
		if r != "" {
			return r
		}
		if slices.ContainsFunc(fields, hasURL) {
			return "url"
		}
		if r := segmentIngests(fields); r != "" {
			return r
		}
		glued := fields
		if len(assigns) > 0 {
			var prefix []string
			for _, a := range assigns {
				if a.Value == nil {
					continue
				}
				fs, r := j.fields(e, text, a.Value)
				if r != "" {
					return r
				}
				prefix = append(prefix, a.Name.Value+"="+strings.Join(fs, " "))
			}
			glued = append(prefix, fields...)
		}
		if r := gluedIngests(glued); r != "" {
			return r
		}
		if slices.ContainsFunc(fields, func(f string) bool { return strings.ContainsAny(f, scriptBytes) }) {
			if r := j.script(strings.Join(fields, " "), depth+1); r != "" {
				return r
			}
		}
	}
	return ""
}

// names judges every listed name among the fields of a loop's list or a
// here-string, as any of them may become a command word later
// (`for c in gh; do $c …`, `read c <<< gh; $c …`).
func (j *judge) names(text string, words ...*syntax.Word) string {
	for _, e := range envsFor(slices.ContainsFunc(words, readsVar)) {
		fields, r := j.fields(e, text, words...)
		if r != "" {
			return r
		}
		if r := namesIngest(fields, true); r != "" {
			return r
		}
	}
	return ""
}

func (j *judge) word(w *syntax.Word, text string, depth int) string {
	for _, e := range envsFor(readsVar(w)) {
		fields, r := j.fields(e, text, w)
		if r != "" {
			return r
		}
		value := strings.Join(fields, " ")
		if hasURL(value) {
			return "url"
		}
		var scripts []string
		if strings.ContainsAny(value, scriptBytes) {
			scripts = append(scripts, value)
		}
		if _, after, ok := strings.Cut(value, "="); ok {
			scripts = append(scripts, after)
		}
		for _, s := range scripts {
			for _, d := range decodings(s) {
				if r := j.script(d, depth+1); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// fields renders words as the arguments the shell would make of them in e,
// or as many as the budget allows, with the reason "budget". Brace expansion
// is the one step whose output can outgrow a word's source, so its whole cost
// is paid before any of it is built.
func (j *judge) fields(e env, text string, words ...*syntax.Word) ([]string, string) {
	r := &render{env: e, spent: &j.parsed}
	if e == assigned {
		r.vars = j.vars
	}
	for _, w := range words {
		alts := [][]syntax.WordPart{w.Parts}
		split := *w
		if syntax.SplitBraces(&split) {
			cost := max(len(source(text, w)), seqDigits)
			n := braceCount(split.Parts)
			if n > (maxParsed-j.parsed)/cost {
				return r.fields, "budget"
			}
			j.parsed += n * cost
			alts = braceAlts(split.Parts)
		}
		for _, parts := range alts {
			r.parts(parts, false)
			r.flush()
			if j.parsed > maxParsed {
				return r.fields, "budget"
			}
		}
	}
	return r.fields, ""
}

// braceCount counts the words brace expansion makes of parts, saturating at
// maxParsed.
func braceCount(parts []syntax.WordPart) int {
	n := 1
	for _, p := range parts {
		b, ok := p.(*syntax.BraceExp)
		if !ok {
			continue
		}
		elems := 0
		if b.Sequence {
			_, _, count, _ := sequence(b)
			elems = int(min(count, maxParsed))
		} else {
			for _, e := range b.Elems {
				elems = min(elems+braceCount(e.Parts), maxParsed)
			}
		}
		n = min(n*max(elems, 1), maxParsed)
	}
	return n
}

// sequence reads {x..y[..incr]} as its first element, the step to the next
// one and its exact element count, which saturates at math.MaxUint64 only for
// the 2^64 elements of {MinInt64..MaxInt64}. letters reports a range of
// letters, such as {a..z}.
func sequence(b *syntax.BraceExp) (from, step int64, count uint64, letters bool) {
	from, err := strconv.ParseInt(b.Elems[0].Lit(), 10, 64)
	to, _ := strconv.ParseInt(b.Elems[1].Lit(), 10, 64)
	if err != nil {
		from, to, letters = int64(b.Elems[0].Lit()[0]), int64(b.Elems[1].Lit()[0]), true
	}
	step = 1
	if len(b.Elems) > 2 {
		if s, _ := strconv.ParseInt(b.Elems[2].Lit(), 10, 64); s != 0 {
			step = s
		}
	}
	// bash steps toward the end whatever the increment's sign. Negating
	// math.MinInt64 leaves it negative, but adding it still moves 2^63
	// either way, as int64 addition wraps.
	if (to < from) != (step < 0) {
		step = -step
	}
	n := new(big.Int).Sub(big.NewInt(to), big.NewInt(from))
	n.Quo(n.Abs(n), new(big.Int).Abs(big.NewInt(step)))
	n.Add(n, big.NewInt(1))
	count = math.MaxUint64
	if n.IsUint64() {
		count = n.Uint64()
	}
	return from, step, count, letters
}

// braceAlts expands the brace expressions in parts, whose count braceCount
// has already paid for.
func braceAlts(parts []syntax.WordPart) [][]syntax.WordPart {
	alts := [][]syntax.WordPart{nil}
	for _, p := range parts {
		b, ok := p.(*syntax.BraceExp)
		if !ok {
			// Every alternative owns its slice, so appending in place is safe.
			for i := range alts {
				alts[i] = append(alts[i], p)
			}
			continue
		}
		var elems [][]syntax.WordPart
		if b.Sequence {
			v, step, count, letters := sequence(b)
			for range count {
				s := strconv.FormatInt(v, 10)
				if letters {
					s = fmt.Sprintf("%c", v)
				}
				elems = append(elems, []syntax.WordPart{&syntax.Lit{Value: s}})
				v += step
			}
		} else {
			for _, e := range b.Elems {
				elems = append(elems, braceAlts(e.Parts)...)
			}
		}
		next := make([][]syntax.WordPart, 0, len(alts)*len(elems))
		for _, a := range alts {
			for _, e := range elems {
				next = append(next, slices.Concat(a, e))
			}
		}
		alts = next
	}
	return alts
}

// render turns word parts into fields without evaluating them, so each part
// renders to at most a constant times its own source plus the values it
// reads. A parameter expansion becomes its variable's stand-in value, or the
// literal of its operand word where the shell would use that word (`${X:-w}`,
// `${X:+w}`, `${X/p/w}`); it never assigns, repeats or replaces. A command
// substitution renders to nothing and an arithmetic one to 1, as their insides
// are judged where they sit.
type render struct {
	env    env
	vars   map[string]string
	fields []string
	cur    strings.Builder
	// whole renders an operand, split into fields only where it is used.
	whole bool
	// spent is the judge's budget: every byte is charged to it before it is
	// written, and none is written once it is over.
	spent *int
}

func (r *render) write(s string) {
	if *r.spent += len(s); *r.spent <= maxParsed {
		r.cur.WriteString(s)
	}
}

func (r *render) flush() {
	if r.cur.Len() > 0 {
		r.fields = append(r.fields, r.cur.String())
		r.cur.Reset()
	}
}

// expansion writes an expansion's value, split into fields on IFS unless
// quoted.
func (r *render) expansion(s string, quoted bool) {
	if quoted || r.whole {
		r.write(s)
		return
	}
	if *r.spent += len(s); *r.spent > maxParsed {
		return
	}
	ifs, _ := r.value("IFS")
	for i := range len(s) {
		if strings.IndexByte(ifs, s[i]) >= 0 {
			r.flush()
			continue
		}
		r.cur.WriteByte(s[i])
	}
}

func (r *render) parts(parts []syntax.WordPart, quoted bool) {
	for i, p := range parts {
		switch p := p.(type) {
		case *syntax.Lit:
			v := p.Value
			if i == 0 && !quoted && strings.HasPrefix(v, "~") {
				_, rest, _ := strings.Cut(v, "/")
				v = "/home/" + rest
			}
			r.write(unescape(v, quoted))
		case *syntax.SglQuoted:
			v := p.Value
			if p.Dollar {
				// A nil config would share expand's one zero config, and its
				// buffer, with every other goroutine.
				v, _, _ = expand.Format(&expand.Config{}, v, nil)
				v, _, _ = strings.Cut(v, "\x00")
			}
			r.write(v)
		case *syntax.DblQuoted:
			r.parts(p.Parts, true)
		case *syntax.ParamExp:
			r.expansion(r.param(p, quoted), quoted)
		case *syntax.CmdSubst:
		case *syntax.ArithmExp:
			r.write("1")
		case *syntax.ProcSubst:
			r.write("/dev/fd/63")
		case *syntax.ExtGlob:
			r.write(p.Op.String() + p.Pattern.Value + ")")
		default:
			panic("unhandled word part")
		}
	}
}

// unescape removes the backslashes bash removes from a literal.
func unescape(v string, quoted bool) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		c := v[i+1]
		switch {
		case c == '\n':
			i++
		case !quoted, strings.IndexByte("$`\"\\", c) >= 0:
			b.WriteByte(c)
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// operand renders w as one string, as an operand or assignment value is
// expanded before it is split.
func (r *render) operand(w *syntax.Word, quoted bool) string {
	if w == nil {
		return ""
	}
	sub := &render{env: r.env, vars: r.vars, whole: true, spent: r.spent}
	sub.parts(w.Parts, quoted)
	return sub.cur.String()
}

func (r *render) param(p *syntax.ParamExp, quoted bool) string {
	if p.Length {
		return "1"
	}
	name := ""
	if p.Param != nil && !p.Excl && p.Names == 0 {
		name = p.Param.Value
	}
	val, set := r.value(name)
	_, own := r.vars[name]
	switch {
	case p.Exp != nil && p.Exp.Op >= syntax.UpperFirst:
		// Of the case and `@` operators, only lowering can spell a listed name.
		val = strings.ToLower(val)
	case r.env != placeholder && !own && (p.Slice != nil || p.Exp != nil && p.Exp.Op >= syntax.RemSmallSuffix):
		// A slice or a removal may leave nothing of a set variable. A
		// placeholder or an assigned value is kept whole, as the unset
		// environment already reads it as nothing.
		val = ""
	}
	if p.Repl != nil {
		if !set {
			return ""
		}
		return r.operand(p.Repl.With, quoted)
	}
	if p.Exp == nil || p.Exp.Op > syntax.AssignUnsetOrNull {
		return val
	}
	op := p.Exp.Op
	null := !set
	if op == syntax.AlternateUnsetOrNull || op == syntax.DefaultUnsetOrNull || op == syntax.ErrorUnsetOrNull || op == syntax.AssignUnsetOrNull {
		null = !set || val == ""
	}
	alternate := op == syntax.AlternateUnset || op == syntax.AlternateUnsetOrNull
	switch {
	case alternate && null:
		return ""
	case alternate, null:
		return r.operand(p.Exp.Word, quoted)
	}
	return val
}

// value is a variable's stand-in: its first assigned value in the assigned
// environment; bash's defaults for HOME, IFS and PS4; and a neutral word for
// the special parameters, the variables bash always sets, and in the
// placeholder environment every other one.
func (r *render) value(name string) (string, bool) {
	if v, ok := r.vars[name]; ok {
		return v, true
	}
	switch {
	case name == "HOME":
		return "/home", true
	case name == "IFS":
		return " \t\n", true
	case name == "PS4":
		return "+ ", true
	case r.env == placeholder, slices.Contains(shellSet, name), len(name) == 1 && strings.Contains("#?$!-", name):
		return "_", true
	}
	return "", false
}

// nesting returns the deepest bracket nesting in text, quoted or not, which
// bounds the parser's recursion; over-counting only flags.
func nesting(text string) int {
	depth, deepest := 0, 0
	for i := range len(text) {
		switch text[i] {
		case '(', '{', '[':
			depth++
			deepest = max(deepest, depth)
		case ')', '}', ']':
			depth = max(depth-1, 0)
		}
	}
	return deepest
}

// decodings returns s and, when it holds a backslash, s with printf and
// echo -e escapes decoded, since a script printed into a shell runs decoded.
// Format with no arguments decodes escapes only, in one pass.
func decodings(s string) []string {
	out := []string{s}
	if !strings.Contains(s, `\`) {
		return out
	}
	for _, format := range []string{s, echoOctal.ReplaceAllString(s, `\$1`)} {
		if d, _, err := expand.Format(&expand.Config{}, format, nil); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func source(text string, n syntax.Node) string {
	return text[n.Pos().Offset():n.End().Offset()]
}

func hasURL(text string) bool {
	lower := strings.ToLower(text)
	return slices.ContainsFunc(schemes, func(s string) bool { return strings.Contains(lower, s) })
}

func fallback(text string) string {
	if overflagReason(text) != "" {
		return "fallback"
	}
	return ""
}

// overflagReason is rule (b), for text the parser cannot see: with every quote
// and backslash deleted, every listed name among its tokens is judged, with no
// inert command word to clear it, and so is every listed value glued to one.
// A parameter expansion opening is read both as it is and as a word break, so
// `${X:-gh}` yields gh and `gh ${X:-issue}` keeps its group. A `$` is read
// both as a word break (`curl$IFS`) and as nothing (`g$""h`). The text is read
// as it is and with printf escapes decoded.
func overflagReason(text string) string {
	for _, d := range decodings(text) {
		for _, p := range []string{d, paramOpens(d)} {
			p = overflagQuotes.Replace(p)
			if hasURL(p) {
				return "url"
			}
			for _, reading := range []string{p, strings.ReplaceAll(p, "$", "")} {
				tokens := strings.FieldsFunc(reading, func(r rune) bool {
					return unicode.IsSpace(r) || strings.ContainsRune(";&|(){}<>=`$", r)
				})
				if r := namesIngest(tokens, false); r != "" {
					return r
				}
				if r := gluedIngests(tokens); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// paramOpens replaces every parameter expansion's opening, up to its operand,
// with a space. A subscript runs to the next `]`, which is looked up again
// only once the scan has passed it, so the scan stays linear.
func paramOpens(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	closing := -1
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], "${") {
			b.WriteByte(s[i])
			i++
			continue
		}
		k := i + 2
		for k < len(s) && (nameByte(s[k]) || strings.IndexByte("@*#?$!-", s[k]) >= 0) {
			k++
		}
		if k < len(s) && s[k] == '[' {
			if closing <= k {
				closing = len(s)
				if c := strings.IndexByte(s[k+1:], ']'); c >= 0 {
					closing = k + 1 + c
				}
			}
			if closing < len(s) {
				k = closing + 1
			}
		}
		for _, op := range paramOps {
			if strings.HasPrefix(s[k:], op) {
				k += len(op)
				break
			}
		}
		b.WriteByte(' ')
		i = k
	}
	return b.String()
}

// gluedIngests judges the command again from every value glued to an option or
// assignment word (`--split-string=gh`, `-Sgh`, `KEY=gh`), as the command word
// it may start, followed by the words after it. Only a listed value starts one,
// as `rg --pre=X` runs X on files, never on the later words. Of a short option's
// tails only the ones no longer than a listed name are tried: a longer tail
// with a listed name ends in a shorter one that is that name, and judging the
// name alone flags whatever the longer tail would. Every such command is judged
// at once, as one segment from the first listed value on, with each later one
// placed after its word: a name in it sees the words it would have seen, a
// listed value among them only makes a later group or verb flag, and no inert
// command word clears it.
func gluedIngests(words []string) string {
	var seg []string
	for _, w := range words {
		if seg != nil {
			seg = append(seg, w)
		}
		if v := gluedValue(w); v != "" {
			seg = append(seg, v)
		}
	}
	if seg == nil {
		return ""
	}
	start, _ := commandWord(seg)
	return namesIngest(seg[start:], start > 0)
}

// gluedValue returns w's longest glued value that is a listed name, or "".
func gluedValue(w string) string {
	switch {
	case strings.HasPrefix(w, "--"), assignment(w):
		if k := strings.IndexByte(w, '='); k >= 0 && k+1 < len(w) && listed(wordName(w[k+1:])) {
			return w[k+1:]
		}
	case strings.HasPrefix(w, "-"):
		for k := max(2, len(w)-maxListed); k < len(w); k++ {
			if listed(wordName(w[k:])) {
				return w[k:]
			}
		}
	}
	return ""
}

func listed(name string) bool {
	return slices.Contains(fetchers, name) || slices.Contains(forgesAny, name) || name == "gh" || name == "glab"
}

// segmentIngests judges one command's fields. Unless its command word is inert,
// every word from it on is a candidate, since any unlisted wrapper (setsid, ssh
// host, nix shell -c) may run a later word: every listed name is judged, so an
// earlier harmless one (`gh auth status`) cannot hide a later read. A command
// word found after a flag may be that flag's value (`sudo -u grep gh ...`), so
// it is not trusted to be inert.
func segmentIngests(words []string) string {
	start, afterFlag := commandWord(words)
	if start < len(words) && !afterFlag && slices.Contains(inert, wordName(words[start])) {
		return ""
	}
	return namesIngest(words[start:], start > 0)
}

// namesIngest judges every listed name in words. A gh/glab word with no group
// after it that is not the command's first word may take its arguments from
// stdin or a placeholder (`xargs gh`); notFirst says words[0] is not.
func namesIngest(words []string, notFirst bool) string {
	var next []int
	at := func(i int) string {
		if i < len(words) {
			return words[i]
		}
		return ""
	}
	for i, w := range words {
		switch name := wordName(w); {
		case slices.Contains(fetchers, name):
			return "fetcher"
		case slices.Contains(forgesAny, name):
			return "forge"
		case name == "gh" || name == "glab":
			if next == nil {
				next = nonFlags(words)
			}
			g := next[i+1]
			group, verb := at(g), at(next[g+1])
			if group == "" && (i > 0 || notFirst) {
				return "forge-bare"
			}
			if forgeIngests(group, verb) {
				return "forge-read"
			}
		}
	}
	return ""
}

// nonFlags returns, for each i up to len(words)+1, the index of the first word
// from i on that is not a flag, or len(words), so a run of flag-shaped names
// is not scanned once per name.
func nonFlags(words []string) []int {
	next := make([]int, len(words)+2)
	next[len(words)], next[len(words)+1] = len(words), len(words)
	for i := len(words) - 1; i >= 0; i-- {
		next[i] = next[i+1]
		if !strings.HasPrefix(words[i], "-") {
			next[i] = i
		}
	}
	return next
}

// commandWord returns the index of the command word, skipping assignments,
// wrappers, flags and numeric arguments; the index is len(words) when there is
// none. afterFlag reports that a flag was skipped, so the word found may be
// that flag's value.
func commandWord(words []string) (idx int, afterFlag bool) {
	for i, w := range words {
		switch {
		case slices.Contains(wrappers, wordName(w)):
		case strings.HasPrefix(w, "-"):
			afterFlag = true
		case assignment(w), number(w):
		default:
			return i, afterFlag
		}
	}
	return len(words), afterFlag
}

// assignment reports a NAME=value word. It and number are matched by hand,
// not by regexp, whose per-match state the race detector's sync.Pool drops,
// so a long command would allocate once per word.
func assignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for i := range len(name) {
		if !nameByte(name[i]) {
			return false
		}
	}
	return true
}

func nameByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// number reports a number, with an optional fraction and a duration suffix
// (`10`, `1.5`, `5s`).
func number(w string) bool {
	if n := len(w); n > 0 && strings.IndexByte("smhd", w[n-1]) >= 0 {
		w = w[:n-1]
	}
	whole, fraction, dotted := strings.Cut(w, ".")
	return digits(whole) && (!dotted || digits(fraction))
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// wordName is w's last path element, without the `!` of a git alias or the
// `=` of a zsh path lookup before it.
func wordName(w string) string {
	return strings.TrimLeft(w[strings.LastIndex(w, "/")+1:], "!=")
}

// forgeIngests judges a gh/glab command by the first two words after it that
// are not flags. A flag that takes a separate value before the group
// (`gh -R o/r pr create`) makes that value the group, which over-flags.
func forgeIngests(group, verb string) bool {
	switch {
	case group == "", slices.Contains(forgeLocal, group):
		return false
	case group == "issue", group == "pr", group == "mr":
		return !slices.Contains(forgeWrite, verb)
	}
	return true
}
