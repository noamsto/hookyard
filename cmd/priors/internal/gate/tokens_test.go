package gate

import (
	"slices"
	"testing"
)

func TestPathTokens(t *testing.T) {
	for _, c := range []struct {
		in     string
		tokens []string
		fields []string
	}{
		{in: "a,/x/y;~/z", tokens: []string{"/x/y", "~/z"}, fields: []string{"a", "/x/y", "~/z"}},
		{in: "--out=/x/y", tokens: []string{"/x/y"}, fields: []string{"--out", "/x/y"}},
		{in: "path=/x", tokens: []string{"/x"}, fields: []string{"path", "/x"}},
		{in: "-o/x/y", tokens: []string{"/x/y", "-o/x/y"}, fields: []string{"-o/x/y"}},
		{in: "a b\nnotes", tokens: nil, fields: []string{"a", "b", "notes"}},
		{in: "x .attest", tokens: []string{".attest"}, fields: []string{"x", ".attest"}},
		{in: "", tokens: nil, fields: nil},
	} {
		if got := PathTokens(c.in); !slices.Equal(got, c.tokens) {
			t.Errorf("PathTokens(%q) = %q, want %q", c.in, got, c.tokens)
		}
		if got := PathFields(c.in); !slices.Equal(got, c.fields) {
			t.Errorf("PathFields(%q) = %q, want %q", c.in, got, c.fields)
		}
	}
}
