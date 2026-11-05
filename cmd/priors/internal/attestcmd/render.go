package attestcmd

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Escape renders fact bytes for a human who is about to sign them. The fact
// body is agent-written, so the human must see every byte they sign: no raw
// control, bidi, tag, invisible or unassigned rune, and no combining mark that
// could attach to the surrounding UI instead of visible text, reaches the
// terminal. Newlines are kept so the body stays readable.
func Escape(b []byte) string {
	var sb strings.Builder
	visibleBase := false
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&sb, `\x{%02X}`, b[0])
			visibleBase = false
			b = b[1:]
			continue
		}
		b = b[size:]
		switch {
		case r == '\n':
			sb.WriteRune(r)
			visibleBase = false
		case r == '\\':
			sb.WriteString(`\\`)
			visibleBase = true
		case mustEscape(r, visibleBase):
			fmt.Fprintf(&sb, `\u{%04X}`, r)
			visibleBase = false
		default:
			sb.WriteRune(r)
			visibleBase = true
		}
	}
	return sb.String()
}

func mustEscape(r rune, visibleBase bool) bool {
	if r == ' ' {
		return false
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp,
		unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector, unicode.Zs) {
		return true
	}
	if !unicode.IsGraphic(r) {
		return true
	}
	return !visibleBase && unicode.In(r, unicode.Mn, unicode.Me)
}
