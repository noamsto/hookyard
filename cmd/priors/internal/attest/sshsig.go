package attest

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"slices"

	"golang.org/x/crypto/ssh"
)

// sshsigMagic opens both the SSHSIG blob and the signed preimage
// (OpenSSH PROTOCOL.sshsig).
const sshsigMagic = "SSHSIG"

const (
	flagUserPresence = 0x01
	flagUserVerified = 0x04
)

// sigBlob is an SSHSIG blob after its magic.
type sigBlob struct {
	Version       uint32
	PublicKey     []byte
	Namespace     string
	Reserved      []byte
	HashAlgorithm string
	Signature     []byte
}

func parseBlob(raw []byte) (sigBlob, error) {
	rest, ok := bytes.CutPrefix(raw, []byte(sshsigMagic))
	if !ok {
		return sigBlob{}, errors.New("not an SSHSIG blob")
	}
	var b sigBlob
	if err := ssh.Unmarshal(rest, &b); err != nil {
		return sigBlob{}, fmt.Errorf("SSHSIG blob: %w", err)
	}
	return b, nil
}

func (b sigBlob) marshal() []byte {
	return append([]byte(sshsigMagic), ssh.Marshal(b)...)
}

// VerifyEntry runs checks 4–6 on e: an SSHSIG over e.Signed in Namespace by a
// key in allow, from a hardware key with user presence and verification, for
// this store and file name. ops lists the accepted ops, OpAttest by default.
func VerifyEntry(e Entry, allow []ssh.PublicKey, storeID, fileName string, ops ...Op) error {
	if len(ops) == 0 {
		ops = []Op{OpAttest}
	}
	if !slices.Contains(ops, e.Op) {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("op %q is not accepted here", e.Op)}
	}

	b, err := parseBlob(e.Signature)
	if err != nil {
		return &CheckError{Check: 4, Reason: err.Error()}
	}
	if b.Version != 1 {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("SSHSIG version %d, want 1", b.Version)}
	}
	if b.Namespace != Namespace {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("namespace %q, want %q", b.Namespace, Namespace)}
	}
	var digest []byte
	switch b.HashAlgorithm {
	case "sha256":
		d := sha256.Sum256(e.Signed)
		digest = d[:]
	case "sha512":
		d := sha512.Sum512(e.Signed)
		digest = d[:]
	default:
		return &CheckError{Check: 4, Reason: fmt.Sprintf("hash algorithm %q, want sha256 or sha512", b.HashAlgorithm)}
	}
	pub, err := ssh.ParsePublicKey(b.PublicKey)
	if err != nil {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("public key: %v", err)}
	}
	if !slices.ContainsFunc(allow, func(a ssh.PublicKey) bool { return bytes.Equal(pub.Marshal(), a.Marshal()) }) {
		return &CheckError{Check: 4, Reason: "signing key is not on the allowlist"}
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(b.Signature, &sig); err != nil {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("signature: %v", err)}
	}

	// Before Verify: x/crypto's sk Verify itself rejects a signature without
	// user presence, which would otherwise surface as check 4.
	if t := pub.Type(); t != ssh.KeyAlgoSKED25519 && t != ssh.KeyAlgoSKECDSA256 {
		return &CheckError{Check: 5, Reason: fmt.Sprintf("key type %s is not a hardware (sk) key", t)}
	}
	// sk signature trailer: flags byte ‖ uint32 counter
	if len(sig.Rest) != 5 {
		return &CheckError{Check: 5, Reason: "sk signature has no flags and counter"}
	}
	const want = flagUserPresence | flagUserVerified
	if flags := sig.Rest[0]; flags&want != want {
		return &CheckError{Check: 5, Reason: fmt.Sprintf("signature flags %#02x lack user presence and verification", flags)}
	}

	preimage := append([]byte(sshsigMagic), ssh.Marshal(struct {
		Namespace     string
		Reserved      []byte
		HashAlgorithm string
		Hash          []byte
	}{b.Namespace, b.Reserved, b.HashAlgorithm, digest})...)
	if err := pub.Verify(preimage, &sig); err != nil {
		return &CheckError{Check: 4, Reason: fmt.Sprintf("signature does not verify: %v", err)}
	}

	if e.Store != storeID {
		return &CheckError{Check: 6, Reason: fmt.Sprintf("entry store %q is not %q", e.Store, storeID)}
	}
	if e.Name != fileName {
		return &CheckError{Check: 6, Reason: fmt.Sprintf("entry name %q is not the file name %q", e.Name, fileName)}
	}
	return nil
}
