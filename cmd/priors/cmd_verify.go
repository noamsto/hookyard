package main

import (
	"context"
	"os"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/verify"
)

func cmdVerify(args []string, s streams) int {
	fs := newFlagSet("verify", s)
	store := fs.String("store", "", "id of the store to verify")
	credential := fs.String("credential", "", "absolute path of the SSH key that fetches the store")
	if _, code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *store == "" || *credential == "" {
		s.errln("priors verify: --store and --credential are required")
		return 1
	}

	cfg, err := config.LoadTrust()
	if err != nil {
		s.errln("priors verify:", err)
		return 1
	}
	job, err := verify.Prepare(cfg, *store, *credential, verify.Env{
		Groups:    attest.OSGroups,
		UID:       os.Getuid(),
		VerifyUID: attest.VerifyUID,
		Root:      attest.VerdictRoot,
	})
	if err != nil {
		s.errln("priors verify:", err)
		return 1
	}
	release, err := verify.Lock(job.Dir)
	if err != nil {
		s.errln("priors verify:", err)
		return 1
	}
	defer release()

	code := 0
	runOnce := func() {
		res, err := job.Run(context.Background())
		if err != nil {
			s.errf("priors verify: %s: %v\n", *store, err)
			code = 1
			return
		}
		code = 0
		line := *store + ": " + string(res.Status)
		if res.Reason != "" {
			line += ": " + res.Reason
		}
		s.outln(line)
	}

	l, err := verify.SystemdListener()
	if err != nil {
		s.errln("priors verify:", err)
		return 1
	}
	if l == nil {
		runOnce()
		return code
	}
	defer func() { _ = l.Close() }()
	verify.Serve(l, runOnce)
	return code
}
