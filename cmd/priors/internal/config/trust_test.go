package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeEntry struct {
	uid    uint32
	mode   fs.FileMode
	target string
	data   string
}

// fakeFS is a StatFS over a map of absolute paths, so ownership can be set
// without root.
type fakeFS map[string]fakeEntry

func (f fakeFS) Lstat(name string) (fs.FileInfo, error) {
	e, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: filepath.Base(name), e: e}, nil
}

func (f fakeFS) Readlink(name string) (string, error) {
	e, ok := f[name]
	if !ok || e.mode&fs.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
	}
	return e.target, nil
}

func (f fakeFS) ReadFile(name string) ([]byte, error) {
	e, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(e.data), nil
}

type fakeInfo struct {
	name string
	e    fakeEntry
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return int64(len(i.e.data)) }
func (i fakeInfo) Mode() fs.FileMode  { return i.e.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.e.mode.IsDir() }
func (i fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: i.e.uid} }

func dir(perm fs.FileMode) fakeEntry { return fakeEntry{mode: fs.ModeDir | perm} }
func link(target string) fakeEntry   { return fakeEntry{mode: fs.ModeSymlink | 0o777, target: target} }
func file(data string) fakeEntry     { return fakeEntry{mode: 0o644, data: data} }

const validTrust = `
profile       = "work"
work_orgs     = ["github.com/Factify-Inc"]
personal_orgs = ["GitHub.com/noamsto"]
trust_root    = "owner-admin"

[stores.personal]
id   = "noamsto-priors"
path = "/m/personal"

[stores.work]
id   = "factify-priors"
path = "/m/work"
`

const etcTrust = "/etc/priors/trust.toml"

// etcFS holds a valid trust file at etcTrust under root-owned 0755 dirs.
func etcFS() fakeFS {
	return fakeFS{
		"/":           dir(0o755),
		"/etc":        dir(0o755),
		"/etc/priors": dir(0o755),
		etcTrust:      file(validTrust),
	}
}

