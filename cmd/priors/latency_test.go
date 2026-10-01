package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	latencyRuns     = 30
	latencyBudget   = 800 * time.Millisecond
	latencyRepos    = 10
	latencyCommon   = "latency-common"
	latencySessRepo = "repo-3"
)

// TestLatency times search and the session_start handler against personal
// stores of growing size. The handlers print nothing once their deadline
// passes, so a run counts only if it printed the fact it should have.
func TestLatency(t *testing.T) {
	if os.Getenv("PRIORS_LATENCY") != "1" {
		t.Skip("set PRIORS_LATENCY=1 to run")
	}

	var table strings.Builder
	table.WriteString("\n| facts | search p50 | search p95 | index p50 | index p95 |\n|---|---|---|---|---|\n")
	failed := false
	for _, n := range []int{10, 100, 1000} {
		searchP50, searchP95, indexP50, indexP95 := measureLatency(t, n)
		fmt.Fprintf(&table, "| %d | %v | %v | %v | %v |\n", n, searchP50, searchP95, indexP50, indexP95)
		if searchP95 > latencyBudget || indexP95 > latencyBudget {
			failed = true
		}
	}
	t.Log(table.String())
	if failed {
		t.Errorf("a p95 exceeds %v", latencyBudget)
	}
}

func measureLatency(t *testing.T, n int) (searchP50, searchP95, indexP50, indexP95 time.Duration) {
	t.Helper()
	sb := newSandbox(t, "personal")
	repo := sb.repo("git@github.com:noamsto/" + latencySessRepo + ".git")

	base := time.Now().UTC().Add(-time.Duration(n+1) * time.Second)
	newest := -1
	for i := range n {
		repoDir := fmt.Sprintf("repo-%d", i%latencyRepos)
		f := newFact(fmt.Sprintf("fact-%d", i), repoDir, "project")
		f.Description = fmt.Sprintf("%s fact number %d", latencyCommon, i)
		f.Body = fmt.Sprintf("%s body with its own term unique-%d\n", latencyCommon, i)
		f.Metadata.Modified = base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		sb.putFact(sb.personal, repoDir+"/"+f.Name+".md", f)
		if repoDir == latencySessRepo {
			newest = i
		}
	}
	for i := range 5 {
		f := newFact(fmt.Sprintf("global-%d", i), "", "user")
		f.Metadata.Scope = "global"
		f.Metadata.Repos = nil
		sb.putFact(sb.personal, fmt.Sprintf("_global/%s.md", f.Name), f)
	}
	sb.indexWrite()
	if newest < 0 {
		t.Fatalf("no fact landed in %s", latencySessRepo)
	}

	wantName := fmt.Sprintf("fact-%d", newest)
	searchArgs := []string{"search", latencyCommon, fmt.Sprintf("unique-%d", newest), "--cwd", repo}
	envelope := sessionStartEnvelope(repo)

	searchTimes := make([]time.Duration, latencyRuns)
	indexTimes := make([]time.Duration, latencyRuns)
	for i := range latencyRuns {
		start := time.Now()
		res := sb.run("", searchArgs...)
		searchTimes[i] = time.Since(start)
		if res.code != 0 || !strings.Contains(res.stdout, " — "+wantName+" — ") {
			t.Fatalf("n=%d search run %d: exit %d, stdout %q, stderr %q", n, i, res.code, res.stdout, res.stderr)
		}

		start = time.Now()
		res = sb.run(envelope, "index")
		indexTimes[i] = time.Since(start)
		if res.code != 0 || res.stdout == "" {
			t.Fatalf("n=%d index run %d: exit %d, stdout %q, stderr %q", n, i, res.code, res.stdout, res.stderr)
		}
		if text := additionalContext(t, res.stdout); !strings.Contains(text, "["+wantName+"]") {
			t.Fatalf("n=%d index run %d: no %s in:\n%s", n, i, wantName, text)
		}
	}
	searchP50, searchP95 = percentiles(searchTimes)
	indexP50, indexP95 = percentiles(indexTimes)
	return searchP50, searchP95, indexP50, indexP95
}

func percentiles(d []time.Duration) (p50, p95 time.Duration) {
	sorted := slices.Sorted(slices.Values(d))
	rank := func(p int) time.Duration { return sorted[(len(sorted)*p+99)/100-1] }
	return rank(50).Round(time.Millisecond), rank(95).Round(time.Millisecond)
}
