package gate

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	containsTouch := func(words []Touch, want Touch) bool {
		want.From = ""
		return slices.ContainsFunc(words, func(t Touch) bool { t.From = ""; return t == want })
	}
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
		{name: "env split string", cmd: "env -S'rm -rf /h/store'", writes: true, want: []Touch{w("/h/store")}},
		{name: "env split string long", cmd: "env --split-string='rm -rf ../store/.git'", writes: true, want: []Touch{w("../store/.git")}},
		{name: "env chdir glued", cmd: "env --chdir=store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "env chdir long", cmd: "env --chdir store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "env chdir short", cmd: "env -C store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "env chdir short glued", cmd: "env -Cstore touch M", writes: true, want: []Touch{cd("store")}},
		{name: "env chdir after unset", cmd: "env -u X -C store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "sudo chdir short", cmd: "sudo -D store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "sudo chdir short glued", cmd: "sudo -Dstore touch M", writes: true, want: []Touch{cd("store")}},
		{name: "sudo chdir long", cmd: "sudo --chdir=store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "sudo -C value is an option dir", cmd: "sudo -C 3 true", writes: true, want: []Touch{cd("3")}},
		{name: "let PATH", cmd: "let PATH=1", writes: true},
		{name: "arithmetic PATH", cmd: "(( PATH=1 ))", writes: true},
		{name: "read IFS", cmd: "read IFS <<< X", writes: true},
		{name: "mapfile IFS", cmd: "mapfile -t IFS", writes: true},
		{name: "printf -v IFS", cmd: "printf -v IFS X", writes: true},
		{name: "nameref to PATH", cmd: "declare -n r=PATH; r=/tmp/evil", writes: true},
		{name: "nameref to CDPATH", cmd: "declare -n r=CDPATH; r=/h; cd store && touch M", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "nameref cluster", cmd: "typeset -rn r=x; cd store", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "nameref after builtin", cmd: "builtin declare -n r=x; cd store", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "nameref in function before call", cmd: "f() { cd store; }; declare -n r=x; f", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "IFS changed splits opaquely", cmd: "IFS=a; IFS=X; d=/h/storeX; rm -rf $d", writes: true, want: []Touch{opaque, m("/h/storeX")}},
		{name: "IFS changed quoted stays known", cmd: `IFS=:; d=/h/store; rm "$d"`, writes: true, want: []Touch{w("/h/store")}, absent: []Touch{opaque}},
		{name: "IFS default assigned", cmd: "echo ${IFS:=x}; d=/h/store; rm $d", writes: true, want: []Touch{opaque}},
		{name: "PATH expansion reads", cmd: `echo $PATH ${PATH} "$PATH" ${PATH:-x} $IFS $GOPATH`},
		{name: "GOPATH export is no writer", cmd: "export GOPATH=/x PATHS=y"},
		{name: "declare without nameref stays", cmd: "declare -x r=1; cd store", absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "export -n is no nameref", cmd: "export -n FOO; cd store", absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "builtin export -n is no nameref", cmd: "builtin export -n FOO; cd store", writes: true, absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "readonly -n is no nameref", cmd: "readonly -n FOO; cd store", absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "opaque arithmetic word without an assignment", cmd: "n=$1; (( i < $n )); d=/h/a; rm $d", writes: true, want: []Touch{w("/h/a")}},
		{name: "let of an opaque word holding an assignment", cmd: `v=i; let "${v^^}FS=0"; d=/h/a; rm $d`, writes: true, want: []Touch{opaque}},
		{name: "unparsable arithmetic value may set any name", cmd: "x='$(rm /h/store/M) b'; (( x )); d=/h/a; rm $d", writes: true, want: []Touch{opaque, w("/h/store/M")}},
		{name: "default assignment inside arithmetic", cmd: "(( ${IFS:=5} )); d=/h/a; rm $d", writes: true, want: []Touch{opaque}},
		{name: "every assigned value is read as arithmetic", cmd: "x=0; x='a[$(rm /h/store/M)]'; (( x ))", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "appended value is read as arithmetic", cmd: "x='a[$(rm /h/st'; x+='ore/M)]'; (( x ))", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v value is read as arithmetic", cmd: "printf -v x %s 'a[$(rm /h/store/M)]'; (( x ))", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v escaped value is read as arithmetic", cmd: `printf -v x 'a[\x24(rm /h/store/M)]'; (( x ))`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read here-string value is read as arithmetic", cmd: "read x <<< 'a[$(rm /h/store/M)]'; (( x ))", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "expansion index is arithmetic", cmd: "a=(1); x='a[$(rm /h/store/M)]'; echo ${a[x]}", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "expansion slice is arithmetic", cmd: "s=abc; x='a[$(rm /h/store/M)]'; echo ${s:x}", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "expansion length index is arithmetic", cmd: "a=(1); x='a[$(rm /h/store/M)]'; echo ${#a[x]}", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "all-elements index is no arithmetic", cmd: `for n in 1 2; do gh issue view $n --jq /h/store/M; done; echo ${#a[@]}`, writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "quoted paren beside an arithmetic substitution", cmd: `(( $(echo "(") + $(rm /h/store/M) ))`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "quoted paren beside a value substitution", cmd: `x='a[$(echo "(") + $(rm /h/store/M)]'; (( x ))`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "IFS unchanged splits", cmd: "d='/h/store /h/x'; echo $d", want: []Touch{m("/h/store"), m("/h/x")}},
		{name: "quote-split declared IFS splits opaquely", cmd: "d=/h/a; declare 'I'FS=Q; rm $d", writes: true, want: []Touch{opaque}},
		{name: "case-changing declare value is opaque", cmd: "declare -l d=/h/A; rm $d", writes: true, want: []Touch{opaque}},
		{name: "exported GIT config", cmd: "export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fsmonitor GIT_CONFIG_VALUE_0=x; git -C /h/store status", writes: true},
		{name: "exported LD_PRELOAD", cmd: "export LD_PRELOAD=/tmp/x.so; cat /h/store/M", writes: true},
		{name: "exported GITHUB and LDFLAGS", cmd: "export GITHUB=x LDFLAGS=-s"},
		{name: "pushd stack index", cmd: "pushd /tmp; pushd /h/work; pushd +1; touch M", writes: true, want: []Touch{{Opaque: true, Chdir: true}}, absent: []Touch{cd("+1")}},
		{name: "pushd negative stack index", cmd: "pushd -1", want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "pushd dir", cmd: "pushd /h/work", want: []Touch{cd("/h/work")}, absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "commit -C takes m as value", cmd: "git commit -Cm /h/store/M", want: []Touch{m("/h/store/M")}},
		{name: "commit flags then m", cmd: "git commit -qvnm 'x /h/store/M'", absent: []Touch{m("/h/store/M")}},
		{name: "commit -s -m", cmd: "git commit -sm 'x /h/store/M'", absent: []Touch{m("/h/store/M")}},
		{name: "commit opaque long message", cmd: `git commit --message="$(date)"`},
		{name: "commit opaque glued m", cmd: `git commit -m"$(date)"`},
		{name: "commit opaque glued am", cmd: `git commit -am"$(date)"`},
		{name: "commit opaque glued Cm", cmd: `git commit -Cm"$(date)"`, writes: true},
		{name: "commit opaque operand", cmd: "git commit $(echo x)", writes: true},
		{name: "commit am plain", cmd: "git commit -am x"},
		{name: "gh release download -b is a path", cmd: "gh release download v1 -b /h/store/M", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "gh release notes", cmd: "gh release create v1 --notes 'x /h/store/M'", writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "glab mr note", cmd: "glab mr note -m 'x /h/store/M'", writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "gh api -t is a path", cmd: "gh api -t /h/store/M", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "git log ext-diff", cmd: "git -C /h/store log --ext-diff -p", writes: true},
		{name: "git diff textconv", cmd: "git diff --textconv", writes: true},
		{name: "git log no-ext-diff", cmd: "git -C /h/store log --no-ext-diff -p", want: []Touch{cd("/h/store")}},

		// A writer's option values.
		{name: "long option value is a write and a dir", cmd: "unshare --wd=store touch M", writes: true, want: []Touch{w("store"), cd("store")}},
		{name: "single-dash name=value is a cluster too", cmd: "go test -count=1", writes: true, want: []Touch{cd("1"), cd("ount=1"), w("t=1")}},
		{name: "single-dash name=value cluster suffixes", cmd: "unshare -wa=b touch M", writes: true, want: []Touch{cd("a=b"), cd("=b"), cd("b")}},
		{name: "cluster suffix is a dir", cmd: "env -vCstore touch M", writes: true, want: []Touch{cd("store")}},
		{name: "cluster past the bound is unreadable", cmd: "env -vvvvvvvvvvvvvvvvvvvvCstore rm -rf M", writes: true, want: []Touch{cd("store"), opaque, {Opaque: true, Chdir: true}}},
		{name: "word after an abbreviated option is a dir", cmd: "env --ch store touch M", writes: true, want: []Touch{cd("store")}},
		{name: "glued shell text is a script", cmd: "foo -c'rm -rf /h/store'", writes: true, want: []Touch{w("/h/store")}},
		{name: "option dirs past the bound", cmd: "rm" + strings.Repeat(" -a 1", maxSubTokens+1) + " x", writes: true, want: []Touch{{Opaque: true, Chdir: true}}},
		{name: "many option dirs stay known", cmd: "rm -a 1 -b 2 -c 3 -d 4 -e 5 -f 6 -g 7 -h 8 -i 9 x", writes: true, want: []Touch{cd("9")}, absent: []Touch{{Opaque: true, Chdir: true}}},
		{name: "data word is no option value", cmd: "claude -p /h/store --model opus", writes: true, absent: []Touch{cd("/h/store"), w("/h/store")}},
		{name: "reader option value is no dir", cmd: "cat -A /h/store", absent: []Touch{cd("/h/store")}},

		// Data positions of a cleanly resolved known program.
		{name: "gh jq filter is data", cmd: `gh pr view 1 --jq '.a // .b'`, writes: true, absent: []Touch{w("//")}},
		{name: "glab jq filter is data", cmd: `glab mr view 3 --jq '.a // .b'`, writes: true, absent: []Touch{w("//")}},
		{name: "gh q filter is data", cmd: `gh api x -q '.a // .b'`, writes: true, absent: []Touch{w("//")}},
		{name: "claude prompt is data", cmd: `claude -p "check the build ."`, writes: true, absent: []Touch{w("check the build ."), w(".")}},
		{name: "claude bare dot is a write", cmd: `claude .`, writes: true, want: []Touch{w(".")}},
		{name: "claude prompt after env unset is data", cmd: `env -u X claude -p /h/store/M`, writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "claude prompt substitution is read", cmd: `claude -p "$(rm /h/store/M)"`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "crew reply body is data", cmd: `crew reply x /h/store/M`, writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "crew reply bare dot is data", cmd: `crew reply x .`, writes: true, absent: []Touch{w(".")}},
		{name: "crew body after a crew id is data", cmd: `crew --crew c reply x "a / b"`, writes: true, absent: []Touch{w("/")}},

		// Resolution the command keeps its writer semantics for.
		{name: "env unset name is not the command", cmd: `env -u claude rm -rf /h/store`, writes: true, want: []Touch{w("/h/store")}},
		{name: "exec name is unclean for claude", cmd: `exec -a claude rm -rf /h/store`, writes: true, want: []Touch{w("/h/store")}},
		{name: "env split string resolves no data", cmd: `env -S 'crew reply x /h/store/M'`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "redefinition withdraws claude prompt", cmd: `alias claude=rm; claude -p /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "manufactured gh name withdraws filter", cmd: `cp /bin/rm /h/bin/gh; gh api x -q /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "per-call command word is exempt", cmd: `gh pr view 1 --jq . && crew reply x /h/store/M`, writes: true, absent: []Touch{w("/h/store/M")}},

		// Finite for-loop variables.
		{name: "loop over issue numbers keeps the filter", cmd: `for n in 254 221; do gh issue view $n --json title --jq '.title, (.comments[-1].body // "" | .[0:600])'; done`, writes: true, absent: []Touch{w("//")}},
		{name: "loop filter with an escaped newline", cmd: `for n in 1 2; do gh issue view $n --jq '"\(.title)\n\(.body)" // .a'; done`, writes: true, absent: []Touch{w("//")}},
		{name: "loop reader with an escaped newline", cmd: `for n in 1 2; do gh issue view $n --jq '.a // .b'; printf '%s\n' x; done`, writes: true, absent: []Touch{w("//")}},
		{name: "nested loops keep the filter", cmd: `for a in 1 2; do for b in x y; do gh issue view $a$b --jq '.a // .b'; done; done`, writes: true, absent: []Touch{w("//")}},
		{name: "brace loop keeps the filter", cmd: `for n in {1..3}; do gh issue view $n --jq '.a // .b'; done`, writes: true, absent: []Touch{w("//")}},
		{name: "quoted loop items keep the filter", cmd: `for n in "1" '2'; do gh issue view "$n" --jq '.a // .b'; done`, writes: true, absent: []Touch{w("//")}},
		{name: "loop value touched per value", cmd: `for f in a M; do touch "/h/store/$f"; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop over REPLY withdraws", cmd: `printf '%s%s\n' /h/bin/g h > /h/f; for REPLY in 1 2; do read; cp /bin/rm "$REPLY"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop over MAPFILE withdraws", cmd: `for MAPFILE in 1 2; do mapfile -t; cp /bin/rm "$MAPFILE"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop over underscore withdraws", cmd: `for _ in 1 2; do cp /bin/rm "$_"; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop item expansion withdraws", cmd: `x=1; { read x; for n in "$x" 2; do cp /bin/rm $n; done; } < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop heredoc redirect variable withdraws", cmd: "for n in 1 2; do exec {n}<<EOF\nx\nEOF\ncp /bin/rm $n; done; gh pr view 1 --jq /h/store/M", writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop value as command word withdraws", cmd: `for c in gh rm; do $c pr view 1 --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop value names a data taker", cmd: `for n in 1 /h/bin/gh; do cp /bin/rm $n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop value shifts the subcommand", cmd: `for s in pr myext; do gh $s view --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "select withdraws", cmd: `select n in 1 2; do gh issue view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "brace loop item names a data taker", cmd: `for n in {g,x}h; do cp /bin/rm /h/bin/$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "glob loop item withdraws", cmd: `for f in *; do cp /bin/rm "$f"; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "opaque loop item withdraws", cmd: `for f in $(ls); do gh issue view $f --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "reassigned loop variable withdraws", cmd: `for n in 1 2; do n=$(cat f); cp /bin/rm $n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop over the cap withdraws", cmd: `for n in {1..65}; do gh issue view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop body redefinition withdraws", cmd: `for n in 1 2; do alias gh=rm; gh pr view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable read withdraws", cmd: `for n in 1 2; do read n; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable printf -v glued withdraws", cmd: `for n in 1 2; do printf -vn %s x; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable printf -v ansi withdraws", cmd: `for n in 1 2; do printf -v $'\x6e' %s x; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable mapfile withdraws", cmd: `for n in 1 2; do mapfile -t n < /h/f; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable arithmetic withdraws", cmd: `for n in 1 2; do (( n = 5 )); cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop redirect variable withdraws", cmd: `for n in 1 2; do exec {n}>/dev/null; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable nameref withdraws", cmd: `for n in 1 2; do declare -n r=n; r=x; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable unset withdraws", cmd: `for n in 1 2; do unset n; cp /bin/rm "$n"; done < /h/f; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "empty loop value with default withdraws", cmd: `for n in "" 1; do : $(( ${n:=h} )); cp /tmp/evil /h/bin/g$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "empty quoted loop value with default withdraws", cmd: `for n in '' 1; do : $(( ${n:=h} )); cp /tmp/evil /h/bin/g$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop arithmetic default withdraws", cmd: `for n in 1 2; do : $(( ${n:=h} )); cp /tmp/evil /h/bin/g$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "computed declare name withdraws", cmd: `for nm in 1 2; do for a in n q; do declare "${a}m=h"; done; cp /tmp/evil /h/bin/g$nm; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "computed export name withdraws", cmd: `for nm in 1 2; do for a in n q; do export "${a}m=h"; done; cp /tmp/evil /h/bin/g$nm; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "computed arithmetic name withdraws", cmd: `for nm in 1 2; do for a in n q; do (( ${a}m = 5 )); done; cp /tmp/evil /h/bin/g$nm; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "substituted declare name withdraws", cmd: `for n in 1 2; do declare "$(echo n)=/h/bin/gh"; cp /bin/rm $n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "substituted printf flag withdraws", cmd: `for n in 1 2; do printf "$(rev <<< nv-)" %s /h/bin/gh; cp /bin/rm $n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "unrelated opaque word withdraws loop", cmd: `for n in 1 2; do gh issue view $n --jq /h/store/M; done; echo "$(date)"`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "IFS changed withdraws loop", cmd: `IFS=:; for n in 1 2; do gh issue view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "nested script is read unpinned", cmd: `set -a; for n in x h; do bash -c "${n:+}cp /bin/rm /h/bin/g\$n"; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read before a subshell loop withdraws", cmd: `read n <<< g; (for n in a b; do :; done); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v before a group loop withdraws", cmd: `printf -v n %s g; { cp /bin/rm /h/bin/${n}h; for n in a b; do :; done; }; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read before a skipped loop withdraws", cmd: `read n <<< g; false && for n in a b; do :; done; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read before a nested script loop withdraws", cmd: `read n <<< g; bash -c 'for n in a b; do :; done'; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v before a piped loop withdraws", cmd: `printf -v n %s g; for n in a b; do :; done | cat; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "opaque data word withdraws loop", cmd: `for n in 1 2; do gh issue view $n --jq "$(date)"; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "indirect setter's name mentioned withdraws loop", cmd: `x=n; false && for n in a b; do :; done; gh issue view 1 --jq "${!x:=g}"; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "opaque here-string withdraws loop", cmd: `for n in 1 2; do gh issue view $n --jq /h/store/M; done; cat <<< "$(date)"`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop variable expanded in arithmetic before its loop withdraws", cmd: `(( $n )); for n in a b; do gh issue view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "indirect arithmetic default withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; (( ${!x:=g} )); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "indirect let default withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; let ${!x:=g}; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "indirect default in an arithmetic index withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; (( y[${!x:=g}] )); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "nested indirect arithmetic default withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; (( ${z:-${!x:=g}} )); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "bare arithmetic name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; (( x )); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "assignment index withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; a[x]=1; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "array element index withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; a=([x]=1); cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "integer test operand withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; [[ x -eq 1 ]]; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "integer attribute withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; declare -i y; y=x; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read subscripted name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; read 'a[x]' <<< h; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v subscripted name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; printf -v 'a[x]' %s h; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "unset subscripted name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; unset 'a[x]'; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "test -v subscripted name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; test -v 'a[x]'; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "-v test subscripted name withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; [[ -v a[x] ]]; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "quoted declare subscript withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; declare 'a[x]=1'; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "C-style loop withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; for (( x; 0; )); do :; done; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "subscripted redirect variable withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; exec {a[x]}>/dev/null; cp /bin/rm /h/bin/${n}h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "unrelated arithmetic withdraws loop", cmd: `for n in 1 2; do gh issue view $n --jq /h/store/M; done; echo $((2+3))`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read of a glob-named variable withdraws loop", cmd: `false && for n in a b; do :; done; read ? < /h/f; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "getopts of a glob-named variable withdraws loop", cmd: `for n in a b; do getopts r ? -r; cp /bin/rm /h/bin/g${n}h; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "mapfile of a glob-named variable withdraws loop", cmd: `false && for n in a b; do :; done; mapfile -t ? < /h/f; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "printf -v of a glob-named variable withdraws loop", cmd: `false && for n in a b; do :; done; printf -v ? %s h; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "declare -l of a glob-named variable withdraws loop", cmd: `declare -l ?; for n in GH X; do cp /bin/rm /h/bin/$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "read of a glob-named IFS withdraws loop", cmd: `read I?? <<< X; for n in ghX a; do cp /bin/rm /h/bin/$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "mapfile callback withdraws loop", cmd: `false && for n in a b; do :; done; mapfile -t -C declare -c 1 arr < /h/f; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "xtrace prompt default withdraws loop", cmd: `read -r x < /h/f; false && for n in a b; do :; done; PS4='${!x:=h}'; set -x; :; set +x; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "wait -p subscript withdraws loop", cmd: `read -r y < /h/f; x='a[${!y:=h}]'; false && for n in a b; do :; done; sleep 0 & wait -p "b[x]" -n; cp /bin/rm /h/bin/g$n; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "quoted glob loop item withdraws", cmd: `for n in 'g?' x; do cp /bin/rm /h/bin/$n; done; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "glob-named data taker withdraws", cmd: `cp /bin/rm /h/bin/g?; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "bracket-named data taker withdraws", cmd: `cp /bin/rm /h/bin/[g]h; gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "negated extglob data taker withdraws", cmd: `cp /bin/rm /h/bin/!(x); gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "glob that cannot name a data taker keeps the filter", cmd: `rm -f *.o; gh pr view 1 --jq /h/store/M`, writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "opaque redirect target withdraws", cmd: `cat /bin/rm > /h/bin/g$(echo h); gh pr view 1 --jq /h/store/M`, writes: true, want: []Touch{w("/h/store/M")}},
		{name: "loop redirect target keeps the filter", cmd: `for n in a b; do echo x > /h/log/$n; done; gh pr view 1 --jq /h/store/M`, writes: true, absent: []Touch{w("/h/store/M")}},
		{name: "opaque here-string to a writer", cmd: `bash <<< "$(cat /h/f)"`, writes: true, want: []Touch{opaque}},
		{name: "loop heredoc to a writer per value", cmd: "for d in a b; do bash <<EOF\nrm /h/$d\nEOF\ndone", writes: true, want: []Touch{w("/h/a"), w("/h/b")}},
		{name: "loop heredoc to a writer keeps the filter", cmd: "for d in a b; do sh <<EOF\necho $d\nEOF\ndone; gh pr view 1 --jq /h/store/M", writes: true, absent: []Touch{w("/h/store/M")}},
		// A false positive: test sets no variable but is outside the closed list.
		{name: "test beside a loop withdraws", cmd: `for n in 1 2; do test -n "$n"; gh issue view $n --jq /h/store/M; done`, writes: true, want: []Touch{w("/h/store/M")}},
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
				if !containsTouch(acc.Words, want) {
					t.Errorf("missing %+v in %+v", want, acc.Words)
				}
			}
			for _, absent := range tt.absent {
				if containsTouch(acc.Words, absent) {
					t.Errorf("unexpected %+v in %+v", absent, acc.Words)
				}
			}
		})
	}
}

