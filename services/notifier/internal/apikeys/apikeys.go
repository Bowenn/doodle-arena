// Package apikeys checks developers' API keys with auth's internal endpoint (design doc 4.2,
// ADR 0001) and caches the answers briefly, so a burst of requests costs one call to auth.
package apikeys

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Identity is who a valid key acts for.
type Identity struct {
	KeyID  uuid.UUID `json:"key_id"`
	UserID string    `json:"user_id"`
	Scopes []string  `json:"scopes"`
}

// HasScope reports whether the key was granted scope.
func (i Identity) HasScope(scope string) bool {
	for _, s := range i.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

var (
	// ErrInvalid: auth doesn't know the key, or it was revoked.
	ErrInvalid = errors.New("invalid API key")
	// ErrUnavailable: auth couldn't be asked; the caller answers 503, never "valid".
	ErrUnavailable = errors.New("API key check unavailable")
)

const (
	positiveTTL = 30 * time.Second
	// Short, so a key that was just created works almost at once.
	negativeTTL = 5 * time.Second
	maxEntries  = 10_000
)

type entry struct {
	id      Identity
	valid   bool
	expires time.Time
}

// Verifier asks auth and remembers the answer. A revoked key keeps working for at most
// positiveTTL; that's the price of not calling auth on every request.
type Verifier struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]entry
}

// NewVerifier calls POST <authInternalURL>/internal/api-keys/verify.
func NewVerifier(authInternalURL string) *Verifier {
	return &Verifier{
		url:    strings.TrimRight(authInternalURL, "/") + "/internal/api-keys/verify",
		client: &http.Client{Timeout: 2 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		now:    time.Now,
		cache:  map[[32]byte]entry{},
	}
}

// Verify returns the key's identity, ErrInvalid or ErrUnavailable. The cache is keyed by the
// key's SHA-256, so the process never keeps the key itself around.
func (v *Verifier) Verify(ctx context.Context, key string) (Identity, error) {
	h := sha256.Sum256([]byte(key))
	v.mu.Lock()
	e, ok := v.cache[h]
	v.mu.Unlock()
	if ok && v.now().Before(e.expires) {
		if !e.valid {
			return Identity{}, ErrInvalid
		}
		return e.id, nil
	}

	id, err := v.ask(ctx, key)
	if err != nil && !errors.Is(err, ErrInvalid) {
		return Identity{}, err // not cached: the next request asks again
	}
	ttl := positiveTTL
	if err != nil {
		ttl = negativeTTL
	}
	v.mu.Lock()
	if len(v.cache) >= maxEntries {
		clear(v.cache) // crude, but bounded; a cold cache only costs calls to auth
	}
	v.cache[h] = entry{id: id, valid: err == nil, expires: v.now().Add(ttl)}
	v.mu.Unlock()
	return id, err
}

func (v *Verifier) ask(ctx context.Context, key string) (Identity, error) {
	body, _ := json.Marshal(map[string]string{"key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url, bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var id Identity
		if err := json.NewDecoder(resp.Body).Decode(&id); err != nil || id.UserID == "" {
			return Identity{}, fmt.Errorf("%w: unreadable answer", ErrUnavailable)
		}
		return id, nil
	case http.StatusUnauthorized:
		return Identity{}, ErrInvalid
	default:
		return Identity{}, fmt.Errorf("%w: auth answered %d", ErrUnavailable, resp.StatusCode)
	}
}
