package gate

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Host is the environment ShellAccess renders a command's paths in.
type Host struct {
	Home string
	// Env looks up a variable the command does not assign.
	Env func(string) (string, bool)
	// Users returns a user's home dir, for ~user.
	Users func(string) (string, bool)
}

// Access is what a shell command may write, and what it names.
type Access struct {
	// Writes reports some call not known to only read, or an output redirect
	// to a file.
	Writes bool
	// Targets are the resolved write targets, whole words.
	Targets []Target
	// Opaque are the unresolved target words, each with its literal prefix in
	// Text and its spelling in From.
	Opaque []Target
	// Dirs are the resolved dirs the command changes into.
	Dirs []string
	// DirLost reports a dir change the reader cannot resolve.
	DirLost bool
	// Unresolved names the first write the reader cannot resolve, "" if none.
	Unresolved string
	// Code names the first shell code the reader cannot see, "" if none.
	Code string
	// Names are every path a word of the command may name, deduplicated.
	Names []string
	// Unreadable names why the command could not be read in full: "budget"
	// or "fallback".
	Unreadable string
}

// Target is one write target: Text the rendered path, From the argument it
// was read out of, empty for a whole word.
type Target struct{ Text, From string }

// maxTouches bounds the targets and names one command may have.
const maxTouches = 1 << 14

var (
	// sinks are redirect targets that are not files.
	sinks = []string{"/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty"}
	// pathBreaks end a path-like token inside a word that is not itself a
	// path: code, a URL query, a key=value list.
	pathBreaks = " \t\n\r\v\f\"'`;&|()<>{}=,:$"
)

// ShellAccess reads the texts a shell call runs, as one command. Each text,
// and every literal script it runs (eval, a shell's -c, heredoc or
// here-string, trap, env -S), is parsed once. A pre-pass over every parsed
// statement collects what the command sets; then every simple command is
// classified on its own: read-only, a modeled writer with exact targets, or an
// unknown program that writes its operands. Code the reader cannot see, writes
// it cannot resolve, and every path any word names are reported for the guard.
func ShellAccess(host *Host, texts ...string) (acc Access) {
	defer func() {
		if recover() != nil {
			acc = Access{Unreadable: "fallback"}
		}
	}()
	if host == nil {
		host = &Host{}
	}
	r := &reader{
		host:   host,
		seen:   map[string]bool{},
		cache:  map[*syntax.Word]rendered{},
		braced: map[*syntax.Word]bool{},
		names:  map[string]bool{},
	}
	r.pre.init()
	for _, text := range texts {
		r.parse(text, 0)
	}
	if !r.over() {
		r.prepass()
	}
	if !r.over() {
		r.judge()
		// A target that may replace an allowlisted program withdraws the
		// allowlist for the whole command, which only the judge finds.
		if r.out.replaces && !r.withdrawn && !r.over() {
			r.withdrawn = true
			r.judge()
		}
	}
	if !r.over() {
		r.collectNames()
	}
	return r.result()
}

// reader reads one command's texts, sharing one budget.
type reader struct {
	host  *Host
	spent int
	// seen are the texts parsed, each once.
	seen    map[string]bool
	scripts []*script
	// text is the script whose statements are being walked.
	text string
	// cache holds each word's host rendering; braced are the words the shell
	// brace-expands.
	cache  map[*syntax.Word]rendered
	braced map[*syntax.Word]bool
	pre    prepass
	// withdrawn turns every call into an unknown program (§4.7).
	withdrawn bool
	// code is the first opaque-code reason found before the judge.
	code       string
	out        out
	names      map[string]bool
	nameList   []string
	unreadable string
}

// out is what the judge finds, reset when it runs again.
type out struct {
	writes     bool
	targets    []Target
	opaque     []Target
	seen       map[string]bool
	oseen      map[Target]bool
	dirs       []string
	dirLost    bool
	unresolved string
	// redefined names a call withdrawal made an unknown program.
	redefined string
	code      string
	// ran reports an unknown program, which writes its cwd.
	ran bool
	// replaces reports a target that may replace an allowlisted program.
	replaces bool
}

