package ticket

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestSignedTicketVerifies(t *testing.T) {
	key := newKey(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := NewSigner(key, fixedClock(now))
	tok, exp, err := s.Sign("u-1", "Ada", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("exp = %v, want 60 s after issue", exp)
	}
	v := NewVerifier([]ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, fixedClock(now.Add(30*time.Second)))
	c, err := v.Verify(tok, "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "u-1" || c.Name != "Ada" || c.RoomID != "r-1" || len(c.ID) != 32 {
		t.Fatalf("claims = %+v", c)
	}
}

func TestTicketIDsAreUnique(t *testing.T) {
	s := NewSigner(newKey(t), time.Now)
	v := NewVerifier([]ed25519.PublicKey{s.key.Public().(ed25519.PublicKey)}, time.Now)
	seen := map[string]bool{}
	for range 100 {
		tok, _, err := s.Sign("u-1", "Ada", "r-1")
		if err != nil {
			t.Fatal(err)
		}
		c, err := v.Verify(tok, "r-1")
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate ticket id %s", c.ID)
		}
		seen[c.ID] = true
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestVerifyRejects(t *testing.T) {
	key := newKey(t)
	pub := key.Public().(ed25519.PublicKey)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	signer := NewSigner(key, fixedClock(now))
	good, _, err := signer.Sign("u-1", "Ada", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := NewSigner(newKey(t), fixedClock(now)).Sign("u-1", "Ada", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	// Signed by another key but claiming the lobby's kid.
	forgedTok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: Issuer, Audience: jwt.ClaimStrings{Audience}, Subject: "u-1", ID: "x",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(TTL))},
		RoomID: "r-1",
	})
	forgedTok.Header["kid"] = signer.KeyID()
	forged, err := forgedTok.SignedString(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	header := func(alg string) string {
		return b64([]byte(`{"alg":"` + alg + `","kid":"` + signer.KeyID() + `","typ":"JWT"}`))
	}
	algNone := header("none") + "." + parts[1] + "."
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(header("HS256") + "." + parts[1]))
	hs256 := header("HS256") + "." + parts[1] + "." + b64(mac.Sum(nil))
	tampered := parts[0] + "." + b64([]byte(`{"sub":"u-2","room_id":"r-1"}`)) + "." + parts[2]
	longLived := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{
			RegisteredClaims: jwt.RegisteredClaims{Issuer: Issuer, Audience: jwt.ClaimStrings{Audience}, Subject: "u-1", ID: "x",
				IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))},
			RoomID: "r-1",
		})
		tok.Header["kid"] = signer.KeyID()
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}()

	tests := []struct {
		name  string
		token string
		room  string
		at    time.Time
	}{
		{"expired", good, "r-1", now.Add(TTL + Leeway + time.Second)},
		{"issued in the future", good, "r-1", now.Add(-time.Minute)},
		{"another room", good, "r-2", now},
		{"another key", other, "r-1", now},
		{"forged with the lobby's kid", forged, "r-1", now},
		{"tampered payload", tampered, "r-1", now},
		{"alg none", algNone, "r-1", now},
		{"HS256 with the public key", hs256, "r-1", now},
		{"lifetime over 60 s", longLived, "r-1", now},
		{"garbage", "not-a-ticket", "r-1", now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewVerifier([]ed25519.PublicKey{pub}, fixedClock(tt.at))
			if _, err := v.Verify(tt.token, tt.room); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// Rotation: a verifier that holds the old and the new public key accepts both.
func TestVerifierAcceptsEveryConfiguredKey(t *testing.T) {
	oldKey, newKey := newKey(t), newKey(t)
	v := NewVerifier([]ed25519.PublicKey{oldKey.Public().(ed25519.PublicKey), newKey.Public().(ed25519.PublicKey)}, time.Now)
	for _, k := range []ed25519.PrivateKey{oldKey, newKey} {
		tok, _, err := NewSigner(k, time.Now).Sign("u-1", "Ada", "r-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(tok, "r-1"); err != nil {
			t.Fatal(err)
		}
	}
}

// RFC 8037, appendix A.3: the thumbprint of the example Ed25519 key.
func TestThumbprintMatchesRFC8037(t *testing.T) {
	x, err := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Thumbprint(ed25519.PublicKey(x)), "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"; got != want {
		t.Fatalf("thumbprint = %s, want %s", got, want)
	}
}

func TestLoadSignerReadsPKCS8PEM(t *testing.T) {
	key := newKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ticket.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID() != Thumbprint(key.Public().(ed25519.PublicKey)) {
		t.Fatal("loaded a different key")
	}
}