func TestCheckTrustPath(t *testing.T) {
	cases := []struct {
		name string
		edit func(f fakeFS)
		// want is the resolved path, or with wantErr the error's substring.
		want    string
		wantErr bool
	}{
		{name: "root-owned file", want: etcTrust},
		{name: "user-owned file", edit: func(f fakeFS) {
			e := f[etcTrust]
			e.uid = 1000
			f[etcTrust] = e
		}, want: etcTrust + " is owned by uid 1000, not root", wantErr: true},
		{name: "user-owned directory", edit: func(f fakeFS) {
			f["/etc/priors"] = fakeEntry{uid: 1000, mode: fs.ModeDir | 0o755}
		}, want: "/etc/priors is owned by uid 1000, not root", wantErr: true},
		{name: "user-owned root", edit: func(f fakeFS) {
			f["/"] = fakeEntry{uid: 65534, mode: fs.ModeDir | 0o755}
		}, want: "/ is owned by uid 65534, not root", wantErr: true},
		{name: "user-owned symlink", edit: func(f fakeFS) {
			f["/etc/real.toml"] = file(validTrust)
			f[etcTrust] = fakeEntry{uid: 1000, mode: fs.ModeSymlink | 0o777, target: "/etc/real.toml"}
		}, want: etcTrust + " is owned by uid 1000, not root", wantErr: true},
		{name: "symlink into a user-owned directory", edit: func(f fakeFS) {
			f["/home"] = dir(0o755)
			f["/home/u"] = fakeEntry{uid: 1000, mode: fs.ModeDir | 0o700}
			f["/home/u/trust.toml"] = file(validTrust)
			f[etcTrust] = link("/home/u/trust.toml")
		}, want: "/home/u is owned by uid 1000, not root", wantErr: true},
		{name: "group-writable file", edit: func(f fakeFS) {
			f[etcTrust] = fakeEntry{mode: 0o664, data: validTrust}
		}, want: etcTrust + " is writable by group or others", wantErr: true},
		{name: "world-writable file", edit: func(f fakeFS) {
			f[etcTrust] = fakeEntry{mode: 0o646, data: validTrust}
		}, want: etcTrust + " is writable by group or others", wantErr: true},
		{name: "group-writable directory", edit: func(f fakeFS) {
			f["/etc/priors"] = dir(0o775)
		}, want: "/etc/priors is writable by group or others", wantErr: true},
		{name: "world-writable directory", edit: func(f fakeFS) {
			f["/etc/priors"] = dir(0o777)
		}, want: "/etc/priors is writable by group or others", wantErr: true},
		{name: "world-writable root", edit: func(f fakeFS) {
			f["/"] = dir(0o777)
		}, want: "/ is writable by group or others", wantErr: true},
		{name: "sticky group-writable directory", edit: func(f fakeFS) {
			f["/etc/priors"] = dir(fs.ModeSticky | 0o775)
		}, want: etcTrust},
		{name: "sticky world-writable directory", edit: func(f fakeFS) {
			f["/etc/priors"] = dir(fs.ModeSticky | 0o777)
		}, want: etcTrust},
		{name: "NixOS etc chain", edit: func(f fakeFS) {
			f[etcTrust] = link("/etc/static/priors/trust.toml")
			f["/etc/static"] = link("/nix/store/x-etc/etc")
			f["/nix"] = dir(0o755)
			f["/nix/store"] = dir(fs.ModeSticky | 0o775)
			f["/nix/store/x-etc"] = dir(0o555)
			f["/nix/store/x-etc/etc"] = dir(0o555)
			f["/nix/store/x-etc/etc/priors"] = dir(0o555)
			f["/nix/store/x-etc/etc/priors/trust.toml"] = link("../../../y-trust.toml")
			f["/nix/store/y-trust.toml"] = fakeEntry{mode: 0o444, data: validTrust}
		}, want: "/nix/store/y-trust.toml"},
		{name: "relative symlink", edit: func(f fakeFS) {
			f["/etc/priors/real"] = dir(0o755)
			f["/etc/priors/real/trust.toml"] = file(validTrust)
			f[etcTrust] = link("real/./trust.toml")
		}, want: "/etc/priors/real/trust.toml"},
		{name: "dot-dot resolves against the real directory", edit: func(f fakeFS) {
			f["/etc/priors"] = link("/opt/p")
			f["/opt"] = dir(0o755)
			f["/opt/p"] = dir(0o755)
			f["/opt/p/trust.toml"] = link("../q/trust.toml")
			f["/opt/q"] = dir(0o755)
			f["/opt/q/trust.toml"] = file(validTrust)
		}, want: "/opt/q/trust.toml"},
		{name: "symlink loop", edit: func(f fakeFS) {
			f[etcTrust] = link("trust.toml")
		}, want: etcTrust + ": too many symlinks", wantErr: true},
		{name: "character device", edit: func(f fakeFS) {
			f[etcTrust] = fakeEntry{mode: fs.ModeDevice | fs.ModeCharDevice | 0o644}
		}, want: etcTrust + " is not a regular file", wantErr: true},
		{name: "directory", edit: func(f fakeFS) {
			f[etcTrust] = dir(0o755)
		}, want: etcTrust + " is not a regular file", wantErr: true},
		{name: "file on the way", edit: func(f fakeFS) {
			f["/etc/priors"] = file("")
		}, want: "/etc/priors is not a directory", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for walk, check := range map[string]func(StatFS, string) (string, error){
				"checkTrustPath": checkTrustPath,
				"CheckOwnedPath": CheckOwnedPath,
			} {
				f := etcFS()
				if tc.edit != nil {
					tc.edit(f)
				}

				got, err := check(f, etcTrust)

				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Errorf("%s = %q, %v; want an error containing %q", walk, got, err, tc.want)
					}
					continue
				}
				if err != nil || got != tc.want {
					t.Errorf("%s = %q, %v; want %q", walk, got, err, tc.want)
				}
			}
		})
	}
}

// The kernel resolves ".." after following the symlink before it.
func TestCheckTrustPathFollowsSymlinkBeforeDotDot(t *testing.T) {
	f := fakeFS{
		"/":               dir(0o755),
		"/etc":            dir(0o755),
		"/etc/link":       link("/opt/real"),
		"/opt":            dir(0o755),
		"/opt/real":       dir(0o755),
		"/opt/trust.toml": file(validTrust),
	}

	got, err := checkTrustPath(f, "/etc/link/../trust.toml")

	if err != nil || got != "/opt/trust.toml" {
		t.Errorf("checkTrustPath = %q, %v; want /opt/trust.toml", got, err)
	}
}

func TestCheckTrustPathNeedsAnAbsolutePath(t *testing.T) {
	for _, p := range []string{"", "etc/priors/trust.toml"} {
		if _, err := checkTrustPath(etcFS(), p); err == nil || !strings.Contains(err.Error(), "is not absolute") {
			t.Errorf("checkTrustPath(%q) = %v, want a not-absolute error", p, err)
		}
	}
}

