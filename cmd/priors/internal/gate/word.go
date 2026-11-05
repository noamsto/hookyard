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

// render renders w in the host's environment (§4.1): literal text, quotes, a
// leading ~ or ~user, brace expansion where the shell does it, glob bytes
// kept as they are, and a plain $NAME of a host variable the command cannot
// set. host false renders literal text only, as a setter's name must be.
func (r *reader) render(w *syntax.Word, host bool) rendered {
	if host {
		if c, ok := r.cache[w]; ok {
			return c
		}
	}
	var out rendered
	alts := [][]syntax.WordPart{w.Parts}
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
		}
	}
	for _, parts := range alts {
		a := &alt{r: r, host: host}
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
	var out []arg
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

// alt renders one brace alternative.
type alt struct {
	r    *reader
	host bool
	b    strings.Builder
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
	for i, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			v := p.Value
			if i == 0 && !quoted && strings.HasPrefix(v, "~") {
				v = a.tilde(v, n)
			}
			a.write(unescape(v, quoted))
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

// tilde expands a leading ~ or ~user, returning the rest of v unexpanded. A
// tilde-prefix ends at the first slash; one with no slash in its own part is
// literal unless it is the whole word, as a quoted or expanded byte after it
// stops the expansion. ~ needs HOME unsettable, and ~+, ~- and ~N name dirs
// the command may change.
func (a *alt) tilde(v string, n int) string {
	user, rest, slash := strings.Cut(v[1:], "/")
	if !slash && n > 1 {
		return v
	}
	if slash {
		rest = "/" + rest
	}
	if !a.host {
		a.cut()
		return rest
	}
	switch {
	case user == "":
		if !a.r.settable("HOME") {
			a.write(a.r.host.Home)
			return rest
		}
	case strings.HasPrefix(user, "+"), strings.HasPrefix(user, "-"), digits(user):
	default:
		if a.r.host.Users != nil {
			if home, ok := a.r.host.Users(user); ok {
				a.write(home)
				return rest
			}
		}
		return v
	}
	a.cut()
	return rest
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
	if !identifier(name) || name == "_" || slices.Contains(shellOwn, name) || strings.HasPrefix(name, "BASH") || a.r.settable(name) {
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
