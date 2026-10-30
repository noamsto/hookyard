package gate

import (
	"slices"
	"strings"
	"testing"
)

func TestShellAccess(t *testing.T) {
	host := &Host{
		Home:  "/h",
		Env:   func(k string) (string, bool) { return map[string]string{"S": "/h/store"}[k], k == "S" },
		Users: func(name string) (string, bool) { return "/u/" + name, name == "bob" },
	}
	w := func(text string) Touch { return Touch{Text: text, Write: true} }
	m := func(text string) Touch { return Touch{Text: text} }
	cd := func(text string) Touch { return Touch{Text: text, Chdir: true} }
	opaque := Touch{Write: true, Opaque: true}
	for _, tt := range []struct {
		name   string
		cmd    string
		writes bool
		want   []Touch
		absent []Touch
	}{
		{name: "reader", cmd: "cat ~/a", want: []Touch{m("/h/a")}, absent: []Touch{w("/h/a")}},
		{name: "redirect", cmd: "echo x > ~/a", writes: true, want: []Touch{w("/h/a")}},
		{name: "sink", cmd: "cat a > /dev/null 2>&1", absent: []Touch{w("/dev/null")}},
		{name: "writer operands", cmd: "cp a $S/b", writes: true, want: []Touch{w("a"), w("/h/store/b")}},
		{name: "tilde user", cmd: "rm ~bob/x", writes: true, want: []Touch{w("/u/bob/x")}},
		{name: "unknown user", cmd: "rm ~eve/x", writes: true, want: []Touch{w("~eve/x")}},
		{name: "git reader", cmd: "git -C ~/r log", want: []Touch{cd("/h/r")}},
		{name: "git writer", cmd: "git checkout -- .", writes: true, want: []Touch{w(".")}},
		{name: "git config flag", cmd: "git -c core.pager=x log", writes: true},
		{name: "sed reader", cmd: "sed -n --expression=p a", absent: []Touch{w("a")}},
		{name: "sed in place", cmd: "sed -i s/a/b/ f", writes: true, want: []Touch{w("f")}},
		{name: "sed w command", cmd: "sed 'w out' f", writes: true},
		{name: "find delete", cmd: "find . -delete", writes: true, want: []Touch{w(".")}},
		{name: "find reader", cmd: "find . -name x", absent: []Touch{w(".")}},
		{name: "cd", cmd: "cd ~/x && touch y", writes: true, want: []Touch{cd("/h/x"), w("y")}},
		{name: "bare cd", cmd: "cd; touch y", writes: true, want: []Touch{cd("/h")}},
		{name: "cd dash", cmd: "cd - && touch y", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "glued -C", cmd: "make -C/h/x", writes: true, want: []Touch{cd("/h/x")}},
		{name: "opaque prefix", cmd: "rm /h/$(echo x)/y", writes: true, want: []Touch{{Text: "/h/", Write: true, Opaque: true}}},
		{name: "assigned var", cmd: "d=/h/s; rm $d/x", writes: true, want: []Touch{w("/h/s/x")}},
		{name: "twice assigned var", cmd: "d=/a; d=/b; rm $d/x", writes: true, want: []Touch{{Text: "", Write: true, Opaque: true}}},
		{name: "xargs", cmd: "echo a | xargs rm", writes: true, want: []Touch{opaque}},
		{name: "piped writer", cmd: "echo rm a | sh", writes: true, want: []Touch{opaque}},
		{name: "stdin writer", cmd: "sh < script", writes: true, want: []Touch{opaque}},
		{name: "nested script", cmd: `bash -c 'echo x > ~/a'`, writes: true, want: []Touch{w("/h/a")}},
		{name: "heredoc to writer", cmd: "bash <<EOF\necho x > ~/a\nEOF", writes: true, want: []Touch{w("/h/a")}},
		{name: "path token in word", cmd: `python3 -c "open('/h/a','w')"`, writes: true, want: []Touch{w("/h/a")}},
		{name: "unparsable", cmd: "echo > ~/a; (", writes: true, want: []Touch{opaque, w("/h/a")}},
		{name: "arithmetic substitution", cmd: "echo $(( $(rm x) ))", writes: true, want: []Touch{opaque}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			acc := ShellAccess(host, tt.cmd)
			if acc.Unreadable != "" {
				t.Fatalf("unreadable: %s", acc.Unreadable)
			}
			if acc.Writes != tt.writes {
				t.Errorf("Writes = %v, want %v", acc.Writes, tt.writes)
			}
			for _, want := range tt.want {
				if !slices.Contains(acc.Words, want) {
					t.Errorf("missing %+v in %+v", want, acc.Words)
				}
			}
			for _, absent := range tt.absent {
				if slices.Contains(acc.Words, absent) {
					t.Errorf("unexpected %+v in %+v", absent, acc.Words)
				}
			}
		})
	}
}

func TestShellAccessUnreadable(t *testing.T) {
	if acc := ShellAccess(nil, strings.Repeat("a", maxText+1)); acc.Unreadable != "budget" {
		t.Errorf("Unreadable = %q, want budget", acc.Unreadable)
	}
	script := "rm x"
	for range maxDepth + 2 {
		script = "bash -c '" + strings.ReplaceAll(script, "'", `'\''`) + "'"
	}
	if acc := ShellAccess(nil, script); acc.Unreadable != "budget" {
		t.Errorf("deep nesting: Unreadable = %q, want budget", acc.Unreadable)
	}
}
