// Package authn verifies the access tokens that the auth service issues (ADR 0001): RS256
// only, against auth's JWKS, with iss, aud, exp and iat checked and 30 seconds of clock skew.
// The shared cases in fixtures/jwt/ pin down exactly what is accepted.
package authn

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audience is the single audience of every access token (ADR 0001).
const Audience = "doodle-api"

// Leeway is the clock skew allowed on exp and iat.
const Leeway = 30 * time.Second

// ErrInvalidToken means the token is missing, malformed, expired or not ours. The wrapped
// error says why; it is for logs, never for the client.
var ErrInvalidToken = errors.New("invalid access token")

// Claims is what the lobby needs from a verified access token.
type Claims struct {
	UserID string
	Roles  []string
}

// KeySource returns the RSA public key for a key id.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Verifier checks access tokens.
type Verifier struct {
	issuer string
	keys   KeySource
}

// NewVerifier returns a verifier for tokens from issuer, signed by keys from keys.
func NewVerifier(issuer string, keys KeySource) *Verifier {
	return &Verifier{issuer: issuer, keys: keys}
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles"`
}

// Verify checks the token and returns its claims, or an error wrapping ErrInvalidToken.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), // never "none", never HMAC
		jwt.WithAudience(Audience),
		jwt.WithIssuer(v.issuer),
		jwt.WithLeeway(Leeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	var c tokenClaims
	_, err := parser.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys.Key(ctx, kid)
	})
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Subject == "" {
		return Claims{}, fmt.Errorf("%w: sub is required", ErrInvalidToken)
	}
	if c.Roles == nil {
		return Claims{}, fmt.Errorf("%w: roles is required", ErrInvalidToken)
	}
	return Claims{UserID: c.Subject, Roles: c.Roles}, nil
}

// StaticKeys is a fixed key set, for tests.
type StaticKeys map[string]*rsa.PublicKey

// Key implements KeySource.
func (s StaticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// JWKS fetches auth's key set and caches it. An unknown key id (auth rotated its key) triggers
// a refetch, at most once per MinRefresh, so forged key ids can't make the lobby hammer auth.
type JWKS struct {
	url        string
	client     *http.Client
	minRefresh time.Duration

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchMu   sync.Mutex // one fetch at a time
	nextFetch time.Time  // guarded by fetchMu
}

// NewJWKS returns a key source that reads the JWKS document at url.
func NewJWKS(url string, client *http.Client, minRefresh time.Duration) *JWKS {
	return &JWKS{url: url, client: client, minRefresh: minRefresh}
}

// Key implements KeySource.
func (j *JWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if k := j.cached(kid); k != nil {
		return k, nil
	}
	j.fetchMu.Lock()
	defer j.fetchMu.Unlock()
	// Another request may have fetched the key while this one waited.
	if k := j.cached(kid); k != nil {
		return k, nil
	}
	if time.Now().Before(j.nextFetch) {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	keys, err := j.fetch(ctx)
	if err != nil {
		// Retry soon: auth may still be starting.
		j.nextFetch = time.Now().Add(time.Second)
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	j.nextFetch = time.Now().Add(j.minRefresh)
	j.mu.Lock()
	j.keys = keys
	j.mu.Unlock()
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (j *JWKS) cached(kid string) *rsa.PublicKey {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.keys[kid]
}

func (j *JWKS) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return ParseJWKS(body)
}

// ParseJWKS reads the RS256 signing keys from a JWKS document, keyed by kid. Other key types
// are skipped.
func ParseJWKS(doc []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(doc, &set); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("key %q: modulus: %w", k.Kid, err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("key %q: exponent: %w", k.Kid, err)
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
			return nil, fmt.Errorf("key %q: bad exponent", k.Kid)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, fmt.Errorf("key %q: modulus shorter than 2048 bits", k.Kid)
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

type ctxKey struct{}

// WithClaims returns a context that carries the caller's verified claims.
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the claims stored by WithClaims.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}
