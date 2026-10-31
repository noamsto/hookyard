// Package guard is priors' pre_tool write guard (§4.4): a tripwire that denies
// an agent's tool call when it would write a store's attest entries, index or
// git dir, a confidence: reviewed claim, or priors' state or config. It reads
// the call and the paths on disk, loads neither the rule set nor a store, and
// sees only the calls hookyard sees. Where it cannot tell, it denies.
package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

const (
	attestDir = ".attest"
	// maxHops bounds the symlinks one path resolves through, as the kernel's
	// own limit does.
	maxHops = 40
	// maxDirs bounds the dirs a command's relative words resolve against.
	maxDirs = 32
	// maxRead bounds a store file read to simulate an edit.
	maxRead = 4 << 20
	// maxNodes and maxDepth bound the walk of a tool's JSON input.
	maxNodes = 1 << 14
	maxDepth = 16
	// maxEntries bounds the attest entries read to match a hard link.
	maxEntries = 4096
	// maxFiles bounds the store files walked to match a hard link.
	maxFiles = 1 << 16
	// maxPath bounds a string under an unknown key that is judged as a path.
	maxPath = 4096
)

// kind is what a guarded root is.
type kind int

const (
	// kindStore is a whole checkout: a shell may not write in it, a file tool
	// may write a fact without a reviewed claim.
	kindStore kind = iota
	kindAttest
	kindIndex
	kindGit
	kindState
	kindConfig
)

var (
	// shellKinds are the roots a shell command may not write in: a shell
	// write's content cannot be told, so a whole store is guarded.
	shellKinds = []kind{kindStore, kindState, kindConfig}
	// toolKinds are the roots a file tool may not write in.
	toolKinds = []kind{kindAttest, kindIndex, kindGit, kindState, kindConfig}
	// readTools never write a file.
	readTools = []string{
		"lsp",
		"read", "grep", "glob", "ls", "find", "view_image", "webfetch", "websearch", "web_fetch",
		"web_search", "todowrite", "todoread", "task", "agent", "notebookread", "exitplanmode",
		"enterplanmode", "bashoutput", "killshell", "toolsearch", "skill", "askuserquestion",
	}
	// pathKeys and contentKeys are a file tool's input keys, lowercased with
	// _ and - removed, across engines (Claude's file_path, Pi's path,
	// Cursor's target_file).
	pathKeys = []string{
		"filepath", "path", "paths", "notebookpath", "targetfile", "targetnotebook", "filename",
		"file", "files", "target", "destination", "dest", "source", "src", "newpath", "oldpath",
		"directory", "dir",
	}
	contentKeys = []string{"content", "contents", "codeedit", "newsource", "text", "newstring", "newtext"}
	keyNorm     = strings.NewReplacer("_", "", "-", "")
	quotes      = strings.NewReplacer(`"`, "", "'", "", `\`, "")
	// mcpReads are the patterns the last segment of an MCP tool's name matches
	// when it reads, and writeVerbs the words that make one a write all the
	// same.
	mcpReads   = []string{"search*", "list_*", "*get_file_contents", "*read_file", "*read_text_file"}
	writeVerbs = []string{
		"replace", "write", "edit", "create", "delete", "remove", "move", "rename", "update", "set", "put",
		"patch", "insert", "append", "upload", "push", "commit", "merge",
		"save", "add", "download", "export", "copy", "send", "run", "exec", "interact", "kill", "start", "stop", "install",
	}
	// fuzzy folds what pi's edit forgives when its old text is not found.
	fuzzy = strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'",
		"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`,
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-",
	)
)

// Store is one checkout the trust file pins.
type Store struct{ Name, Path string }

// Paths are what the guard protects.
type Paths struct {
	Stores []Store
	State  string
	// Config is the user config file, which may link elsewhere. Its dir is
	// guarded as well when it is priors' own.
	Config string
}

// ForConfig takes the paths from cfg, which was loaded from the config file at
// path.
func ForConfig(cfg config.Config, path string) Paths {
	p := Paths{State: cfg.State(), Config: path}
	if cfg.PersonalStore != "" {
		p.Stores = append(p.Stores, Store{"personal", cfg.PersonalStore})
	}
	if cfg.WorkStore != "" {
		p.Stores = append(p.Stores, Store{"work", cfg.WorkStore})
	}
	return p
}

type root struct {
	path, label string
	kind        kind
}

