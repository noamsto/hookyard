package gate

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// rendered is a word as the shell would make it, without evaluating anything:
// each brace alternative either renders, as at most one field, or is
// unresolved, keeping the text before its first part that does not render.
type rendered struct {
	fields []string
	// opaque holds each unresolved alternative's literal prefix.
	opaque []string
	// chunks are the literal runs between the parts that do not render, of
	// every unresolved alternative, which may still name a path.
	chunks []string
}

// arg is one argument a command's words make: a field, or an unresolved
// alternative whose text is its literal prefix.
type arg struct {
	text string
	ok   bool
	word *syntax.Word
	// src is the word as the command spells it, for a message.
	src string
}

// shellOwn are the variables bash sets or changes itself, whose host value is
// not the one a word reads.
var shellOwn = []string{
	"PWD", "OLDPWD", "RANDOM", "SRANDOM", "SECONDS", "LINENO", "EPOCHSECONDS", "EPOCHREALTIME",
	"FUNCNAME", "GROUPS", "HOSTNAME", "HOSTTYPE", "MACHTYPE", "OSTYPE", "PPID", "UID", "EUID",
	"SHLVL", "SHELLOPTS", "PIPESTATUS", "REPLY", "OPTARG", "OPTIND", "OPTERR", "DIRSTACK",
	"HISTCMD", "MAPFILE", "COPROC", "IFS", "PS1", "PS2", "PS3", "PS4", "COLUMNS", "LINES",
}

// bashAssigns reports a name bash assigns whatever the host holds.
func bashAssigns(name string) bool {
	return name == "_" || strings.HasPrefix(name, "BASH") || strings.HasPrefix(name, "COMP_") ||
		strings.HasPrefix(name, "READLINE_") || slices.Contains(shellOwn, name)
}

// bashDefaults are the names bash assigns only when the host leaves them
// unset; HOME is the host's Home.
var bashDefaults = []string{"PATH", "SHELL", "TERM", "MAILCHECK", "HISTFILE", "HISTSIZE", "HISTFILESIZE", "POSIXLY_CORRECT"}

// render renders w in the host's environment (§4.1): literal text, quotes, a
// ~ or ~user where bash expands one, brace expansion where the shell does it,
// glob bytes kept as they are, and a plain $NAME of a host variable the
// command cannot set. host false renders literal text only, as a setter's
// name must be.
func (r *reader) render(w *syntax.Word, host bool) rendered {
	if host {
		if c, ok := r.cache[w]; ok {
			return c
		}
	}
	var out rendered
	alts := [][]syntax.WordPart{w.Parts}
	mode := plainWord
	switch {
	case r.values[w]:
		mode = valueWord
	case assignLike(w.Parts):
		mode = assignWord
	}
	if r.braced[w] {
		split := *w
		if syntax.SplitBraces(&split) {
			cost := max(len(source(r.text, w)), seqDigits)
			n := braceCount(split.Parts)
			if n > (maxParsed-r.spent)/cost {
				r.fail("budget")
				return out
			}
			r.spent += n * cost
			alts = braceAlts(split.Parts)
			// bash drops the assignment reading of a word brace expansion
			// changes.
			mode = plainWord
		}
	}
	for _, parts := range alts {
		a := &alt{r: r, host: host, mode: mode}
		a.parts(parts, false, len(parts))
		a.end(&out)
		if r.over() {
			return out
		}
	}
	if host {
		r.cache[w] = out
	}
	return out
}

// args renders words into a command's arguments.
func (r *reader) args(words []*syntax.Word, host bool) []arg {
	out := []arg{}
	for _, w := range words {
		src := source(r.text, w)
		rw := r.render(w, host)
		for _, f := range rw.fields {
			out = append(out, arg{text: f, ok: true, word: w, src: src})
		}
		for _, p := range rw.opaque {
			out = append(out, arg{text: p, word: w, src: src})
		}
	}
	return out
}

// tildeMode is where bash starts a tilde-prefix in a word (§4.1).
type tildeMode int

const (
	// plainWord starts one only at the word's start.
	plainWord tildeMode = iota
	// assignWord, a word bash reads as an assignment (`of=~/x`), starts one
	// after its first = and after each unquoted :, and ends one at a :.
	assignWord
	// valueWord, an assignment's value, starts one at its start and after
	// each unquoted :, and ends one at a :.
	valueWord
)

// alt renders one brace alternative.
type alt struct {
	r    *reader
	host bool
	mode tildeMode
	// eq reports the word's first = passed.
	eq bool
	b  strings.Builder
	// open reports a part that does not render; prefix is the text before it.
	open   bool
	prefix string
	chunks []string
	// quoted reports a quote, which keeps an empty word as a field.
	quoted bool
}

func (a *alt) write(s string) {
	if a.r.spend(len(s)) {
		a.b.WriteString(s)
	}
}

