//go:build !priorstest

package config

const trustPath = "/etc/priors/trust.toml"

// trustSource is the trust file's fixed path and the file system it is read
// from.
func trustSource() (string, statFS) { return trustPath, osFS{} }