// Guard checks tool calls against the paths it was built with.
type Guard struct {
	roots []root
	// links are the guarded files a hard link elsewhere could reach.
	links []string
	// attests are the attest dirs whose entries a hard link could reach.
	attests []string
	// stores are the resolved store roots, whose files a hard link could reach.
	stores []string
	host   *gate.Host
	// tokens are the lowercased texts, and names the lowercased path
	// elements, whose mention in a command makes a write through a path it
	// builds at run time a write to a guarded root.
	tokens, names []string
	// fold compares paths without case, as the file system does.
	fold bool
	// broken is why the paths are unknown: the guard then denies every call
	// that may write.
	broken error
}

// New returns a guard over p, rendering shell paths in host.
func New(p Paths, host *gate.Host) *Guard {
	g := &Guard{host: host, fold: runtime.GOOS == "darwin", tokens: []string{attestDir, strings.ToLower(store.IndexFile)}}
	for _, s := range p.Stores {
		label := "the " + s.Name + " store"
		g.add(s.Path, kindStore, label)
		g.add(filepath.Join(s.Path, attestDir), kindAttest, label+"'s attest entries")
		g.add(filepath.Join(s.Path, store.IndexFile), kindIndex, label+"'s generated index")
		g.add(filepath.Join(s.Path, ".git"), kindGit, label+"'s git dir")
		g.links = append(g.links, filepath.Join(s.Path, store.IndexFile))
		g.attests = append(g.attests, filepath.Join(s.Path, attestDir))
		res := filepath.Clean(s.Path)
		if r, ok := physical(res); ok {
			res = r
		}
		g.stores = append(g.stores, res)
		g.names = append(g.names, strings.ToLower(filepath.Base(s.Path)))
	}
	if p.State != "" {
		g.add(p.State, kindState, "priors' state dir")
	}
	if p.Config != "" {
		if dir := filepath.Dir(p.Config); filepath.Base(dir) == "priors" {
			g.add(dir, kindConfig, "priors' config dir")
		}
		g.add(p.Config, kindConfig, "priors' config file")
		g.links = append(g.links, p.Config)
		if r, ok := physical(filepath.Clean(p.Config)); ok && r != filepath.Clean(p.Config) {
			g.links = append(g.links, r)
		}
	}
	return g
}

// Broken returns a guard for a host whose paths could not be loaded: it denies
// every call that may write, naming err.
func Broken(err error, host *gate.Host) *Guard {
	return &Guard{host: host, broken: err}
}

// add guards p as given and resolved, and its last two elements as a token.
func (g *Guard) add(p string, k kind, label string) {
	p = filepath.Clean(p)
	forms := []string{p}
	if r, ok := physical(p); ok && r != p {
		forms = append(forms, r)
	}
	for _, f := range forms {
		g.roots = append(g.roots, root{path: f, label: label, kind: k})
		if k != kindStore && k != kindState && k != kindConfig {
			continue
		}
		g.tokens = append(g.tokens, strings.ToLower(filepath.Join(filepath.Base(filepath.Dir(f)), filepath.Base(f))))
	}
}

func deny(format string, a ...any) string {
	return "priors write guard: " + fmt.Sprintf(format, a...)
}

func (g *Guard) brokenReason() string {
	return deny("cannot tell where priors' stores and dirs are, so every call that may write is denied: %v", g.broken)
}

// Check returns why the tool call must be denied, or "" to let it run. cwd is
// the absolute dir the call runs in.
func (g *Guard) Check(cwd, tool string, input json.RawMessage) string {
	if slices.Contains(readTools, strings.ToLower(tool)) {
		return ""
	}
	shell := gate.IsShellTool(tool)
	texts, missing, ok := gate.ShellTexts(input)
	switch {
	case !ok && shell:
		return deny("the %s command is neither a string nor a list of strings", tool)
	case ok && !missing:
		if r := g.shell(cwd, texts); r != "" {
			return r
		}
	}
	if shell || readsMCP(tool) {
		return ""
	}
	return g.tool(cwd, tool, input)
}

// readsMCP reports an MCP tool named for a read: its last name segment
// matches a read pattern and holds no write verb.
func readsMCP(tool string) bool {
	tool = strings.ToLower(tool)
	if !strings.HasPrefix(tool, "mcp__") {
		return false
	}
	seg := tool[strings.LastIndex(tool, "__")+2:]
	if slices.ContainsFunc(writeVerbs, func(v string) bool { return strings.Contains(seg, v) }) {
		return false
	}
	return slices.ContainsFunc(mcpReads, func(p string) bool { ok, _ := path.Match(p, seg); return ok })
}