type script struct {
	text  string
	stmts []*syntax.Stmt
	depth int
}

func (r *reader) over() bool { return r.unreadable != "" }

func (r *reader) fail(why string) {
	if r.unreadable == "" {
		r.unreadable = why
	}
}

// spend charges n to the budget and reports whether it is still within it.
func (r *reader) spend(n int) bool {
	if r.spent += n; r.spent > maxParsed {
		r.fail("budget")
		return false
	}
	return true
}

// charge charges the budget for parsing text at depth, and reports whether
// the budget and the parser's limits leave it readable.
func (r *reader) charge(text string, depth int) bool {
	r.spent += parseBase + parseCost*len(text)
	if depth > maxDepth || r.spent > maxParsed || len(text) > maxText || nesting(text) > maxNesting {
		r.fail("budget")
		return false
	}
	return true
}

// codeFirst records opaque code found before the judge.
func (r *reader) codeFirst(why string) {
	if r.code == "" {
		r.code = why
	}
}

// parse parses text and every literal script it runs, breadth first.
func (r *reader) parse(text string, depth int) {
	type pending struct {
		text  string
		depth int
	}
	queue := []pending{{text, depth}}
	for len(queue) > 0 && !r.over() {
		p := queue[0]
		queue = queue[1:]
		if r.seen[p.text] {
			continue
		}
		r.seen[p.text] = true
		if !r.charge(p.text, p.depth) {
			return
		}
		s := &script{text: p.text, depth: p.depth}
		// The parser yields each statement as it ends, so those before a
		// syntax error are still read.
		for stmt, err := range syntax.NewParser(syntax.Variant(syntax.LangBash)).StmtsSeq(strings.NewReader(p.text)) {
			if err != nil {
				r.codeFirst("text the guard cannot parse")
				break
			}
			s.stmts = append(s.stmts, stmt)
		}
		r.scripts = append(r.scripts, s)
		for _, t := range r.discover(s) {
			queue = append(queue, pending{t, p.depth + 1})
		}
	}
}

// discover marks the words the shell brace-expands and returns the literal
// scripts s runs.
func (r *reader) discover(s *script) []string {
	var nested []string
	r.walk(s, func(n syntax.Node, f feed) {
		switch n := n.(type) {
		case *syntax.CallExpr:
			for _, w := range n.Args {
				r.braced[w] = true
			}
			nested = append(nested, scriptsOf(unwrap(r.args(n.Args, false)), f)...)
		case *syntax.WordIter:
			for _, w := range n.Items {
				r.braced[w] = true
			}
		case *syntax.ArrayElem:
			if n.Value != nil {
				r.braced[n.Value] = true
			}
		case *syntax.DeclClause:
			for _, a := range n.Args {
				if a.Naked && a.Value != nil {
					r.braced[a.Value] = true
				}
			}
		case *syntax.Redirect:
			if n.Word != nil && n.Op != syntax.Hdoc && n.Op != syntax.DashHdoc && n.Op != syntax.WordHdoc {
				r.braced[n.Word] = true
			}
		}
	})
	return nested
}

// feedKind is where a command's stdin comes from.
type feedKind int

const (
	noFeed feedKind = iota
	pipeFeed
	fileFeed
	docFeed
)

type feed struct {
	kind feedKind
	// doc is the heredoc or here-string of a docFeed.
	doc *syntax.Redirect
}

