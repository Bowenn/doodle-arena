// Package api serves the developers' REST API (design doc 6.3): webhook endpoints and their
// delivery log, authenticated with API keys.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/apikeys"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/authn"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/secretbox"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/ssrf"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

// Scope an API key needs for this API.
const Scope = "webhooks"

// KeyVerifier checks an API key; *apikeys.Verifier is the real one.
type KeyVerifier interface {
	Verify(ctx context.Context, key string) (apikeys.Identity, error)
}

// TokenVerifier checks an access token (ADR 0001); *authn.Verifier is the real one.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (authn.Claims, error)
}

// API holds the handlers' dependencies.
type API struct {
	Store  *store.Store
	Keys   KeyVerifier
	Tokens TokenVerifier
	Box    *secretbox.Box
	Policy *ssrf.Policy
	Log    *slog.Logger
	Now    func() time.Time
}

// Register adds the routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/notifier/endpoints", a.auth(a.createEndpoint))
	mux.Handle("GET /api/notifier/endpoints", a.auth(a.listEndpoints))
	mux.Handle("DELETE /api/notifier/endpoints/{id}", a.auth(a.deleteEndpoint))
	mux.Handle("GET /api/notifier/endpoints/{id}/deliveries", a.auth(a.listDeliveries))
	mux.Handle("POST /api/notifier/endpoints/{id}/test", a.auth(a.testEndpoint))
	mux.Handle("POST /api/notifier/endpoints/{id}/enable", a.auth(a.enableEndpoint))
}

type handler func(w http.ResponseWriter, r *http.Request, who apikeys.Identity)

// auth requires Authorization: Bearer with an API key that has the webhooks scope (scripts and
// servers), or the web app's access token (its settings page): a session acts as its user with
// every scope of this API.
func (a *API) auth(next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, credential, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "bearer") || credential == "" {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "an API key or access token is required: Authorization: Bearer ...")
			return
		}
		if !strings.HasPrefix(credential, "da_") {
			claims, err := a.Tokens.Verify(r.Context(), credential)
			if err != nil {
				writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid or expired access token")
				return
			}
			a.checkRevoked(w, r, apikeys.Identity{UserID: claims.UserID, Scopes: []string{Scope}, IssuedAt: claims.IssuedAt}, next)
			return
		}
		who, err := a.Keys.Verify(r.Context(), credential)
		switch {
		case errors.Is(err, apikeys.ErrInvalid):
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "unknown or revoked API key")
			return
		case err != nil:
			a.Log.WarnContext(r.Context(), "API key check failed", "err", err)
			writeError(w, r, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "API keys can't be checked right now")
			return
		case !who.HasScope(Scope):
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "this API key lacks the webhooks scope")
			return
		}
		a.checkRevoked(w, r, who, next)
	})
}

// checkRevoked refuses a credential issued at or before its user's credential revocation (ADR
// 0015): the thief's access token, which still verifies for up to 15 minutes, or a revoked API
// key this process still has cached as valid. Endpoint changes check again under the owner's
// lock (store.ErrCredentialsRevoked), so a revocation landing mid-request can't be missed.
func (a *API) checkRevoked(w http.ResponseWriter, r *http.Request, who apikeys.Identity, next handler) {
	revokedAt, revoked, err := a.Store.RevokedAt(r.Context(), who.UserID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if revoked && !who.IssuedAt.After(revokedAt) {
		writeRevoked(w, r)
		return
	}
	next(w, r, who)
}

func writeRevoked(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "this credential was revoked: sign in again")
}