// CallDir is the dir a call runs in: the envelope's, else the tool input's
// own (Cursor sends an empty envelope cwd), else the hook's.
func CallDir(envelopeCWD string, input json.RawMessage) string {
	dir := envelopeCWD
	if dir == "" {
		var in struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(input, &in) == nil {
			dir = in.CWD
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return ""
}

func (g *Guard) shell(cwd string, texts []string) string {
	acc := gate.ShellAccess(g.host, texts...)
	switch {
	case acc.Unreadable != "":
		return deny("the command is past what the guard reads (%s)", acc.Unreadable)
	case g.broken != nil && acc.Writes:
		return g.brokenReason()
	case g.broken != nil:
		return ""
	}
	c := g.newCheck(cwd, acc.Words)
	if acc.Writes {
		for _, d := range c.dirs {
			if r := c.relate(d, shellKinds); r.rel == inside {
				return deny("the command may write, and runs in %s, %s", d, describe(r.root, d))
			}
		}
	}
	involved := g.involves(texts)
	for _, t := range acc.Words {
		if t.Chdir {
			continue
		}
		if r := c.shellTouch(t, acc.Writes, involved); r != "" {
			return r
		}
	}
	return ""
}

// shellTouch judges one word of a command that writes when writes is set.
func (c *check) shellTouch(t gate.Touch, writes, involved bool) string {
	if t.Opaque {
		if r := c.prefix(t.Text); r.rel == inside && writes || r.rel == ancestor && t.Write {
			return deny("the command may write through %q, which reaches into %s", t.Text+"…", r.root.label)
		}
		if t.Write && involved {
			return deny("the command writes through a path it builds at run time, and names a guarded path")
		}
		return ""
	}
	relative := !filepath.IsAbs(t.Text)
	if t.Write && relative && c.lost && involved {
		return deny("the command writes %q from a dir it changes into at run time, and names a guarded path", t.Text)
	}
	cands := c.candidates(t.Text)
	for _, p := range cands {
		r := c.relate(p, shellKinds)
		switch {
		case r.rel == inside && t.Write:
			return deny("the command writes %s, %s", p, describe(r.root, p))
		case r.rel == inside && writes:
			return deny("the command names %s, %s, and may write", p, describe(r.root, p))
		case r.rel == ancestor && t.Write && !t.Presumed:
			return deny("the command may write %s, which holds %s", p, r.root.label)
		}
	}
	if !t.Write {
		return ""
	}
	for _, p := range cands {
		to, unknown := c.linked(p)
		switch {
		case unknown:
			return deny("the command writes %s, which has hard links the guard cannot match to a store file", p)
		case len(to) > 0:
			return deny("the command writes %s, a hard link to %s", p, to[0])
		}
	}
	return ""
}

// involves reports a text naming a guarded root or the guarded names, as it
// is, with printf escapes decoded, and with its quotes deleted. A store's
// own name counts only as a whole path element, so a store named `work`
// leaves `network` alone.
func (g *Guard) involves(texts []string) bool {
	for _, text := range texts {
		for _, dec := range gate.Decodings(text) {
			for _, d := range []string{dec, quotes.Replace(dec)} {
				d = strings.ToLower(d)
				if slices.ContainsFunc(g.tokens, func(t string) bool { return strings.Contains(d, t) }) {
					return true
				}
				if slices.ContainsFunc(strings.FieldsFunc(d, elementBreak), func(e string) bool { return slices.Contains(g.names, e) }) {
					return true
				}
			}
		}
	}
	return false
}

func elementBreak(r rune) bool {
	return strings.ContainsRune(" \t\n\r\v\f\"'`;&|()<>{}=,:$/\\", r)
}

func (g *Guard) tool(cwd, name string, input json.RawMessage) string {
	if t := bytes.TrimSpace(input); len(t) == 0 || string(t) == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return deny("the %s input cannot be read: %v", name, err)
	}
	in := &toolInput{}
	in.walk(v, 0)
	paths := in.judged()
	if len(paths) == 0 && len(in.patches) == 0 && !in.truncated {
		return ""
	}
	switch {
	case g.broken != nil:
		return g.brokenReason()
	case in.truncated:
		return deny("the %s input is past what the guard reads", name)
	case in.twice:
		return deny("the %s input names an edit text more than once, and the guard cannot tell which the tool uses", name)
	}
	c := g.newCheck(cwd, nil)
	for _, pf := range in.patches {
		for _, p := range pf.paths {
			if r := c.toolPath(name, p, nil); r != "" {
				return r
			}
			if r := c.patch(name, p, pf); r != "" {
				return r
			}
		}
	}
	for _, p := range paths {
		if r := c.toolPath(name, p, in); r != "" {
			return r
		}
	}
	return ""
}

// toolPath judges one path a file tool names, and the content in writes it
// to a store file, or a hard link to one. in is nil for a patch, judged by
// patch.
func (c *check) toolPath(name, p string, in *toolInput) string {
	cands := c.toolCandidates(p)
	for _, cand := range cands {
		r := c.relate(cand, toolKinds)
		switch r.rel {
		case inside:
			return deny("%s would write %s, %s", name, cand, describe(r.root, cand))
		case ancestor:
			return deny("%s would write %s, which holds %s", name, cand, r.root.label)
		case none:
		}
	}
	for _, cand := range cands {
		to, unknown := c.linked(cand)
		if unknown {
			return deny("%s would write %s, which has hard links the guard cannot match to a store file", name, cand)
		}
		for _, t := range to {
			if r := c.relate(t, toolKinds); r.rel == inside {
				return deny("%s would write %s, a hard link to %s, %s", name, cand, t, describe(r.root, t))
			}
		}
	}
	if in == nil {
		return ""
	}
	for _, cand := range cands {
		if !c.storeFile(cand) {
			continue
		}
		if r := c.reviewed(name, cand, in); r != "" {
			return r
		}
	}
	return ""
}

// toolCandidates are the paths a file tool's path p may be: as given, and as
// pi resolves it.
func (c *check) toolCandidates(p string) []string {
	cands := c.candidates(p)
	if n := c.g.piPath(p); n != p {
		cands = append(cands, c.candidates(n)...)
	}
	slices.Sort(cands)
	return slices.Compact(cands)
}

// piPath is p as pi's file tools resolve it: Unicode spaces as spaces, one
// leading @ dropped, ~ as the home, and a file:// URL as its path.
func (g *Guard) piPath(p string) string {
	p = strings.Map(func(r rune) rune {
		if r == '\u00a0' || r >= '\u2000' && r <= '\u200a' || r == '\u202f' || r == '\u205f' || r == '\u3000' {
			return ' '
		}
		return r
	}, p)
	p = strings.TrimPrefix(p, "@")
	if home := g.host.Home; home != "" {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = home + "/" + rest
		} else if p == "~" {
			p = home
		}
	}
	if len(p) < len("file://") || !strings.EqualFold(p[:len("file://")], "file://") {
		return p
	}
	host, rest, _ := strings.Cut(p[len("file://"):], "/")
	if host != "" && host != "localhost" {
		return p
	}
	rest, _, _ = strings.Cut(rest, "?")
	rest, _, _ = strings.Cut(rest, "#")
	if dec, err := url.PathUnescape(rest); err == nil {
		rest = dec
	}
	return "/" + rest
}

