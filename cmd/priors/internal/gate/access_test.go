package gate

import (
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// accessHost is the environment the reader tests render in.
func accessHost(env map[string]string) *Host {
	return &Host{
		Home: "/h",
		Env: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		Users: func(name string) (string, bool) { return "/u/" + name, name == "bob" },
	}
}

// accessCase pins what ShellAccess reports for one command. A nil slice is
// not checked; unresolved and code are compared exactly, "" meaning none.
type accessCase struct {
	name, cmd  string
	env        map[string]string
	writes     bool
	targets    []string
	notTargets []string
	opaque     []string
	dirs       []string
	lost       bool
	unresolved string
	code       string
	names      []string
	notNames   []string
	own        []string
	notOwn     []string
}

func (c accessCase) check(t *testing.T) {
	t.Helper()
	env := map[string]string{"S": "/h/store", "SP": "/h/a b", "USER": "me", "SHELL": "/bin/bash"}
	maps.Copy(env, c.env)
	acc := ShellAccess(accessHost(env), c.cmd)
	if acc.Unreadable != "" {
		t.Fatalf("Unreadable = %q", acc.Unreadable)
	}
	texts := func(ts []Target) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.Text
		}
		return out
	}
	targets, opaque := texts(acc.Targets), texts(acc.Opaque)
	if acc.Writes != c.writes {
		t.Errorf("Writes = %v, want %v", acc.Writes, c.writes)
	}
	for _, w := range c.targets {
		if !slices.Contains(targets, w) {
			t.Errorf("target %q missing from %q", w, targets)
		}
	}
	for _, w := range c.notTargets {
		if slices.Contains(targets, w) {
			t.Errorf("target %q unexpected in %q", w, targets)
		}
	}
	if c.opaque != nil && !slices.Equal(opaque, c.opaque) {
		t.Errorf("Opaque = %q, want %q", opaque, c.opaque)
	}
	if c.dirs != nil && !slices.Equal(acc.Dirs, c.dirs) {
		t.Errorf("Dirs = %q, want %q", acc.Dirs, c.dirs)
	}
	if acc.DirLost != c.lost {
		t.Errorf("DirLost = %v, want %v", acc.DirLost, c.lost)
	}
	if acc.Unresolved != c.unresolved {
		t.Errorf("Unresolved = %q, want %q", acc.Unresolved, c.unresolved)
	}
	if acc.Code != c.code {
		t.Errorf("Code = %q, want %q", acc.Code, c.code)
	}
	for _, n := range c.names {
		if !slices.Contains(acc.Names, n) {
			t.Errorf("name %q missing from %q", n, acc.Names)
		}
	}
	for _, n := range c.notNames {
		if slices.Contains(acc.Names, n) {
			t.Errorf("name %q unexpected in %q", n, acc.Names)
		}
	}
	for _, n := range c.own {
		if !slices.Contains(acc.OwnNames, n) {
			t.Errorf("own name %q missing from %q", n, acc.OwnNames)
		}
	}
	for _, n := range c.notOwn {
		if slices.Contains(acc.OwnNames, n) {
			t.Errorf("own name %q unexpected in %q", n, acc.OwnNames)
		}
	}
}

func runAccess(t *testing.T, cases []accessCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, c.check)
	}
}

