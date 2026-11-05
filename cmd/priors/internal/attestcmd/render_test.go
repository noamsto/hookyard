package attestcmd

import (
	"strings"
	"testing"
)

func TestEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"tab", "a\tb", `a\u{0009}b`},
		{"ansi escape", "x\x1b[31m", `x\u{001B}[31m`},
		{"delete", "\x7f", `\u{007F}`},
		{"next line", "\u0085", `\u{0085}`},
		{"rtl override", "\u202e", `\u{202E}`},
		{"lri isolate", "\u2066", `\u{2066}`},
		{"zero width space", "\u200b", `\u{200B}`},
		{"zero width joiner", "\u200d", `\u{200D}`},
		{"tag character", "\U000E0041", `\u{E0041}`},
		{"line separator", " ", `\u{2028}`},
		{"no-break space", " ", `\u{00A0}`},
		{"variation selector", "️", `\u{FE0F}`},
		{"leading combining mark", "́x", `\u{0301}x`},
		{"precomposed kept", "é", "é"},
		{"invalid byte", "\xff", `\x{FF}`},
		{"backslash", `a\b`, `a\\b`},
		{"newline kept", "line1\nline2", "line1\nline2"},
		{"graphic text kept", "ü 日本", "ü 日本"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Escape([]byte(tt.in)); got != tt.want {
				t.Errorf("Escape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEscapeLeavesNoRawEscapedRune(t *testing.T) {
	escaped := []rune{
		'\t', '\x1b', '\x7f', '\u0085', '‮', '⁦', '​', '‍',
		'\U000E0041', ' ', ' ', ' ', '️', '́',
	}
	for _, r := range escaped {
		in := "a" + string(r) + " "
		if r == '́' {
			in = string(r) + "x"
		}
		if got := Escape([]byte(in)); strings.ContainsRune(got, r) {
			t.Errorf("Escape(%q) = %q still holds raw %U", in, got, r)
		}
	}
	if got := Escape([]byte("a\xffb")); strings.ContainsRune(got, '\xff') || strings.Contains(got, "\xff") {
		t.Errorf("Escape of invalid byte = %q holds raw 0xff", got)
	}
}