// describe names what in r a path is.
func describe(r root, p string) string {
	if r.kind == kindIndex || r.path == p {
		return r.label
	}
	return "inside " + r.label
}

// reviewed denies a write that leaves a store file claiming confidence:
// reviewed: the full content, or the file as each edit would leave it.
func (c *check) reviewed(name, p string, in *toolInput) string {
	const claim = "%s would leave %s claiming confidence: reviewed, which only attestation writes"
	if slices.ContainsFunc(in.contents, reviewedClaim) || slices.ContainsFunc(in.other, reviewedClaim) {
		return deny(claim, name, p)
	}
	if len(in.edits) == 0 {
		return ""
	}
	if slices.ContainsFunc(in.edits, func(e edit) bool { return e.pi && yamlRisk(e.new, true) }) {
		return deny("%s's new text may leave %s claiming confidence: reviewed, which only attestation writes", name, p)
	}
	base, err := readSmall(p)
	if err != nil {
		return deny("%s edits %s, which the guard cannot read to check the edit: %v", name, p, err)
	}
	first, all := base, base
	for _, e := range in.edits {
		if e.old == "" {
			first, all = e.new, e.new
			continue
		}
		if !e.all {
			first = strings.Replace(first, e.old, e.new, 1)
		}
		all = strings.ReplaceAll(all, e.old, e.new)
		if e.all {
			first = strings.ReplaceAll(first, e.old, e.new)
		}
	}
	// pi applies every edit against the original file, on a normalised copy
	// when an old text is not found as it is.
	if reviewedClaim(first) || reviewedClaim(all) ||
		reviewedClaim(splice(base, in.edits, func(s string) string { return s })) ||
		reviewedClaim(splice(fuzzyText(base), in.edits, fuzzyText)) {
		return deny(claim, name, p)
	}
	return ""
}

