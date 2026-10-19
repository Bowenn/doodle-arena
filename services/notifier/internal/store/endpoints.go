package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxEndpointsPerOwner caps how many live endpoints one user registers.
const MaxEndpointsPerOwner = 10

var (
	// ErrNotFound: no such live endpoint for this owner.
	ErrNotFound = errors.New("not found")
	// ErrTooManyEndpoints: the owner already has MaxEndpointsPerOwner.
	ErrTooManyEndpoints = errors.New("too many endpoints")
	// ErrEndpointActive: the endpoint isn't disabled, so there is nothing to enable.
	ErrEndpointActive = errors.New("endpoint is active")
	// ErrCredentialsRevoked: the caller's credential was issued at or before the owner's
	// credential revocation (ADR 0015).
	ErrCredentialsRevoked = errors.New("credentials revoked")
)

// Endpoint is a developer's webhook URL and what it subscribes to.
type Endpoint struct {
	ID               uuid.UUID
	OwnerID          string
	APIKeyID         *uuid.UUID // nil when registered with a session (the web app)
	URL              string
	EventTypes       []string
	SecretCiphertext []byte
	SecretNonce      []byte
	CreatedAt        time.Time
	// Set while the endpoint is disabled (credentials_revoked: auth revoked its owner's
	// credentials, ADR 0015); it receives nothing until enabled again.
	DisabledAt     *time.Time
	DisabledReason *string
}

const endpointColumns = "id, owner_id, api_key_id, url, event_types, secret_ciphertext, secret_nonce, created_at, disabled_at, disabled_reason"

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.OwnerID, &e.APIKeyID, &e.URL, &e.EventTypes, &e.SecretCiphertext, &e.SecretNonce, &e.CreatedAt,
		&e.DisabledAt, &e.DisabledReason)
	return e, err
}

// CreateEndpoint stores e, unless its owner already has MaxEndpointsPerOwner or the caller's
// credential (issued at issuedAt) predates the owner's credential revocation. Both checks run
// under the owner's advisory lock, which RevokeCredentials takes too: a registration either
// commits before a revocation (which then disables it) or sees the revocation and is refused.
func (s *Store) CreateEndpoint(ctx context.Context, e Endpoint, issuedAt time.Time) (Endpoint, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Endpoint{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	// An advisory lock per owner serializes registrations of one owner (a count can't lock rows
	// that don't exist yet).
	if err := lockOwner(ctx, tx, e.OwnerID); err != nil {
		return Endpoint{}, err
	}
	if err := checkNotRevoked(ctx, tx, e.OwnerID, issuedAt); err != nil {
		return Endpoint{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM endpoints WHERE owner_id = $1 AND deleted_at IS NULL", e.OwnerID).Scan(&n); err != nil {
		return Endpoint{}, fmt.Errorf("count endpoints: %w", err)
	}
	if n >= MaxEndpointsPerOwner {
		return Endpoint{}, ErrTooManyEndpoints
	}
	created, err := scanEndpoint(tx.QueryRow(ctx, `
		INSERT INTO endpoints (id, owner_id, api_key_id, url, event_types, secret_ciphertext, secret_nonce)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+endpointColumns,
		e.ID, e.OwnerID, e.APIKeyID, e.URL, e.EventTypes, e.SecretCiphertext, e.SecretNonce))
	if err != nil {
		return Endpoint{}, fmt.Errorf("insert endpoint: %w", err)
	}
	return created, tx.Commit(ctx)
}

// ListEndpoints returns the owner's live endpoints, newest first.
func (s *Store) ListEndpoints(ctx context.Context, ownerID string) ([]Endpoint, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+endpointColumns+
		" FROM endpoints WHERE owner_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC, id DESC", ownerID)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(r) })
}

// GetEndpoint returns one of the owner's live endpoints, or ErrNotFound (also for someone
// else's endpoint: ids leak nothing).
func (s *Store) GetEndpoint(ctx context.Context, ownerID string, id uuid.UUID) (Endpoint, error) {
	e, err := scanEndpoint(s.Pool.QueryRow(ctx, "SELECT "+endpointColumns+
		" FROM endpoints WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL", id, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return e, err
}

// DeleteEndpoint marks the endpoint deleted and cancels its pending deliveries, in one
// transaction, so no worker sends to it afterwards.
func (s *Store) DeleteEndpoint(ctx context.Context, ownerID string, id uuid.UUID) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	tag, err := tx.Exec(ctx, "UPDATE endpoints SET deleted_at = now() WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL", id, ownerID)
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'CANCELLED', finished_at = now()
		WHERE endpoint_id = $1 AND status = 'PENDING'`, id); err != nil {
		return fmt.Errorf("cancel deliveries: %w", err)
	}
	return tx.Commit(ctx)
}

// Subscribers returns the live, enabled endpoints of any of these owners that subscribe to
// eventType.
func (s *Store) Subscribers(ctx context.Context, ownerIDs []string, eventType string) ([]Endpoint, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+endpointColumns+` FROM endpoints
		WHERE owner_id = ANY($1) AND $2 = ANY(event_types) AND deleted_at IS NULL AND disabled_at IS NULL`, ownerIDs, eventType)
	if err != nil {
		return nil, fmt.Errorf("subscribers: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(r) })
}