func TestShellAccessReadOnly(t *testing.T) {
	runAccess(t, []accessCase{
		{name: "cat", cmd: "cat ~/a", names: []string{"/h/a"}, notTargets: []string{"/h/a"}},
		{name: "sink redirect", cmd: "cat a > /dev/null 2>&1"},
		{name: "fd dup", cmd: "echo x >&2; echo y 2>&-"},
		{name: "printf -v", cmd: "printf -v x %s y"},
		{name: "priors", cmd: "priors list"},
		{name: "crew", cmd: "crew msg a b /h/store/M"},
		{name: "date", cmd: "date -d tomorrow +%s"},
		{name: "date set", cmd: "date -s now", writes: true, targets: []string{"now"}},
		{name: "diff", cmd: "diff -u a b"},
		{name: "diff paginate", cmd: "diff -l a b", writes: true, targets: []string{"a", "b"}},
		{name: "diff paginate abbreviated", cmd: "diff --pa a b", writes: true},
		{name: "diff of process substitutions", cmd: "diff <(sort a) <(sort b)"},
		{name: "sort", cmd: "sort -nr -k2 -t, in"},
		{name: "sort output", cmd: "sort -o out in", writes: true, targets: []string{"out"}, notTargets: []string{"in"}},
		{name: "sort temporary dir", cmd: "sort -T /tmp in", writes: true, targets: []string{"/tmp"}},
		{name: "sort compress program", cmd: "sort --compress-program=gzip in", writes: true},
		{name: "uniq one operand", cmd: "uniq -c a"},
		{name: "uniq output operand", cmd: "uniq -c a b", writes: true, targets: []string{"b"}, notTargets: []string{"a"}},
		{name: "sed", cmd: "sed -n -e p --expr=s/a/b/ -Ens --silent a"},
		{name: "sed w command", cmd: "sed 'w out' f", writes: true, unresolved: "runs sed with a script that may write"},
		{name: "sed script file", cmd: "sed -f s.sed f", writes: true, unresolved: "reads a sed script from a file"},
		{name: "awk division", cmd: "awk '{print $1 / $2}' f"},
		{name: "awk slash separator", cmd: "awk -F/ -v x=1 '{print $NF}' f"},
		{name: "awk comparison", cmd: "awk '$1 > 5' f", writes: true, unresolved: "runs awk on inline code"},
		{name: "awk alternation", cmd: "awk '/a|b/' f", writes: true, unresolved: "runs awk on inline code"},
		{name: "awk system", cmd: "awk '{system(\"x\")}' f", writes: true, unresolved: "runs awk on inline code"},
		{name: "awk include", cmd: `awk '@include "/tmp/a.awk"' f`, writes: true, targets: []string{"f"}, unresolved: "runs awk on inline code", names: []string{"/tmp/a.awk"}},
		{name: "gawk output flag", cmd: "gawk -o/h/notes/x '{print}' f", writes: true, targets: []string{"/h/notes/x"}, unresolved: "runs gawk on inline code"},
		{name: "rg", cmd: "rg -i -A3 --type go foo src"},
		{name: "rg preprocessor", cmd: "rg --pre=x foo", writes: true, targets: []string{"x"}},
		{name: "rg search zip", cmd: "rg -z foo", writes: true},
		{name: "rg pattern built at run time", cmd: "p=x; rg \"$p\" src", writes: true, opaque: []string{""}, unresolved: `writes through "$p"`},
		{name: "rg pattern value built at run time", cmd: "p=x; rg -e \"$p\" src"},
		{name: "fd", cmd: "fd -tx -ex -d1 x /h/store"},
		{name: "fd exec", cmd: "fd -x touch . /h/store", writes: true, targets: []string{"/h/store"}, unresolved: "runs fd with --exec"},
		{name: "fd list details", cmd: "fd -l x", writes: true},
		{name: "find", cmd: "find . -name '*.go' -type f -print0"},
		{name: "find delete", cmd: "find . -delete", writes: true, targets: []string{"."}, unresolved: "runs find with -delete"},
		{name: "find files0-from", cmd: "find . -files0-from f", writes: true, unresolved: "runs find with -files0-from"},
		{name: "find start built at run time", cmd: "d=x; find \"$d\" -name x", writes: true, unresolved: "runs find with an expression built at run time"},
		{name: "git log", cmd: "git log --oneline -n 3 -- /h/a", notTargets: []string{"/h/a"}},
		{name: "git config flag", cmd: "git -c core.pager=x log", writes: true},
		{name: "git output", cmd: "git log --output=/h/x", writes: true, targets: []string{"/h/x"}},
		{name: "git worktree list", cmd: "git worktree list --porcelain"},
		{name: "git worktree add", cmd: "git worktree add ../x", writes: true, targets: []string{"../x"}},
		{name: "git writer", cmd: "git checkout -- .", writes: true, targets: []string{"."}},
		{name: "git commit message", cmd: "git commit -am 'update /h/store/M'", notTargets: []string{"/h/store/M"}},
		{name: "git commit heredoc message", cmd: "git commit -m \"$(cat <<'EOF'\nfix MEMORY.md\nEOF\n)\""},
		{name: "gh view", cmd: "gh pr view 1 --jq '.a // .b'"},
		{name: "gh create with prose", cmd: `gh repo create owner/repo --private --description "A / B / C"`, notNames: []string{"/"}},
		{name: "gh body heredoc", cmd: "gh pr create --title t --body \"$(cat <<'EOF'\nfix /h/store/M\nEOF\n)\""},
		{name: "gh checkout", cmd: "gh pr checkout 1", writes: true},
		{name: "gh download dir", cmd: "gh release download v1 -D /h/x", writes: true, targets: []string{"/h/x"}},
		{name: "gh create clone", cmd: "gh repo create x --clone", writes: true},
		{name: "gh extension", cmd: "gh myext run", writes: true},
		{name: "glab checkout", cmd: "glab mr checkout 1", writes: true},
		{name: "glab view", cmd: "glab mr view 3"},
		{name: "command -v", cmd: "command -v rm"},
		{name: "set builtin", cmd: "set -e; priors list"},
		{name: "read loop", cmd: `while read l; do echo "$l"; done < f`},
		{name: "mapfile", cmd: "mapfile -t -d , x < f; shift; sleep 1"},
		{name: "builtin redirect", cmd: "read x > /h/out", writes: true, targets: []string{"/h/out"}},
		{name: "regex text is a name", cmd: `nix path-info x | sed -E 's|.*/nix/store/[a-z0-9]{32}-||' | sort`, writes: true, names: []string{".*/nix/store/[a-z0-9]"}},
	})
}

