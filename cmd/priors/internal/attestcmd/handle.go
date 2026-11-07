package attestcmd

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/ssh"
)

const handleMaxBytes = 64 << 10

const opensshMagic = "openssh-key-v1\x00"

// handleKey returns the public key of the sk key handle at path once it is
// on the allowlist. The public key blob of an openssh-key-v1 file is
// plaintext even when the private part is encrypted, so the allowlist check
// needs no passphrase and runs before signing.
func handleKey(path string, allow []ssh.PublicKey) (ssh.PublicKey, error) {
	f, err := os.Open(path) //nolint:gosec // the key handle is the user's own -f path, read only to compare its public key
	if err != nil {
		return nil, fmt.Errorf("open key handle: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat key handle: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("key handle %s is not a regular file", path)
	}

	data, err := io.ReadAll(io.LimitReader(f, handleMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read key handle: %w", err)
	}
	if len(data) > handleMaxBytes {
		return nil, fmt.Errorf("key handle %s is over 64 KiB", path)
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, fmt.Errorf("key handle %s is not an OPENSSH PRIVATE KEY PEM block", path)
	}
	body, ok := bytes.CutPrefix(block.Bytes, []byte(opensshMagic))
	if !ok {
		return nil, errors.New("key handle is not an openssh-key-v1 file")
	}

	var hdr struct {
		Cipher, KDF, KDFOpts string
		N                    uint32
		Pub                  []byte
		Rest                 []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(body, &hdr); err != nil {
		return nil, fmt.Errorf("parse key handle: %w", err)
	}
	if hdr.N != 1 {
		return nil, fmt.Errorf("key handle holds %d keys, want 1 key", hdr.N)
	}

	pub, err := ssh.ParsePublicKey(hdr.Pub)
	if err != nil {
		return nil, fmt.Errorf("parse key handle public key: %w", err)
	}
	if t := pub.Type(); t != ssh.KeyAlgoSKED25519 && t != ssh.KeyAlgoSKECDSA256 {
		return nil, fmt.Errorf("key handle type %s is not a hardware (sk) key", t)
	}
	for _, a := range allow {
		if bytes.Equal(a.Marshal(), pub.Marshal()) {
			return pub, nil
		}
	}
	return nil, errors.New("key handle is not on the allowlist")
}