// walk visits every node of s's statements with the stdin feed of the
// statement it sits in, charging each node to the budget. Like walkBounded it
// sets aside a node maxWalk levels down and walks it later from its own root,
// carrying its feed.
func (r *reader) walk(s *script, visit func(syntax.Node, feed)) {
	r.text = s.text
	type pending struct {
		n syntax.Node
		f feed
	}
	var stack []pending
	for _, st := range slices.Backward(s.stmts) {
		stack = append(stack, pending{st, feed{}})
	}
	piped := map[*syntax.Stmt]bool{}
	for len(stack) > 0 && !r.over() {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		feeds := []feed{top.f}
		syntax.Walk(top.n, func(n syntax.Node) bool {
			if n == nil {
				feeds = feeds[:len(feeds)-1]
				return true
			}
			cur := feeds[len(feeds)-1]
			switch {
			case len(feeds) > maxWalk:
				stack = append(stack, pending{n, cur})
				return false
			case !r.spend(1):
				return false
			}
			switch n := n.(type) {
			case *syntax.Stmt:
				if f, ok := stdinOf(n); ok {
					cur = f
				} else if piped[n] {
					cur = feed{kind: pipeFeed}
				}
			case *syntax.BinaryCmd:
				if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
					piped[n.Y] = true
				}
			case *syntax.CoprocClause:
				cur = feed{kind: pipeFeed}
			}
			visit(n, cur)
			feeds = append(feeds, cur)
			return !r.over()
		})
	}
}

// stdinOf returns the feed a statement's own redirects give its stdin.
func stdinOf(st *syntax.Stmt) (feed, bool) {
	var f feed
	found := false
	for _, rd := range st.Redirs {
		if rd.N != nil && rd.N.Value != "0" {
			continue
		}
		switch rd.Op {
		case syntax.RdrIn, syntax.RdrInOut:
			f, found = feed{kind: fileFeed}, true
			if rd.Word != nil && literal(rd.Word) == "/dev/null" {
				f = feed{}
			}
		case syntax.DplIn:
			f, found = feed{kind: fileFeed}, true
			if rd.Word != nil && literal(rd.Word) == "-" {
				f = feed{}
			}
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			f, found = feed{kind: docFeed, doc: rd}, true
		default:
		}
	}
	return f, found
}

// judge classifies every simple command and redirect of every parsed text.
func (r *reader) judge() {
	r.out = out{seen: map[string]bool{}, oseen: map[Target]bool{}}
	for _, s := range r.scripts {
		r.walk(s, func(n syntax.Node, f feed) {
			switch n := n.(type) {
			case *syntax.Stmt:
				for _, rd := range n.Redirs {
					r.redirect(rd)
				}
			case *syntax.CallExpr:
				r.call(n, f)
			}
		})
		if r.over() {
			return
		}
	}
}