// splice is base with each edit applied at the first occurrence of its old
// text in base, as read through fold; an edit whose text is missing or
// overlaps an earlier one is skipped, and one without old text replaces the
// whole file.
func splice(base string, edits []edit, fold func(string) string) string {
	type span struct {
		from, to int
		new      string
	}
	var spans []span
	for _, e := range edits {
		if e.old == "" {
			return e.new
		}
		old := fold(e.old)
		i := strings.Index(base, old)
		if old == "" || i < 0 || slices.ContainsFunc(spans, func(s span) bool { return i < s.to && i+len(old) > s.from }) {
			continue
		}
		spans = append(spans, span{i, i + len(old), e.new})
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from - b.from })
	var b strings.Builder
	at := 0
	for _, s := range spans {
		b.WriteString(base[at:s.from])
		b.WriteString(s.new)
		at = s.to
	}
	b.WriteString(base[at:])
	return b.String()
}

// fuzzyText is s as pi's edit compares it when an old text is not found:
// NFKC, no trailing white space on a line, and quotes and dashes folded.
func fuzzyText(s string) string {
	lines := strings.Split(norm.NFKC.String(strings.ReplaceAll(s, "\r\n", "\n")), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
	}
	return fuzzy.Replace(strings.Join(lines, "\n"))
}

// reviewedClaim reports a text that claims review as priors reads a fact, or,
// when it is no fact, one that holds the word at all.
func reviewedClaim(text string) bool {
	if f, err := fact.Parse([]byte(text)); err == nil {
		return attest.Claims(f)
	}
	return strings.Contains(strings.ToLower(text), "reviewed")
}

// patch judges a patch's change to a store file, or a hard link to one. A new
// file's content is read as a fact. A change to an existing one is judged by
// its lines, since the guard does not apply hunks: an added line that holds the
// word, or an escape, tag or alias that could spell it, or a file that already
// holds any of them (a deleted line can join what is left), is denied.
func (c *check) patch(name, p string, pf patchFile) string {
	cands := c.toolCandidates(p)
	if !slices.ContainsFunc(cands, c.storeFile) {
		return ""
	}
	const claim = "%s may leave %s claiming confidence: reviewed, which only attestation writes"
	added := strings.Join(pf.added, "\n")
	if !pf.update {
		if reviewedClaim(added) {
			return deny(claim, name, p)
		}
		return ""
	}
	if yamlRisk(added, true) {
		return deny(claim, name, p)
	}
	for _, cand := range cands {
		base, err := readSmall(cand)
		if err != nil {
			return deny("%s patches %s, which the guard cannot read to check the patch: %v", name, cand, err)
		}
		if yamlRisk(base, false) {
			return deny(claim, name, p)
		}
	}
	return ""
}

// yamlRisk reports text holding the word reviewed, an escape or a tag, and
// with alias an alias as well.
func yamlRisk(text string, alias bool) bool {
	risky := `\!`
	if alias {
		risky += "*"
	}
	return strings.Contains(strings.ToLower(text), "reviewed") || strings.ContainsAny(text, risky)
}

// readSmall reads a file of at most maxRead bytes; a missing one is empty.
func readSmall(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // a store file the call names, read to judge the call
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var b bytes.Buffer
	n, err := b.ReadFrom(io.LimitReader(f, maxRead+1))
	if err != nil {
		return "", err
	}
	if n > maxRead {
		return "", fmt.Errorf("larger than %d bytes", maxRead)
	}
	return b.String(), nil
}

// check is one call's view of the guard: the dirs its relative paths resolve
// against.
type check struct {
	g    *Guard
	dirs []string
	// lost reports a dir the call changes into that cannot be told.
	lost bool
	// linkInfo are the files a hard link could reach, read once; cut reports
	// a store walk that ran out of budget or could not read a dir.
	linkInfo []linkTarget
	linkRead bool
	cut      bool
	// budget is the store files the walk may read.
	budget int
}

type linkTarget struct {
	info fs.FileInfo
	path string
}

// newCheck resolves the dirs a command's relative words may be relative to:
// cwd and every dir it changes into, each against every dir before it.
func (g *Guard) newCheck(cwd string, touches []gate.Touch) *check {
	c := &check{g: g, budget: maxFiles}
	if filepath.IsAbs(cwd) {
		c.dirs = c.candidates(cwd)
	} else {
		c.lost = true
	}
	for _, t := range touches {
		if !t.Chdir {
			continue
		}
		if t.Opaque || t.Text == "" {
			c.lost = true
			continue
		}
		for _, d := range c.candidates(t.Text) {
			if filepath.IsAbs(d) && !slices.Contains(c.dirs, d) {
				c.dirs = append(c.dirs, d)
			}
		}
		if len(c.dirs) > maxDirs {
			c.lost = true
			break
		}
	}
	return c
}

