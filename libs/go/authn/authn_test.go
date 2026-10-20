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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The shared cases from fixtures/jwt/ (ADR 0001): auth runs the same ones in Python.
func TestSharedJWTFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "fixtures", "jwt")
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
		"sub": "u-1", "name": "Ada", "roles": []string{"player"}, "iss": testIssuer, "aud": Audience,
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
			c, err := v.Verify(context.Background(), sign(t, key, "k1", tt.iat, tt.exp))
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok = %v", err, tt.ok)
			}
			if tt.ok && c.Name != "Ada" {
				t.Fatalf("name = %q, want the token's name claim", c.Name)
			}
		})
	}
}

func jwksJSON(keys map[string]*rsa.PublicKey) []byte {
	b64 := base64.RawURLEncoding.EncodeToString
	set := []map[string]string{}
	for kid, pub := range keys {
		set = append(set, map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	doc, _ := json.Marshal(map[string]any{"keys": set})
	return doc
}

// fakeAuth serves a JWKS document that a test can change, or fail with 503.
type fakeAuth struct {
	fetches atomic.Int32
	doc     atomic.Pointer[[]byte]
	failing atomic.Bool
	url     string
	client  *http.Client
}

func newFakeAuth(t *testing.T, keys map[string]*rsa.PublicKey) *fakeAuth {
	t.Helper()
	f := &fakeAuth{}
	f.serve(keys)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		if f.failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(*f.doc.Load())
	}))
	t.Cleanup(srv.Close)
	f.url, f.client = srv.URL, srv.Client()
	return f
}