func TestShellAccessWriters(t *testing.T) {
	runAccess(t, []accessCase{
		{name: "redirect", cmd: "echo x > ~/a", writes: true, targets: []string{"/h/a"}},
		{name: "redirect fd variable", cmd: "echo x {fd}>/h/f", writes: true, targets: []string{"/h/f"}},
		{name: "process substitution target", cmd: "ls > >(tee /h/x)", writes: true, targets: []string{"/h/x"}, notTargets: []string{"/dev/fd/63"}},
		{name: "trailing slash is one target", cmd: "rm -rf build/", writes: true, targets: []string{"build/"}, notTargets: []string{"/"}, notNames: []string{"/"}},
		{name: "host variable", cmd: "cp a $S/b", writes: true, targets: []string{"a", "/h/store/b"}},
		{name: "tilde user", cmd: "rm ~bob/x ~eve/x", writes: true, targets: []string{"/u/bob/x", "~eve/x"}},
		{name: "glued target dir", cmd: "cp -tDIR a", writes: true, targets: []string{"DIR", "a"}},
		{name: "glued mode", cmd: "mkdir -m755 d", writes: true, targets: []string{"d"}, notTargets: []string{"755", "-m755"}},
		{name: "symbolic mode", cmd: "chmod -x f", writes: true, targets: []string{"f"}},
		{name: "unknown writer flag", cmd: "rm --frob x", writes: true, targets: []string{"x", "--frob"}},
		{name: "install strip program", cmd: "install --strip-program=x a b", writes: true, targets: []string{"x"}},
		{name: "sed in place", cmd: "sed -i 's/x/y/' f", writes: true, targets: []string{"f"}, notTargets: []string{"s/x/y/"}},
		{name: "sed in place bracket", cmd: `sed -i 's|^- \[x\].*|- [ ] y|' /h/work/a.md`, writes: true, targets: []string{"/h/work/a.md"}},
		{name: "sed in place writing script", cmd: "sed -i -e 's/a/b/w out' f", writes: true, targets: []string{"f"}, unresolved: "runs sed with a script that may write"},
		{name: "sed backup path", cmd: "sed -i.bak/x s/a/b/ f", writes: true, targets: []string{"f"}, unresolved: "runs sed -i with a backup suffix that may name a path"},
		{name: "dd", cmd: "dd if=/a of=/x bs=1", writes: true, targets: []string{"/a", "/x"}, notTargets: []string{"bs=1", "1"}},
		{name: "unresolved operand", cmd: "d=/x; cp a \"$d\"/b", writes: true, targets: []string{"a"}, opaque: []string{""}, unresolved: `writes through "$d"/b`},
		{name: "unresolved prefix", cmd: "rm /h/$(echo x)/y", writes: true, opaque: []string{"/h/"}, unresolved: `writes through "/h/$(echo x)/y"`},
		{name: "unresolved redirect", cmd: "o=x; echo > \"$o\"", writes: true, opaque: []string{""}, unresolved: `writes through "$o"`},
		{name: "unquoted value with a space", cmd: "rm $SP", writes: true, opaque: []string{""}, unresolved: `writes through "$SP"`},
		{name: "quoted value with a space", cmd: "rm \"$SP\"", writes: true, targets: []string{"/h/a b"}},
		{name: "settable IFS", cmd: "IFS=:; rm $S", writes: true, opaque: []string{""}, unresolved: `writes through "$S"`},
		{name: "unset variable renders empty", cmd: `rm -rf "$UNSET/"`, writes: true, targets: []string{"/"}},
		{name: "special parameter", env: map[string]string{"_": "/h/x"}, cmd: `rm "$_" "$1"`, writes: true, opaque: []string{"", ""}, unresolved: `writes through "$_"`},
		{name: "settable HOME", cmd: "HOME=/x; rm ~/y", writes: true, opaque: []string{""}, unresolved: `writes through "~/y"`},
	})
}

func TestShellAccessUnknownPrograms(t *testing.T) {
	runAccess(t, []accessCase{
		{
			name: "packed operands", cmd: "foo key=/abs out:/abs a,/abs --out=/x -o../x of=/", writes: true,
			targets: []string{"key=/abs", "/abs", "/x", "../x", "/"},
		},
		{name: "inline code", cmd: `python3 -c "open('/h/a','w')"`, writes: true, notTargets: []string{"/h/a"}, unresolved: "runs python3 on inline code", names: []string{"/h/a"}},
		{name: "piped input", cmd: "cat x | foo", writes: true, unresolved: "feeds foo input that may name paths"},
		{name: "file input", cmd: "foo < in", writes: true, unresolved: "feeds foo input that may name paths"},
		{name: "null input", cmd: "foo < /dev/null", writes: true},
		{name: "here-string input", cmd: "foo <<< data", writes: true, unresolved: "feeds foo input that may name paths"},
		{name: "path command", cmd: "$HOME/bin/tool x", writes: true, targets: []string{"x"}},
		{name: "path with a space", cmd: `"/my dir/tool" x`, writes: true, targets: []string{"x"}},
		{name: "unresolved path command", cmd: "d=x; $d/tool y", writes: true, targets: []string{"y"}},
		{name: "source", cmd: "source ./env.sh", writes: true, targets: []string{"./env.sh"}, unresolved: "runs source on a script file"},
		{name: "shell script file", cmd: "bash x.sh", writes: true, targets: []string{"x.sh"}, unresolved: "runs bash on a script file"},
		{name: "shell script from a file", cmd: "sh < script", writes: true, unresolved: "runs sh on a script file"},
		{name: "many option values", cmd: "rsync -a --exclude=u1 --exclude=u2 src/ dst/", writes: true, targets: []string{"u1", "src/", "dst/"}},
	})
}

