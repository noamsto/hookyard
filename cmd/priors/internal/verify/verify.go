// Package verify is the attestation verdict job, `priors verify` (memory-layer §4.4).
package verify

import (
	"time"

	"golang.org/x/crypto/ssh"
)

type Status string

const (
	StatusOK          Status = "ok"
	StatusBehind      Status = "behind"
	StatusOversize    Status = "oversize"
	StatusStaleRemote Status = "stale-remote"
)

type Budget struct {
	Wall        time.Duration // whole run after gating: 300 s
	Reserve     time.Duration // kept for checkpoint + verdict write: 20 s
	Commits     int           // newly walked commits: 50_000
	MirrorBytes int64         // mirror cap: 2 << 30
}

var DefaultBudget = Budget{Wall: 300 * time.Second, Reserve: 20 * time.Second, Commits: 50_000, MirrorBytes: 2 << 30}

type Store struct{ ID, Remote string }

type Job struct {
	Store       Store
	Credential  string          // ssh private key path (--credential)
	Keys        []ssh.PublicKey // allowlist after CheckEvidence
	TrustDigest string
	Dir         string // <VerdictRoot>/<id>
	Budget      Budget
	Now         func() time.Time
}

type Result struct {
	Status Status
	Reason string // short, never a fact name or path
	Wrote  bool   // a verdict file was written this run
}
