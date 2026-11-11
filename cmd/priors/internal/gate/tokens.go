package gate

import "strings"

// PathFields returns the non-empty runs of s between pathBreaks: the names a
// string packs when it holds a list of paths.
func PathFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(pathBreaks, r) })
}

// PathTokens returns the path-like tokens in s: runs between pathBreaks that
// hold a slash or start with ~. A token starting with - is also taken from
// its first slash or ~ on, as `-o/path` glues one to a flag.
func PathTokens(s string) []string {
	out := []string{}
	for _, t := range PathFields(s) {
		if !strings.ContainsAny(t, "/~") {
			continue
		}
		if k := strings.IndexAny(t, "/~"); k > 0 && t[0] == '-' {
			out = append(out, t[k:])
		}
		out = append(out, t)
	}
	return out
}
