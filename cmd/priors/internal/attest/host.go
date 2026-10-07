package attest

import (
	"fmt"
	"os"
	"os/user"
	"slices"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

// privilegedGroups are the groups whose members can act as the store owner
// or read its keys, so a signer in one is not separate from the agent.
var privilegedGroups = []string{"wheel", "admin", "docker", "libvirtd", "input", "disk", "lxd", "incus-admin"}

// GroupSource reads the running process's group membership.
type GroupSource interface {
	Gids() ([]int, error)
	Name(gid int) (string, error)
}

type osGroups struct{}

func (osGroups) Gids() ([]int, error) {
	gids, err := os.Getgroups()
	if err != nil {
		return nil, err
	}
	return append(gids, os.Getgid()), nil
}

func (osGroups) Name(gid int) (string, error) {
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return "", err
	}
	return g.Name, nil
}

// OSGroups is the production GroupSource.
var OSGroups GroupSource = osGroups{}

// Host is the verdict of the host classification: attestation is on only when
// Off is empty.
type Host struct {
	Off  string
	Keys []ssh.PublicKey
}

func (h Host) On() bool { return h.Off == "" }

// Classify decides whether this host may honour attestation. Group membership
// comes from the process's own gids, never /etc/group, and an unnamed gid
// fails closed because it could be any privileged group.
func Classify(cfg config.Config, g GroupSource) Host {
	if cfg.TrustRoot != "separate" {
		return Host{Off: "attestation off: trust_root " + cfg.TrustRootLabel}
	}
	if off := privilegedGroup(g); off != "" {
		return Host{Off: off}
	}
	if len(cfg.AttestKeys) == 0 {
		return Host{Off: "no attestation key enrolled"}
	}
	keys := make([]ssh.PublicKey, len(cfg.AttestKeys))
	for i, k := range cfg.AttestKeys {
		keys[i] = k.Key
	}
	return Host{Keys: keys}
}

func privilegedGroup(g GroupSource) string {
	gids, err := g.Gids()
	if err != nil {
		return fmt.Sprintf("attestation off: groups unreadable: %v", err)
	}
	slices.Sort(gids)
	for _, gid := range slices.Compact(gids) {
		name, err := g.Name(gid)
		if err != nil {
			return fmt.Sprintf("attestation off: group %d", gid)
		}
		if slices.Contains(privilegedGroups, name) {
			return "attestation off: group " + name
		}
	}
	return ""
}
