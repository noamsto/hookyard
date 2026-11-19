package gate

import (
	"regexp"
	"strings"
)

var (
	// imperativeSplit cuts a line into sentences and clauses, so an instruction
	// that follows a sentence, a colon or a leading clause ("If X, run ...") is
	// still judged from its first word.
	imperativeSplit = regexp.MustCompile(`\. |: |, `)

	imperativeHead = regexp.MustCompile("(?i)^[ \\t]*(?:(?:[-*+]|\\d+[.)])[ \\t]+)?" +
		`(?:(?:then|first|now|next|please|just|always|you should|you must|remember to|make sure to|make sure you|be sure to|don't forget to|do not forget to)[ ,]+)*` +
		`(?:run|execute|exec|invoke|type|paste|enter|eval|source|pipe|install|sudo|curl|wget|use|call|pass|set|add|append|prefix|export)` +
		`(?:[ \t]+(.*))?$`)

	imperativeQuoted = regexp.MustCompile("`[^`]+`|\"[^\"]+\"|'[^']+'")
)

// nounUse lists the words that, right after a run verb, mark a noun phrase
// rather than a command ("Source code for ...", "Type checking runs ...",
// "Call sites of ...", "Use of ..."). They are the measured false positives
// of the corpus; keep the list short.
var nounUse = map[string]bool{
	"of": true, "code": true, "checking": true, "check": true, "sites": true, "site": true,
	"cases": true, "is": true, "are": true, "was": true, "were": true,
	"lives": true, "runs": true, "files": true, "file": true,
}

// ImperativeCommand reports whether text reads as an instruction to run a
// command: a sentence or clause that opens with a run verb and goes on to name
// a code span, a quoted string or a path-, flag- or dotted-looking word. It
// reads prose only, not the frontmatter.
func ImperativeCommand(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		for _, sentence := range imperativeSplit.Split(line, -1) {
			m := imperativeHead.FindStringSubmatch(sentence)
			if m != nil && !isNounUse(m[1]) && namesCommand(m[1]) {
				return true
			}
		}
	}
	return false
}

func isNounUse(rest string) bool {
	next, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	return nounUse[strings.ToLower(next)]
}

func namesCommand(rest string) bool {
	if imperativeQuoted.MatchString(rest) {
		return true
	}
	for w := range strings.FieldsSeq(rest) {
		if strings.ContainsAny(strings.TrimRight(w, ".,;:!?)"), "/-.") {
			return true
		}
	}
	return false
}