// redirect records an output redirect to anything but a sink as a target.
func (r *reader) redirect(rd *syntax.Redirect) {
	if !outputOp(rd.Op) || rd.Word == nil {
		return
	}
	rw := r.render(rd.Word, true)
	src := source(r.text, rd.Word)
	for _, f := range rw.fields {
		if rd.Op == syntax.DplOut && (f == "-" || digits(f)) || slices.Contains(sinks, f) || strings.HasPrefix(f, "/dev/fd/") {
			continue
		}
		r.out.writes = true
		r.target(f, "")
	}
	for _, p := range rw.opaque {
		r.out.writes = true
		r.opaque(arg{text: p, word: rd.Word, src: src})
	}
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

func (r *reader) unresolved(why string) {
	r.out.writes = true
	if r.out.unresolved == "" {
		r.out.unresolved = why
	}
}

func (r *reader) codeOut(why string) {
	if r.out.code == "" {
		r.out.code = why
	}
}

func (r *reader) dirLost() { r.out.dirLost = true }

// dir records a dir changed into, from a value that may not render.
func (r *reader) dir(a arg) {
	if !a.ok {
		r.dirLost()
		return
	}
	if !slices.Contains(r.out.dirs, a.text) {
		r.out.dirs = append(r.out.dirs, a.text)
	}
}

func (r *reader) touches() int { return len(r.out.targets) + len(r.out.opaque) + len(r.nameList) }

// target records a resolved write target.
func (r *reader) target(text, from string) {
	if text == "" || r.out.seen[text] {
		return
	}
	if r.touches() >= maxTouches {
		r.fail("budget")
		return
	}
	r.out.seen[text] = true
	r.out.targets = append(r.out.targets, Target{Text: text, From: from})
	if !r.out.replaces && replacesProgram(text) {
		r.out.replaces = true
	}
}

// opaque records an unresolved write target: its literal prefix, and the
// unresolved write it is.
func (r *reader) opaque(a arg) {
	src := a.src
	if !strings.HasPrefix(src, `"`) && !strings.HasPrefix(src, "'") {
		src = `"` + src + `"`
	}
	r.unresolved("writes through " + src)
	r.out.replaces = true
	t := Target{Text: a.text, From: a.src}
	if r.out.oseen[t] {
		return
	}
	if r.touches() >= maxTouches {
		r.fail("budget")
		return
	}
	r.out.oseen[t] = true
	r.out.opaque = append(r.out.opaque, t)
}

// collectNames records the reach names (§3.1) of every word of every parsed
// text.
func (r *reader) collectNames() {
	for _, s := range r.scripts {
		r.text = s.text
		for _, st := range s.stmts {
			walkBounded(st, func(n syntax.Node) bool {
				if !r.spend(1) {
					return false
				}
				if w, ok := n.(*syntax.Word); ok {
					rw := r.render(w, true)
					for _, f := range rw.fields {
						r.name(f, true)
					}
					for _, p := range rw.opaque {
						r.name(p, true)
					}
					for _, c := range rw.chunks {
						r.name(c, false)
					}
				}
				return !r.over()
			})
		}
		if r.over() {
			return
		}
	}
}

// name records s, when whole, and the paths inside it: the suffixes of a
// short-option cluster from its third byte, and its path tokens with $HOME
// and ~ expanded, as it is and printf-decoded. A token that is only / names
// nothing unless it is the whole word.
func (r *reader) name(s string, whole bool) {
	if s == "" {
		return
	}
	if whole {
		r.addName(s)
		if len(s) > 2 && s[0] == '-' && s[1] != '-' {
			for k := 2; k < len(s) && !r.over(); k++ {
				if r.spend(len(s) - k) {
					r.subName(s[k:])
				}
			}
		}
	}
	home := r.host.Home
	for _, d := range Decodings(s) {
		if !r.spend(len(d)) {
			return
		}
		for _, t := range PathTokens(strings.NewReplacer("${HOME}", home, "$HOME", home).Replace(d)) {
			if t == "~" || strings.HasPrefix(t, "~/") {
				t = home + t[1:]
			}
			r.subName(t)
		}
	}
}

// subName records a path token inside a word, unless it is only slashes
// (`//` in a jq or awk program).
func (r *reader) subName(t string) {
	if strings.Trim(t, "/") != "" {
		r.addName(t)
	}
}

func (r *reader) addName(t string) {
	if r.names[t] {
		return
	}
	if r.touches() >= maxTouches {
		r.fail("budget")
		return
	}
	r.names[t] = true
	r.nameList = append(r.nameList, t)
}

func (r *reader) result() Access {
	o := r.out
	acc := Access{
		Writes:     o.writes,
		Targets:    o.targets,
		Opaque:     o.opaque,
		Dirs:       o.dirs,
		DirLost:    o.dirLost,
		Unresolved: o.unresolved,
		Code:       r.code,
		Names:      r.nameList,
		Unreadable: r.unreadable,
	}
	if acc.Code == "" {
		acc.Code = o.code
	}
	if acc.Unresolved == "" {
		acc.Unresolved = o.redefined
	}
	if acc.Code != "" {
		acc.Writes = true
	}
	// An unknown program writes its cwd, which an unresolved dir change
	// leaves unknown.
	if o.dirLost && o.ran && acc.Unresolved == "" {
		acc.Unresolved = "runs a program in a dir the guard cannot tell"
	}
	return acc
}
