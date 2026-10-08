package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const pinnedFactName = "pinned-fact"

// pinnedStore is a personal checkout whose trust entry pins a bare remote.
type pinnedStore struct {
	// bare is the pinned remote's directory and url the pin itself.
	bare, url string
}

// pinPersonal pins the personal store to a fresh bare remote and publishes one
// fact there. The pin is a file:// URL because git ignores a [remote "/path"]
// section for a plain path, which would make the hijack vectors inert.
func (sb *sandbox) pinPersonal() pinnedStore {
	sb.t.Helper()
	bare := filepath.Join(sb.dir, "remote.git")
	sb.git(sb.dir, "init", "--bare", "-q", bare)
	pin := pinnedStore{bare: bare, url: "file://" + bare}
	sb.personalRemote = pin.url
	sb.writeTrust()
	sb.git(sb.personal, "remote", "add", "origin", pin.url)
	f := newFact(pinnedFactName, "demo", "project")
	f.Body = "the okapi crossing rule\n"
	sb.putFact(sb.personal, "demo/"+pinnedFactName+".md", f)
	sb.indexWrite()
	sb.git(sb.personal, "push", "-q", "-u", "origin", "main")
	if got, want := sb.ref(bare, "main"), sb.head(sb.personal); got != want {
		sb.t.Fatalf("pinned bare main = %s, want %s", got, want)
	}
	return pin
}

// decoy is a bare clone of bare, so a push that followed a redirect to it
// would land.
func (sb *sandbox) decoy(bare string) string {
	sb.t.Helper()
	decoy := filepath.Join(sb.dir, "decoy.git")
	sb.git(sb.dir, "clone", "--bare", "-q", bare, decoy)
	return decoy
}

func (sb *sandbox) ref(dir, name string) string {
	sb.t.Helper()
	return strings.TrimSpace(sb.gitOut(dir, "rev-parse", name))
}

func (sb *sandbox) readFile(path string) string {
	sb.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		sb.t.Fatal(err)
	}
	return string(data)
}

// injected is the text session_start injects; a refusal may leave nothing.
func injected(t *testing.T, stdout string) string {
	t.Helper()
	if stdout == "" {
		return ""
	}
	return additionalContext(t, stdout)
}

func TestUserConfigCannotSetStorePaths(t *testing.T) {
	for _, key := range []string{"personal_store", "work_store"} {
		t.Run(key, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			elsewhere := filepath.Join(sb.dir, "elsewhere")
			sb.mkdir(elsewhere)
			sb.writeConfig(fmt.Sprintf("%s = %q", key, elsewhere))
			want := key + " is read from the trust file"

			res := sb.run("", "index", "--write")
			if res.code == 0 {
				t.Fatalf("index --write succeeded\nstderr: %s", res.stderr)
			}
			wantContains(t, "index --write stderr", res.stderr, want)

			res = sb.run(sessionStartEnvelope(repo))
			wantExit(t, res, 0)
			wantContains(t, "session_start stderr", res.stderr, want)
			if res.stdout != "" {
				t.Errorf("session_start stdout %q, want none", res.stdout)
			}

			for _, dir := range []string{sb.personal, sb.work, elsewhere, sb.state} {
				if files := filesUnder(t, dir); len(files) != 0 {
					t.Errorf("files under %s: %v, want none", dir, files)
				}
			}
			if h := sb.head(sb.personal); h != "" {
				t.Errorf("personal HEAD = %s, want no commit", h)
			}
		})
	}
}

