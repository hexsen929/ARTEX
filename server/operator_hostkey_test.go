package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestOperatorVPSOriginalTOFUAndChangedKey(t *testing.T) {
	newKey := func() ssh.PublicKey {
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewPublicKey(public)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	key, changed := newKey(), newKey()
	path := filepath.Join(t.TempDir(), "app", "vps_known_hosts")
	check := operatorVPSHostKeyCallback(path, "fixture.invalid", nil)
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18895}
	for i := 0; i < 2; i++ {
		if err := check("fixture.invalid:18895", remote, key); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = check("fixture.invalid:18895", remote, changed); err == nil {
		t.Fatal("changed SSH key was trusted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("key mismatch changed TOFU state")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	check = operatorVPSHostKeyCallback(path, "fixture.invalid", map[string]string{"host": "fixture.invalid", "key": ssh.FingerprintSHA256(changed)})
	if err = check("fixture.invalid:18895", remote, changed); err != nil {
		t.Fatal("explicit pin did not override TOFU", err)
	}
	if err = check("fixture.invalid:18895", remote, key); err == nil {
		t.Fatal("explicit pin accepted wrong key")
	}
}
