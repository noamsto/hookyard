package main

import (
	"context"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attestcmd"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const attestUsage = "usage: priors attest [-f key] [--store id] <name>\n" +
	"       priors attest --revoke [-f key] [--store id] <name>\n" +
	"       priors attest --check [--store id] <name> <sha> <commit>"

func cmdAttest(args []string, s streams) int {
	if vars := attestcmd.LoaderVars(os.Environ()); len(vars) > 0 {
		s.errf("priors attest: refusing to run with %s in the environment\n", strings.Join(vars, ", "))
		return 1
	}
	fs := newFlagSet("attest", s)
	keyFile := fs.String("f", "", "key handle to sign with (default ~/.ssh/id_ed25519_sk)")
	revoke := fs.Bool("revoke", false, "revoke the fact's attestation")
	check := fs.Bool("check", false, "re-report an entry without signing")
	store := fs.String("store", "", "id of the store holding the fact")
	pos, code, ok := parseFlags(fs, args)
	if !ok {
		return code
	}
	want := 1
	if *check {
		want = 3
	}
	if (*check && *revoke) || len(pos) != want {
		s.errln(attestUsage)
		return 1
	}

	cfg, err := loadConfig("")
	if err != nil {
		s.errln("config:", err)
		return 2
	}
	if !*check {
		for _, p := range []string{tools.SSHKeygen, tools.SSHSKHelper} {
			if err := tools.Check(p); err != nil {
				s.errln("priors attest:", err)
				return 1
			}
		}
	}
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		s.errln("priors attest:", err)
		return 1
	}
	home := u.HomeDir
	handle := *keyFile
	if handle == "" {
		handle = filepath.Join(home, ".ssh", "id_ed25519_sk")
	}
	if handle, err = filepath.Abs(handle); err != nil {
		s.errln("priors attest:", err)
		return 1
	}
	rules, err := gate.LoadRules(cfg.Rules)
	if err != nil {
		s.errln("priors attest:", err)
		return 1
	}
	var scanner gate.Scanner
	if !*check {
		if scanner, err = gate.PinnedScanner(); err != nil {
			s.errln("priors attest:", err)
			return 1
		}
	}

	signer := attestcmd.Keygen{Path: tools.SSHKeygen, Helper: tools.SSHSKHelper, Home: home, RequireTTY: attestcmd.RequireTTY()}
	o := attestcmd.Options{
		Config:    cfg,
		Rules:     rules,
		Scanner:   scanner,
		Groups:    attest.OSGroups,
		Env:       os.Environ(),
		StatFS:    cfg.FS(),
		VerifyUID: attest.VerifyUID,
		Signer:    signer,
		Terminal:  signer.CheckTerminal,
		Now:       time.Now,
		Wait:      attestcmd.DefaultWait,
		Poll:      time.Second,
		Trigger: func(path string) error {
			c, err := net.DialTimeout("unix", path, 2*time.Second)
			if err != nil {
				return err
			}
			return c.Close()
		},
		Out: s.out,
		Err: s.err,
	}
	ctx := context.Background()
	if *check {
		return attestcmd.Check(ctx, o, pos[0], *store, pos[1], pos[2])
	}
	req := attestcmd.Request{Name: pos[0], Store: *store, Handle: handle, Op: attest.OpAttest}
	if *revoke {
		req.Op = attest.OpRevoke
	}
	return attestcmd.Attest(ctx, o, req)
}