func TestRepointedStoreFailsClosed(t *testing.T) {
	vectors := []struct {
		name string
		// why is what the refusal names.
		why   string
		apply func(sb *sandbox, pin pinnedStore, decoyURL string)
	}{
		{"origin url", "not the pinned", func(sb *sandbox, _ pinnedStore, decoyURL string) {
			sb.git(sb.personal, "remote", "set-url", "origin", decoyURL)
		}},
		{"insteadOf", "not the pinned", func(sb *sandbox, pin pinnedStore, decoyURL string) {
			sb.git(sb.personal, "config", "url."+decoyURL+".insteadOf", pin.url)
		}},
		{"pushInsteadOf", "not the pinned", func(sb *sandbox, pin pinnedStore, decoyURL string) {
			sb.git(sb.personal, "config", "url."+decoyURL+".pushInsteadOf", pin.url)
		}},
		{"pushurl", "not the pinned", func(sb *sandbox, _ pinnedStore, decoyURL string) {
			sb.git(sb.personal, "config", "remote.origin.pushurl", decoyURL)
		}},
		{"global insteadOf", "not the pinned", func(sb *sandbox, pin pinnedStore, decoyURL string) {
			sb.writeFile(sb.gitConfig, sb.readFile(sb.gitConfig)+fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", decoyURL, pin.url))
		}},
		{"branch remote is a url", "branch.main.remote is file://", func(sb *sandbox, _ pinnedStore, decoyURL string) {
			sb.git(sb.personal, "config", "branch.main.remote", decoyURL)
		}},
		{"extra remote", "has remote(s) origin, other; the trust file pins origin alone", func(sb *sandbox, _ pinnedStore, decoyURL string) {
			sb.git(sb.personal, "remote", "add", "other", decoyURL)
		}},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			sb.record("sess-1")
			pin := sb.pinPersonal()
			decoy := sb.decoy(pin.bare)
			sb.writeConfig("push = true")
			head := sb.head(sb.personal)
			pinnedMain, decoyMain := sb.ref(pin.bare, "main"), sb.ref(decoy, "main")

			control := sb.run("", "search", "okapi", "--cwd", repo)
			wantExit(t, control, 0)
			wantContains(t, "unrepointed search", control.stdout, pinnedFactName)
			wantContains(t, "unrepointed session_start", injected(t, sb.run(sessionStartEnvelope(repo)).stdout), pinnedFactName)

			v.apply(sb, pin, "file://"+decoy)
			before := filesUnder(t, sb.personal)

			res := sb.run("", "search", "okapi", "--cwd", repo)
			wantExit(t, res, 0)
			if strings.Contains(res.stdout, pinnedFactName) {
				t.Errorf("search shows the fact of a repointed checkout:\n%s", res.stdout)
			}
			wantContains(t, "search stderr", res.stderr, "refused")

			res = sb.run(sessionStartEnvelope(repo))
			wantExit(t, res, 0)
			if got := injected(t, res.stdout); strings.Contains(got, pinnedFactName) {
				t.Errorf("session_start injects the fact of a repointed checkout:\n%s", got)
			}
			wantContains(t, "session_start stderr", res.stderr, "personal store:", v.why)

			for _, c := range []struct {
				args    []string
				refusal string
			}{
				{[]string{"index", "--write"}, "refused: personal store: "},
				{[]string{"lint"}, "personal store: "},
				{[]string{"list", "--cwd", repo}, "refused: personal store: "},
				{[]string{"add", "--name", "new-fact", "--description", "d", "--type", "project", "--cwd", repo, "--session", "sess-1"}, "refused: personal store: "},
			} {
				res := sb.run("", c.args...)
				if res.code != 1 {
					t.Errorf("%s: exit %d, want 1\nstdout: %s\nstderr: %s", c.args[0], res.code, res.stdout, res.stderr)
				}
				wantContains(t, c.args[0]+" stderr", res.stderr, c.refusal, v.why)
				if after := filesUnder(t, sb.personal); !slices.Equal(after, before) {
					t.Errorf("%s changed the files under the store: %v, was %v", c.args[0], after, before)
				}
				if got := sb.head(sb.personal); got != head {
					t.Errorf("%s moved personal HEAD to %s, was %s", c.args[0], got, head)
				}
			}
			if got := sb.ref(pin.bare, "main"); got != pinnedMain {
				t.Errorf("pinned bare main moved to %s, was %s", got, pinnedMain)
			}
			if got := sb.ref(decoy, "main"); got != decoyMain {
				t.Errorf("decoy main moved to %s, was %s", got, decoyMain)
			}
		})
	}
}

func TestPinnedPushEndToEnd(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	pin := sb.pinPersonal()
	decoy := sb.decoy(pin.bare)
	decoyMain := sb.ref(decoy, "main")
	sb.writeConfig("push = true")

	addArgs := func(name string) []string {
		return []string{"add", "--name", name, "--description", "d", "--type", "project", "--cwd", repo, "--session", "sess-1"}
	}
	add := func(name string) {
		t.Helper()
		res := sb.run("", addArgs(name)...)
		wantExit(t, res, 0)
		wantContains(t, "add stdout", res.stdout, "published personal "+filepath.Join(sb.personal, "demo", name+".md"))
		if strings.Contains(res.stderr, "not pushed") {
			t.Fatalf("add did not push: %s", res.stderr)
		}
		head := sb.head(sb.personal)
		if got := sb.ref(pin.bare, "main"); got != head {
			t.Fatalf("after adding %s the pinned bare main is %s, want HEAD %s\nstderr: %s", name, got, head, res.stderr)
		}
	}
	add("first-fact")
	add("second-fact")

	// A [remote "<pin>"] section would redirect a push run in the checkout;
	// it is a second remote, so the store is refused before anything is written.
	sb.git(sb.personal, "config", "remote."+pin.url+".url", "file://"+decoy)
	if raw := sb.readFile(filepath.Join(sb.personal, ".git", "config")); !strings.Contains(raw, "file://"+decoy) {
		t.Fatalf("the hijack is not in the checkout's config:\n%s", raw)
	}
	head, pinnedMain := sb.head(sb.personal), sb.ref(pin.bare, "main")
	res := sb.run("", addArgs("third-fact")...)
	wantExit(t, res, 1)
	wantContains(t, "add stderr", res.stderr, "refused: personal store: ", "has remote(s) "+pin.url+", origin")
	if _, err := os.Stat(filepath.Join(sb.personal, "demo", "third-fact.md")); err == nil {
		t.Error("the refused add wrote its fact into the store")
	}
	if got := sb.head(sb.personal); got != head {
		t.Errorf("personal HEAD moved to %s, was %s", got, head)
	}
	if got := sb.ref(pin.bare, "main"); got != pinnedMain {
		t.Errorf("pinned bare main moved to %s, was %s", got, pinnedMain)
	}
	if got := sb.ref(decoy, "main"); got != decoyMain {
		t.Errorf("decoy main moved to %s, was %s", got, decoyMain)
	}
}