func TestShellAccessWrappers(t *testing.T) {
	cluster := "-" + strings.Repeat("v", 40) + "Clink"
	runAccess(t, []accessCase{
		{name: "env cluster with no cap", cmd: "env " + cluster + " rm -rf facts", writes: true, targets: []string{"facts"}, dirs: []string{"link"}},
		{name: "unknown program cluster suffix", cmd: "foo " + cluster + " rm -rf facts", writes: true, targets: []string{"link", "facts"}},
		{name: "env function import", cmd: `env 'BASH_FUNC_x%%=() { rm -rf ../notes; }' bash -c 'x'`, writes: true, code: "an environment entry whose name is not a variable"},
		{name: "env chdir", cmd: "env -C store touch M", writes: true, dirs: []string{"store"}, targets: []string{"M"}},
		{name: "env chdir abbreviated", cmd: "env --chd=store touch M", writes: true, dirs: []string{"store"}},
		{name: "env split string", cmd: "env -S 'rm -rf /h/x'", writes: true, targets: []string{"/h/x"}},
		{name: "env split string with env's flags", cmd: "env -S '-Cstore touch M'", writes: true, dirs: []string{"store"}, targets: []string{"M"}},
		{name: "env split string built at run time", cmd: "s=x; env -S \"$s\"", writes: true, code: "env runs a split string built at run time"},
		{name: "env reader", cmd: "env LANG=C cat /h/a"},
		{name: "sudo chdir", cmd: "sudo -D store touch M", writes: true, dirs: []string{"store"}},
		{name: "sudo shell", cmd: "sudo -s touch M", writes: true, lost: true, targets: []string{"M"}, unresolved: "runs a program in a dir the guard cannot tell"},
		{name: "path-invoked wrapper", cmd: "/usr/bin/env touch M", writes: true, lost: true, targets: []string{"M"}, unresolved: "runs a program in a dir the guard cannot tell"},
		{name: "quoted assignment as command", cmd: "'X=1' touch M", writes: true, lost: true, targets: []string{"M"}, unresolved: `runs a command named "X=1"`},
		{name: "nice", cmd: "nice -10 rm /h/x; nice -n 5 rm /h/y", writes: true, targets: []string{"/h/x", "/h/y"}},
		{name: "timeout", cmd: "timeout -s KILL 5s rm /h/x", writes: true, targets: []string{"/h/x"}, notTargets: []string{"5s", "KILL"}},
		{name: "stdbuf time nohup doas exec", cmd: "stdbuf -oL rm /h/a; time -p rm /h/b; nohup rm /h/c; doas -u root rm /h/d; exec -a x rm /h/e", writes: true, targets: []string{"/h/a", "/h/b", "/h/c", "/h/d", "/h/e"}},
		{name: "xargs reader", cmd: "echo a | xargs cat"},
		{name: "xargs shaped reader", cmd: "echo a | xargs git log", writes: true, unresolved: "takes git's operands from stdin"},
		{name: "xargs writer", cmd: "echo a | xargs rm -f", writes: true, opaque: []string{""}, unresolved: "takes rm's operands from stdin"},
		{name: "xargs unknown", cmd: "echo a | xargs foo", writes: true, unresolved: "takes foo's operands from stdin"},
		{name: "xargs replace", cmd: "xargs -I{} cp {} /h/x", writes: true, targets: []string{"/h/x"}, opaque: []string{""}, unresolved: "takes cp's operands from stdin"},
		{name: "xargs bare replace", cmd: "xargs -i cp x{} /h/x", writes: true, targets: []string{"/h/x"}, opaque: []string{"x"}, unresolved: "takes cp's operands from stdin"},
		{name: "xargs replace into a script", cmd: "xargs -I{} sh -c 'echo {}'", writes: true, code: "a shell runs a script built at run time"},
		{name: "xargs unrecognised flag", cmd: "xargs --process-slot-var=X rm", writes: true, lost: true, unresolved: "runs a program in a dir the guard cannot tell"},
	})
}

