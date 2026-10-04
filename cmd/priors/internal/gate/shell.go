package gate

import (
	"encoding/json"
	"io"
	"math"
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

	assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	numberWord     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
	// echo -e reads an octal escape as \0NNN where printf reads \NNN.
	echoOctal = regexp.MustCompile(`\\0([0-7]{1,3})`)
	// overflagQuotes are deleted by rule (b), as bash joins the words around them.
	overflagQuotes = strings.NewReplacer(`"`, "", "'", "", `\`, "")
)

const (
	maxDepth  = 8
	maxParsed = 1 << 20
	// maxText and maxNesting keep the parser's recursion far from the end of
	// the goroutine stack, whose overflow is fatal, not a panic.
	maxText    = 1 << 18
	maxNesting = 256
	// braceLimit is where expand stops a brace expansion with an error.
	braceLimit = 16 << 10
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
	// whose joined form starts with "bash" and would hide it.
	return ingestReason(strings.Join(parts, " ")) != "" ||
		slices.ContainsFunc(parts, func(p string) bool { return ingestReason(p) != "" })
}

// ingestReason names why text ingests, or returns "" when it does not. The
// text is parsed as bash, statement by statement, and every simple command is
// expanded as the shell would, once with every variable unset and once with
// each set to a placeholder, then judged by segmentIngests, again from each
// value glued to an option or assignment, and again as a script one level
// deeper when its fields still hold a shell metacharacter, as `eval`,
// `ssh host` and `echo … | sh` join their words into one. Every word is also
// judged as a script one level deeper, in full when it still holds a shell
// metacharacter, from after its first `=`, and with printf escapes decoded,
// since such a word may reach a shell (`sh -c`, `ssh -o ProxyCommand=`,
// `printf … | sh`). Any URL flags. What the parser cannot see falls to the
// over-flagging overflagReason: the whole text on a parse error, after the
// statements before it are judged, and a word's source on its expansion
// error. A text past the depth, byte or nesting budget flags, so the gate
// fails closed.
func ingestReason(text string) (reason string) {
	// The expander can panic on word parts it does not handle; a panic must
	// flag, not fail open.
	defer func() {
		if recover() != nil {
			reason = "fallback"
		}
	}()
	j := &judge{
		seen: map[string]bool{},
		cfgs: [2]*expand.Config{newConfig(false), newConfig(true)},
	}
	return j.script(text, 0)
}

// judge holds the budget shared by every script judged for one command.
type judge struct {
	parsed int
	seen   map[string]bool
	cfgs   [2]*expand.Config
}

func (j *judge) script(text string, depth int) string {
	if hasURL(text) {
		return "url"
	}
	if j.seen[text] {
		return ""
	}
	j.seen[text] = true
	j.parsed += len(text)
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

func (j *judge) stmt(s *syntax.Stmt, text string, depth int) string {
	var reason string
	// Walk goes on to a node's siblings after the callback returns false, so
	// a found reason must stop every later node too.
	syntax.Walk(s, func(n syntax.Node) bool {
		if reason != "" {
			return false
		}
		switch n := n.(type) {
		case *syntax.CallExpr:
			reason = j.call(n, text, depth)
		case *syntax.Word:
			reason = j.word(n, text, depth)
		}
		return reason == ""
	})
	return reason
}

// call judges a simple command's fields. Its prefix assignments lead the glued
// judgement, as `GIT_PAGER=curl git log` runs the value.
func (j *judge) call(n *syntax.CallExpr, text string, depth int) string {
	for _, cfg := range j.cfgs {
		var assigns []string
		for _, a := range n.Assigns {
			if a.Value == nil {
				continue
			}
			value, err := expand.Literal(cfg, a.Value)
			if err != nil {
				continue
			}
			assigns = append(assigns, a.Name.Value+"="+value)
		}
		var fields []string
		for _, arg := range n.Args {
			fs, r := j.fields(cfg, arg, text)
			if r != "" {
				return r
			}
			fields = append(fields, fs...)
		}
		fields = slices.DeleteFunc(fields, func(f string) bool { return f == "" })
		if slices.ContainsFunc(fields, hasURL) {
			return "url"
		}
		if r := segmentIngests(fields); r != "" {
			return r
		}
		if r := gluedIngests(append(assigns, fields...)); r != "" {
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

func (j *judge) word(w *syntax.Word, text string, depth int) string {
	for _, cfg := range j.cfgs {
		fields, r := j.fields(cfg, w, text)
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

// fields expands w as an argument and charges its fields to the budget. A
// word whose brace expansions would overrun the budget is not expanded. On an
// expansion error, w's source read as rule (b) reads it, every quote and
// backslash deleted, stands in for its fields.
func (j *judge) fields(cfg *expand.Config, w *syntax.Word, text string) ([]string, string) {
	src := source(text, w)
	if braceFields(w)*len(src) > maxParsed-j.parsed {
		return nil, "budget"
	}
	fields, err := expand.Fields(cfg, w)
	if err != nil {
		if r := fallback(src); r != "" {
			return nil, r
		}
		return []string{overflagQuotes.Replace(src)}, ""
	}
	for _, f := range fields {
		j.parsed += len(f)
	}
	if j.parsed > maxParsed {
		return nil, "budget"
	}
	return fields, ""
}

// braceFields counts the fields brace expansion makes of w, up to braceLimit.
func braceFields(w *syntax.Word) int {
	split := *w
	if !syntax.SplitBraces(&split) {
		return 1
	}
	return splitFields(split.Parts)
}

func splitFields(parts []syntax.WordPart) int {
	n := 1
	for _, p := range parts {
		b, ok := p.(*syntax.BraceExp)
		if !ok {
			continue
		}
		elems := 0
		if b.Sequence {
			elems = sequenceLen(b)
		} else {
			for _, e := range b.Elems {
				elems = min(elems+splitFields(e.Parts), braceLimit)
			}
		}
		n = min(n*elems, braceLimit)
	}
	return n
}

// sequenceLen counts the elements of {x..y[..incr]}, up to braceLimit. The
// count is taken in floating point, as the bounds may span all of int64.
func sequenceLen(b *syntax.BraceExp) int {
	from, err1 := strconv.ParseInt(b.Elems[0].Lit(), 10, 64)
	to, err2 := strconv.ParseInt(b.Elems[1].Lit(), 10, 64)
	if err1 != nil || err2 != nil {
		// A letter range, such as {a..z}.
		from, to = int64(b.Elems[0].Lit()[0]), int64(b.Elems[1].Lit()[0])
	}
	step := 1.0
	if len(b.Elems) > 2 {
		if s, _ := strconv.ParseInt(b.Elems[2].Lit(), 10, 64); s != 0 {
			step = math.Abs(float64(s))
		}
	}
	return int(min(math.Abs(float64(to)-float64(from))/step+1, braceLimit))
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
func decodings(s string) []string {
	out := []string{s}
	if !strings.Contains(s, `\`) {
		return out
	}
	// A nil config would share expand's one zero config, and its buffer,
	// with every other goroutine.
	for _, format := range []string{s, echoOctal.ReplaceAllString(s, `\$1`)} {
		if d, _, err := expand.Format(&expand.Config{}, format, nil); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// shellEnv stands in for the unknown environment: every variable but HOME and
// the ones bash always sets is unset, or, with placeholder, every one holds a
// neutral word, so an argument taken from a variable still holds its place
// (`gh "$@"`). IFS and PS4 hold bash's defaults. HOME and every user's home
// are set, so a tilde never looks up a real user. An assignment made while
// expanding (`$((i=1))`, `${x:=…}`) holds for the rest of the judge. A
// spelling built from a variable's real value (`${0/bas/g}`) is not seen.
type shellEnv struct {
	placeholder bool
	set         map[string]expand.Variable
}

func (e *shellEnv) Get(name string) expand.Variable {
	if vr, ok := e.set[name]; ok {
		return vr
	}
	value := "_"
	switch {
	case name == "HOME", strings.HasPrefix(name, "HOME "):
		value = "/home"
	case name == "IFS":
		value = " \t\n"
	case name == "PS4":
		value = "+ "
	case !e.placeholder && !slices.Contains(shellSet, name):
		return expand.Variable{}
	}
	return expand.Variable{Set: true, Kind: expand.String, Str: value}
}

func (e *shellEnv) Set(name string, vr expand.Variable) error {
	e.set[name] = vr
	return nil
}

func (*shellEnv) Each(func(string, expand.Variable) bool) {}

// newConfig expands a command substitution to nothing, since its commands are
// judged where they sit, and leaves globbing off.
func newConfig(placeholder bool) *expand.Config {
	return &expand.Config{
		Env:       &shellEnv{placeholder: placeholder, set: map[string]expand.Variable{}},
		CmdSubst:  func(io.Writer, *syntax.CmdSubst) error { return nil },
		ProcSubst: func(*syntax.ProcSubst) (string, error) { return "/dev/fd/63", nil },
	}
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
// The text is read as it is and with printf escapes decoded.
func overflagReason(text string) string {
	for _, d := range decodings(text) {
		d = overflagQuotes.Replace(d)
		if hasURL(d) {
			return "url"
		}
		tokens := strings.FieldsFunc(d, func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(";&|(){}<>=`", r)
		})
		if r := namesIngest(tokens, false); r != "" {
			return r
		}
		if r := gluedIngests(tokens); r != "" {
			return r
		}
	}
	return ""
}

