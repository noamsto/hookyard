//go:build priorstest

package config

import (
	"io/fs"
	"os"
	"syscall"
)

// trustSource reads the trust path from $PRIORS_TEST_TRUST and reports every
// file as root-owned, so the subprocess tests run the trust walk over sandbox
// files without root. Only a build with -tags priorstest compiles this file;
// the nix build of priors sets no tags, so no flag, env var or file can reach
// it in a production binary.
func trustSource() (string, StatFS) { return os.Getenv("PRIORS_TEST_TRUST"), rootOwnedFS{} }

// rootOwnedFS is the real file system with every owner reported as uid 0;
// types, modes and symlinks stay real.
type rootOwnedFS struct{ osFS }

func (r rootOwnedFS) Lstat(name string) (fs.FileInfo, error) {
	fi, err := r.osFS.Lstat(name)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi, nil
	}
	owned := *st
	owned.Uid = 0
	return rootOwnedInfo{fi, &owned}, nil
}

type rootOwnedInfo struct {
	fs.FileInfo
	sys *syscall.Stat_t
}

func (i rootOwnedInfo) Sys() any { return i.sys }