func TestShellAccessDirs(t *testing.T) {
	runAccess(t, []accessCase{
		{name: "cd", cmd: "cd ~/x && touch y", writes: true, dirs: []string{"/h/x"}, targets: []string{"y"}},
		{name: "bare cd", cmd: "cd", dirs: []string{"/h"}},
		{name: "cd root", cmd: "cd /", dirs: []string{"/"}, names: []string{"/"}},
		{name: "cd dash", cmd: "cd - && touch y", writes: true, lost: true},
		{name: "cd built at run time", cmd: "cd \"$(pwd)\"", lost: true},
		{name: "popd", cmd: "popd; ls", lost: true},
		{name: "popd then program", cmd: "popd; make", writes: true, lost: true, unresolved: "runs a program in a dir the guard cannot tell"},
		{name: "pushd stack index", cmd: "pushd +1", lost: true},
		{name: "pushd dir", cmd: "pushd /h/work", dirs: []string{"/h/work"}},
		{name: "host CDPATH", env: map[string]string{"CDPATH": "/h"}, cmd: "cd store", lost: true},
		{name: "host CDPATH dot", env: map[string]string{"CDPATH": "/h"}, cmd: "cd ./store", dirs: []string{"./store"}},
		{name: "assigned CDPATH", cmd: "CDPATH=/h cd store", lost: true, writes: true, unresolved: "runs cd, which the command may have redefined"},
		{name: "settable HOME", cmd: "HOME=/x; cd", writes: true, lost: true, unresolved: "runs cd, which the command may have redefined"},
		{name: "git dirs", cmd: "git --git-dir=/h/g --work-tree /h/w -C /h/c status", dirs: []string{"/h/g", "/h/w", "/h/c"}},
		{name: "git dir built at run time", cmd: "d=x; git -C \"$d\" status", lost: true},
		{name: "make", cmd: "make -kC/h/y all", writes: true, dirs: []string{"/h/y"}},
		{name: "tar", cmd: "tar -C /h/z -xf a.tar", writes: true, dirs: []string{"/h/z"}},
		{name: "tar old-style", cmd: "tar xzCf d a.tar", writes: true, lost: true, unresolved: "runs a program in a dir the guard cannot tell"},
	})
}

