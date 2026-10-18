package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 16)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k) // what make up puts in .env
}

func TestRoundTrip(t *testing.T) {
	box, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := box.Seal([]byte("whsec_test"), []byte("endpoint-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("whsec_test")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := box.Open(ct, nonce, []byte("endpoint-1"))
	if err != nil || string(got) != "whsec_test" {
		t.Fatalf("open = %q, %v", got, err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	box, _ := New(newKey(t))
	ct, nonce, _ := box.Seal([]byte("whsec_test"), []byte("endpoint-1"))
	other, _ := New(newKey(t))

	flipped := bytes.Clone(ct)
	flipped[0] ^= 1
	cases := map[string]func() ([]byte, error){
		"another endpoint":   func() ([]byte, error) { return box.Open(ct, nonce, []byte("endpoint-2")) },
		"changed byte":       func() ([]byte, error) { return box.Open(flipped, nonce, []byte("endpoint-1")) },
		"another key":        func() ([]byte, error) { return other.Open(ct, nonce, []byte("endpoint-1")) },
		"wrong nonce length": func() ([]byte, error) { return box.Open(ct, nonce[:4], []byte("endpoint-1")) },
	}
	for name, open := range cases {
		if _, err := open(); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: err = %v, want ErrOpen", name, err)
		}
	}
}

func TestNoncesAreFresh(t *testing.T) {
	box, _ := New(newKey(t))
	_, n1, _ := box.Seal([]byte("a"), nil)
	_, n2, _ := box.Seal([]byte("a"), nil)
	if bytes.Equal(n1, n2) {
		t.Fatal("two seals used the same nonce")
	}
}

func TestBadKeysAreRefused(t *testing.T) {
	for _, key := range []string{"", "short", strings.Repeat("a", MinKeyLength-1)} {
		if _, err := New(key); err == nil {
			t.Errorf("New(%q) accepted", key)
		}
	}
}