// candidates are the absolute paths p may be: against each dir, cleaned as
// the shell's logical cd does, resolved as the kernel does with `..` taken
// after the symlink before it, and resolved after a lexical clean, as Claude
// Code and pi pass a path.
func (c *check) candidates(p string) []string {
	if p == "" {
		return nil
	}
	var abs []string
	if filepath.IsAbs(p) {
		abs = []string{p}
	} else {
		for _, d := range c.dirs {
			abs = append(abs, d+string(filepath.Separator)+p)
		}
		if c.lost {
			// A path relative to a dir that cannot be told still names
			// .attest by its elements.
			abs = append(abs, p)
		}
	}
	var out []string
	for _, a := range abs {
		clean := filepath.Clean(a)
		out = append(out, clean)
		if glob(a) {
			continue
		}
		for _, f := range []string{a, clean} {
			if r, ok := physical(f); ok {
				out = append(out, r)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

type relation int

const (
	none relation = iota
	ancestor
	inside
)

type match struct {
	rel  relation
	root root
}

// relate reports whether p is inside a root of kinds, or holds one, or
// names an .attest dir by its elements; a glob matches what it may expand to.
func (c *check) relate(p string, kinds []kind) match {
	g := c.g
	elems := strings.Split(filepath.ToSlash(p), "/")
	for i, e := range elems {
		// A glob element matches a dot name only when it starts with a dot,
		// as bash globs do without dotglob.
		if c.same(e, attestDir) || glob(e) && strings.HasPrefix(e, ".") && c.matches(e, attestDir) {
			return match{inside, root{path: strings.Join(elems[:i+1], "/"), label: "an .attest dir", kind: kindAttest}}
		}
	}
	best := match{}
	for _, r := range g.roots {
		if !slices.Contains(kinds, r.kind) {
			continue
		}
		rel := c.relation(p, r.path)
		if rel > best.rel {
			best = match{rel, r}
		}
	}
	return best
}

// relation is p's relation to the root dir, with a glob read element by
// element.
func (c *check) relation(p, dir string) relation {
	if !glob(p) {
		switch {
		case c.within(p, dir):
			return inside
		case c.within(dir, p):
			return ancestor
		}
		return none
	}
	pe := strings.Split(filepath.ToSlash(p), "/")
	de := strings.Split(filepath.ToSlash(dir), "/")
	// The literal head resolves symlinks like any path.
	head := 0
	for head < len(pe) && !glob(pe[head]) {
		head++
	}
	if r, ok := physical(strings.Join(pe[:head], "/")); ok && head > 0 {
		pe = append(strings.Split(filepath.ToSlash(r), "/"), pe[head:]...)
	}
	for i := 0; i < len(pe) && i < len(de); i++ {
		if pe[i] == ".." {
			return inside
		}
		if !c.matches(pe[i], de[i]) {
			return none
		}
	}
	if len(pe) >= len(de) {
		return inside
	}
	return ancestor
}

// prefix judges what precedes an opaque word's first opaque part: the dir it
// ends in, and the element it starts, which the rest continues.
func (c *check) prefix(text string) match {
	if text == "" {
		return match{}
	}
	dir, partial := text, ""
	if !strings.HasSuffix(text, "/") {
		dir, partial = filepath.Dir(text), filepath.Base(text)
	}
	best := match{}
	for _, d := range c.candidates(dir) {
		if m := c.relate(d, shellKinds); m.rel == inside {
			return m
		}
		for _, r := range c.g.roots {
			if !slices.Contains(shellKinds, r.kind) || !c.within(r.path, d) || r.path == d {
				continue
			}
			rel, err := filepath.Rel(d, r.path)
			if err != nil {
				continue
			}
			next, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if c.hasPrefix(next, partial) {
				best = match{ancestor, r}
			}
		}
	}
	return best
}

// storeFile reports a path inside a store, or a hard link to one of its files.
func (c *check) storeFile(p string) bool {
	to, _ := c.linked(p)
	return c.relate(p, []kind{kindStore}).rel == inside || len(to) > 0
}

// linked returns the guarded files p is a hard link to. unknown is set when p
// has other links and the store walk ran out of budget before matching one.
func (c *check) linked(p string) (to []string, unknown bool) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Nlink < 2 {
		return nil, false
	}
	for _, t := range c.links() {
		if os.SameFile(fi, t.info) {
			to = append(to, t.path)
		}
	}
	return to, len(to) == 0 && c.cut
}

func (c *check) links() []linkTarget {
	if c.linkRead {
		return c.linkInfo
	}
	c.linkRead = true
	add := func(p string) {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			c.linkInfo = append(c.linkInfo, linkTarget{fi, p})
		}
	}
	for _, p := range c.g.links {
		add(p)
	}
	for _, dir := range c.g.attests {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries[:min(len(entries), maxEntries)] {
			add(filepath.Join(dir, e.Name()))
		}
	}
	for _, dir := range c.g.stores {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				c.cut = c.cut || !errors.Is(err, fs.ErrNotExist)
				return nil //nolint:nilerr // the walk goes on past an unreadable entry, and is cut
			case !d.Type().IsRegular():
				return nil
			case c.budget == 0:
				c.cut = true
				return filepath.SkipAll
			}
			c.budget--
			if fi, err := d.Info(); err == nil {
				c.linkInfo = append(c.linkInfo, linkTarget{fi, p})
			}
			return nil
		})
	}
	return c.linkInfo
}

