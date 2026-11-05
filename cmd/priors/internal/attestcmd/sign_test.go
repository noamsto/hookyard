package attestcmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const fakeKeygenPkg = "github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest/fakekeygen"

// fakeKeygen builds the test stand-in for ssh-keygen into its own temp dir.
func fakeKeygen(t *testing.T) string {
	t.Helper()
	dir, err := exec.Command("go", "list", "-f", "{{.Dir}}", fakeKeygenPkg).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	out := filepath.Join(t.TempDir(), "ssh-keygen")
	build := exec.Command("go", "build", "-o", out, ".")
	build.Dir = strings.TrimSpace(string(dir))
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build fakekeygen: %v\n%s", err, msg)
	}
	return out
}

type keygenRecord struct {
	Env   []string `json:"env"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func TestKeygen(t *testing.T) {
	payload := attesttest.Entry("personal-t", "h", "facts/h.md", strings.Repeat("a", 64), 1, "attest")

	t.Run("signs with a scrubbed environment", func(t *testing.T) {
		fake := fakeKeygen(t)
		key := attesttest.NewSKEd25519(t)
		dir := t.TempDir()
		handle := filepath.Join(dir, "handle")
		if err := os.WriteFile(handle, key.Handle(0x05), 0o600); err != nil {
			t.Fatal(err)
		}

		for k, v := range map[string]string{
			"SSH_SK_PROVIDER":      "/evil",
			"SSH_SK_HELPER":        "/evil",
			"OPENSSL_CONF":         "/evil",
			"LIBPCSCLITE_DELEGATE": "/evil",
			"SSH_AUTH_SOCK":        "/evil",
			"SSH_ASKPASS":          "/evil",
			"DISPLAY":              ":0",
			"TMPDIR":               "/evil",
		} {
			t.Setenv(k, v)
		}
		var stderr strings.Builder
		sig, err := Keygen{Path: fake, Helper: "/pinned/ssh-sk-helper", Home: "/home/x", Stderr: &stderr}.
			Sign(context.Background(), handle, payload)
		if err != nil {
			t.Fatal(err)
		}
		e, err := attest.ParseEntry(append(append([]byte(nil), payload...), sig...))
		if err != nil {
			t.Fatalf("ParseEntry: %v", err)
		}
		if string(e.Signed) != string(payload) {
			t.Fatalf("Signed = %q, want %q", e.Signed, payload)
		}

		raw, err := os.ReadFile(handle + ".log")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("keygen ran %d times, want 1", len(lines))
		}
		var rec keygenRecord
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatal(err)
		}
		wantEnv := []string{"PATH=" + filepath.Dir(fake), "HOME=/home/x", "LANG=C", "SSH_SK_HELPER=/pinned/ssh-sk-helper"}
		if !reflect.DeepEqual(rec.Env, wantEnv) {
			t.Fatalf("env = %q, want %q", rec.Env, wantEnv)
		}
		wantArgs := []string{"-Y", "sign", "-n", "priors-attest@hookyard", "-f", handle}
		if !reflect.DeepEqual(rec.Args, wantArgs) {
			t.Fatalf("args = %q, want %q", rec.Args, wantArgs)
		}
		if rec.Stdin != string(payload) {
			t.Fatalf("stdin = %q, want %q", rec.Stdin, payload)
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ent := range entries {
			names = append(names, ent.Name())
		}
		if want := []string{"handle", "handle.log"}; !reflect.DeepEqual(names, want) {
			t.Fatalf("handle dir = %v, want %v", names, want)
		}
	})

	t.Run("failure names the keygen error", func(t *testing.T) {
		fake := fakeKeygen(t)
		handle := filepath.Join(t.TempDir(), "handle")
		if err := os.WriteFile(handle, attesttest.NewSKEd25519(t).Handle(0x05), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handle+".fail", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		_, err := Keygen{Path: fake, Helper: "/h", Home: "/home/x", Stderr: &stderr}.
			Sign(context.Background(), handle, payload)
		if err == nil || !strings.Contains(err.Error(), "sign failed") {
			t.Fatalf("err = %v, want one mentioning sign failed", err)
		}
	})

	t.Run("oversized output errs", func(t *testing.T) {
		script := filepath.Join(t.TempDir(), "ssh-keygen")
		body := "#!/bin/sh\ni=0\nwhile [ $i -lt 100 ]; do printf '%050d\\n' 0; i=$((i+1)); done\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		sig, err := Keygen{Path: script, Helper: "/h", Home: "/home/x", Stderr: &stderr}.
			Sign(context.Background(), "/unused", payload)
		if err == nil {
			t.Fatalf("Sign accepted %d bytes, want an error over %d", len(sig), attest.MaxEntryBytes)
		}
	})

	t.Run("RequireTTY without a terminal does not run keygen", func(t *testing.T) {
		old := ttyPath
		ttyPath = filepath.Join(t.TempDir(), "no-tty")
		t.Cleanup(func() { ttyPath = old })

		fake := fakeKeygen(t)
		handle := filepath.Join(t.TempDir(), "handle")
		if err := os.WriteFile(handle, attesttest.NewSKEd25519(t).Handle(0x05), 0o600); err != nil {
			t.Fatal(err)
		}
		k := Keygen{Path: fake, Helper: "/h", Home: "/home/x", RequireTTY: true, Stderr: &strings.Builder{}}
		if err := k.CheckTerminal(); err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
			t.Fatalf("CheckTerminal err = %v", err)
		}
		_, err := k.Sign(context.Background(), handle, payload)
		if err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
			t.Fatalf("Sign err = %v", err)
		}
		if _, err := os.Stat(handle + ".log"); !os.IsNotExist(err) {
			t.Fatalf("keygen ran: stat log err = %v", err)
		}
	})

	var _ Signer = Keygen{}
}