func (f *fakeAuth) serve(keys map[string]*rsa.PublicKey) {
	doc := jwksJSON(keys)
	f.doc.Store(&doc)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestJWKS returns a JWKS reading from auth, on a fake clock.
func newTestJWKS(t *testing.T, auth *fakeAuth, minRefresh time.Duration) (*JWKS, *fakeClock) {
	t.Helper()
	jwks, err := NewJWKS(auth.url, auth.client, minRefresh)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	jwks.now = clock.Now
	return jwks, clock
}

// validToken returns a token signed by key that is valid by the real clock for the whole test;
// only the JWKS cache runs on the fake clock.
func validToken(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	now := time.Now()
	return sign(t, key, kid, now, now.Add(time.Hour))
}

func wantFetches(t *testing.T, auth *fakeAuth, want int32) {
	t.Helper()
	if n := auth.fetches.Load(); n != want {
		t.Fatalf("fetches = %d, want %d", n, want)
	}
}

// After a key rotation the service refetches the JWKS on the new kid, but an unknown kid can't
// make it refetch more than once per MinRefresh.
func TestJWKSRefetchesOnUnknownKidRateLimited(t *testing.T) {
	oldKey, newKeyPair := newKey(t), newKey(t)
	auth := newFakeAuth(t, map[string]*rsa.PublicKey{"old": &oldKey.PublicKey})
	jwks, clock := newTestJWKS(t, auth, 30*time.Second)
	v := NewVerifier(testIssuer, jwks)
	ctx := context.Background()

	if _, err := v.Verify(ctx, validToken(t, oldKey, "old")); err != nil {
		t.Fatalf("old key: %v", err)
	}
	// Cached: a second token with a known kid causes no fetch.
	if _, err := v.Verify(ctx, validToken(t, oldKey, "old")); err != nil {
		t.Fatal(err)
	}
	wantFetches(t, auth, 1)

	// auth rotates; within MinRefresh the new kid isn't fetched yet.
	auth.serve(map[string]*rsa.PublicKey{"new": &newKeyPair.PublicKey})
	for range 5 {
		if _, err := v.Verify(ctx, validToken(t, newKeyPair, "new")); err == nil {
			t.Fatal("accepted a key before refetching")
		}
	}
	wantFetches(t, auth, 1)

	// Once MinRefresh has passed, the unknown kid triggers exactly one refetch.
	clock.Advance(31 * time.Second)
	if _, err := v.Verify(ctx, validToken(t, newKeyPair, "new")); err != nil {
		t.Fatalf("new key after refetch: %v", err)
	}
	wantFetches(t, auth, 2)
}

// A key that auth removes from its JWKS stops being accepted once the cached set is older than
// KeysMaxAge, and the set isn't refetched on every request before that.
func TestJWKSDropsRemovedKeyAfterMaxAge(t *testing.T) {
	oldKey, newKeyPair := newKey(t), newKey(t)
	auth := newFakeAuth(t, map[string]*rsa.PublicKey{"old": &oldKey.PublicKey, "new": &newKeyPair.PublicKey})
	jwks, clock := newTestJWKS(t, auth, 30*time.Second)
	v := NewVerifier(testIssuer, jwks)
	ctx := context.Background()
	oldToken, newToken := validToken(t, oldKey, "old"), validToken(t, newKeyPair, "new")

	if _, err := v.Verify(ctx, oldToken); err != nil {
		t.Fatalf("old key: %v", err)
	}
	// auth withdraws the old key. Until KeysMaxAge the cached set still has it, and a hundred
	// requests spread over that time cause no fetch.
	auth.serve(map[string]*rsa.PublicKey{"new": &newKeyPair.PublicKey})
	for range 100 {
		clock.Advance((KeysMaxAge - time.Second) / 100)
		if _, err := v.Verify(ctx, oldToken); err != nil {
			t.Fatalf("old key within the max age: %v", err)
		}
		if _, err := v.Verify(ctx, newToken); err != nil {
			t.Fatalf("new key: %v", err)
		}
	}
	wantFetches(t, auth, 1)

	// Past KeysMaxAge one request refreshes the set, and the old key is gone.
	clock.Advance(time.Second)
	if _, err := v.Verify(ctx, oldToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("accepted a withdrawn key after the max age (err = %v)", err)
	}
	wantFetches(t, auth, 2)
	// The refreshed set is cached again: no fetch per request.
	for range 10 {
		if _, err := v.Verify(ctx, newToken); err != nil {
			t.Fatalf("new key after the refresh: %v", err)
		}
	}
	wantFetches(t, auth, 2)
}

// While auth can't be reached the last good set stays in use, but only until KeysMaxStale;
// then every token is refused until a fetch succeeds.
func TestJWKSKeepsLastGoodSetUntilMaxStale(t *testing.T) {
	key := newKey(t)
	auth := newFakeAuth(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	jwks, clock := newTestJWKS(t, auth, 30*time.Second)
	v := NewVerifier(testIssuer, jwks)
	ctx := context.Background()
	token := validToken(t, key, "k1")

	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	auth.failing.Store(true)

	clock.Advance(KeysMaxAge)
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatalf("refused while the last good set is still usable: %v", err)
	}
	wantFetches(t, auth, 2) // the refresh failed
	// The next attempt waits a second (then longer), and requests meanwhile use the cached set.
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	wantFetches(t, auth, 2)

	clock.Advance(KeysMaxStale - KeysMaxAge - time.Second)
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatalf("refused just before the max staleness: %v", err)
	}
	wantFetches(t, auth, 3)

	// Past the max staleness the set is refused, even before the next fetch is due (the second
	// failure backed off to 2 s).
	clock.Advance(time.Second)
	if _, err := v.Verify(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("accepted a key from a set past the max staleness (err = %v)", err)
	}
	wantFetches(t, auth, 3)

	// auth is back: the next fetch, 2 s after the last failure, restores the set.
	auth.failing.Store(false)
	clock.Advance(time.Second)
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatalf("refused after auth came back: %v", err)
	}
	wantFetches(t, auth, 4)
}

