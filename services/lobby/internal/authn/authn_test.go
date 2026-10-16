package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The shared cases from fixtures/jwt/ (ADR 0001): auth runs the same ones in Python.
func TestSharedJWTFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "fixtures", "jwt")
	jwksDoc, err := os.ReadFile(filepath.Join(dir, "jwks.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseJWKS(jwksDoc)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
		Cases  []struct {
			Name  string   `json:"name"`
			Valid bool     `json:"valid"`
			Token string   `json:"token"`
			Sub   string   `json:"sub"`
			Roles []string `json:"roles"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	v := NewVerifier(doc.Issuer, StaticKeys(keys))
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), c.Token)
			if !c.Valid {
				if !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("accepted an invalid token (err = %v)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected a valid token: %v", err)
			}
			if got.UserID != c.Sub || !slices.Equal(got.Roles, c.Roles) {
				t.Fatalf("claims = %+v, want sub %q roles %v", got, c.Sub, c.Roles)
			}
		})
	}
}

const testIssuer = "http://localhost:8080/api/auth"

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, iat, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u-1", "roles": []string{"player"}, "iss": testIssuer, "aud": Audience,
		"iat": iat.Unix(), "exp": exp.Unix(),
	})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClockSkew(t *testing.T) {
	key := newKey(t)
	v := NewVerifier(testIssuer, StaticKeys{"k1": &key.PublicKey})
	now := time.Now()
	tests := []struct {
		name     string
		iat, exp time.Time
		ok       bool
	}{
		{"expired 10s ago, within leeway", now.Add(-15 * time.Minute), now.Add(-10 * time.Second), true},
		{"expired 60s ago", now.Add(-15 * time.Minute), now.Add(-60 * time.Second), false},
		{"issued 10s in the future, within leeway", now.Add(10 * time.Second), now.Add(15 * time.Minute), true},
		{"issued 60s in the future", now.Add(60 * time.Second), now.Add(15 * time.Minute), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), sign(t, key, "k1", tt.iat, tt.exp))
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok = %v", err, tt.ok)
			}
		})
	}
}

func jwksJSON(kid string, pub *rsa.PublicKey) []byte {
	b64 := base64.RawURLEncoding.EncodeToString
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	return doc
}

// After a key rotation the lobby refetches the JWKS on the new kid, but an unknown kid can't
// make it refetch more than once per MinRefresh.
func TestJWKSRefetchesOnUnknownKidRateLimited(t *testing.T) {
	oldKey, newKeyPair := newKey(t), newKey(t)
	var fetches atomic.Int32
	current := atomic.Pointer[[]byte]{}
	first := jwksJSON("old", &oldKey.PublicKey)
	current.Store(&first)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write(*current.Load())
	}))
	defer srv.Close()

	jwks := NewJWKS(srv.URL, srv.Client(), time.Hour)
	v := NewVerifier(testIssuer, jwks)
	now := time.Now()
	ctx := context.Background()

	if _, err := v.Verify(ctx, sign(t, oldKey, "old", now, now.Add(time.Minute))); err != nil {
		t.Fatalf("old key: %v", err)
	}
	// Cached: a second token with a known kid causes no fetch.
	if _, err := v.Verify(ctx, sign(t, oldKey, "old", now, now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetches = %d, want 1", n)
	}

	// auth rotates; within MinRefresh the new kid isn't fetched yet.
	rotated := jwksJSON("new", &newKeyPair.PublicKey)
	current.Store(&rotated)
	for range 5 {
		if _, err := v.Verify(ctx, sign(t, newKeyPair, "new", now, now.Add(time.Minute))); err == nil {
			t.Fatal("accepted a key before refetching")
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetches = %d, want 1 (rate-limited)", n)
	}

	// Once MinRefresh has passed, the unknown kid triggers exactly one refetch.
	jwks.fetchMu.Lock()
	jwks.nextFetch = time.Time{}
	jwks.fetchMu.Unlock()
	if _, err := v.Verify(ctx, sign(t, newKeyPair, "new", now, now.Add(time.Minute))); err != nil {
		t.Fatalf("new key after refetch: %v", err)
	}
	if n := fetches.Load(); n != 2 {
		t.Fatalf("fetches = %d, want 2", n)
	}
}

func TestParseJWKSRejectsShortKeys(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // deliberately weak for the test
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJWKS(jwksJSON("weak", &k.PublicKey)); err == nil {
		t.Fatal("accepted a 1024-bit key")
	}
}
