// Package attesttest is a software stand-in for a FIDO2 authenticator: it
// builds sk-format SSHSIG signatures with any flags, so tests can exercise
// the attest checks without hardware.
package attesttest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const (
	application = "ssh:"
	counter     = 1
)

// Key is a private key and its SSH public key.
type Key struct {
	t      testing.TB
	pub    ssh.PublicKey
	ed     ed25519.PrivateKey
	ec     *ecdsa.PrivateKey
	signer ssh.Signer
}

// PublicKey is the key as an allowlist holds it.
func (k Key) PublicKey() ssh.PublicKey { return k.pub }

// NewSKEd25519 is an sk-ssh-ed25519@openssh.com key.
func NewSKEd25519(t testing.TB) Key {
	t.Helper()
	pubBytes, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := ssh.Marshal(struct {
		Type        string
		Key         []byte
		Application string
	}{ssh.KeyAlgoSKED25519, pubBytes, application})
	return Key{t: t, pub: parse(t, blob), ed: priv}
}

// NewSKECDSA is an sk-ecdsa-sha2-nistp256@openssh.com key.
func NewSKECDSA(t testing.TB) Key {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
		return Key{}
	}
	point, err := priv.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	blob := ssh.Marshal(struct {
		Type        string
		Curve       string
		Point       []byte
		Application string
	}{ssh.KeyAlgoSKECDSA256, "nistp256", point, application})
	return Key{t: t, pub: parse(t, blob), ec: priv}
}

// NewEd25519 is a plain software ssh-ed25519 key.
func NewEd25519(t testing.TB) Key {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
		return Key{}
	}
	return Key{t: t, pub: signer.PublicKey(), signer: signer}
}

func parse(t testing.TB, blob []byte) ssh.PublicKey {
	t.Helper()
	pub, err := ssh.ParsePublicKey(blob)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// panicTB lets keys rebuilt outside a test sign; a failure panics.
type panicTB struct{ testing.TB }

func (panicTB) Helper() {}

func (panicTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

func (panicTB) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }

const (
	handleMagic = "openssh-key-v1\x00"
	handleType  = "OPENSSH PRIVATE KEY"
)

type handleOuter struct {
	Cipher, KDF string
	KDFOpts     string
	Keys        uint32
	Pub         []byte
	Priv        []byte
}

type handleInner struct {
	Check1, Check2 uint32
	Type           string
	Pub            []byte
	Application    string
	Flags          uint8
	Handle         []byte
	Reserved       string
	Comment        string
	Padding        []byte `ssh:"rest"`
}

// Handle is the key as an OpenSSH private key file holding an sk-ed25519
// handle: the key handle is the ed25519 seed and flags ride in the file, so
// ParseHandle can rebuild a signer from it.
func (k Key) Handle(flags byte) []byte {
	k.t.Helper()
	if k.ed == nil {
		k.t.Fatal("handles are for sk-ed25519 keys only")
	}
	var check [4]byte
	_, _ = rand.Read(check[:])
	c := binary.BigEndian.Uint32(check[:])
	priv := ssh.Marshal(handleInner{
		Check1: c, Check2: c,
		Type:        ssh.KeyAlgoSKED25519,
		Pub:         k.ed.Public().(ed25519.PublicKey),
		Application: application,
		Flags:       flags,
		Handle:      k.ed.Seed(),
	})
	for i := byte(1); len(priv)%8 != 0; i++ {
		priv = append(priv, i)
	}
	body := append([]byte(handleMagic), ssh.Marshal(handleOuter{
		Cipher: "none", KDF: "none", Keys: 1, Pub: k.pub.Marshal(), Priv: priv,
	})...)
	return pem.EncodeToMemory(&pem.Block{Type: handleType, Bytes: body})
}

// ParseHandle reverses Handle.
func ParseHandle(raw []byte) (Key, byte, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != handleType {
		return Key{}, 0, errors.New("not an OpenSSH private key")
	}
	body, ok := bytes.CutPrefix(block.Bytes, []byte(handleMagic))
	if !ok {
		return Key{}, 0, errors.New("bad openssh-key-v1 magic")
	}
	var outer handleOuter
	if err := ssh.Unmarshal(body, &outer); err != nil {
		return Key{}, 0, err
	}
	var inner handleInner
	if err := ssh.Unmarshal(outer.Priv, &inner); err != nil {
		return Key{}, 0, err
	}
	if inner.Check1 != inner.Check2 || inner.Type != ssh.KeyAlgoSKED25519 || len(inner.Handle) != ed25519.SeedSize {
		return Key{}, 0, errors.New("not an sk-ed25519 handle")
	}
	pub, err := ssh.ParsePublicKey(outer.Pub)
	if err != nil {
		return Key{}, 0, err
	}
	return Key{t: panicTB{}, pub: pub, ed: ed25519.NewKeyFromSeed(inner.Handle)}, inner.Flags, nil
}

// AuthorizedKey is the key in authorized_keys form.
func (k Key) AuthorizedKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.pub)))
}

