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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Audience is the single audience of every access token (ADR 0001).
const Audience = "doodle-api"

// Leeway is the clock skew allowed on exp and iat.
const Leeway = 30 * time.Second

// ErrInvalidToken means the token is missing, malformed, expired or not ours. The wrapped
// error says why; it is for logs, never for the client.
var ErrInvalidToken = errors.New("invalid access token")

// Claims is what the notifier needs from a verified access token.
type Claims struct {
	UserID string
	// Name is the display name (may be empty with tokens issued before the claim existed).
	Name  string
	Roles []string
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
	Name  string   `json:"name"`
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
	return Claims{UserID: c.Subject, Name: c.Name, Roles: c.Roles}, nil
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

// KeysMaxAge is how long a fetched key set is used before it is refreshed, so a key that auth
// removes from its JWKS (rotated out, or withdrawn after a leak) stops being accepted within
// this time.
const KeysMaxAge = 10 * time.Minute

// KeysMaxStale is how long the last good key set stays in use while refreshes fail. Past it,
// every token is refused until a fetch succeeds, so blocking the fetch can't keep a withdrawn
// key trusted. Only auth issues access tokens, and they last 15 minutes, so an auth outage
// this long leaves few valid tokens to refuse.
const KeysMaxStale = time.Hour

// errTooSoon means the key set was fetched too recently to fetch it again.
var errTooSoon = errors.New("JWKS fetched too recently to fetch again")

// JWKS fetches auth's key set and caches it for KeysMaxAge. An unknown key id (auth rotated its
// key) triggers a refetch too. Fetches happen at most once per MinRefresh, so forged key ids
// can't make this service hammer auth.
type JWKS struct {
	url        string
	client     *http.Client
	minRefresh time.Duration
	now        func() time.Time // replaced in tests
	fetches    metric.Int64Counter

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time // of the last successful fetch

	fetchMu   sync.Mutex // one fetch at a time
	nextFetch time.Time  // guarded by fetchMu
	failures  int        // consecutive failed fetches, guarded by fetchMu
}

// NewJWKS returns a key source that reads the JWKS document at url.
func NewJWKS(url string, client *http.Client, minRefresh time.Duration) (*JWKS, error) {
	fetches, err := otel.Meter("authn").Int64Counter("authn.jwks.fetches",
		metric.WithDescription("JWKS fetches by reason (max_age, unknown_kid) and result (success, failed)"),
		metric.WithUnit("{fetch}"))
	if err != nil {
		return nil, fmt.Errorf("authn.jwks.fetches: %w", err)
	}
	return &JWKS{url: url, client: client, minRefresh: minRefresh, now: time.Now, fetches: fetches}, nil
}

// Key implements KeySource.
func (j *JWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	k, fetchedAt := j.cached(kid)
	age := j.now().Sub(fetchedAt)
	if k != nil && age < KeysMaxAge {
		return k, nil
	}
	usable := k != nil && age < KeysMaxStale
	if usable {
		// Stale: one request refreshes the set while the others keep using it, rather than all
		// of them waiting for a fetch that can take until its timeout when auth is unreachable.
		if !j.fetchMu.TryLock() {
			return k, nil
		}
	} else {
		j.fetchMu.Lock()
	}
	defer j.fetchMu.Unlock()
	// Another request may have refreshed the set while this one waited, and the new set may
	// have dropped the key, so decide again from what is cached now.
	k, fetchedAt = j.cached(kid)
	age = j.now().Sub(fetchedAt)
	if k != nil && age < KeysMaxAge {
		return k, nil
	}
	usable = k != nil && age < KeysMaxStale
	reason := "unknown_kid"
	if k != nil {
		reason = "max_age"
	}
	keys, err := j.refresh(ctx, reason)
	switch {
	case err != nil && usable:
		return k, nil // the last good set, until KeysMaxStale
	case err != nil && k != nil:
		return nil, fmt.Errorf("signing key %q: key set not refreshed for %s: %w", kid, KeysMaxStale, err)
	case err != nil:
		return nil, fmt.Errorf("unknown signing key %q: %w", kid, err)
	}
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// refresh fetches the key set and caches it, unless the last fetch was too recent: MinRefresh
// after a success; after failures, a second, then doubling up to MinRefresh. The caller holds
// fetchMu.
func (j *JWKS) refresh(ctx context.Context, reason string) (map[string]*rsa.PublicKey, error) {
	now := j.now()
	if now.Before(j.nextFetch) {
		return nil, errTooSoon
	}
	// The new set serves every request, so the caller leaving doesn't cancel the fetch.
	keys, err := j.fetch(context.WithoutCancel(ctx))
	if err != nil {
		j.count(ctx, reason, "failed")
		// Retry soon, since auth may still be starting, but back off while it stays down.
		j.failures++
		j.nextFetch = now.Add(retryDelay(j.failures, j.minRefresh))
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	j.count(ctx, reason, "success")
	j.failures = 0
	j.nextFetch = now.Add(j.minRefresh)
	j.mu.Lock()
	j.keys, j.fetchedAt = keys, now
	j.mu.Unlock()
	return keys, nil
}

// retryDelay is how long to wait after the nth consecutive failed fetch: 1 s, 2 s, 4 s, ...,
// never more than max.
func retryDelay(n int, max time.Duration) time.Duration {
	d := time.Second
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

func (j *JWKS) count(ctx context.Context, reason, result string) {
	j.fetches.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason), attribute.String("result", result)))
}

// cached returns the cached key for kid (nil if there is none) and when the set was fetched.
func (j *JWKS) cached(kid string) (*rsa.PublicKey, time.Time) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.keys[kid], j.fetchedAt
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
