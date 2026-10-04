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
	// wrappers run their arguments as another command, so the real command word
	// is further along.
	wrappers = []string{"env", "sudo", "doas", "command", "exec", "time", "nice", "nohup", "xargs", "timeout", "stdbuf", "builtin"}
	// keywords precede a command word without running one of their own.
	keywords   = []string{"if", "then", "elif", "else", "do", "while", "until", "!"}
	fetchers   = []string{"curl", "wget", "xh", "http", "https", "aria2c", "lynx", "w3m", "links", "elinks"}
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

// ingestsCommand matches a listed name in command position, so `rg curl src/`
// stays clean while `echo x | curl …` and `bash -c "gh pr view 2"` match.
func ingestsCommand(text string) bool {
	lower := strings.ToLower(text)
	for _, scheme := range []string{"http://", "https://", "ftp://"} {
		if strings.Contains(lower, scheme) {
			return true
		}
	}
	segments := strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune("\n;&|(){}`\"'", r)
	})
	for _, seg := range segments {
		words := strings.FieldsFunc(seg, func(r rune) bool {
			return unicode.IsSpace(r) || r == '<' || r == '>'
		})
		if segmentIngests(words) {
			return true
		}
	}
	return false
}

// segmentIngests judges one segment by its command word. After a wrapper every
// remaining word is a candidate, since a wrapper's option value (`sudo -u bob
// curl`) would otherwise pass for the command word: the first listed name
// decides.
func segmentIngests(words []string) bool {
	i, wrapped := commandWord(words)
	for ; i < len(words); i++ {
		switch name := wordName(words[i]); {
		case slices.Contains(fetchers, name), slices.Contains(forgesAny, name):
			return true
		case name == "gh" || name == "glab":
			return forgeIngests(words[i+1:])
		case !wrapped:
			return false
		}
	}
	return false
}

// commandWord returns the index of the segment's command word, skipping
// assignments, keywords, wrappers and their flags and numeric arguments, and
// whether it skipped a wrapper; the index is len(words) when there is none.
func commandWord(words []string) (int, bool) {
	wrapped := false
	for i, w := range words {
		switch name := wordName(w); {
		case slices.Contains(wrappers, name):
			wrapped = true
		case assignmentWord.MatchString(w), strings.HasPrefix(w, "-"),
			numberWord.MatchString(w), slices.Contains(keywords, name):
		default:
			return i, wrapped
		}
	}
	return len(words), wrapped
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

// nextNonFlag returns the first word not starting with "-" and the words after
// it.
func nextNonFlag(words []string) (word string, rest []string) {
	for i, w := range words {
		if !strings.HasPrefix(w, "-") {
			return w, words[i+1:]
		}
	}
	return "", nil
}