func TestCheckTrustPathKeepsNotExist(t *testing.T) {
	for name, edit := range map[string]func(f fakeFS){
		"missing file":      func(f fakeFS) { delete(f, etcTrust) },
		"missing directory": func(f fakeFS) { delete(f, "/etc/priors"); delete(f, etcTrust) },
		"dangling symlink":  func(f fakeFS) { f[etcTrust] = link("/absent") },
	} {
		t.Run(name, func(t *testing.T) {
			f := etcFS()
			edit(f)
			if _, err := checkTrustPath(f, etcTrust); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("err = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

func TestCheckTrustPathRealFSRefusesAUserOwnedFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("every temp file is root-owned when run as root")
	}
	path := filepath.Join(t.TempDir(), "trust.toml")
	if err := os.WriteFile(path, []byte(validTrust), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := checkTrustPath(osFS{}, path)

	// The nix sandbox's / is not root-owned, so the offending component varies.
	if err == nil || !strings.Contains(err.Error(), "not root") {
		t.Errorf("err = %v, want an ownership error", err)
	}
}

func TestReadTrust(t *testing.T) {
	const (
		orgs     = "work_orgs = [\"github.com/w\"]\n"
		personal = "\n[stores.personal]\nid = \"p\"\npath = \"/m/p\"\n"
		work     = "\n[stores.work]\nid = \"w\"\npath = \"/m/w\"\n"
	)
	rejected := map[string]struct{ body, want string }{
		"unparsable":                {"profile = \n", "toml: line 1 (last key \"profile\"): expected value"},
		"unknown key":               {"profile = \"personal\"\n" + orgs + "seam_probe = 1\n" + personal, "unknown keys: seam_probe"},
		"unknown store key":         {"profile = \"personal\"\n" + orgs + personal + "url = \"/x\"\n", "unknown keys: stores.personal.url"},
		"unknown store kind":        {"profile = \"personal\"\n" + orgs + personal + "\n[stores.other]\nid = \"o\"\n", "unknown keys: stores.other"},
		"no profile":                {orgs + personal, `profile must be "work" or "personal", got ""`},
		"bad profile":               {"profile = \"office\"\n" + orgs + personal, `profile must be "work" or "personal", got "office"`},
		"no work orgs":              {"profile = \"personal\"\n" + personal, "work_orgs is required"},
		"empty work orgs":           {"profile = \"personal\"\nwork_orgs = []\n" + personal, "work_orgs is required"},
		"org without slash":         {"profile = \"personal\"\nwork_orgs = [\"factify-inc\"]\n" + personal, `work_orgs: "factify-inc" must be host/owner`},
		"org empty owner":           {"profile = \"personal\"\nwork_orgs = [\"github.com/\"]\n" + personal, "must be host/owner"},
		"org empty host":            {"profile = \"personal\"\n" + orgs + "personal_orgs = [\"/noamsto\"]\n" + personal, `personal_orgs: "/noamsto" must be host/owner`},
		"org with two slashes":      {"profile = \"personal\"\nwork_orgs = [\"github.com/a/b\"]\n" + personal, "must be host/owner"},
		"no personal store":         {"profile = \"personal\"\n" + orgs, "stores.personal is required"},
		"empty store id":            {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\n", `stores.personal.id "" must match`},
		"upper-case store id":       {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"P\"\n", `stores.personal.id "P" must match`},
		"store id with a slash":     {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"a/b\"\n", "must match"},
		"store id too long":         {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"" + strings.Repeat("a", 65) + "\"\n", "must match"},
		"no personal path":          {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"p\"\n", "stores.personal.path is required"},
		"relative personal path":    {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"p\"\npath = \"m/p\"\n", `stores.personal.path "m/p" must be absolute`},
		"tilde personal path":       {"profile = \"personal\"\n" + orgs + "\n[stores.personal]\nid = \"p\"\npath = \"~/p\"\n", `stores.personal.path "~/p" must be absolute`},
		"personal remote with dash": {"profile = \"personal\"\n" + orgs + personal + "remote = \"-oProxyCommand=x\"\n", `stores.personal.remote "-oProxyCommand=x" must not start with "-"`},
		"personal remote w/ space":  {"profile = \"personal\"\n" + orgs + personal + "remote = \"git@h:p .git\"\n", "stores.personal.remote"},
		"personal remote w/ tab":    {"profile = \"personal\"\n" + orgs + personal + "remote = \"git@h:p\\t.git\"\n", "whitespace or control characters"},
		"no work path":              {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"w\"\n", "stores.work.path is required"},
		"relative work path":        {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"w\"\npath = \"m/w\"\n", `stores.work.path "m/w" must be absolute`},
		"work remote with dash":     {"profile = \"work\"\n" + orgs + personal + work + "remote = \"-x\"\n", `stores.work.remote "-x" must not start with "-"`},
		"bad work store id":         {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"../w\"\n", `stores.work.id "../w" must match`},
		"duplicate store ids":       {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"p\"\npath = \"/m/w\"\n", `must differ, both are "p"`},
		"work profile no work":      {"profile = \"work\"\n" + orgs + personal, "stores.work is required on a work profile"},
		"personal profile has work": {"profile = \"personal\"\n" + orgs + personal + work, "stores.work is listed on a personal profile"},
		"scalar work names":         {"profile = \"personal\"\n" + orgs + "work_names = \"acme\"\n" + personal, "work_names"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			f := etcFS()
			f[etcTrust] = file(c.body)
			_, err := readTrust(f, etcTrust)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("readTrust(%q) = %v, want an error containing %q", c.body, err, c.want)
			}
		})
	}

	t.Run("full", func(t *testing.T) {
		got, err := readTrust(etcFS(), etcTrust)
		if err != nil {
			t.Fatal(err)
		}
		want := trust{
			Profile:      "work",
			WorkOrgs:     []string{"github.com/factify-inc"},
			PersonalOrgs: []string{"github.com/noamsto"},
			TrustRoot:    "owner-admin",
		}
		want.Stores.Personal = &trustStore{ID: "noamsto-priors", Path: "/m/personal"}
		want.Stores.Work = &trustStore{ID: "factify-priors", Path: "/m/work"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("readTrust = %+v\nwant       %+v", got, want)
		}
	})

	t.Run("store remotes", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file("profile = \"work\"\n" + orgs + personal + "remote = \"git@h:p.git\"\n" + work + "remote = \"\"\n")
		got, err := readTrust(f, etcTrust)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stores.Personal.Remote != "git@h:p.git" || got.Stores.Work.Remote != "" {
			t.Errorf("remotes = %q, %q; want git@h:p.git and none", got.Stores.Personal.Remote, got.Stores.Work.Remote)
		}
	})

	t.Run("work_names", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file("profile = \"personal\"\n" + orgs + "work_names = [\"acme\", \"globex\"]\n" + personal)
		got, err := readTrust(f, etcTrust)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"acme", "globex"}; !reflect.DeepEqual(got.WorkNames, want) {
			t.Errorf("WorkNames = %v, want %v", got.WorkNames, want)
		}
	})

	t.Run("minimal personal", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file("profile = \"personal\"\n" + orgs + personal)
		got, err := readTrust(f, etcTrust)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stores.Work != nil || got.PersonalOrgs != nil || got.TrustRoot != nil || got.AttestKeys != nil {
			t.Errorf("readTrust = %+v, want no work store, personal orgs, trust_root or attest keys", got)
		}
	})

	for name, body := range map[string]string{
		"trust_root = true":    "trust_root = true\n",
		"trust_root = 1":       "trust_root = 1\n",
		"trust_root = [1]":     "trust_root = [1]\n",
		"trust_root = {a = 1}": "trust_root = {a = 1}\n",
		"[[trust_root]]":       "[[trust_root]]\na = 1\n",
	} {
		t.Run(name+" is off, not an error", func(t *testing.T) {
			f := etcFS()
			f[etcTrust] = file("profile = \"personal\"\n" + orgs + body + personal)
			got, err := readTrust(f, etcTrust)
			if err != nil {
				t.Fatal(err)
			}
			if s, _ := got.TrustRoot.(string); s != "" {
				t.Errorf("TrustRoot = %v, want a non-string", got.TrustRoot)
			}
		})
	}

	t.Run("checks ownership before reading", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = fakeEntry{uid: 1000, mode: 0o644, data: "not toml ="}
		if _, err := readTrust(f, etcTrust); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Errorf("err = %v, want an ownership error before any parse", err)
		}
	})
}

