package ticket

import (
	"context"
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

	"github.com/Bowenn/gap-project_private/services/game-server/internal/redistest"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

func TestMain(m *testing.M) { redistest.Main(m) }

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sign makes a ticket the way the lobby does (services/lobby/internal/ticket), with overrides.
func sign(t *testing.T, key ed25519.PrivateKey, mod func(*claims, *jwt.Token)) string {
	t.Helper()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: Issuer, Audience: jwt.ClaimStrings{Audience}, Subject: "u-1", ID: "jti-1",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(TTL)),
		},
		RoomID: "r1", Name: "Ada",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, &c)
	tok.Header["kid"] = Thumbprint(key.Public().(ed25519.PublicKey))
	if mod != nil {
		mod(&c, tok)
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyAcceptsALobbyTicket(t *testing.T) {
	key := newKey(t)
	v := NewVerifier([]ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, clock(now.Add(30*time.Second)))
	got, err := v.Verify(sign(t, key, nil), "r1")
	if err != nil {
		t.Fatal(err)
	}
	want := Ticket{ID: "jti-1", UserID: "u-1", Name: "Ada", RoomID: "r1", ExpiresAt: now.Add(TTL)}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestVerifyRejects(t *testing.T) {
	key := newKey(t)
	pub := key.Public().(ed25519.PublicKey)
	good := sign(t, key, nil)
	parts := strings.Split(good, ".")
	b64 := base64.RawURLEncoding.EncodeToString
	header := func(alg string) string {
		return b64([]byte(`{"alg":"` + alg + `","kid":"` + Thumbprint(pub) + `","typ":"JWT"}`))
	}
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(header("HS256") + "." + parts[1]))

	tests := []struct {
		name, token, room string
		at                time.Time
	}{
		{"expired", good, "r1", now.Add(TTL + Leeway + time.Second)},
		{"issued in the future", good, "r1", now.Add(-time.Minute)},
		{"another room", good, "r2", now},
		{"another key", sign(t, newKey(t), nil), "r1", now},
		{"forged with the lobby's kid", sign(t, newKey(t), func(_ *claims, tok *jwt.Token) { tok.Header["kid"] = Thumbprint(pub) }), "r1", now},
		{"alg none", header("none") + "." + parts[1] + ".", "r1", now},
		{"HS256 with the public key", header("HS256") + "." + parts[1] + "." + b64(mac.Sum(nil)), "r1", now},
		{"wrong issuer", sign(t, key, func(c *claims, _ *jwt.Token) { c.Issuer = "someone" }), "r1", now},
		{"wrong audience", sign(t, key, func(c *claims, _ *jwt.Token) { c.Audience = jwt.ClaimStrings{"doodle-api"} }), "r1", now},
		{"no subject", sign(t, key, func(c *claims, _ *jwt.Token) { c.Subject = "" }), "r1", now},
		{"no id", sign(t, key, func(c *claims, _ *jwt.Token) { c.ID = "" }), "r1", now},
		{"lifetime over 60 s", sign(t, key, func(c *claims, _ *jwt.Token) { c.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour)) }), "r1", now},
		{"garbage", "not-a-ticket", "r1", now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewVerifier([]ed25519.PublicKey{pub}, clock(tt.at))
			if _, err := v.Verify(tt.token, tt.room); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNamesAreCleaned(t *testing.T) {
	key := newKey(t)
	v := NewVerifier([]ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, clock(now))
	tests := map[string]string{
		"":                      DefaultName,
		"  \u0007 ":             DefaultName,
		" Grace\nHopper ":       "GraceHopper",
		strings.Repeat("x", 40): strings.Repeat("x", 32),
	}
	for in, want := range tests {
		tok := sign(t, key, func(c *claims, _ *jwt.Token) { c.Name = in })
		got, err := v.Verify(tok, "r1")
		if err != nil || got.Name != want {
			t.Errorf("name %q: got %q (err %v), want %q", in, got.Name, err, want)
		}
	}
}

// RFC 8037, appendix A.3; the lobby's test checks the same vector, so both agree on kids.
func TestThumbprintMatchesRFC8037(t *testing.T) {
	x, _ := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if got := Thumbprint(ed25519.PublicKey(x)); got != "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k" {
		t.Fatalf("thumbprint = %s", got)
	}
}

func TestLoadPublicKeysReadsSeveralFiles(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"old.pem", "new.pem"} {
		der, err := x509.MarshalPKIXPublicKey(newKey(t).Public())
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	keys, err := LoadPublicKeys(strings.Join(paths, ", "))
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %d, err = %v", len(keys), err)
	}
	if _, err := LoadPublicKeys(""); err == nil {
		t.Fatal("accepted no keys")
	}
}

func TestRedisRedeemerIsSingleUse(t *testing.T) {
	rdb := redistest.New(t)
	r := NewRedisRedeemer(rdb, clock(now))
	tk := Ticket{ID: "jti-1", UserID: "u-1", RoomID: "r1", ExpiresAt: now.Add(TTL)}
	if err := r.Redeem(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if err := r.Redeem(t.Context(), tk); !errors.Is(err, ErrReused) {
		t.Fatalf("second redeem: err = %v, want ErrReused", err)
	}
	// The record outlives the ticket: exp + leeway, 65 s here.
	ttl := rdb.TTL(t.Context(), Key("r1", "jti-1")).Val()
	if ttl <= TTL || ttl > TTL+Leeway {
		t.Fatalf("ttl = %v, want (60s, 65s]", ttl)
	}
	// Another room's ticket with the same id is a different key.
	if err := r.Redeem(t.Context(), Ticket{ID: "jti-1", RoomID: "r2", ExpiresAt: now.Add(TTL)}); err != nil {
		t.Fatal(err)
	}
}

func TestRedisRedeemerFailsClosed(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer rdb.Close()
	err := NewRedisRedeemer(rdb, clock(now)).Redeem(context.Background(), Ticket{ID: "x", RoomID: "r1", ExpiresAt: now.Add(TTL)})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
