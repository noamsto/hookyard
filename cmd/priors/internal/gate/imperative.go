package gate

import (
	"regexp"
	"strings"
)

var (
	// imperativeSplit cuts a line into sentences, so an instruction that follows
	// a sentence or a colon is still judged from its first word.
	imperativeSplit = regexp.MustCompile(`\. |: `)

	imperativeHead = regexp.MustCompile("(?i)^[ \\t]*(?:(?:[-*+]|\\d+[.)])[ \\t]+)?" +
		`(?:(?:then|first|now|next|please|just|always|you should|you must)[ ,]+)*` +
		`(?:run|execute|exec|invoke|type|paste|enter|eval|source|pipe|install|sudo|curl|wget|use|call|pass|set|add|append|prefix|export)` +
		`(?:[ \t]+(.*))?$`)

	imperativeQuoted = regexp.MustCompile("`[^`]+`|\"[^\"]+\"|'[^']+'")
)

// ImperativeCommand reports whether body reads as an instruction to run a
// command: a sentence that opens with a run verb and goes on to name a code
// span, a quoted string or a path-, flag- or dotted-looking word. It reads
// prose only, so callers pass the fact body, not the frontmatter.
func ImperativeCommand(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		for _, sentence := range imperativeSplit.Split(line, -1) {
			m := imperativeHead.FindStringSubmatch(sentence)
			if m != nil && namesCommand(m[1]) {
				return true
			}
		}
	}
	return false
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