func (c *check) within(p, dir string) bool {
	if c.g.fold {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}

func (c *check) same(a, b string) bool {
	if c.g.fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (c *check) hasPrefix(s, prefix string) bool {
	if c.g.fold {
		s, prefix = strings.ToLower(s), strings.ToLower(prefix)
	}
	return strings.HasPrefix(s, prefix)
}

// matches reports whether the glob element pattern may match name; a pattern
// it cannot read matches.
func (c *check) matches(pattern, name string) bool {
	if c.g.fold {
		pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	}
	ok, err := path.Match(convertBrackets(convertExtglob(pattern)), name)
	return ok || err != nil
}

// convertBrackets rewrites bash bracket classes into path.Match syntax. Bash
// reads a `[` with no close as a literal and a `]` first in a class (after an
// optional `!` or `^`) as a member, where path.Match errors on both and an
// error would count as a match. A class negated with `!` becomes `^`.
func convertBrackets(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			b.WriteByte('\\')
			if i+1 < len(p) {
				i++
				b.WriteByte(p[i])
			}
			continue
		case '[':
		default:
			b.WriteByte(p[i])
			continue
		}
		j := i + 1
		negated := j < len(p) && (p[j] == '!' || p[j] == '^')
		if negated {
			j++
		}
		leadingClose := j < len(p) && p[j] == ']'
		if leadingClose {
			j++
		}
		end := classEnd(p, j)
		if end < 0 {
			b.WriteString(`\[`)
			continue
		}
		b.WriteByte('[')
		if negated {
			b.WriteByte('^')
		}
		if leadingClose {
			b.WriteString(`\]`)
		}
		b.WriteString(p[j:end])
		b.WriteByte(']')
		i = end
	}
	return b.String()
}

// classEnd is the index of the `]` closing a bracket class whose members
// start at p[from], reading `[:…:]`, `[.….]` and `[=…=]` as units, or -1.
func classEnd(p string, from int) int {
	for k := from; k < len(p); k++ {
		switch p[k] {
		case '\\':
			k++
		case '[':
			if k+1 < len(p) && strings.IndexByte(":.=", p[k+1]) >= 0 {
				if n := strings.Index(p[k+2:], string(p[k+1])+"]"); n >= 0 {
					k += n + 3
				}
			}
		case ']':
			return k
		}
	}
	return -1
}