type endpointView struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"event_types"`
	CreatedAt  string   `json:"created_at"`
	// active, or disabled (it receives nothing until enabled: disabled_reason says why).
	Status         string  `json:"status"`
	DisabledReason *string `json:"disabled_reason"`
	Secret         string  `json:"secret,omitempty"`
}

func view(e store.Endpoint) endpointView {
	status := "active"
	if e.DisabledAt != nil {
		status = "disabled"
	}
	return endpointView{ID: e.ID.String(), URL: e.URL, EventTypes: e.EventTypes, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano),
		Status: status, DisabledReason: e.DisabledReason}
}

func (a *API) createEndpoint(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	var in struct {
		URL        string   `json:"url"`
		EventTypes []string `json:"event_types"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "the body must be {\"url\": ..., \"event_types\": [...]}")
		return
	}
	u, err := a.Policy.CheckURL(in.URL)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_URL", err.Error())
		return
	}
	types := slices.Compact(slices.Sorted(slices.Values(in.EventTypes)))
	if len(types) == 0 || slices.ContainsFunc(types, func(t string) bool { return !slices.Contains(webhook.Subscribable, t) }) {
		writeError(w, r, http.StatusBadRequest, "INVALID_EVENT_TYPES", "event_types must be a non-empty subset of "+strings.Join(webhook.Subscribable, ", "))
		return
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	id := uuid.New()
	ct, nonce, err := a.Box.Seal([]byte(secret), []byte(id.String()))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.Store.CreateEndpoint(r.Context(), store.Endpoint{
		ID: id, OwnerID: who.UserID, APIKeyID: who.KeyID, URL: u.String(), EventTypes: types,
		SecretCiphertext: ct, SecretNonce: nonce,
	}, who.IssuedAt)
	if errors.Is(err, store.ErrCredentialsRevoked) {
		writeRevoked(w, r)
		return
	}
	if errors.Is(err, store.ErrTooManyEndpoints) {
		writeError(w, r, http.StatusConflict, "TOO_MANY_ENDPOINTS", "at most "+strconv.Itoa(store.MaxEndpointsPerOwner)+" endpoints per user")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "endpoint registered", "endpoint_id", e.ID, "user_id", who.UserID, "event_types", types)
	out := view(e)
	out.Secret = secret // shown once; stored only encrypted
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) listEndpoints(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	es, err := a.Store.ListEndpoints(r.Context(), who.UserID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]endpointView, len(es))
	for i, e := range es {
		out[i] = view(e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

func (a *API) deleteEndpoint(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteEndpoint(r.Context(), who.UserID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "no such endpoint")
		return
	} else if err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "endpoint deleted", "endpoint_id", id, "user_id", who.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// enableEndpoint makes a disabled endpoint live again with a new signing secret, shown once: the
// old one may be known to whoever had the revoked credentials.
func (a *API) enableEndpoint(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	ct, nonce, err := a.Box.Seal([]byte(secret), []byte(id.String()))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.Store.EnableEndpoint(r.Context(), who.UserID, id, ct, nonce, who.IssuedAt)
	switch {
	case errors.Is(err, store.ErrCredentialsRevoked):
		writeRevoked(w, r)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "no such endpoint")
		return
	case errors.Is(err, store.ErrEndpointActive):
		writeError(w, r, http.StatusConflict, "ENDPOINT_ACTIVE", "the endpoint isn't disabled")
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "endpoint enabled with a new secret", "endpoint_id", e.ID, "user_id", who.UserID)
	out := view(e)
	out.Secret = secret // shown once; stored only encrypted
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

type attemptView struct {
	Attempt    int     `json:"attempt"`
	StatusCode int     `json:"status_code"`
	Error      *string `json:"error"`
	DurationMs int     `json:"duration_ms"`
	At         string  `json:"at"`
}

type deliveryView struct {
	ID             string        `json:"id"`
	EventID        string        `json:"event_id"`
	EventType      string        `json:"event_type"`
	Status         string        `json:"status"`
	Attempts       int           `json:"attempts"`
	LastStatusCode *int          `json:"last_status_code"`
	LastError      *string       `json:"last_error"`
	NextAttemptAt  *string       `json:"next_attempt_at"`
	CreatedAt      string        `json:"created_at"`
	FinishedAt     *string       `json:"finished_at"`
	AttemptLog     []attemptView `json:"attempt_log"`
}

func ts(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func (a *API) listDeliveries(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 50 {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 50")
			return
		}
		limit = n
	}
	before := uuid.Nil
	if c := r.URL.Query().Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err == nil {
			before, err = uuid.ParseBytes(raw)
		}
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
			return
		}
	}
	ds, err := a.Store.ListDeliveries(r.Context(), who.UserID, id, before, limit+1)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "no such endpoint")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var next *string
	if len(ds) > limit {
		ds = ds[:limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(ds[limit-1].ID.String()))
		next = &c
	}
	out := make([]deliveryView, len(ds))
	for i, d := range ds {
		v := deliveryView{
			ID: d.ID.String(), EventID: d.SourceEventID, EventType: d.EventType, Status: d.Status,
			Attempts: d.Attempts, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
			CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339Nano), FinishedAt: ts(d.FinishedAt),
			AttemptLog: make([]attemptView, len(d.AttemptLog)),
		}
		if d.Status == "PENDING" {
			v.NextAttemptAt = ts(&d.NextAttemptAt)
		}
		for j, at := range d.AttemptLog {
			v.AttemptLog[j] = attemptView{Attempt: at.No, StatusCode: at.StatusCode, Error: at.Error, DurationMs: at.DurationMs, At: at.At.UTC().Format(time.RFC3339Nano)}
		}
		out[i] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out, "next_cursor": next})
}

// testEndpoint queues a ping, so a developer can check their receiver and its signature check.
func (a *API) testEndpoint(w http.ResponseWriter, r *http.Request, who apikeys.Identity) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.Store.GetEndpoint(r.Context(), who.UserID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "no such endpoint")
		return
	} else if err != nil {
		a.fail(w, r, err)
		return
	}
	eventID := "ping_" + uuid.NewString()
	data, _ := json.Marshal(map[string]string{"endpoint_id": id.String()})
	body, err := webhook.Body(eventID, webhook.TypePing, a.Now(), data)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := store.Delivery{ID: uuid.New(), EndpointID: id, SourceEventID: eventID, EventType: webhook.TypePing, Payload: body}
	if _, err := a.Store.InsertDeliveries(r.Context(), []store.Delivery{d}); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"delivery_id": d.ID.String(), "event_id": eventID})
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// A malformed id gets the same answer as an unknown one.
		writeError(w, r, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "no such endpoint")
		return uuid.Nil, false
	}
	return id, true
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
	writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers in the shared format (design doc 6.3).
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": r.Header.Get("X-Request-Id"),
	}})
}
