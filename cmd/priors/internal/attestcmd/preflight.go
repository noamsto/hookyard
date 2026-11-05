package attestcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func loaderVars(env []string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			out = append(out, name)
		}
	}
	return out
}

// gitDirs reads the layout from files so no git process runs before the
// preflight has vetted the environment.
func gitDirs(root string) (gitDir, common string, err error) {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", "", err
	}
	gitDir = dotGit
	if !info.IsDir() {
		b, err := os.ReadFile(dotGit) //nolint:gosec // dotGit is the configured store checkout's .git
		if err != nil {
			return "", "", err
		}
		p, ok := strings.CutPrefix(string(b), "gitdir: ")
		if !ok {
			return "", "", fmt.Errorf("%s: no gitdir: line", dotGit)
		}
		gitDir = joinIfRelative(root, strings.TrimRight(p, "\r\n"))
	}
	common = gitDir
	switch b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); { //nolint:gosec // gitDir is the store checkout's own git dir
	case err == nil:
		common = joinIfRelative(gitDir, strings.TrimSpace(string(b)))
	case !errors.Is(err, os.ErrNotExist):
		return "", "", err
	}
	for _, d := range []*string{&gitDir, &common} {
		if *d, err = filepath.Abs(*d); err != nil {
			return "", "", err
		}
		if fi, err := os.Stat(*d); err != nil || !fi.IsDir() { //nolint:gosec // only stats the dir the checkout names, to refuse a missing one
			return "", "", fmt.Errorf("%s is not a git directory", *d)
		}
	}
	return gitDir, common, nil
}

func joinIfRelative(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func refuseGrafts(gitDir, common string) error {
	for _, dir := range []string{gitDir, common} {
		for _, rel := range []string{filepath.Join("info", "grafts"), "shallow"} {
			path := filepath.Join(dir, rel)
			_, err := os.Lstat(path)
			if err == nil {
				return fmt.Errorf("%s exists: priors attest needs a full clone without grafts", path)
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