// convertExtglob reads an extglob group (`@(x)`, `!(x)`) as `*`.
func convertExtglob(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if i+1 < len(p) && p[i+1] == '(' && strings.IndexByte("@!?*+", p[i]) >= 0 {
			if end := strings.IndexByte(p[i:], ')'); end >= 0 {
				b.WriteByte('*')
				i += end
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

func glob(p string) bool {
	return strings.ContainsAny(p, "*?[") || strings.Contains(p, "@(") || strings.Contains(p, "!(") || strings.Contains(p, "+(")
}

// physical is p with every symlink resolved, a dangling one included, and
// `..` taken after the link it follows, as the kernel does. ok is false past
// maxHops.
func physical(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	pending := strings.Split(p, "/")
	cur, hops := "/", 0
	for len(pending) > 0 {
		e := pending[0]
		pending = pending[1:]
		switch e {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, e)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxHops {
			return "", false
		}
		target, err := os.Readlink(next)
		if err != nil {
			cur = next
			continue
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return cur, true
}

// toolInput is what a file tool's input names: paths, full contents, edits
// and patches.
type toolInput struct {
	paths    []string
	contents []string
	// other are the strings under keys the guard does not know: a path or
	// content there is as good as one under a known key.
	other   []string
	edits   []edit
	patches []patchFile
	nodes   int
	// twice reports an edit text a call names under two keys that normalise
	// alike.
	twice     bool
	truncated bool
}

type edit struct {
	old, new string
	all      bool
	// pi is an edit in pi's form, oldText and newText.
	pi bool
}

// judged are the strings judged as paths, as they are and trimmed: those
// under a path key, and the short ones of other. A call that names no path
// is judged by its content too.
func (in *toolInput) judged() []string {
	var out []string
	add := func(s string) {
		out = append(out, s)
		if t := strings.TrimSpace(s); t != s {
			out = append(out, t)
		}
	}
	for _, s := range in.paths {
		add(s)
	}
	short := in.other
	if len(in.paths) == 0 {
		short = slices.Concat(short, in.contents)
	}
	for _, s := range short {
		if len(s) <= maxPath {
			add(s)
		}
	}
	return out
}

// patchFile is one file a Codex-style patch changes.
type patchFile struct {
	// paths are the file and, for a move, where it goes.
	paths  []string
	added  []string
	update bool
}

func (in *toolInput) walk(v any, depth int) {
	if in.nodes++; in.nodes > maxNodes || depth > maxDepth {
		in.truncated = true
		return
	}
	switch v := v.(type) {
	case map[string]any:
		in.object(v, depth)
	case []any:
		for _, e := range v {
			in.walk(e, depth+1)
		}
	case string:
		if strings.Contains(v, "*** Begin Patch") {
			in.patches = append(in.patches, parsePatch(v)...)
		}
	}
}

func (in *toolInput) object(m map[string]any, depth int) {
	byKey := make(map[string][]any, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		nk := keyNorm.Replace(strings.ToLower(k))
		byKey[nk] = append(byKey[nk], m[k])
	}
	taken := in.editsOf(byKey)
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		for _, v := range byKey[k] {
			switch {
			case slices.Contains(taken, k):
			case slices.Contains(pathKeys, k):
				in.strings(v, &in.paths, depth+1)
			case slices.Contains(contentKeys, k):
				in.strings(v, &in.contents, depth+1)
			default:
				in.strings(v, &in.other, depth+1)
			}
			in.walk(v, depth+1)
		}
	}
}

// editsOf reads old/new replacements: Claude's old_string and new_string,
// Pi's oldText and newText. It returns the keys the edits it read take, which
// are neither paths nor contents.
func (in *toolInput) editsOf(m map[string][]any) (taken []string) {
	for _, pair := range []struct {
		old, new string
		pi       bool
	}{{"oldstring", "newstring", false}, {"oldtext", "newtext", true}} {
		olds, news := m[pair.old], m[pair.new]
		if len(olds) > 1 || len(news) > 1 {
			in.twice = true
			continue
		}
		if len(olds) == 0 || len(news) == 0 {
			continue
		}
		old, ok1 := olds[0].(string)
		nw, ok2 := news[0].(string)
		if ok1 && ok2 {
			in.edits = append(in.edits, edit{old: old, new: nw, all: slices.Contains(m["replaceall"], any(true)), pi: pair.pi})
			taken = append(taken, pair.old, pair.new, "replaceall")
		}
	}
	return taken
}

// strings adds the strings in v, in arrays nested to any depth, to out.
func (in *toolInput) strings(v any, out *[]string, depth int) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case []any:
		if depth > maxDepth {
			in.truncated = true
			return
		}
		for _, e := range v {
			in.strings(e, out, depth+1)
		}
	}
}

// parsePatch reads the files a patch in Codex's apply_patch format changes,
// with the lines it adds to each.
func parsePatch(text string) []patchFile {
	var files []patchFile
	var cur *patchFile
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			files = append(files, patchFile{paths: []string{strings.TrimPrefix(line, "*** Add File: ")}})
			cur = &files[len(files)-1]
		case strings.HasPrefix(line, "*** Update File: "):
			files = append(files, patchFile{paths: []string{strings.TrimPrefix(line, "*** Update File: ")}, update: true})
			cur = &files[len(files)-1]
		case strings.HasPrefix(line, "*** Delete File: "):
			files = append(files, patchFile{paths: []string{strings.TrimPrefix(line, "*** Delete File: ")}, update: true})
			cur = &files[len(files)-1]
		case strings.HasPrefix(line, "*** Move to: ") && cur != nil:
			cur.paths = append(cur.paths, strings.TrimPrefix(line, "*** Move to: "))
		case strings.HasPrefix(line, "+") && cur != nil:
			cur.added = append(cur.added, line[1:])
		}
	}
	return files
}
