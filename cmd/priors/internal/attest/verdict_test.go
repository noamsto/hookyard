package attest

import (
	"io/fs"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	testOwner       = 993
	testStoreDir    = "/var/lib/priors/" + testStore
	testVerdictFile = testStoreDir + "/verdicts"
	testTrustDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTip         = "0123456789abcdef0123456789abcdef01234567"
	testFetched     = 1790000000

	digestA = "1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "2222222222222222222222222222222222222222222222222222222222222222"
	digestC = "3333333333333333333333333333333333333333333333333333333333333333"
	digestD = "4444444444444444444444444444444444444444444444444444444444444444"
)

// verdictHeader is a valid header for testStore, one line per element.
func verdictHeader() []string {
	return []string{
		"priors-verdicts v1",
		"store: " + testStore,
		"trust: " + testTrustDigest,
		"tip: " + testTip,
		"fetched: 1790000000",
		"status: ok",
	}
}

// verdictLines joins lines, each "\n"-terminated.
func verdictLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func verdictFile(body ...string) string {
	return verdictLines(append(verdictHeader(), body...)...)
}

func withHeaderLine(i int, line string) string {
	h := verdictHeader()
	h[i] = line
	return verdictLines(h...)
}

func TestParseVerdictsCanonical(t *testing.T) {
	raw := verdictFile(
		digestA+" reviewed",
		digestB+" revoked",
		digestC+" lapsed",
		digestD+" superseded 9223372036854775807",
	)

	v, err := ParseVerdicts([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}

	if v.Store != testStore || v.Trust != testTrustDigest || v.Tip != testTip || v.Status != "ok" || !v.Fetched.Equal(time.Unix(testFetched, 0)) {
		t.Errorf("header = %+v", v)
	}
	wantStates := map[string]State{digestA: StateReviewed, digestB: StateRevoked, digestC: StateLapsed, digestD: StateSuperseded}
	if !maps.Equal(v.States, wantStates) {
		t.Errorf("States = %v, want %v", v.States, wantStates)
	}
	wantSuperseded := map[string]uint64{digestD: 1<<63 - 1}
	if !maps.Equal(v.Superseded, wantSuperseded) {
		t.Errorf("Superseded = %v, want %v", v.Superseded, wantSuperseded)
	}
}

func TestParseVerdictsEmptyBody(t *testing.T) {
	for _, status := range []string{"ok", "behind", "oversize", "stale-remote"} {
		t.Run(status, func(t *testing.T) {
			v, err := ParseVerdicts([]byte(withHeaderLine(5, "status: "+status)))
			if err != nil {
				t.Fatal(err)
			}
			if v.Status != status || len(v.States) != 0 || len(v.Superseded) != 0 {
				t.Errorf("parsed %+v", v)
			}
		})
	}
}

func TestParseVerdictsHeaderValues(t *testing.T) {
	cases := map[string]string{
		"fetched zero":   withHeaderLine(4, "fetched: 0"),
		"sha256 tip":     withHeaderLine(3, "tip: "+digestA),
		"64-char store":  withHeaderLine(1, "store: "+strings.Repeat("a", 64)),
		"dashed store":   withHeaderLine(1, "store: a-1-b"),
		"stale-remote":   withHeaderLine(5, "status: stale-remote"),
		"body any order": verdictFile(digestD+" superseded 1", digestA+" reviewed"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseVerdicts([]byte(raw)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseVerdictsMalformed(t *testing.T) {
	h := verdictHeader()
	cases := []struct {
		name string
		raw  string
		line int
	}{
		{"empty file", "", 1},
		{"bad magic", withHeaderLine(0, "priors-verdicts v2"), 1},
		{"header out of order", verdictLines(h[0], h[2], h[1], h[3], h[4], h[5]), 2},
		{"missing header line", verdictLines(h[0], h[1], h[2], h[4], h[5]), 4},
		{"truncated header", verdictLines(h[:5]...), 6},
		{"uppercase store", withHeaderLine(1, "store: Store-1"), 2},
		{"long store", withHeaderLine(1, "store: "+strings.Repeat("a", 65)), 2},
		{"short trust", withHeaderLine(2, "trust: "+testTrustDigest[1:]), 3},
		{"bad tip length", withHeaderLine(3, "tip: "+testTip[1:]), 4},
		{"tip between lengths", withHeaderLine(3, "tip: "+digestA[:50]), 4},
		{"fetched leading zero", withHeaderLine(4, "fetched: 01790000000"), 5},
		{"fetched signed", withHeaderLine(4, "fetched: +1790000000"), 5},
		{"fetched empty", withHeaderLine(4, "fetched: "), 5},
		{"bad status", withHeaderLine(5, "status: fine"), 6},
		{"header extra field", withHeaderLine(5, "status: ok ok"), 6},
		{"blank line", verdictFile("", digestA+" reviewed"), 7},
		{"trailing blank line", verdictFile(digestA+" reviewed", ""), 8},
		{"unknown state", verdictFile(digestA + " approved"), 7},
		{"uppercase digest", verdictFile(strings.ToUpper("abcdef"+digestA[6:]) + " reviewed"), 7},
		{"short digest", verdictFile(digestA[1:] + " reviewed"), 7},
		{"double space", verdictFile(digestA + "  reviewed"), 7},
		{"duplicate digest same state", verdictFile(digestA+" reviewed", digestB+" lapsed", digestA+" reviewed"), 9},
		{"duplicate digest different state", verdictFile(digestA+" reviewed", digestA+" revoked"), 8},
		{"duplicate superseded", verdictFile(digestD+" superseded 3", digestD+" superseded 4"), 8},
		{"superseded without sequence", verdictFile(digestD + " superseded"), 7},
		{"superseded zero", verdictFile(digestD + " superseded 0"), 7},
		{"superseded leading zero", verdictFile(digestD + " superseded 07"), 7},
		{"superseded 2^63", verdictFile(digestD + " superseded 9223372036854775808"), 7},
		{"reviewed with extra field", verdictFile(digestA + " reviewed 3"), 7},
		{"no trailing newline", strings.TrimSuffix(verdictFile(digestA+" reviewed"), "\n"), 7},
		{"header without trailing newline", strings.TrimSuffix(verdictFile(), "\n"), 6},
		{"crlf", strings.ReplaceAll(verdictFile(digestA+" reviewed"), "\n", "\r\n"), 1},
		{"cr in body", verdictFile(digestA + " reviewed\r"), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseVerdicts([]byte(tc.raw))

			want := "verdict file malformed: line " + strconv.Itoa(tc.line)
			if err == nil || err.Error() != want {
				t.Errorf("err = %v, want %q", err, want)
			}
		})
	}
}

type ownedEntry struct {
	uid    uint32
	mode   fs.FileMode
	target string
	data   string
	// size, when set, is what Lstat reports in place of len(data).
	size int64
}

// ownedFS is a config.StatFS over a map of absolute paths, so ownership can
// be set without root.
type ownedFS map[string]ownedEntry

func (f ownedFS) Lstat(name string) (fs.FileInfo, error) {
	e, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
	}
	return ownedInfo{name: filepath.Base(name), e: e}, nil
}

func (f ownedFS) Readlink(name string) (string, error) {
	e, ok := f[name]
	if !ok || e.mode&fs.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
	}
	return e.target, nil
}

func (f ownedFS) ReadFile(name string) ([]byte, error) {
	e, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(e.data), nil
}

type ownedInfo struct {
	name string
	e    ownedEntry
}

func (i ownedInfo) Name() string { return i.name }
func (i ownedInfo) Size() int64 {
	if i.e.size != 0 {
		return i.e.size
	}
	return int64(len(i.e.data))
}
func (i ownedInfo) Mode() fs.FileMode  { return i.e.mode }
func (i ownedInfo) ModTime() time.Time { return time.Time{} }
func (i ownedInfo) IsDir() bool        { return i.e.mode.IsDir() }
func (i ownedInfo) Sys() any           { return &syscall.Stat_t{Uid: i.e.uid} }

// verdictFS holds a valid verdict file at testVerdictFile, under root-owned
// 0755 dirs and a store dir owned by testOwner.
func verdictFS() ownedFS {
	return ownedFS{
		"/":               {mode: fs.ModeDir | 0o755},
		"/var":            {mode: fs.ModeDir | 0o755},
		"/var/lib":        {mode: fs.ModeDir | 0o755},
		"/var/lib/priors": {mode: fs.ModeDir | 0o755},
		testStoreDir:      {uid: testOwner, mode: fs.ModeDir | 0o755},
		testVerdictFile:   {uid: testOwner, mode: 0o644, data: verdictFile(digestA + " reviewed")},
	}
}

func withVerdictData(f ownedFS, data string) {
	e := f[testVerdictFile]
	e.data = data
	f[testVerdictFile] = e
}

func TestReadVerdicts(t *testing.T) {
	fetched := time.Unix(testFetched, 0)
	cases := []struct {
		name string
		edit func(f ownedFS)
		now  time.Time
		// want is the exact error, or with prefix its prefix and substring;
		// empty for a pass.
		want   string
		prefix bool
	}{
		{name: "pass", now: fetched.Add(time.Hour)},
		{name: "fetched now", now: fetched},
		{name: "fetched 24h less a second ago", now: fetched.Add(24*time.Hour - time.Second)},
		{name: "fetched 24h ago", now: fetched.Add(24 * time.Hour), want: "verdicts stale: fetched 24h0m0s ago"},
		{name: "fetched in the future", now: fetched.Add(-time.Second), want: "verdict file fetched in the future"},
		{name: "store mismatch", edit: func(f ownedFS) {
			withVerdictData(f, withHeaderLine(1, "store: other-store"))
		}, want: "verdict file names store other-store"},
		{name: "trust mismatch", edit: func(f ownedFS) {
			withVerdictData(f, withHeaderLine(2, "trust: "+digestA))
		}, want: "verdict file is for another trust file"},
		{name: "malformed", edit: func(f ownedFS) {
			withVerdictData(f, verdictFile(digestA+" approved"))
		}, want: "verdict file malformed: line 7"},
		{name: "at 1 MiB", edit: func(f ownedFS) {
			e := f[testVerdictFile]
			e.size = MaxVerdictBytes
			f[testVerdictFile] = e
		}},
		{name: "over 1 MiB by Lstat", edit: func(f ownedFS) {
			e := f[testVerdictFile]
			e.size = MaxVerdictBytes + 1
			f[testVerdictFile] = e
		}, want: "verdict file over 1 MiB"},
		{name: "grew over 1 MiB after Lstat", edit: func(f ownedFS) {
			e := f[testVerdictFile]
			e.size = 100
			e.data = verdictFile(digestA+" reviewed") + strings.Repeat("x", MaxVerdictBytes)
			f[testVerdictFile] = e
		}, want: "verdict file over 1 MiB"},
		{name: "missing file", edit: func(f ownedFS) {
			delete(f, testVerdictFile)
		}, want: "no verdict file"},
		{name: "missing store dir", edit: func(f ownedFS) {
			delete(f, testStoreDir)
			delete(f, testVerdictFile)
		}, want: "no verdict file"},
		{name: "file owned by another uid", edit: func(f ownedFS) {
			f[testVerdictFile] = ownedEntry{uid: 1000, mode: 0o644, data: verdictFile()}
		}, want: testVerdictFile + " is owned by uid 1000, not uid 993", prefix: true},
		{name: "root-owned file", edit: func(f ownedFS) {
			f[testVerdictFile] = ownedEntry{mode: 0o644, data: verdictFile()}
		}, want: testVerdictFile + " is owned by uid 0, not uid 993", prefix: true},
		{name: "group-writable file", edit: func(f ownedFS) {
			f[testVerdictFile] = ownedEntry{uid: testOwner, mode: 0o664, data: verdictFile()}
		}, want: testVerdictFile + " is writable by group or others", prefix: true},
		{name: "world-writable file", edit: func(f ownedFS) {
			f[testVerdictFile] = ownedEntry{uid: testOwner, mode: 0o646, data: verdictFile()}
		}, want: testVerdictFile + " is writable by group or others", prefix: true},
		{name: "root-owned store dir", edit: func(f ownedFS) {
			f[testStoreDir] = ownedEntry{mode: fs.ModeDir | 0o755}
		}, want: testStoreDir + " is owned by uid 0, not uid 993", prefix: true},
		{name: "group-writable store dir", edit: func(f ownedFS) {
			f[testStoreDir] = ownedEntry{uid: testOwner, mode: fs.ModeDir | 0o775}
		}, want: testStoreDir + " is writable by group or others", prefix: true},
		{name: "verdict root owned by the owner", edit: func(f ownedFS) {
			f["/var/lib/priors"] = ownedEntry{uid: testOwner, mode: fs.ModeDir | 0o755}
		}, want: "/var/lib/priors is owned by uid 993, not root", prefix: true},
		{name: "world-writable non-sticky verdict root", edit: func(f ownedFS) {
			f["/var/lib/priors"] = ownedEntry{mode: fs.ModeDir | 0o777}
		}, want: "/var/lib/priors is writable by group or others", prefix: true},
		{name: "user-owned symlink above", edit: func(f ownedFS) {
			f["/srv"] = ownedEntry{mode: fs.ModeDir | 0o755}
			f["/srv/priors"] = ownedEntry{mode: fs.ModeDir | 0o755}
			f["/srv/priors/"+testStore] = f[testStoreDir]
			f["/srv/priors/"+testStore+"/verdicts"] = f[testVerdictFile]
			f["/var/lib/priors"] = ownedEntry{uid: 1000, mode: fs.ModeSymlink | 0o777, target: "/srv/priors"}
		}, want: "/var/lib/priors is owned by uid 1000, not root", prefix: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := verdictFS()
			if tc.edit != nil {
				tc.edit(f)
			}
			now := tc.now
			if now.IsZero() {
				now = fetched.Add(time.Hour)
			}

			v, err := ReadVerdicts(f, testVerdictFile, testStore, testTrustDigest, testOwner, now)

			switch {
			case tc.want == "":
				if err != nil {
					t.Fatal(err)
				}
				if v.Store != testStore || v.States[digestA] != StateReviewed {
					t.Errorf("parsed %+v", v)
				}
			case tc.prefix:
				if err == nil || !strings.HasPrefix(err.Error(), "verdict file: ") || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want \"verdict file: ...%s\"", err, tc.want)
				}
			default:
				if err == nil || err.Error() != tc.want {
					t.Errorf("err = %v, want %q", err, tc.want)
				}
			}
		})
	}
}