// While auth stays down, fetches back off: 1 s, 2 s, 4 s ... up to MinRefresh, so an outage
// costs auth at most one fetch per MinRefresh per process.
func TestJWKSBacksOffWhileAuthIsDown(t *testing.T) {
	key := newKey(t)
	auth := newFakeAuth(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	auth.failing.Store(true)
	jwks, clock := newTestJWKS(t, auth, 30*time.Second)
	v := NewVerifier(testIssuer, jwks)
	ctx := context.Background()
	token := validToken(t, key, "k1")

	var gaps []time.Duration
	last, fetches := clock.Now(), int32(0)
	for range 150 { // two and a half minutes, one request a second
		_, _ = v.Verify(ctx, token)
		if n := auth.fetches.Load(); n != fetches {
			if fetches > 0 {
				gaps = append(gaps, clock.Now().Sub(last))
			}
			last, fetches = clock.Now(), n
		}
		clock.Advance(time.Second)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(gaps, want) {
		t.Fatalf("gaps between fetches = %v, want %v", gaps, want)
	}

	auth.failing.Store(false)
	clock.Advance(30 * time.Second)
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatalf("refused after auth came back: %v", err)
	}
}

// While one request refreshes a stale set, the others keep using it instead of waiting for
// the fetch.
func TestJWKSStaleRefreshDoesNotBlockOtherRequests(t *testing.T) {
	key := newKey(t)
	doc := jwksJSON(map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	var fetches atomic.Int32
	inFetch, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 2 {
			close(inFetch)
			<-release
		}
		_, _ = w.Write(doc)
	}))
	defer srv.Close()
	jwks, err := NewJWKS(srv.URL, srv.Client(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	jwks.now = clock.Now
	v := NewVerifier(testIssuer, jwks)
	ctx := context.Background()
	token := validToken(t, key, "k1")

	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	clock.Advance(KeysMaxAge)
	refreshed := make(chan error)
	go func() {
		_, err := v.Verify(ctx, token)
		refreshed <- err
	}()
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFetch() // so a failed test doesn't leave the handler waiting
	select {
	case <-inFetch: // the refresh is in flight and holds the fetch lock
	case <-time.After(5 * time.Second):
		t.Fatal("the stale set wasn't refreshed")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			_, err := v.Verify(ctx, token)
			errs <- err
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second): // well under the fetch's 3-second timeout
		t.Fatal("requests waited for the refresh instead of using the cached set")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a request during the refresh failed: %v", err)
		}
	}
	releaseFetch()
	if err := <-refreshed; err != nil {
		t.Fatalf("the refreshing request: %v", err)
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
	if _, err := ParseJWKS(jwksJSON(map[string]*rsa.PublicKey{"weak": &k.PublicKey})); err == nil {
		t.Fatal("accepted a 1024-bit key")
	}
}

// A token without iat passes by default (the lobby compares nothing with it) and is refused
// with RequireIssuedAt (the notifier compares it with credential revocations, ADR 0015).
func TestRequireIssuedAt(t *testing.T) {
	key := newKey(t)
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u-1", "roles": []string{"player"}, "iss": testIssuer, "aud": Audience,
		"exp": now.Add(time.Minute).Unix(),
	})
	tok.Header["kid"] = "k1"
	noIat, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	keys := StaticKeys{"k1": &key.PublicKey}

	c, err := NewVerifier(testIssuer, keys).Verify(t.Context(), noIat)
	if err != nil || !c.IssuedAt.IsZero() {
		t.Fatalf("default verifier: claims %+v, err %v; want accepted with a zero IssuedAt", c, err)
	}
	if _, err := NewVerifier(testIssuer, keys, RequireIssuedAt()).Verify(t.Context(), noIat); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("RequireIssuedAt: err = %v, want ErrInvalidToken", err)
	}
	iat := now.Add(-time.Minute).Truncate(time.Second)
	c, err = NewVerifier(testIssuer, keys, RequireIssuedAt()).Verify(t.Context(), sign(t, key, "k1", iat, now.Add(time.Minute)))
	if err != nil || !c.IssuedAt.Equal(iat) {
		t.Fatalf("RequireIssuedAt with iat: IssuedAt %v, err %v; want %v", c.IssuedAt, err, iat)
	}
}
