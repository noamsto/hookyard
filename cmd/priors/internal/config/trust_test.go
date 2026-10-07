package config

import (
	"errors"
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

// fakeFS is a statFS over a map of absolute paths, so ownership can be set
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
trust_root    = "separate"

[stores.personal]
id = "noamsto-priors"

[stores.work]
id = "factify-priors"
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
			f := etcFS()
			if tc.edit != nil {
				tc.edit(f)
			}

			got, err := checkTrustPath(f, etcTrust)

			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("checkTrustPath = %q, %v; want an error containing %q", got, err, tc.want)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("checkTrustPath = %q, %v; want %q", got, err, tc.want)
			}
		})
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
		personal = "\n[stores.personal]\nid = \"p\"\n"
		work     = "\n[stores.work]\nid = \"w\"\n"
	)
	rejected := map[string]struct{ body, want string }{
		"unparsable":                {"profile = \n", "toml"},
		"unknown key":               {"profile = \"personal\"\n" + orgs + "seam_probe = 1\n" + personal, "unknown keys: seam_probe"},
		"unknown store key":         {"profile = \"personal\"\n" + orgs + personal + "path = \"/x\"\n", "unknown keys: stores.personal.path"},
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
		"bad work store id":         {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"../w\"\n", `stores.work.id "../w" must match`},
		"duplicate store ids":       {"profile = \"work\"\n" + orgs + personal + "\n[stores.work]\nid = \"p\"\n", `must differ, both are "p"`},
		"work profile no work":      {"profile = \"work\"\n" + orgs + personal, "stores.work is required on a work profile"},
		"personal profile has work": {"profile = \"personal\"\n" + orgs + personal + work, "stores.work is listed on a personal profile"},
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
			TrustRoot:    "separate",
		}
		want.Stores.Personal = &trustStore{ID: "noamsto-priors"}
		want.Stores.Work = &trustStore{ID: "factify-priors"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("readTrust = %+v\nwant       %+v", got, want)
		}
	})

	t.Run("minimal personal", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = file("profile = \"personal\"\n" + orgs + personal)
		got, err := readTrust(f, etcTrust)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stores.Work != nil || got.PersonalOrgs != nil || got.TrustRoot != "" {
			t.Errorf("readTrust = %+v, want no work store, personal orgs or trust_root", got)
		}
	})

	t.Run("checks ownership before reading", func(t *testing.T) {
		f := etcFS()
		f[etcTrust] = fakeEntry{uid: 1000, mode: 0o644, data: "not toml ="}
		if _, err := readTrust(f, etcTrust); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Errorf("err = %v, want an ownership error before any parse", err)
		}
	})
}