func TestShellAccessCode(t *testing.T) {
	const arithmetic = "arithmetic over a value set at run time"
	runAccess(t, []accessCase{
		{name: "parse error", cmd: "echo > ~/a; (", writes: true, targets: []string{"/h/a"}, code: "text the guard cannot parse"},
		{name: "eval of a run-time string", cmd: "x=1; eval \"$x\"", writes: true, opaque: []string{""}, unresolved: `writes through "$x"`, code: "eval of a run-time string"},
		{name: "eval literal", cmd: "eval 'rm /h/x'", writes: true, targets: []string{"/h/x"}, unresolved: "runs eval on inline code"},
		{name: "command name built at run time", cmd: "c=rm; $c x", writes: true, code: "a command name built at run time"},
		{name: "glob command name", cmd: "e?al x", writes: true, code: "a command name built at run time"},
		{name: "glob command path", cmd: "/bin/r? x", writes: true, targets: []string{"x"}},
		{name: "test is no glob", cmd: "[ -f x ]"},
		{name: "shell reading a process substitution", cmd: "bash <(echo 'rm x')", writes: true, unresolved: "runs bash on a script file", code: "a script piped into a shell"},
		{name: "source of a process substitution", cmd: ". <(echo 'rm x')", writes: true, unresolved: "runs . on a script file", code: "a script piped into a shell"},
		{name: "script piped into a shell", cmd: "echo rm a | sh", writes: true, code: "a script piped into a shell"},
		{name: "shell script built at run time", cmd: "x=1; bash -c \"$x\"", writes: true, code: "a shell runs a script built at run time"},
		{name: "shell here-string built at run time", cmd: "bash <<< \"$(date)\"", writes: true, code: "a shell runs a script built at run time"},
		{name: "shell literal script", cmd: "bash -c 'echo x > ~/a'", writes: true, targets: []string{"/h/a"}},
		{name: "path-invoked shell", cmd: "/bin/bash -c 'rm /h/x'", writes: true, targets: []string{"/h/x"}},
		{name: "shell heredoc", cmd: "bash <<EOF\necho x > ~/a\nEOF", writes: true, targets: []string{"/h/a"}},
		{name: "trap literal", cmd: "trap 'rm /h/x' EXIT", writes: true, targets: []string{"/h/x"}},
		{name: "trap built at run time", cmd: "t=x; trap \"$t\" EXIT", writes: true, code: "trap text built at run time"},
		{name: "alias built at run time", cmd: "a=x; alias \"$a\"", writes: true, opaque: []string{""}, unresolved: `writes through "$a"`, code: "alias text built at run time"},
		{name: "indirection", cmd: "x=$(date); echo ${!x}", writes: true, code: "indirection over a value set at run time"},
		{name: "indirection while tame", cmd: "x=y; echo ${!x}"},
		{name: "prompt expansion", cmd: "x=$(date); echo ${x@P}", writes: true, code: "a prompt expansion of a value set at run time"},
		{name: "-v test", cmd: "x=$(date); [[ -v $x ]]", writes: true, code: "a -v test of a name set at run time"},
		{name: "integer declaration", cmd: "x=$(date); declare -i y", writes: true, code: "an integer variable assigned at run time"},
		{name: "subscripted setter name", cmd: "printf -v 'a[x]' %s 1", writes: true, code: "a subscripted name set at run time"},
		{name: "subscripted -v name while tame", cmd: "test -v 'a[x]'"},
		{name: "code in a subscripted read name", cmd: "read 'a[$(rm -rf ../notes)]' <<< x", writes: true, code: "code hidden in a subscripted name"},
		{name: "mapfile callback", cmd: "mapfile -C f x < y", writes: true, code: "mapfile runs a callback as code"},
		{name: "mapfile callback after a value flag", cmd: "mapfile -d , -tC f x < y", writes: true, code: "mapfile runs a callback as code"},
		{name: "mapfile flag built at run time", cmd: "o=x; mapfile \"$o\" x < y", writes: true, unresolved: `writes through "$o"`, code: "mapfile runs a callback as code"},
		{name: "PS4 assignment", cmd: "PS4='$(rm)' bash -xc true", writes: true, unresolved: "runs bash, which the command may have redefined", code: "a PS4 prompt set by the command"},
		{name: "PS4 through env", cmd: "env SHELLOPTS=xtrace PS4='$(rm -rf ../notes)' bash -c true", writes: true, unresolved: "runs bash, which the command may have redefined", code: "a PS4 prompt set by the command"},
		{name: "integer test", cmd: "x=$(cat f); [[ $x -eq 1 ]]", writes: true, code: arithmetic},
		{name: "test -eq is no arithmetic", cmd: "x=$(cat f); [ \"$x\" -eq 1 ]"},
		{name: "printf %d is no arithmetic", cmd: "x=$(cat f); printf %d \"$x\""},
		{name: "substitution in arithmetic", cmd: "echo $((1+$(cat f)))", writes: true, code: arithmetic},
		{name: "substitution in a subscript", cmd: "a[$(cat f)]=1", writes: true, code: arithmetic},
		{name: "substitution in an offset", cmd: "echo ${s:$(cat f)}", writes: true, code: arithmetic},
		{name: "positional set", cmd: "set -- 'a[$(rm -rf ../notes)]'; (( $1 ))", writes: true, code: arithmetic},
		{name: "function argument", cmd: "f() { echo $(( $1 )); }; f 'a[$(rm -rf ../notes)]'", writes: true, unresolved: `writes through "$(( $1 ))"`, code: arithmetic},
		{name: "nested script argument", cmd: "sh -c 'x' arg0 'a[$(rm)]'; (( $1 ))", writes: true, code: arithmetic},
		{name: "find runs a shell", cmd: "find . -exec sh -c '(( $1 ))' _ {} \\;", writes: true, unresolved: "runs find with -exec", code: arithmetic},
		{name: "family 2 concatenation", cmd: "a='a[$'; b='(rm -rf ../notes/facts)]'; x=$a$b; (( x ))", writes: true, code: arithmetic},
		{name: "family 2 array append", cmd: "x=('a[$' b); x[0]+='(rm -rf ../notes/facts)]'; (( x ))", writes: true, code: arithmetic},
		{name: "family 2 exported append", cmd: "export 'x=a[$'; x+='(rm -rf ../notes/facts)]'; (( x ))", writes: true, code: arithmetic},
		{name: "family 3", cmd: `x='a[$(echo "(") + $(cd ..; rm -rf no*s)] + 1 a'; (( x ))`, writes: true, code: arithmetic},
		{name: "a name holding a name", cmd: "x=a; (( x ))", writes: true, code: arithmetic},
	})
}

func TestShellAccessTaint(t *testing.T) {
	const arithmetic = "arithmetic over a value set at run time"
	runAccess(t, []accessCase{
		{name: "loop over numbers", cmd: "for i in 1 2 3; do echo $((i*2)); done"},
		{name: "counter", cmd: "n=0; ((n++))"},
		{name: "arithmetic value", cmd: "n=$((1+2)); echo $((n+1))"},
		{name: "unset name", cmd: "echo $(( x + 1 ))"},
		{name: "read value", cmd: "while read i; do echo $((i+1)); done", writes: true, code: arithmetic},
		{name: "substituted value", cmd: "n=$(wc -l <f); echo $((n+1))", writes: true, code: arithmetic},
		{name: "loop over a glob", cmd: "for i in *; do echo $((i)); done", writes: true, code: arithmetic},
	})
}

