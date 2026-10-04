package gate

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var (
	shellTools = []string{"bash", "shell", "exec_command", "local_shell", "run_terminal_cmd"}
	// wrappers run their arguments as another command; they are skipped to find
	// the command word, so an inert command directly behind one stays clean.
	wrappers = []string{"env", "sudo", "doas", "command", "exec", "time", "nice", "nohup", "xargs", "timeout", "stdbuf", "builtin"}
	// keywords precede a command word without running one of their own; they are
	// skipped like wrappers.
	keywords = []string{"if", "then", "elif", "else", "do", "while", "until", "!"}
	fetchers = []string{"curl", "wget", "xh", "http", "https", "aria2c", "lynx", "w3m", "links", "elinks"}
	// inert commands search or print their arguments and never run one as a command.
	inert      = []string{"grep", "egrep", "fgrep", "rg", "which", "whereis", "type"}
	forgesAny  = []string{"hub", "tea", "jira"}
	forgeLocal = []string{"auth", "config", "alias", "completion", "help", "version", "secret", "variable", "ssh-key", "gpg-key"}
	// forgeWrite verbs print only a URL or status the agent itself caused.
	forgeWrite = []string{"create", "close", "reopen", "edit", "update", "comment", "note", "merge", "approve", "ready", "lock", "unlock", "delete", "transfer", "pin", "unpin", "review", "subscribe", "unsubscribe"}

	assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	numberWord     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
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
	if json.Unmarshal(toolInput, &in) != nil || in.Command == nil {
		return isShell
	}
	var s string
	if json.Unmarshal(in.Command, &s) == nil {
		return ingestsCommand(s)
	}
	var parts []string
	if json.Unmarshal(in.Command, &parts) == nil {
		// Codex puts the script in one element (["bash", "-lc", "<script>"]),
		// whose joined form starts with "bash" and would hide it.
		return ingestsCommand(strings.Join(parts, " ")) || slices.ContainsFunc(parts, ingestsCommand)
	}
	return true
}

// segmentBreaks separate commands; two readings of ingestsCommand add quotes.
const segmentBreaks = "\n;&|(){}`"

var (
	unescape   = strings.NewReplacer("\\\n", "", "\\", "")
	dropQuotes = strings.NewReplacer(`"`, "", "'", "")
)

// ingestsCommand matches a listed name in any segment whose command word is not
// inert, so `rg curl src/` stays clean while `echo x | curl …`, `setsid gh pr
// view 2` and `bash -c "gh pr view 2"` match. Three readings are ORed: the raw
// text with quotes as segment breaks, so a quoted script is judged on its own;
// the unescaped text likewise, so a script with escapes is too; and the
// unescaped text with quotes deleted, as bash joins words (`g""h`, `"g"h`,
// `g\h`), so a quoted argument stays with the gh that owns it.
func ingestsCommand(text string) bool {
	lower := strings.ToLower(text)
	for _, scheme := range []string{"http://", "https://", "ftp://"} {
		if strings.Contains(lower, scheme) {
			return true
		}
	}
	unescaped := unescape.Replace(text)
	return ingestsSegments(text, segmentBreaks+`"'`) ||
		ingestsSegments(unescaped, segmentBreaks+`"'`) ||
		ingestsSegments(dropQuotes.Replace(unescaped), segmentBreaks)
}

// ingestsSegments also judges each segment with env's attached values split off,
// in addition to the unsplit words, so the split can only add flags.
func ingestsSegments(text, breaks string) bool {
	segments := strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune(breaks, r)
	})
	return slices.ContainsFunc(segments, func(seg string) bool {
		words := splitWords(seg)
		return segmentIngests(words) || segmentIngests(splitEnvValues(words))
	})
}

// splitWords splits a segment into words.
func splitWords(seg string) []string {
	return strings.FieldsFunc(seg, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("<>", r)
	})
}

// splitEnvValues splits off, after an env word, a flag's attached value as its
// own word, since env -S/--split-string runs it as a command.
func splitEnvValues(fields []string) []string {
	var words []string
	seenEnv := false
	for _, w := range fields {
		if !seenEnv {
			seenEnv = wordName(w) == "env"
			words = append(words, w)
			continue
		}
		i := -1
		switch {
		case strings.HasPrefix(w, "--"):
			i = strings.IndexByte(w, '=')
		case strings.HasPrefix(w, "-"):
			i = strings.IndexByte(w, 'S')
			if i < 1 {
				i = -1
			}
		}
		if i < 0 || i+1 >= len(w) {
			words = append(words, w)
			continue
		}
		words = append(words, w[:i+1], w[i+1:])
	}
	return words
}

// segmentIngests judges one segment. Unless its command word is inert, every
// word from it on is a candidate, since any unlisted wrapper (setsid, ssh host,
// nix shell -c) may run a later word: the first listed name decides. A command
// word found after a wrapper's flag may be that flag's value (`sudo -u grep gh
// ...`), so it is not trusted to be inert. A gh/glab word with no group after
// it that is not the segment's first word may take its arguments from stdin or
// a placeholder (`xargs gh`).
func segmentIngests(words []string) bool {
	start, afterFlag := commandWord(words)
	if start < len(words) && !afterFlag && slices.Contains(inert, wordName(words[start])) {
		return false
	}
	for i := start; i < len(words); i++ {
		switch name := wordName(words[i]); {
		case slices.Contains(fetchers, name), slices.Contains(forgesAny, name):
			return true
		case name == "gh" || name == "glab":
			if group, _ := nextNonFlag(words[i+1:]); group == "" && i > 0 {
				return true
			}
			return forgeIngests(words[i+1:])
		}
	}
	return false
}

// commandWord returns the index of the segment's command word, skipping
// assignments, keywords, wrappers and their flags and numeric arguments; the
// index is len(words) when there is none. afterFlag reports that a wrapper's
// flag was skipped, so the word found may be that flag's value.
func commandWord(words []string) (idx int, afterFlag bool) {
	wrapped := false
	for i, w := range words {
		name := wordName(w)
		switch {
		case slices.Contains(wrappers, name):
			wrapped = true
		case strings.HasPrefix(w, "-"):
			afterFlag = afterFlag || wrapped
		case assignmentWord.MatchString(w), numberWord.MatchString(w), slices.Contains(keywords, name):
		default:
			return i, afterFlag
		}
	}
	return len(words), afterFlag
}

// wordName is w's last path element, without the backslash that bypasses an
// alias (`\gh`).
func wordName(w string) string {
	w = strings.TrimPrefix(w, "\\")
	return w[strings.LastIndex(w, "/")+1:]
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
