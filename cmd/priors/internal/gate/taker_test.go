package gate

import "testing"

func TestDataTaker(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want bool
	}{
		{"gh", true},
		{"/h/bin/crew", true},
		{"g?", true},
		{"??", true},
		{"*", true},
		{"cl*", true},
		{"[g]h", true},
		{"[!x]h", true},
		{"[]g]h", true},
		{"[[:alpha:]]h", true},
		{"!(x)", true},
		{"/h/bin/@(gh)", true},
		{"+(g)h", true},
		{"=g?", true},
		{"ghx", false},
		{"*.o", false},
		{"x*", false},
		{"g??", false},
		{".[0]", false},
		{"[", false},
		{"a/g?/x", false},
	} {
		if got := dataTaker(tt.s); got != tt.want {
			t.Errorf("dataTaker(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