// SignSSHSIG signs msg as `ssh-keygen -Y sign -n namespace` would and returns
// the armoured signature. flags is ignored for a non-sk key.
func (k Key) SignSSHSIG(msg []byte, namespace string, flags byte) []byte {
	k.t.Helper()
	return k.SignSSHSIGHash(msg, namespace, "sha512", flags)
}

// SignSSHSIGHash is SignSSHSIG with hashAlg "sha256" or "sha512".
func (k Key) SignSSHSIGHash(msg []byte, namespace, hashAlg string, flags byte) []byte {
	k.t.Helper()
	var digest []byte
	switch hashAlg {
	case "sha256":
		d := sha256.Sum256(msg)
		digest = d[:]
	case "sha512":
		d := sha512.Sum512(msg)
		digest = d[:]
	default:
		k.t.Fatalf("unsupported hash algorithm %q", hashAlg)
	}
	preimage := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Hash          []byte
	}{namespace, "", hashAlg, digest})...)

	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version       uint32
		PublicKey     []byte
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Signature     []byte
	}{1, k.pub.Marshal(), namespace, "", hashAlg, ssh.Marshal(k.sign(preimage, flags))})...)
	return armour(blob)
}

// sign follows OpenSSH PROTOCOL.u2f for sk keys: the authenticator signs
// sha256(application) ‖ flags ‖ counter ‖ sha256(data).
func (k Key) sign(data []byte, flags byte) *ssh.Signature {
	k.t.Helper()
	if k.signer != nil {
		sig, err := k.signer.Sign(rand.Reader, data)
		if err != nil {
			k.t.Fatal(err)
		}
		return sig
	}
	appDigest := sha256.Sum256([]byte(application))
	dataDigest := sha256.Sum256(data)
	rest := binary.BigEndian.AppendUint32([]byte{flags}, counter)
	signed := bytes.Join([][]byte{appDigest[:], rest, dataDigest[:]}, nil)

	if k.ed != nil {
		return &ssh.Signature{Format: ssh.KeyAlgoSKED25519, Blob: ed25519.Sign(k.ed, signed), Rest: rest}
	}
	digest := sha256.Sum256(signed)
	r, s, err := ecdsa.Sign(rand.Reader, k.ec, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	return &ssh.Signature{
		Format: ssh.KeyAlgoSKECDSA256,
		Blob:   ssh.Marshal(struct{ R, S *big.Int }{r, s}),
		Rest:   rest,
	}
}

func armour(blob []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(blob)
	var b bytes.Buffer
	b.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n")
	b.WriteString("-----END SSH SIGNATURE-----\n")
	return b.Bytes()
}

// Entry is the seven signed lines of an attest entry.
func Entry(store, name, path, sha256 string, seq uint64, op string) []byte {
	return fmt.Appendf(nil, "priors-attest v1\nstore: %s\nname: %s\npath: %s\nsha256: %s\nsequence: %d\nop: %s\n",
		store, name, path, sha256, seq, op)
}

// Signed is lines followed by k's armoured signature over them.
func Signed(k Key, lines []byte, flags byte, namespace string) []byte {
	return append(bytes.Clone(lines), k.SignSSHSIG(lines, namespace, flags)...)
}