func TestShellAccessWithdrawal(t *testing.T) {
	const cat = "runs cat, which the command may have redefined"
	runAccess(t, []accessCase{
		{name: "function", cmd: "f() { :; }; cat x", writes: true, targets: []string{"x"}, unresolved: "runs :, which the command may have redefined"},
		{name: "alias", cmd: "alias ll=ls; cat x", writes: true, targets: []string{"x"}, unresolved: cat},
		{name: "hash", cmd: "hash -r; cat x", writes: true, targets: []string{"x"}, unresolved: cat},
		{name: "source", cmd: "source ./e.sh; cat x", writes: true, targets: []string{"x"}, unresolved: "runs source on a script file"},
		{name: "computed setter", cmd: "v=x; read \"$v\" <<< y; cat z", writes: true, targets: []string{"z"}, unresolved: `writes through "$v"`},
		{name: "export", cmd: "export FOO=1; cat x", writes: true, targets: []string{"x"}, unresolved: cat},
		{name: "export -n un-exports", cmd: "export -n FOO; cat x"},
		{name: "declare -n is a nameref", cmd: "declare -n r=x; cat y", writes: true, unresolved: cat},
		{name: "declare -x", cmd: "declare -x FOO=1; cat x", writes: true, unresolved: cat},
		{name: "set -a", cmd: "set -a; cat x", writes: true, unresolved: "runs set, which the command may have redefined"},
		{name: "allexport", cmd: "set -o allexport; cat x", writes: true, unresolved: "runs set, which the command may have redefined"},
		{name: "env assignment", cmd: "env FOO=1 cat x", writes: true, unresolved: cat},
		{name: "sudo assignment", cmd: "sudo FOO=1 cat x", writes: true, unresolved: cat},
		{name: "exported pager", cmd: "GIT_PAGER=cat git log", writes: true, targets: []string{"log"}, unresolved: "runs git, which the command may have redefined"},
		{name: "safe set", cmd: "LANG=C git log; LC_ALL=C cat x; TERM=xterm NO_COLOR=1 cat y"},
		{name: "host-exported name", cmd: "USER=x; cat y", writes: true, unresolved: cat},
		{name: "unexported name", cmd: "i=1; cat x"},
		{name: "bash-special name", cmd: "IFS=:; cat x", writes: true, unresolved: cat},
		{name: "PATH", cmd: "PATH=/x cat y", writes: true, unresolved: cat},
		{name: "target replacing a program", cmd: "ln -s /bin/rm ~/bin/gh; cat x", writes: true, targets: []string{"/h/bin/gh", "x"}, unresolved: "runs ln, which the command may have redefined"},
		{name: "glob target", cmd: "rm -f *; cat x", writes: true, targets: []string{"*", "x"}, unresolved: "runs rm, which the command may have redefined"},
		{name: "glob that names no program", cmd: "rm -f *.o; cat x", writes: true, targets: []string{"*.o"}, notTargets: []string{"x"}},
		{name: "unresolved target", cmd: "d=x; cp a \"$d\"; cat y", writes: true, targets: []string{"y"}, unresolved: `writes through "$d"`},
	})
}

func TestShellAccessNames(t *testing.T) {
	runAccess(t, []accessCase{
		{name: "prose", cmd: `echo "A / B"`, names: []string{"A / B"}, notNames: []string{"/"}},
		{name: "whole-word root", cmd: "echo /", names: []string{"/"}},
		{name: "cluster suffix", cmd: "foo -o../x", writes: true, names: []string{"../x"}},
		{name: "trailing slash", cmd: "rm -rf build/", writes: true, names: []string{"build/"}, notNames: []string{"/"}},
		{name: "home in a quoted word", cmd: `echo '$HOME/notes' '~/x'`, names: []string{"/h/notes", "/h/x"}},
		{name: "printf escapes", cmd: `printf '\x2fh\x2fnotes'`, names: []string{"/h/notes"}},
		{name: "literal parts of an unresolved word", cmd: `x=1; echo "$x/notes/a"`, names: []string{"/notes/a"}},
		{name: "assignment values and heredocs", cmd: "d=/h/notes; cat <<EOF\n/h/state/x\nEOF", names: []string{"/h/notes", "/h/state/x"}},
	})
}

func TestShellAccessOwnNames(t *testing.T) {
	runAccess(t, []accessCase{
		{name: "inline root", cmd: `python3 -c 'shutil.rmtree("/")'`, writes: true, unresolved: "runs python3 on inline code", own: []string{"/", "shutil.rmtree", "-c", "python3"}},
		{name: "sed w file", cmd: "sed -n 'w hard' f", writes: true, unresolved: "runs sed with a script that may write", own: []string{"w", "hard", "f"}},
		{name: "heredoc dot", cmd: "python3 - <<EOF\nrm -rf .\nEOF", writes: true, unresolved: "feeds python3 input that may name paths", own: []string{".", "rm", "-"}},
		{name: "here-string home", cmd: "foo <<< '~/x ..'", writes: true, unresolved: "feeds foo input that may name paths", own: []string{"/h/x", ".."}},
		{name: "only the unresolved call's words", cmd: `echo "a / b"; python3 -c 'print(1)'`, writes: true, unresolved: "runs python3 on inline code", own: []string{"print", "1"}, notOwn: []string{"/", "a", "b"}},
		{name: "read-only prose owns nothing", cmd: `gh repo create o/r --description "A / B"`, notOwn: []string{"/", "A"}},
		{name: "resolved writer owns nothing", cmd: "rm -rf build/", writes: true, targets: []string{"build/"}, notOwn: []string{"build"}},
	})
}