// cut ends the literal run at a part that does not render.
func (a *alt) cut() {
	if !a.open {
		a.open, a.prefix = true, a.b.String()
	}
	if a.b.Len() > 0 {
		a.chunks = append(a.chunks, a.b.String())
	}
	a.b.Reset()
}

func (a *alt) end(out *rendered) {
	if a.open {
		if a.b.Len() > 0 {
			a.chunks = append(a.chunks, a.b.String())
		}
		out.opaque = append(out.opaque, a.prefix)
		out.chunks = append(out.chunks, a.chunks...)
		return
	}
	if s := a.b.String(); s != "" || a.quoted {
		out.fields = append(out.fields, s)
	}
}

// parts renders parts; n is how many parts the word has at the top level,
// as a tilde and a process substitution are read only in a word's own shape.
func (a *alt) parts(parts []syntax.WordPart, quoted bool, n int) {
	for i := 0; i < len(parts); i++ {
		switch p := parts[i].(type) {
		case *syntax.Lit:
			if quoted {
				a.write(unescape(p.Value, true))
				continue
			}
			v, end := litRun(parts, i)
			a.lit(v, i == 0, end == n-1)
			i = end
		case *syntax.SglQuoted:
			a.quoted = true
			a.write(ansi(p))
		case *syntax.DblQuoted:
			a.quoted = true
			a.parts(p.Parts, true, n)
		case *syntax.ParamExp:
			v, ok := a.param(p, quoted)
			if !ok {
				a.cut()
				continue
			}
			a.write(v)
		case *syntax.ProcSubst:
			// bash replaces a word that is only a process substitution with the
			// pipe's path, which no spelling can steer.
			if n == 1 && !quoted {
				a.write("/dev/fd/63")
				continue
			}
			a.cut()
		case *syntax.ExtGlob:
			a.write(p.Op.String() + p.Pattern.Value + ")")
		default:
			a.cut()
		}
	}
}

// ansi returns a single-quoted part's text, $'…' escapes decoded.
func ansi(p *syntax.SglQuoted) string {
	if !p.Dollar {
		return p.Value
	}
	// A nil config would share expand's one zero config, and its buffer,
	// with every other goroutine.
	v, _, _ := expand.Format(&expand.Config{}, p.Value, nil)
	v, _, _ = strings.Cut(v, "\x00")
	return v
}

// lit writes an unquoted literal run, first and last reporting that it starts
// and ends the word, expanding each tilde-prefix bash expands in it (§4.1): at
// the word's start, and in an assignment after its first = and after each
// unquoted :. A setter's literal text keeps a ~ after = or :.
func (a *alt) lit(v string, first, last bool) {
	open := first
	done := 0
	for k := 0; k < len(v); k++ {
		c := v[k]
		if c == '~' && open {
			if end, ok := a.prefixEnd(v, k, last); ok {
				a.write(unescape(v[done:k], false))
				if !a.tilde(v[k+1 : end]) {
					a.write(v[k:end])
				}
				done, k, open = end, end-1, false
				continue
			}
		}
		open = false
		switch {
		case c == '\\':
			k++
		case c == ':' && a.mode != plainWord:
			open = a.host
		case c == '=' && a.mode == assignWord && !a.eq:
			a.eq, open = true, a.host
		}
	}
	a.write(unescape(v[done:], false))
}

// litRun returns the text of the run of literal parts from parts[i], which
// the parser and brace expansion may split, and the index of its last part.
func litRun(parts []syntax.WordPart, i int) (string, int) {
	var b strings.Builder
	for ; i < len(parts); i++ {
		lit, ok := parts[i].(*syntax.Lit)
		if !ok {
			break
		}
		b.WriteString(lit.Value)
	}
	return b.String(), i - 1
}

// prefixEnd returns the end of the tilde-prefix at v[k]: the first slash, or
// colon in an assignment, after it. One that runs to the end of a part that is
// not the word's last, or holds a backslash, is literal, as a quoted or
// expanded byte stops the expansion.
func (a *alt) prefixEnd(v string, k int, last bool) (int, bool) {
	for j := k + 1; j < len(v); j++ {
		switch {
		case v[j] == '/', v[j] == ':' && a.mode != plainWord:
			return j, true
		case v[j] == '\\':
			return 0, false
		}
	}
	return len(v), last
}

// tilde writes the expansion of the tilde-prefix ~user, and reports false
// when bash leaves it literal. ~ needs HOME unsettable, and ~+, ~- and ~N name
// dirs the command may change.
func (a *alt) tilde(user string) bool {
	if a.host {
		switch {
		case user == "":
			if !a.r.settable("HOME") {
				a.write(a.r.host.Home)
				return true
			}
		case strings.HasPrefix(user, "+"), strings.HasPrefix(user, "-"), digits(user):
		default:
			if a.r.host.Users == nil {
				return false
			}
			home, ok := a.r.host.Users(user)
			if ok {
				a.write(home)
			}
			return ok
		}
	}
	a.cut()
	return true
}