const personalTrust = "profile = \"personal\"\nwork_orgs = [\"github.com/w\"]\n\n[stores.personal]\nid = \"p\"\npath = \"/m/p\"\n"

// attestEntry is one [[attest_keys]] table as the removed attestation schema
// wrote it; trust files generated for that schema must still load.
func attestEntry(key, attestation, challenge string) string {
	return fmt.Sprintf("\n[[attest_keys]]\nkey = %q\nattestation = %q\nchallenge = %q\n", key, attestation, challenge)
}

// TestReadTrustIgnoresAttestKeys pins D2 of #289: attest_keys is deprecated,
// accepted in any shape and never validated.
func TestReadTrustIgnoresAttestKeys(t *testing.T) {
	for name, body := range map[string]string{
		"scalar":         "attest_keys = \"x\"\n" + personalTrust,
		"array":          "attest_keys = [\"a\", \"b\"]\n" + personalTrust,
		"tables":         personalTrust + attestEntry("ssh-ed25519 AAAA", "YXR0ZXN0", "Y2hhbA=="),
		"two tables":     personalTrust + attestEntry("k1", "a", "b") + attestEntry("k2", "c", "d"),
		"malformed":      personalTrust + attestEntry("", "not base64!", ""),
		"extra sub-keys": personalTrust + attestEntry("k", "a", "b") + "comment = \"x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := etcFS()
			f[etcTrust] = file(body)
			got, err := readTrust(f, etcTrust)
			if err != nil {
				t.Fatalf("readTrust: %v, want the deprecated attest_keys ignored", err)
			}
			if got.Profile != "personal" || got.Stores.Personal == nil {
				t.Errorf("readTrust = %+v, want the rest of the file read", got)
			}
		})
	}
}

