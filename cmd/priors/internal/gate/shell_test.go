package gate

import (
	"slices"
	"testing"
)

func TestIngestsCall(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  bool
	}{
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
		// design, since every quoted string is judged as a script.
		{"bash -c two quoted scripts", "Bash", `{"command":"bash -c 'gh auth status' \"g''h issue view 1\""}`, true},
		{"nested quoted joined gh", "Bash", `{"command":"bash -c \"sh -c 'gh auth status' \\\"g''h issue view 1\\\"\""}`, true},
		{"escaped quote before quoted scripts", "Bash", `{"command":"echo \\\" 'gh auth status' \"g''h issue view 1\""}`, true},
		{"single-quoted joined gh", "Bash", `{"command":"printf '%s\\n' 'gh auth status' 'g\"\"h issue view 1' | sh"}`, true},

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
		{"read tool", "Read", `{"file_path":"/x"}`, false},
		{"mcp tool", "mcp__x__y", `{}`, false},
		{"web fetch tool", "WebFetch", `{"url":"https://x"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IngestsCall(tt.tool, []byte(tt.input)); got != tt.want {
				t.Errorf("IngestsCall(%q, %s) = %v, want %v", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

func TestQuotedStrings(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"two strings", `'a' "b"`, []string{"a", "b"}},
		{"escaped single quote", `\'a`, nil},
		{"escaped double quote", `"a\"b"`, []string{`a"b`}},
		{"other backslash kept", `"a\xb"`, []string{`a\xb`}},
		{"unterminated", `'unterminated`, []string{"unterminated"}},
		{"single quotes inside double", `"x'y'z"`, []string{"x'y'z"}},
		{"backslash literal in single", `'a\'`, []string{`a\`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotedStrings(tt.text); !slices.Equal(got, tt.want) {
				t.Errorf("quotedStrings(%s) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}
