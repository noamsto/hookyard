// Command fakekeygen is a test-only stand-in for `ssh-keygen -Y sign`, never
// built into priors.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

func main() {
	if len(os.Args) != 7 || os.Args[1] != "-Y" || os.Args[2] != "sign" || os.Args[3] != "-n" || os.Args[5] != "-f" {
		fmt.Fprintln(os.Stderr, "usage: fakekeygen -Y sign -n <namespace> -f <handle>")
		os.Exit(2)
	}
	namespace, handle := os.Args[4], os.Args[6]

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(err)
	}
	raw, err := os.ReadFile(handle) //nolint:gosec // handle is the -f argv of a test-only stand-in
	if err != nil {
		fatal(err)
	}
	key, flags, err := attesttest.ParseHandle(raw)
	if err != nil {
		fatal(err)
	}

	record, err := json.Marshal(struct {
		Env   []string `json:"env"`
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}{os.Environ(), os.Args[1:], string(payload)})
	if err != nil {
		fatal(err)
	}
	log, err := os.OpenFile(handle+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // handle is the -f argv of a test-only stand-in
	if err != nil {
		fatal(err)
	}
	if _, err := log.Write(append(record, '\n')); err != nil {
		fatal(err)
	}
	if err := log.Close(); err != nil {
		fatal(err)
	}

	if _, err := os.Stat(handle + ".fail"); err == nil { //nolint:gosec // handle is the -f argv of a test-only stand-in
		fmt.Fprintln(os.Stderr, "sign failed")
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(key.SignSSHSIG(payload, namespace, flags)); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fakekeygen:", err)
	os.Exit(1)
}