// TestReadTrustTrustRoot pins D1 of #289: trust_root is ignored unless it is
// "separate", which fails with a message naming the removal.
func TestReadTrustTrustRoot(t *testing.T) {
	for name, line := range map[string]string{
		"absent":      "",
		"owner-admin": "trust_root = \"owner-admin\"\n",
		"other text":  "trust_root = \"whatever\"\n",
		"true":        "trust_root = true\n",
		"array":       "trust_root = [\"a\"]\n",
		"table":       "trust_root = {a = 1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := etcFS()
			f[etcTrust] = file(line + personalTrust)
			if _, err := readTrust(f, etcTrust); err != nil {
				t.Errorf("readTrust: %v, want it to load", err)
			}
		})
	}

	t.Run("tables", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file(personalTrust + "\n[[trust_root]]\na = 1\n")
		if _, err := readTrust(f, etcTrust); err != nil {
			t.Errorf("readTrust: %v, want it to load", err)
		}
	})

	t.Run("separate", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file("trust_root = \"separate\"\n" + personalTrust)
		_, err := readTrust(f, etcTrust)
		if err == nil || !strings.Contains(err.Error(), "attestation was removed") {
			t.Errorf("readTrust = %v, want an error containing %q", err, "attestation was removed")
		}
	})
}

func TestLoadTrustReadsOnlyTheTrustFile(t *testing.T) {
	user := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(user, []byte("profile = \"x\"\nbogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRIORS_CONFIG", user)

	body := strings.NewReplacer(
		`id   = "noamsto-priors"`, "id   = \"noamsto-priors\"\nremote = \"git@h:p.git\"",
		`id   = "factify-priors"`, "id   = \"factify-priors\"\nremote = \"git@h:w.git\"",
		`trust_root    = "owner-admin"`, "trust_root    = \"owner-admin\"\nwork_names    = [\"acme\"]",
	).Replace(validTrust) + attestEntry("ssh-ed25519 AAAA", "YXR0ZXN0", "Y2hhbA==")
	f := etcFS()
	f[etcTrust] = file(body)

	got, err := loadTrust(etcTrust, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "work" {
		t.Errorf("profile = %q", got.Profile)
	}
	if !reflect.DeepEqual(got.WorkOrgs, []string{"github.com/factify-inc"}) || !reflect.DeepEqual(got.PersonalOrgs, []string{"github.com/noamsto"}) {
		t.Errorf("orgs = %v, %v", got.WorkOrgs, got.PersonalOrgs)
	}
	if got.PersonalStoreID != "noamsto-priors" || got.PersonalStore != "/m/personal" || got.PersonalRemote != "git@h:p.git" ||
		got.WorkStoreID != "factify-priors" || got.WorkStore != "/m/work" || got.WorkRemote != "git@h:w.git" {
		t.Errorf("stores = %+v", got)
	}
	if want := []string{"acme"}; !reflect.DeepEqual(got.WorkNames(), want) {
		t.Errorf("WorkNames() = %v, want exactly the floor %v", got.WorkNames(), want)
	}

	_, err = loadTrust("/etc/priors/absent.toml", etcFS())
	if !errors.Is(err, ErrNoTrust) {
		t.Errorf("missing trust file: %v, want ErrNoTrust", err)
	}
}
