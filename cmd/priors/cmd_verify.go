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

	runs := &verifyRuns{store: *store, credential: *credential, loadTrust: config.LoadTrust, s: s}
	job, err := runs.prepare()
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

	l, err := verify.SystemdListener()
	if err != nil {
		s.errln("priors verify:", err)
		return 1
	}
	if l == nil {
		runs.once()
		return runs.code
	}
	defer func() { _ = l.Close() }()
	verify.Serve(l, runs.once)
	return runs.code
}

// verifyRuns runs the job once per trigger. Each run gates again, so one
// queued on the socket reads the trust file as it is when it starts.
type verifyRuns struct {
	store, credential string
	loadTrust         func() (config.Config, error)
	s                 streams
	code              int
}

func (v *verifyRuns) prepare() (*verify.Job, error) {
	cfg, err := v.loadTrust()
	if err != nil {
		return nil, err
	}
	return verify.Prepare(cfg, v.store, v.credential, verify.Env{
		Groups:    attest.OSGroups,
		UID:       os.Getuid(),
		VerifyUID: attest.VerifyUID,
		Root:      attest.VerdictRoot,
	})
}

func (v *verifyRuns) once() {
	job, err := v.prepare()
	if err != nil {
		v.s.errln("priors verify:", err)
		v.code = 1
		return
	}
	res, err := job.Run(context.Background())
	if err != nil {
		v.s.errf("priors verify: %s: %v\n", v.store, err)
		v.code = 1
		return
	}
	v.code = 0
	line := v.store + ": " + string(res.Status)
	if res.Reason != "" {
		line += ": " + res.Reason
	}
	v.s.outln(line)
}