// gluedIngests judges the command again from every value glued to an option or
// assignment word (`--split-string=gh`, `-Sgh`, `KEY=gh`), as the command word
// it may start, followed by the words after it. Only a listed value starts one,
// as `rg --pre=X` runs X on files, never on the later words. Of a short option's
// tails only the ones no longer than a listed name are tried: a longer tail
// with a listed name ends in a shorter one that is that name, and judging the
// name alone flags whatever the longer tail would.
func gluedIngests(words []string) string {
	for i, w := range words {
		var values []string
		switch {
		case strings.HasPrefix(w, "--"), assignmentWord.MatchString(w):
			if k := strings.IndexByte(w, '='); k >= 0 && k+1 < len(w) {
				values = append(values, w[k+1:])
			}
		case strings.HasPrefix(w, "-"):
			for k := max(2, len(w)-maxListed); k < len(w); k++ {
				values = append(values, w[k:])
			}
		}
		for _, v := range values {
			if !listed(wordName(v)) {
				continue
			}
			if r := segmentIngests(append([]string{v}, words[i+1:]...)); r != "" {
				return r
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
	for i, w := range words {
		switch name := wordName(w); {
		case slices.Contains(fetchers, name):
			return "fetcher"
		case slices.Contains(forgesAny, name):
			return "forge"
		case name == "gh" || name == "glab":
			if group, _ := nextNonFlag(words[i+1:]); group == "" && (i > 0 || notFirst) {
				return "forge-bare"
			}
			if forgeIngests(words[i+1:]) {
				return "forge-read"
			}
		}
	}
	return ""
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
		case assignmentWord.MatchString(w), numberWord.MatchString(w):
		default:
			return i, afterFlag
		}
	}
	return len(words), afterFlag
}

// wordName is w's last path element, without the `!` of a git alias or the
// `=` of a zsh path lookup before it.
func wordName(w string) string {
	return strings.TrimLeft(w[strings.LastIndex(w, "/")+1:], "!=")
}

// forgeIngests judges the words after a gh/glab word. A flag that
// takes a separate value before the group (`gh -R o/r pr create`) makes that
// value the group, which over-flags.
func forgeIngests(words []string) bool {
	group, rest := nextNonFlag(words)
	switch {
	case group == "", slices.Contains(forgeLocal, group):
		return false
	case group == "issue", group == "pr", group == "mr":
		verb, _ := nextNonFlag(rest)
		return !slices.Contains(forgeWrite, verb)
	}
	return true
}

func nextNonFlag(words []string) (word string, rest []string) {
	for i, w := range words {
		if !strings.HasPrefix(w, "-") {
			return w, words[i+1:]
		}
	}
	return "", nil
}