func TestShellAccessOptions(t *testing.T) {
	acc := ShellAccess(accessHost(nil), "unshare --wd=w -C d -xvf a.tar rm ../x -- -Cz")
	if want := []string{"w", "d", "vf", "f", "a.tar"}; len(acc.Options) != 1 || !slices.Equal(acc.Options[0], want) {
		t.Errorf("Options = %q, want [%q]", acc.Options, want)
	}
	if !slices.Contains(acc.Targets, Target{Text: "../x", Call: 1}) {
		t.Errorf("Targets = %+v", acc.Targets)
	}
	if acc := ShellAccess(accessHost(nil), "rm -r -- ../x"); len(acc.Options) != 0 {
		t.Errorf("a modeled writer has Options %q", acc.Options)
	}
}

func TestShellAccessTargetFrom(t *testing.T) {
	acc := ShellAccess(accessHost(nil), "foo --out=/x")
	if !slices.Contains(acc.Targets, Target{Text: "/x", From: "--out=/x", Call: 1}) || !slices.Contains(acc.Targets, Target{Text: "--out=/x", Call: 1}) {
		t.Errorf("Targets = %+v", acc.Targets)
	}
	if len(acc.Options) != 1 || !slices.Equal(acc.Options[0], []string{"/x"}) {
		t.Errorf("Options = %q", acc.Options)
	}
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

func TestShellAccessBudget(t *testing.T) {
	body := strings.Repeat("\tfmt.Println(\"a/b/c\", x{1, 2}) // keep\n", 8192/42)
	acc := ShellAccess(accessHost(nil), "cat > f <<'EOF'\n"+body+"EOF")
	if acc.Unreadable != "" || acc.Code != "" || acc.Unresolved != "" {
		t.Errorf("8 KB heredoc: Unreadable %q, Code %q, Unresolved %q", acc.Unreadable, acc.Code, acc.Unresolved)
	}
	for _, tt := range []struct{ name, cmd string }{
		{"nesting", strings.Repeat("$(", 300) + strings.Repeat(")", 300)},
		{"text", strings.Repeat("a", maxText+1)},
		{"depth", func() string {
			script := "rm x"
			for range maxDepth + 2 {
				script = "bash -c '" + strings.ReplaceAll(script, "'", `'\''`) + "'"
			}
			return script
		}()},
	} {
		if acc := ShellAccess(nil, tt.cmd); acc.Unreadable != "budget" {
			t.Errorf("%s: Unreadable = %q, want budget", tt.name, acc.Unreadable)
		}
	}
}

// TestShellAccessBounded feeds ShellAccess commands built to make it read a
// lot: each must come back read in full or past the budget, within the stack
// and time bounds TestIngestReasonBounded holds gate 2 to. The judge may run
// twice, so the allocation bound is twice gate 2's.
func TestShellAccessBounded(t *testing.T) {
	limitStack(t)
	timeLimit := time.Second
	if raceEnabled {
		timeLimit *= 10
	}
	// repeated builds a command whose words are word(i), up to maxText bytes.
	repeated := func(prefix string, word func(int) string) string {
		var b strings.Builder
		b.WriteString(prefix)
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
		{"option clusters", repeated("foo", func(i int) string { return "-abcdefghijklmnop" + strconv.Itoa(i) })},
		{"option values", repeated("rsync", func(i int) string { return "--exclude=u" + strconv.Itoa(i) })},
		{"unresolved targets", repeated("ln -s x", func(i int) string { return "\"$x" + strconv.Itoa(i) + "\"" })},
		{"globs", repeated("rm", func(i int) string { return "[" + strconv.Itoa(i) + "]*?" })},
		{"long cluster", "foo -" + strings.Repeat("v", maxText-10)},
		{"arithmetic", "(( " + strings.Repeat("x+", maxText/2-10) + "1 ))"},
		{"nested scripts", repeated("true", func(i int) string { return "; bash -c 'echo " + strconv.Itoa(i) + "'" })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.cmd
			done := make(chan string, 1)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			go func() { done <- ShellAccess(accessHost(nil), cmd).Unreadable }()
			var reason string
			select {
			case reason = <-done:
			case <-time.After(hangGuard):
				t.Fatalf("reading %d bytes took over %v", len(cmd), hangGuard)
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d bytes read in %v, %d KiB allocated, Unreadable %q", len(cmd), elapsed, alloc>>10, reason)
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
