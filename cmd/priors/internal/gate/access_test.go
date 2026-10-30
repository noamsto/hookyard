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
		{name: "env flag value as command", cmd: "env -u cat rm -rf /h/store", writes: true, want: []Touch{w("/h/store")}},
		{name: "exec flag value as command", cmd: "exec -a cat bash -c 'echo x > /h/store/M'", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "xargs flag value as command", cmd: "xargs -I cat rm -rf /h/store", writes: true, want: []Touch{opaque}},
		{name: "env reader", cmd: "env cat /h/a", absent: []Touch{w("/h/a")}},
		{name: "git unknown global option", cmd: "git -C /h/store --attr-source log reset --hard", writes: true},
		{name: "git bogus global option", cmd: "git --bogus log", writes: true},
		{name: "git harmless global options", cmd: "git --no-pager -C /h/store --git-dir=/h/g --work-tree /h/w -P log", want: []Touch{cd("/h/store")}},
		{name: "sed abbreviated in-place", cmd: "sed --in-pl s/a/b/ /h/store/M", writes: true},
		{name: "sed abbreviated expression", cmd: "sed --expr='w /h/store/M' x", writes: true},
		{name: "sed expression in cluster", cmd: "sed -ne'w /h/store/M' x", writes: true},
		{name: "sed separate value shifts script", cmd: "sed --line-length 5 'w /h/store/M' x", writes: true},
		{name: "sed short l value", cmd: "sed -l 5 'w /h/store/M' x", writes: true},
		{name: "sed script after double dash", cmd: "sed -n -- 'w /h/store/M' x", writes: true},
		{name: "sed unknown long option", cmd: "sed --quie p x", writes: true},
		{name: "sed opaque expression", cmd: `sed -e "$(echo p)" x`, writes: true, want: []Touch{opaque}},
		{name: "sed readers", cmd: "sed -n -e p --expr=s/a/b/ -Ens --silent a", absent: []Touch{w("a")}},
		{name: "sed cluster script", cmd: "sed -ne p a", absent: []Touch{w("a")}},
		{name: "git grep abbreviated pager", cmd: "git grep --open='touch /h/store/M;:' x", writes: true},
		{name: "git fetch abbreviated upload-pack", cmd: "git fetch --upload=sh o", writes: true},
		{name: "git grep bundled O", cmd: "git grep -iOcmd x", writes: true},
		{name: "git log reader", cmd: "git log --oneline -n 3 -- /h/a", absent: []Touch{w("/h/a")}},
		{name: "fd bundled exec", cmd: "fd . /h/store -Hx touch", writes: true},
		{name: "fd abbreviated exec", cmd: "fd --exe touch . /h/store", writes: true},
		{name: "fd abbreviated exec-batch", cmd: "fd --exec-b touch . /h/store", writes: true},
		{name: "fd value letters", cmd: "fd -tx -ex -d1 x /h/store", absent: []Touch{w("/h/store")}},
		{name: "assigned PATH", cmd: "PATH=/tmp/evil:$PATH; cat /h/store/M", writes: true},
		{name: "exported BASH_ENV", cmd: "export BASH_ENV=/tmp/evil; cat /h/store/M", writes: true},
		{name: "default-assigned ENV", cmd: "echo ${ENV:=/tmp/evil}", writes: true},
		{name: "loop over PATH", cmd: "for PATH in /tmp/evil; do :; done", writes: true},
		{name: "printf -v", cmd: "printf -v PATH %s /tmp/evil", writes: true},
		{name: "printf reader", cmd: "printf '%s' /h/a", absent: []Touch{w("/h/a")}},
		{name: "commit message heredoc", cmd: "git commit -m \"$(cat <<'EOF'\nfix MEMORY.md handling\nEOF\n)\"", absent: []Touch{opaque, w("fix MEMORY.md handling\n")}},
		{name: "commit message path", cmd: "git commit -am 'update /h/store/M'", absent: []Touch{w("update /h/store/M"), w("/h/store/M")}},
		{name: "commit message glued", cmd: "git commit --message='update /h/store/M' --amend", absent: []Touch{w("--message=update /h/store/M"), w("/h/store/M")}},
		{name: "commit message substitution", cmd: `git commit -m "$(rm /h/store/M)"`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "commit path stays", cmd: "git commit -m x /h/store/M", want: []Touch{m("/h/store/M")}},
		{name: "checkout -m is a flag", cmd: "git checkout -m /h/store/M", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "gh body", cmd: "gh pr create --title t --body \"$(cat <<'EOF'\nfix /h/store/M\nEOF\n)\"", writes: true, absent: []Touch{opaque}},
		{name: "gh body path", cmd: "gh issue create -b 'fix /h/store/M' --title=x", writes: true, absent: []Touch{w("fix /h/store/M"), w("/h/store/M")}},
		{name: "glab glued description", cmd: `glab mr create --description="$(echo x)"`, writes: true, absent: []Touch{{Text: "--description=", Write: true, Opaque: true}}},
		{name: "gh other operand stays", cmd: "gh api -X DELETE /h/store/M", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "gh message substitution", cmd: `gh pr comment -b "$(rm /h/store/M)"`, writes: true, want: []Touch{w("/h/store/M")}},
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

func TestShellAccessCDPATH(t *testing.T) {
	unknown := Touch{Opaque: true, Chdir: true}
	for _, tt := range []struct {
		name  string
		cdp   string
		cmd   string
		blind bool
	}{
		{name: "assigned", cmd: "CDPATH=/h cd store && touch M", blind: true},
		{name: "exported", cmd: "export CDPATH=/h; cd store", blind: true},
		{name: "pushd", cmd: "CDPATH=/h pushd store", blind: true},
		{name: "host", cdp: "/h", cmd: "cd store && touch M", blind: true},
		{name: "unset", cmd: "cd store"},
		{name: "dot", cdp: "/h", cmd: "cd ./store"},
		{name: "dotdot", cdp: "/h", cmd: "cd ../store"},
		{name: "absolute", cdp: "/h", cmd: "cd /h/store"},
		{name: "home", cdp: "/h", cmd: "cd ~/store"},
		{name: "bare", cdp: "/h", cmd: "cd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := &Host{Home: "/h", Env: func(k string) (string, bool) { return tt.cdp, k == "CDPATH" && tt.cdp != "" }}
			acc := ShellAccess(host, tt.cmd)
			if got := slices.Contains(acc.Words, unknown); got != tt.blind {
				t.Errorf("opaque chdir = %v, want %v in %+v", got, tt.blind, acc.Words)
			}
		})
	}
}
