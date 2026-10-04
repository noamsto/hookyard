package gate

import (
	"encoding/json"
	"strings"
	"testing"
)

// commandInput returns the JSON tool input for a shell command.
func commandInput(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

// nestedBash wraps `gh issue view 1` in depth levels of bash -c and returns the
// JSON tool input for it.
func nestedBash(depth int) string {
	script := "gh issue view 1"
	for range depth {
		script = "bash -c '" + strings.ReplaceAll(script, "'", `'\''`) + "'"
	}
	return commandInput(script)
}

// nestedHome replaces every character of HOME with the next level's value,
// depth levels deep, so expanding it grows fivefold per level.
func nestedHome(depth int) string {
	return strings.Repeat("${HOME//?/", depth) + "$HOME" + strings.Repeat("}", depth)
}

type ingestsCallTest struct {
	name  string
	tool  string
	input string
	want  bool
}

// ingestsCallTests are TestIngestsCall's rows; BenchmarkIngestReason judges
// their commands too.
func ingestsCallTests() []ingestsCallTest {
	return []ingestsCallTest{
		// ingestion
		{"gh issue view", "Bash", `{"command":"gh issue view 12"}`, true},
		{"gh pr view json", "Bash", `{"command":"gh pr view 3 --json body"}`, true},
		{"gh api comments", "Bash", `{"command":"gh api repos/o/r/issues/1/comments"}`, true},
		{"gh api graphql", "Bash", `{"command":"gh api graphql -f q=x"}`, true},
		{"gh search", "Bash", `{"command":"gh search issues foo"}`, true},
		{"gh repo view", "Bash", `{"command":"gh repo view o/r"}`, true},
		{"gh run view", "Bash", `{"command":"gh run view 1 --log"}`, true},
		{"gh pr checkout", "Bash", `{"command":"gh pr checkout 5"}`, true},
		{"gh pr no verb", "Bash", `{"command":"gh pr"}`, true},
		{"gh issue list", "Bash", `{"command":"gh issue list"}`, true},
		{"glab mr view", "Bash", `{"command":"glab mr view 4"}`, true},
		{"glab issue view", "Bash", `{"command":"glab issue view 2"}`, true},
		{"curl", "Bash", `{"command":"curl -s example.org"}`, true},
		{"wget", "Bash", `{"command":"wget x"}`, true},
		{"xh", "Bash", `{"command":"xh get x"}`, true},
		{"piped curl", "Bash", `{"command":"echo hi | curl x"}`, true},
		{"chained gh", "Bash", `{"command":"cd a && gh pr diff 3"}`, true},
		{"env assignment", "Bash", `{"command":"GH_TOKEN=x gh issue view 1"}`, true},
		{"env wrapper", "Bash", `{"command":"env FOO=1 gh pr view 1"}`, true},
		{"sudo wrapper", "Bash", `{"command":"sudo gh pr view 1"}`, true},
		{"xargs wrapper", "Bash", `{"command":"xargs -n1 gh issue view"}`, true},
		{"timeout wrapper", "Bash", `{"command":"timeout 10 curl x"}`, true},
		{"bash -c", "Bash", `{"command":"bash -c \"gh pr view 2\""}`, true},
		{"command substitution", "Bash", `{"command":"x=$(gh issue view 1 --json body)"}`, true},
		{"backticks", "Bash", "{\"command\":\"`curl x`\"}", true},
		{"absolute path", "Bash", `{"command":"/usr/bin/gh issue view 1"}`, true},
		{"git clone url", "Bash", `{"command":"git clone https://github.com/o/r"}`, true},
		{"python url", "Bash", `{"command":"python3 -c 'import urllib; urllib.request.urlopen(\"HTTPS://x\")'"}`, true},
		{"hub", "Bash", `{"command":"hub issue"}`, true},
		{"tea", "Bash", `{"command":"tea issues"}`, true},
		{"jira", "Bash", `{"command":"jira issue view X-1"}`, true},
		{"gh flag before group", "Bash", `{"command":"gh -R o/r pr create"}`, true},
		{"array script element", "Bash", `{"command":["bash","-lc","gh issue view 1"]}`, true},
		{"array joined", "Bash", `{"command":["gh","issue","view","1"]}`, true},
		{"command not readable", "Bash", `{"command":42}`, true},
		{"bash no command", "Bash", `{}`, true},
		{"shell no command", "Shell", `{"cwd":"/"}`, true},
		{"exec_command cmd key", "exec_command", `{"cmd":"gh issue view 1"}`, true},
		{"local_shell no command", "local_shell", `{}`, true},
		{"run_terminal_cmd no command", "run_terminal_cmd", `{}`, true},
		{"pi bash no command", "bash", `{}`, true},
		{"url in write command", "Bash", `{"command":"gh pr create --body \"see https://x\""}`, true},
		{"for loop", "Bash", `{"command":"for n in 1 2; do gh issue view $n; done"}`, true},
		{"while loop", "Bash", `{"command":"while read n; do gh api repos/o/r/issues/$n/comments; done < ids"}`, true},
		{"if condition", "Bash", `{"command":"if gh pr view 12 --json body; then echo ok; fi"}`, true},
		{"negation", "Bash", `{"command":"! gh pr checks 12"}`, true},
		{"until loop", "Bash", `{"command":"until gh run view 1; do sleep 1; done"}`, true},
		{"array for loop", "Bash", `{"command":["bash","-lc","for n in 1 2; do gh issue view $n; done"]}`, true},
		{"sudo option value", "Bash", `{"command":"sudo -u bob curl x"}`, true},
		{"sudo -u inert user", "Bash", `{"command":"sudo -u grep gh issue view 1"}`, true},
		{"doas -u inert user", "Bash", `{"command":"doas -u rg gh pr view 1"}`, true},
		{"env -u inert name", "Bash", `{"command":"env -u grep gh issue view 1"}`, true},
		{"time -o inert file", "Bash", `{"command":"time -o grep gh issue view 1"}`, true},
		{"xargs -I inert replstr", "Bash", `{"command":"echo 1 | xargs -I rg gh issue view rg"}`, true},
		{"parallel gh placeholder", "Bash", `{"command":"parallel gh {} ::: \"issue view 1\""}`, true},
		{"xargs gh stdin", "Bash", `{"command":"echo \"issue view 1\" | xargs gh"}`, true},
		{"xargs -a gh", "Bash", `{"command":"xargs -a args.txt gh"}`, true},
		{"timeout option value", "Bash", `{"command":"timeout -s KILL 10 gh issue view 1"}`, true},
		{"env option value", "Bash", `{"command":"env -u FOO gh pr view 1"}`, true},
		{"backslash alias bypass", "Bash", `{"command":"\\gh issue view 1"}`, true},
		{"setsid wrapper", "Bash", `{"command":"setsid gh issue view 1"}`, true},
		{"flock wrapper", "Bash", `{"command":"flock /tmp/l gh issue view 1"}`, true},
		{"watch wrapper", "Bash", `{"command":"watch gh pr view 1"}`, true},
		{"ionice wrapper", "Bash", `{"command":"ionice -c3 gh issue view 1"}`, true},
		{"taskset wrapper", "Bash", `{"command":"taskset 1 gh issue view 1"}`, true},
		{"strace wrapper", "Bash", `{"command":"strace gh issue view 1"}`, true},
		{"chronic wrapper", "Bash", `{"command":"chronic gh issue view 1"}`, true},
		{"unbuffer wrapper", "Bash", `{"command":"unbuffer gh issue view 1"}`, true},
		{"parallel wrapper", "Bash", `{"command":"parallel gh issue view ::: 1 2"}`, true},
		{"ssh remote command", "Bash", `{"command":"ssh host gh issue view 1"}`, true},
		{"nix shell -c", "Bash", `{"command":"nix shell nixpkgs#curl -c curl foo"}`, true},
		{"busybox applet", "Bash", `{"command":"busybox wget foo"}`, true},
		{"coproc", "Bash", `{"command":"coproc gh issue view 1"}`, true},
		{"echo piped to bash", "Bash", `{"command":"echo gh issue view 1 | bash"}`, true},
		// echo is not inert: piped into a shell it runs, so it flags by design.
		{"echo gh", "Bash", `{"command":"echo gh issue view 1"}`, true},
		{"gh -R quoted repo", "Bash", `{"command":"gh -R \"$REPO\" pr view 1"}`, true},
		{"gh --repo quoted", "Bash", `{"command":"gh --repo \"o/r\" issue view 1"}`, true},
		{"gh double-quoted group", "Bash", `{"command":"gh \"issue\" view 1"}`, true},
		{"gh single-quoted group", "Bash", `{"command":"gh 'issue' view 1"}`, true},
		{"glab quoted group", "Bash", `{"command":"glab \"mr\" view 1"}`, true},
		{"bash -c gh args", "Bash", `{"command":"bash -c 'gh \"$@\"' _ issue view 1"}`, true},
		{"quoted command word", "Bash", `{"command":"\"gh\" issue view 1"}`, true},
		{"env -S fused", "Bash", `{"command":"env -Sgh issue view 1"}`, true},
		{"env --split-string fused", "Bash", `{"command":"env --split-string=gh issue view 1"}`, true},
		{"sudo env -S fused", "Bash", `{"command":"sudo env -Sgh issue view 1"}`, true},
		{"escaped spaces", "Bash", `{"command":"bash -c gh\\ issue\\ view\\ 1"}`, true},
		{"inner backslashes", "Bash", `{"command":"\\g\\h issue view 1"}`, true},
		{"line continuation", "Bash", `{"command":"gh \\\nissue view 1"}`, true},
		{"quoted script after write verb", "Bash", `{"command":"gh pr create --body \"gh issue view 1\""}`, true},
		{"escaped space in command word", "Bash", `{"command":"grep\\ x gh issue view 1"}`, true},
		{"env gh attached short value", "Bash", `{"command":"env gh issue -Racme/Sedit view 1"}`, true},
		{"env gh attached long value", "Bash", `{"command":"env gh --x=auth issue view 1"}`, true},
		{"env gh attached jq value", "Bash", `{"command":"env gh issue --jq=close view 1"}`, true},
		{"empty quotes inside word", "Bash", `{"command":"g\"\"h issue view 1"}`, true},
		{"empty single quotes inside word", "Bash", `{"command":"g''h issue view 1"}`, true},
		{"quoted piece joined to word", "Bash", `{"command":"\"g\"h issue view 1"}`, true},
		{"empty quotes in fetcher", "Bash", `{"command":"c''url example.com"}`, true},
		{"bash -c joined quotes", "Bash", `{"command":"bash -c 'g\"\"h issue view 1'"}`, true},
		{"backslash newline inside word", "Bash", `{"command":"g\\\nh issue view 1"}`, true},
		{"rg --pre escaped script", "Bash", `{"command":"rg --pre 'g\\h issue view 1' ."}`, true},
		{"earlier gh hides joined gh", "Bash", `{"command":"printf '%s\\n' 'gh auth status' \"g''h issue view 1\" | sh"}`, true},
		{"parallel earlier gh hides joined gh", "Bash", `{"command":"parallel ::: 'gh auth status' \"g''h issue view 1\""}`, true},
		{"xargs sh -c earlier gh hides joined gh", "Bash", `{"command":"printf '%s\\0' 'gh auth status' \"g''h issue view 1\" | xargs -0 -I{} sh -c '{}'"}`, true},
		// bash binds the second string to $0 and does not run it; it flags by
		// design, since every listed name is judged.
		{"bash -c two quoted scripts", "Bash", `{"command":"bash -c 'gh auth status' \"g''h issue view 1\""}`, true},
		{"nested quoted joined gh", "Bash", `{"command":"bash -c \"sh -c 'gh auth status' \\\"g''h issue view 1\\\"\""}`, true},
		{"escaped quote before quoted scripts", "Bash", `{"command":"echo \\\" 'gh auth status' \"g''h issue view 1\""}`, true},
		{"single-quoted joined gh", "Bash", `{"command":"printf '%s\\n' 'gh auth status' 'g\"\"h issue view 1' | sh"}`, true},
		{"bare gh joined to single-quoted args", "Bash", `{"command":"printf '%s\\n' 'gh auth status' gh' issue view 1' | sh"}`, true},
		{"bare gh joined to double-quoted args", "Bash", `{"command":"printf '%s\\n' 'gh auth status' gh\" issue view 1\" | sh"}`, true},
		{"quoted gh joined to quoted args", "Bash", `{"command":"printf '%s\\n' 'gh auth status' \"gh\"' issue view 1' | sh"}`, true},
		{"empty quotes and quoted args", "Bash", `{"command":"printf '%s\\n' 'gh auth status' g''h' issue view 1' | sh"}`, true},
		{"escaped spaces joined gh", "Bash", `{"command":"printf '%s\\n' 'gh auth status' g''h\\ issue\\ view\\ 1 | sh"}`, true},
		{"escaped spaces earlier gh", "Bash", `{"command":"printf '%s\\n' gh\\ auth\\ status gh\\ issue\\ view\\ 1 | sh"}`, true},
		{"parallel bare gh joined", "Bash", `{"command":"parallel ::: 'gh auth status' gh' issue view 1'"}`, true},
		{"bash -c joined word script", "Bash", `{"command":"bash -c 'gh auth status' \"gh\"' issue view 1'"}`, true},
		{"quotes restart in command substitution", "Bash", `{"command":"sh -c \"$(printf '%s\\n' 'gh auth status' \"g''h issue view 1\")\""}`, true},
		{"line continuation joins gh", "Bash", `{"command":"printf '%s\\n' 'gh auth status' gh\\\n' issue view 1' | sh"}`, true},
		{"line continuation inside word", "Bash", `{"command":"printf '%s\\n' 'gh auth status' g\\\nh' issue view 1' | sh"}`, true},
		{"line continuation in double quotes", "Bash", `{"command":"printf '%s\\n' 'gh auth status' g\"\\\n\"h' issue view 1' | sh"}`, true},
		{"line continuation between quoted parts", "Bash", `{"command":"printf '%s\\n' 'gh auth status' 'g'\\\n'h issue view 1' | sh"}`, true},
		{"glab line continuation", "Bash", `{"command":"printf '%s\\n' 'glab auth status' gl\\\nab' mr view 1' | sh"}`, true},
		{"printf newline escape", "Bash", `{"command":"printf 'gh auth status\\ngh issue view 1\\n' | sh"}`, true},
		{"echo -e newline escape", "Bash", `{"command":"echo -e 'gh auth status\\ngh issue view 1' | sh"}`, true},
		{"printf %b escape", "Bash", `{"command":"printf '%b' 'gh auth status\\ngh issue view 1' | sh"}`, true},
		{"printf octal newline", "Bash", `{"command":"printf 'gh auth status\\012gh issue view 1' | sh"}`, true},
		{"printf hex newline", "Bash", `{"command":"printf 'gh auth status\\x0agh issue view 1' | sh"}`, true},
		{"printf tab escape", "Bash", `{"command":"printf 'gh\\tissue view 1' | sh"}`, true},
		{"printf hex space", "Bash", `{"command":"printf 'gh\\x20issue view 1' | sh"}`, true},
		{"printf joined quoted gh", "Bash", `{"command":"printf 'gh auth status\\n'\"g''h issue view 1\" | sh"}`, true},
		{"plain earlier gh hides later gh", "Bash", `{"command":"printf '%s %s %s %s\\n' gh auth status x gh issue view 1 | sh"}`, true},
		// a later listed name flags even in a write or local segment, by design:
		// every listed name is judged.
		{"later name in write segment", "Bash", `{"command":"gh pr create --title t --label gh"}`, true},
		{"fetcher in write body", "Bash", `{"command":"gh pr comment 1 --body curl"}`, true},
		{"escaped newline in commit message", "Bash", `{"command":"git commit -m 'fix: gh auth status\\nand gh issue view'"}`, true},
		{"ssh ProxyCommand joined gh", "Bash", `{"command":"ssh -o ProxyCommand=\"g''h issue view 1\" host"}`, true},
		{"ssh ProxyCommand single-quoted joined gh", "Bash", `{"command":"ssh -o ProxyCommand='g\"\"h issue view 1' host"}`, true},
		{"ssh fused ProxyCommand joined gh", "Bash", `{"command":"ssh -oProxyCommand=\"g''h issue view 1\" host"}`, true},
		{"git core.pager joined gh", "Bash", `{"command":"git -c core.pager=\"g''h issue view 1\" log"}`, true},
		{"flock --command joined gh", "Bash", `{"command":"flock --command=\"g''h issue view 1\" /tmp/l"}`, true},
		{"rsync --rsh joined gh api", "Bash", `{"command":"rsync --rsh=\"g''h api repos/o/r/issues\" a b"}`, true},
		{"GIT_PAGER joined gh", "Bash", `{"command":"rg x; GIT_PAGER=\"g''h issue view 1\" git log"}`, true},
		{"rg --pre joined gh", "Bash", `{"command":"rg --pre \"g''h issue view 1\" ."}`, true},
		{"rg --pre sh -c joined gh", "Bash", `{"command":"timeout 5 rg --pre \"sh -c 'g\\\"\\\"h issue view 1'\" ."}`, true},
		{"ansi-c script", "Bash", `{"command":"$'\\x67h issue view 1'"}`, true},
		{"ansi-c command word", "Bash", `{"command":"$'\\x67h' issue view 1"}`, true},
		{"dollar empty quotes in word", "Bash", `{"command":"g$\"\"h issue view 1"}`, true},
		{"brace expansion", "Bash", `{"command":"{gh,issue,view,1}"}`, true},
		{"default value expansion", "Bash", `{"command":"${X:-gh} issue view 1"}`, true},
		{"env fused -S after flag", "Bash", `{"command":"env -iSgh issue view 1"}`, true},
		{"script -c fused escaped", "Bash", `{"command":"script -qcgh\\ issue\\ view\\ 1"}`, true},
		{"printf hex command word", "Bash", `{"command":"printf '\\x67h issue view 1' | sh"}`, true},
		{"printf %b echo octal", "Bash", `{"command":"printf '%b' 'gh auth status\\0012gh issue view 1' | sh"}`, true},
		{"ansi-c url", "Bash", `{"command":"client $'\\x68ttps://e'"}`, true},
		{"eval joined gh", "Bash", `{"command":"eval \"g''h issue view 1\""}`, true},
		{"exported script", "Bash", `{"command":"export X=\"g''h issue view 1\"; sh -c \"$X\""}`, true},
		{"for item script", "Bash", `{"command":"for c in \"g''h issue view 1\"; do sh -c \"$c\"; done"}`, true},
		{"quoted heredoc to sh", "Bash", `{"command":"sh <<'EOF'\ng''h issue view 1\nEOF"}`, true},
		{"joined gh in command substitution", "Bash", `{"command":"x=$(g''h issue view 1)"}`, true},
		{"unparsable with read", "Bash", `{"command":"gh issue view 1; )"}`, true},
		{"nested past depth bound", "Bash", nestedBash(9), true},
		{"eval quoted joined word", "Bash", `{"command":"eval \"g''h\" issue view 1"}`, true},
		{"eval escaped joined word", "Bash", `{"command":"eval g\\'\\'h issue view 1"}`, true},
		{"echo escaped joined word to sh", "Bash", `{"command":"echo g\\'\\'h issue view 1 | sh"}`, true},
		{"printf escaped joined word to sh", "Bash", `{"command":"printf \"%s \" g\\'\\'h issue view 1 | sh"}`, true},
		{"ssh quoted joined word", "Bash", `{"command":"ssh host \"g''h\" issue view 1"}`, true},
		{"watch escaped joined word", "Bash", `{"command":"watch -n1 g\\'\\'h issue view 1"}`, true},
		{"parallel escaped joined word", "Bash", `{"command":"parallel g\\'\\'h issue view 1 ::: 1"}`, true},
		{"glued value before syntax error", "Bash", `{"command":"env -Sgh issue view 1; echo ${x~~}"}`, true},
		{"fused script before syntax error", "Bash", `{"command":"script -qc'gh issue view 1' /dev/null; !"}`, true},
		{"printf escape before unclosed heredoc", "Bash", `{"command":"printf 'gh\\x20issue view 1' | sh\ncat <<EOF"}`, true},
		{"echo -e escape before stray fi", "Bash", `{"command":"echo -e 'gh\\tissue view 1' | sh\nfi"}`, true},
		{"ansi-c before syntax error", "Bash", `{"command":"$'\\x67h' issue view 1; echo ${x~~}"}`, true},
		{"ansi-c with arithmetic assignment", "Bash", `{"command":"$'\\x67h' issue view $((i=1))"}`, true},
		{"locale-empty with arithmetic increment", "Bash", `{"command":"g$\"\"h issue view $((i++))"}`, true},
		{"default value with arithmetic assignment", "Bash", `{"command":"${X:-gh} issue view 1 $((i=1))"}`, true},
		{"env arithmetic value before ansi-c", "Bash", `{"command":"env A=$((i=1)) $'\\x67h' issue view 1"}`, true},
		{"brace past expand limit before ansi-c", "Bash", `{"command":"env A{1..17000}=1 $'\\x67h' issue view 1"}`, true},
		{"IFS between words", "Bash", `{"command":"gh${IFS}issue${IFS}view${IFS}1"}`, true},
		{"IFS after fetcher", "Bash", `{"command":"curl${IFS}example.org"}`, true},
		{"bash -c IFS between words", "Bash", `{"command":"bash -c 'gh${IFS}issue${IFS}view${IFS}1'"}`, true},
		{"shell-set PATH alternate value", "Bash", `{"command":"${X:-g}${PATH:+h} issue view 1"}`, true},
		{"git alias quoted value", "Bash", `{"command":"git -c alias.v='!gh issue view 1' v"}`, true},
		{"git alias quoted option", "Bash", `{"command":"git -c 'alias.v=!gh issue view 1' v"}`, true},
		{"git config alias", "Bash", `{"command":"git config alias.v '!gh issue view 1'; git v"}`, true},
		{"git alias fetcher", "Bash", `{"command":"git -c alias.v='!curl x' v"}`, true},
		{"zsh path lookup", "Bash", `{"command":"=gh issue view 1"}`, true},
		{"GIT_PAGER fetcher", "Bash", `{"command":"GIT_PAGER=curl git log"}`, true},
		{"null command", "Bash", `{"command":null}`, true},
		{"null array element", "Bash", `{"command":["bash","-lc",null]}`, true},
		{"self-feeding assignment expansions", "Bash", commandInput(`: ${a:=xxxxxxxxxx}${b:=${a//x/$a}}${c:=${b//x/$b}}${d:=${c//x/$c}}${d//x/$d}; gh issue view 1`), true},
		{"nested HOME replacements", "Bash", commandInput(": " + nestedHome(14) + "; gh issue view 1"), true},
		{"default after subshell assignment", "Bash", commandInput(`( : ${X:=x} ); ${X:-gh} issue view 1`), true},
		{"default after untaken assignment", "Bash", commandInput(`false && : ${X:=x}; ${X:-gh} issue view 1`), true},
		{"default after function assignment", "Bash", commandInput(`f() { : ${X:=x}; }; ${X:-gh} issue view 1`), true},
		{"alternate value with arithmetic error", "Bash", commandInput(`gh${x:+$((1/0))} issue view 1`), true},
		{"ansi-c with transform operator", "Bash", commandInput(`$'\x67h'${x@A} issue view 1`), true},
		{"default before same-statement syntax error", "Bash", commandInput(`${X:-gh} issue view 1 ${x~~}`), true},
		{"assigned command word", "Bash", commandInput(`X=gh; $X issue view 1`), true},
		{"exported command word", "Bash", commandInput(`export X=gh; $X issue view 1`), true},
		{"empty slice of HOME", "Bash", commandInput(`${HOME:0:0}gh issue view 1`), true},
		{"assigned default joined to word", "Bash", commandInput(`: ${X:=g}; ${X}h issue view 1`), true},
		{"array run as command", "Bash", commandInput(`cmd=(gh issue view 1); "${cmd[@]}"`), true},
		{"array fetcher", "Bash", commandInput(`a=(curl -s example.com); "${a[@]}"`), true},
		{"declared array", "Bash", commandInput(`declare -a cmd=(gh pr diff 3); "${cmd[@]}"`), true},
		{"appended array", "Bash", commandInput(`cmd+=(gh api repos/x/y); "${cmd[@]}"`), true},
		{"array through eval", "Bash", commandInput(`cmd=(gh issue view 1); eval "${cmd[@]}"`), true},
		{"array star unquoted", "Bash", commandInput(`cmd=(gh issue view 1); ${cmd[*]}`), true},
		{"for list command word", "Bash", commandInput(`for c in x gh; do $c issue view 1; done`), true},
		{"for list fetcher", "Bash", commandInput(`for c in a curl; do $c -s example.com; done`), true},
		{"select list fetcher", "Bash", commandInput(`select c in a curl; do $c; done`), true},
		{"here-string read", "Bash", commandInput(`read X <<< gh; $X issue view 1`), true},
		{"here-string in command substitution", "Bash", commandInput(`X=$(cat <<< gh); $X issue view 1`), true},
		{"here-string mapfile", "Bash", commandInput(`mapfile -t a <<< gh; ${a[0]} issue view 1`), true},
		{"here-string into while read", "Bash", commandInput(`while read c; do $c issue view 1; done <<< gh`), true},
		{"here-string into xargs", "Bash", commandInput(`cat <<< gh | xargs -I{} {} issue view 1`), true},
		{"array script element here-string", "Bash", `{"command":["bash","-lc","read X <<< gh; $X issue view 1"]}`, true},
		{"here-string read with flag", "Bash", commandInput(`read X <<< "gh -R"; $X issue view 1`), true},
		{"here-string read with joined flag", "Bash", commandInput(`read X <<< 'gh --repo=o/r'; $X issue view 1`), true},
		{"here-string read with tab escape", "Bash", commandInput(`read X <<< $'gh\t-R'; $X issue view 1`), true},
		{"here-string while read with flag", "Bash", commandInput(`while read c; do $c issue view 1; done <<< "glab -R"`), true},
		{"here-string mapfile with flag", "Bash", commandInput(`mapfile -t a <<< "gh -R"; ${a[0]} issue view 1`), true},
		{"here-string substitution with flag", "Bash", commandInput(`X=$(cat <<< "gh -R"); $X issue view 1`), true},
		{"for item with joined flag", "Bash", commandInput(`for c in x 'gh --repo=o/r'; do $c issue view 1; done`), true},
		{"select item with flag", "Bash", commandInput(`select c in x "glab -R"; do $c issue view 1; done`), true},
		{"here-string read backslash", "Bash", commandInput(`read X <<< 'g\h'; $X issue view 1`), true},
		{"for item backslash through eval", "Bash", commandInput(`for c in x 'g\h'; do eval $c issue view 1; done`), true},
		{"here-string inner quotes through eval", "Bash", commandInput(`read X <<< '"gh"'; eval $X issue view 1`), true},
		{"here-string ansi quote through eval", "Bash", commandInput(`read -r X <<< "\$'gh'"; eval $X issue view 1`), true},
		{"for item inner quotes through eval", "Bash", commandInput(`for c in x "'glab'"; do eval $c issue view 1; done`), true},
		{"fuzz read -a quote join", "Bash", commandInput(`read -a X <<< g"lab -R"; ${X[0]} issue view 1`), true},
		{"fuzz read backslash", "Bash", commandInput(`read X <<< "g\h"; $X -s x`), true},
		{"fuzz read joined flag", "Bash", commandInput(`read X <<< "g\h --repo=o/r"; $X -s x`), true},
		{"fuzz mapfile escaped space eval", "Bash", commandInput(`mapfile -t X <<< g\lab\ --fill; eval $X issue view 1`), true},
		{"fuzz mapfile quote join", "Bash", commandInput(`mapfile -t X <<< g"h --repo=o/r"; $X issue view 1`), true},
		{"fuzz mapfile flag", "Bash", commandInput(`mapfile -t X <<< 'gh -s'; ${X[0]} issue view 1`), true},
		{"fuzz while backslash eval", "Bash", commandInput(`while read X; do eval $X issue view 1; done <<< 'g\h -R'`), true},
		{"fuzz while backslash array", "Bash", commandInput(`while read X; do "${X[@]}" issue view 1; done <<< 'g\lab'`), true},
		{"fuzz while flag", "Bash", commandInput(`while read X; do $X -s x; done <<< 'glab -R'`), true},
		{"fuzz for backslash eval", "Bash", commandInput(`for X in x "g\lab -R"; do eval $X issue view 1; done`), true},
		{"fuzz for escaped space", "Bash", commandInput(`for X in x g\h\ -s; do $X issue view 1; done`), true},
		{"fuzz for quote join", "Bash", commandInput(`for X in x y g"h --fill"; do $X -s x; done`), true},
		{"fuzz select escaped space", "Bash", commandInput(`select X in x g\h\ -s; do ${X[0]} issue view 1; break; done`), true},
		{"fuzz select quote join", "Bash", commandInput(`select X in x g"lab -R"; do ${X[0]} issue view 1; break; done`), true},
		{"fuzz select flag eval", "Bash", commandInput(`select X in x "glab -s"; do eval $X issue view 1; break; done`), true},
		{"default group before syntax error", "Bash", commandInput(`gh ${X:-issue} create ${x~~}`), true},

		// not ingestion
		{"git log", "Bash", `{"command":"git log"}`, false},
		{"git log oneline", "Bash", `{"command":"git log --oneline"}`, false},
		{"time rg", "Bash", `{"command":"time rg curl src/"}`, false},
		{"if grep", "Bash", `{"command":"if grep -q curl f; then echo y; fi"}`, false},
		{"xargs grep", "Bash", `{"command":"xargs grep curl"}`, false},
		{"which gh", "Bash", `{"command":"which gh"}`, false},
		{"ssh host gh pr create", "Bash", `{"command":"ssh host gh pr create --title t"}`, false},
		{"go test", "Bash", `{"command":"go test ./..."}`, false},
		{"git status", "Bash", `{"command":"git status"}`, false},
		{"rg curl", "Bash", `{"command":"rg curl src/"}`, false},
		{"grep gh", "Bash", `{"command":"grep -n gh notes.md"}`, false},
		{"gh pr create", "Bash", `{"command":"gh pr create --title t --body b"}`, false},
		{"sudo gh pr create", "Bash", `{"command":"sudo -u bob gh pr create --title t"}`, false},
		{"gh pr merge", "Bash", `{"command":"gh pr merge 3 --squash"}`, false},
		{"gh issue comment", "Bash", `{"command":"gh issue comment 1 --body b"}`, false},
		{"gh pr edit", "Bash", `{"command":"gh pr edit 2 --add-label x"}`, false},
		{"gh auth", "Bash", `{"command":"gh auth status"}`, false},
		{"gh config", "Bash", `{"command":"gh config get editor"}`, false},
		{"bare gh", "Bash", `{"command":"gh"}`, false},
		{"glab auth", "Bash", `{"command":"glab auth login"}`, false},
		{"echo curling", "Bash", `{"command":"echo curling"}`, false},
		{"ls ghost", "Bash", `{"command":"ls ghost"}`, false},
		{"gh quoted write args", "Bash", `{"command":"gh pr create --title \"fix it\" --body b"}`, false},
		{"env -S quoted assignment", "Bash", `{"command":"env -S 'FOO=1' git status"}`, false},
		{"gh write apostrophe", "Bash", `{"command":"gh pr comment 1 --body 'it'\"'\"'s done'"}`, false},
		{"rg --pre glued inert", "Bash", `{"command":"rg --pre=grep x"}`, false},
		{"env assignment bare gh", "Bash", `{"command":"GH_TOKEN=x gh"}`, false},
		{"grep -rn curl", "Bash", `{"command":"grep -rn curl ."}`, false},
		{"grep --color=auto curl", "Bash", `{"command":"grep --color=auto curl ."}`, false},
		{"rg quoted gh", "Bash", `{"command":"rg 'gh' docs"}`, false},
		{"commit message apostrophe", "Bash", `{"command":"git commit -m \"it's done\""}`, false},
		{"null command on read tool", "Read", `{"command":null}`, false},
		{"read tool", "Read", `{"file_path":"/x"}`, false},
		{"mcp tool", "mcp__x__y", `{}`, false},
		{"web fetch tool", "WebFetch", `{"url":"https://x"}`, false},
	}
}

func TestIngestsCall(t *testing.T) {
	for _, tt := range ingestsCallTests() {
		t.Run(tt.name, func(t *testing.T) {
			if got := IngestsCall(tt.tool, []byte(tt.input)); got != tt.want {
				t.Errorf("IngestsCall(%q, %s) = %v, want %v", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

// TestOverflagReason checks rule (b) on its own, for text the parser rejects.
func TestOverflagReason(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"glued value", "env -Sgh issue view 1; echo ${x~~}", true},
		{"fused script", "script -qc'gh issue view 1' /dev/null; !", true},
		{"printf hex escape", "printf 'gh\\x20issue view 1' | sh\ncat <<EOF", true},
		{"echo -e tab escape", "echo -e 'gh\\tissue view 1' | sh\nfi", true},
		{"listed name", "gh issue view 1; )", true},
		{"default group", "gh ${X:-issue} create ${x~~}", true},
		{"local group", "gh auth status; )", false},
		{"unlisted glued value", "env -S 'FOO=1' git status; )", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := overflagReason(tt.text); (got != "") != tt.want {
				t.Errorf("overflagReason(%q) = %q, want flagged %v", tt.text, got, tt.want)
			}
		})
	}
}