func TestSubtokenFrom(t *testing.T) {
	acc := ShellAccess(nil, `python3 -c "open('/h/a','w')"`)
	for _, touch := range acc.Words {
		if touch.Text == "/h/a" {
			if touch.From != `open('/h/a','w')` {
				t.Errorf("From = %q, want %q", touch.From, `open('/h/a','w')`)
			}
			return
		}
	}
	t.Errorf("no /h/a touch in %+v", acc.Words)
}

func TestSedScript(t *testing.T) {
	for _, tt := range []struct {
		script string
		reads  bool
	}{
		{`s/refused: [^:]+: /refused: /`, true},
		{`/e/p`, true},
		{`s|e|x|g`, true},
		{`1,/re/{s/e/x/;p}`, true},
		{`:l;N;$!bl;s/\n/e/g`, true},
		{`\,e,d`, true},
		{`/e/I!d`, true},
		{`0~3{/re/d}`, true},
		{`s/[^:]+e/x/`, true},
		{`w x`, false},
		{`W x`, false},
		{`1e x`, false},
		{`s/a/b/w x`, false},
		{`s/a/b/e`, false},
		{`s/[/]/x/w f`, false},
		{`s/[\]/x/w f` + "\n" + `#]/y/`, false},
		{`s/[]/]/x/w f`, false},
		{`:a;w f`, false},
		{`:x w f`, false},
		{`b x w f`, false},
		{`1a e`, false},
		{`y/e/x/`, false},
		{`r e`, false},
		{`swewxw`, false},
		{`s/e/`, false},
		{`s/[e/x/`, false},
		{`s/e/x/\`, false},
		{`:x;/\` + "\n" + `w f\` + "\n" + `/p`, false},
		{`1#e`, false},
		{`s/\x65/x/w f`, false},
		{`s[e[x[`, false},
		{`s]e]x]`, false},
		{`s/[[:alpha:/x/e`, false},
		{`s/[[.e/x/`, false},
		{`s/[/e]/x/`, false},
	} {
		if got := sedScript(tt.script); got != tt.reads {
			t.Errorf("sedScript(%q) = %v, want %v", tt.script, got, tt.reads)
		}
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

// TestShellAccessBounded feeds ShellAccess commands built to make it read
// them more than once, or read many option values of one writer. Each must
// come back read in full or past the budget, within the stack and time bounds
// TestIngestReasonBounded holds gate 2 to. A command read again allocates up
// to twice what one read does, so its allocation bound is twice gate 2's.
func TestShellAccessBounded(t *testing.T) {
	limitStack(t)
	timeLimit := time.Second
	if raceEnabled {
		timeLimit *= 10
	}
	trigger := `d=/a; f(){ rm -rf $d; }; declare 'I'FS=Q; f`
	padded := func(size int) string { return strings.Repeat(": a; ", (size-len(trigger))/5) + trigger }
	// repeated builds a writer whose words are word(i) for i = 0, 1, …, up to
	// maxText bytes.
	repeated := func(word func(int) string) string {
		var b strings.Builder
		b.WriteString("rm")
		for i := 0; ; i++ {
			w := " " + word(i)
			if b.Len()+len(w) > maxText {
				return b.String()
			}
			b.WriteString(w)
		}
	}
	for _, tt := range []struct {
		name string
		cmd  string
	}{
		{"setter after a function body, padded to the text bound", padded(maxText)},
		{"setter after a function body, padded so both reads finish", padded(maxText * 5 / 8)},
		// No suffix of a cluster parses, so each is tried.
		{"option clusters holding shell text", repeated(func(i int) string { return "'-abcdefghijklmnop;;" + strconv.Itoa(i) + "'" })},
		{"option values past the dir cap", repeated(func(i int) string { return "--o" + strconv.Itoa(i) + "=d" + strconv.Itoa(i) })},
		{"unclosed brackets in a glob", "echo '" + strings.Repeat("[)", 40<<10) + "'"},
		{"unclosed class openers in a glob", "echo '" + strings.Repeat("[:)", 2<<10) + "'"},
		{"unclosed class openers before a close", "echo '" + strings.Repeat("[:)", 2<<10) + "]'"},
		{"unclosed brackets before an escaped close", "echo '" + strings.Repeat("[)", 40<<10) + `\]'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.cmd
			done := make(chan string, 1)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			go func() { done <- ShellAccess(&Host{Home: "/h"}, cmd).Unreadable }()
			var reason string
			select {
			case reason = <-done:
			case <-time.After(hangGuard):
				t.Fatalf("reading %d bytes took over %v", len(cmd), hangGuard)
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d bytes read in %v, %d KiB allocated", len(cmd), elapsed, alloc>>10)
			if reason != "" && reason != "budget" {
				t.Errorf("Unreadable = %q, want none or budget", reason)
			}
			if elapsed > timeLimit {
				t.Errorf("reading %d bytes took %v, over %v", len(cmd), elapsed, timeLimit)
			}
			if alloc > 2*boundAlloc {
				t.Errorf("reading %d bytes allocated %d MiB", len(cmd), alloc>>20)
			}
		})
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
