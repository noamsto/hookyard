package gate

import "testing"

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

		// not ingestion
		{"go test", "Bash", `{"command":"go test ./..."}`, false},
		{"git status", "Bash", `{"command":"git status"}`, false},
		{"rg curl", "Bash", `{"command":"rg curl src/"}`, false},
		{"grep gh", "Bash", `{"command":"grep -n gh notes.md"}`, false},
		{"gh pr create", "Bash", `{"command":"gh pr create --title t --body b"}`, false},
		{"gh pr merge", "Bash", `{"command":"gh pr merge 3 --squash"}`, false},
		{"gh issue comment", "Bash", `{"command":"gh issue comment 1 --body b"}`, false},
		{"gh pr edit", "Bash", `{"command":"gh pr edit 2 --add-label x"}`, false},
		{"gh auth", "Bash", `{"command":"gh auth status"}`, false},
		{"gh config", "Bash", `{"command":"gh config get editor"}`, false},
		{"bare gh", "Bash", `{"command":"gh"}`, false},
		{"glab auth", "Bash", `{"command":"glab auth login"}`, false},
		{"echo curling", "Bash", `{"command":"echo curling"}`, false},
		{"ls ghost", "Bash", `{"command":"ls ghost"}`, false},
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