// assignLike reports a word bash reads as an assignment: its leading unquoted
// literal is a name, maybe subscripted, then = or +=.
func assignLike(parts []syntax.WordPart) bool {
	v, _ := litRun(parts, 0)
	k := 0
	for k < len(v) && nameByte(v[k]) {
		k++
	}
	if !identifier(v[:k]) {
		return false
	}
	if k < len(v) && v[k] == '[' {
		end := strings.IndexByte(v[k:], ']')
		if end < 0 {
			return false
		}
		k += end + 1
	}
	return strings.HasPrefix(strings.TrimPrefix(v[k:], "+"), "=")
}

// param renders a plain $NAME or ${NAME} of a host variable the command
// cannot set; an unset one renders empty. An unquoted one renders only when
// its value holds no IFS whitespace or glob byte and IFS cannot be set.
func (a *alt) param(p *syntax.ParamExp, quoted bool) (string, bool) {
	if !a.host || p.Param == nil || p.Excl || p.Length || p.Width || p.Names != 0 || p.Index != nil ||
		p.Slice != nil || p.Repl != nil || p.Exp != nil || p.Flags != nil || p.NestedParam != nil || len(p.Modifiers) > 0 {
		return "", false
	}
	name := p.Param.Value
	if !identifier(name) || bashAssigns(name) || a.r.settable(name) {
		return "", false
	}
	v, set := a.r.host.Home, true
	if name != "HOME" {
		v, set = "", false
		if a.r.host.Env != nil {
			v, set = a.r.host.Env(name)
		}
	}
	if !set {
		if slices.Contains(bashDefaults, name) {
			return "", false
		}
		v = ""
	}
	if !quoted && (strings.ContainsAny(v, " \t\n*?[") || strings.Contains(v, "@(") || strings.Contains(v, "!(") ||
		strings.Contains(v, "+(") || a.r.settable("IFS")) {
		return "", false
	}
	return v, true
}

// identifier reports a shell variable name.
func identifier(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := range len(s) {
		if !nameByte(s[i]) {
			return false
		}
	}
	return true
}

// isLiteral reports a word that is the same text however the shell runs:
// only literal and quoted parts, with no tilde, glob or brace byte unquoted.
// A script the reader parses must be one.
func isLiteral(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if i == 0 && strings.HasPrefix(p.Value, "~") || strings.ContainsAny(p.Value, "*?[{") {
				return false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if _, ok := q.(*syntax.Lit); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// literalText returns a literal word's text.
func literalText(w *syntax.Word) string {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value, false))
		case *syntax.SglQuoted:
			b.WriteString(ansi(p))
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if l, ok := q.(*syntax.Lit); ok {
					b.WriteString(unescape(l.Value, true))
				}
			}
		}
	}
	return b.String()
}

// hasLiteralSlash reports a / in any literal part of w: a command word
// holding one names a file, never a builtin or function.
func hasLiteralSlash(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	var has func(parts []syntax.WordPart) bool
	has = func(parts []syntax.WordPart) bool {
		return slices.ContainsFunc(parts, func(part syntax.WordPart) bool {
			switch p := part.(type) {
			case *syntax.Lit:
				return strings.Contains(p.Value, "/")
			case *syntax.SglQuoted:
				return strings.Contains(p.Value, "/")
			case *syntax.DblQuoted:
				return has(p.Parts)
			}
			return false
		})
	}
	return has(w.Parts)
}

// docScript returns the text a heredoc or here-string feeds, when the text
// shows all of it.
func docScript(rd *syntax.Redirect) (string, bool) {
	if rd.Op == syntax.WordHdoc {
		if !isLiteral(rd.Word) {
			return "", false
		}
		return literalText(rd.Word) + "\n", true
	}
	if rd.Hdoc == nil {
		return "", true
	}
	quoted := slices.ContainsFunc(rd.Word.Parts, func(p syntax.WordPart) bool {
		switch p := p.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		case *syntax.Lit:
			return strings.Contains(p.Value, `\`)
		}
		return false
	})
	var b strings.Builder
	for _, part := range rd.Hdoc.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			return "", false
		}
		if quoted {
			b.WriteString(lit.Value)
		} else {
			b.WriteString(hdocUnescape(lit.Value))
		}
	}
	return b.String(), true
}

// hdocUnescape removes the backslashes an unquoted heredoc removes: before $,
// `, \ and a newline.
func hdocUnescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		switch c := v[i+1]; c {
		case '\n':
			i++
		case '$', '`', '\\':
			b.WriteByte(c)
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}
