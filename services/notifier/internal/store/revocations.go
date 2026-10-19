package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DisabledCredentialsRevoked is the disabled_reason of an endpoint whose owner's credentials auth
// revoked (CredentialsRevoked, ADR 0015).
const DisabledCredentialsRevoked = "credentials_revoked"

// RevokeCredentials handles a CredentialsRevoked: in one transaction it raises the owner's
// watermark to revokedAt, disables all the owner's live endpoints and cancels their pending
// deliveries. A revocation that isn't newer than the stored watermark was handled before (a
// redelivered or replayed record) and changes nothing, so an endpoint the owner enabled again
// stays enabled. Returns how many endpoints it disabled and whether the event was new.
func (s *Store) RevokeCredentials(ctx context.Context, ownerID string, revokedAt time.Time) (int, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := lockOwner(ctx, tx, ownerID); err != nil {
		return 0, false, err
	}
	var owner string
	err = tx.QueryRow(ctx, `
		INSERT INTO revocations (owner_id, revoked_at) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET revoked_at = EXCLUDED.revoked_at
			WHERE revocations.revoked_at < EXCLUDED.revoked_at
		RETURNING owner_id`, ownerID, revokedAt).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil // not newer than the watermark: handled before
	}
	if err != nil {
		return 0, false, fmt.Errorf("raise watermark: %w", err)
	}
	rows, err := tx.Query(ctx, `
		UPDATE endpoints SET disabled_at = now(), disabled_reason = $2
		WHERE owner_id = $1 AND deleted_at IS NULL AND disabled_at IS NULL RETURNING id`, ownerID, DisabledCredentialsRevoked)
	if err != nil {
		return 0, false, fmt.Errorf("disable endpoints: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, false, fmt.Errorf("disable endpoints: %w", err)
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'CANCELLED', finished_at = now()
			WHERE endpoint_id = ANY($1) AND status = 'PENDING'`, ids); err != nil {
			return 0, false, fmt.Errorf("cancel deliveries: %w", err)
		}
	}
	return len(ids), true, tx.Commit(ctx)
}

// RevokedAt returns the owner's newest credential revocation, if any: access tokens issued at
// or before it aren't trusted.
func (s *Store) RevokedAt(ctx context.Context, ownerID string) (time.Time, bool, error) {
	var t time.Time
	err := s.Pool.QueryRow(ctx, "SELECT revoked_at FROM revocations WHERE owner_id = $1", ownerID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("revoked at: %w", err)
	}
	return t, true, nil
}

// EnableEndpoint makes a disabled endpoint live again with a new signing secret (the old one may
// be known to whoever had the credentials). ErrNotFound for an unknown or someone else's
// endpoint, ErrEndpointActive when it isn't disabled, ErrCredentialsRevoked when the caller's
// credential (issued at issuedAt) predates the owner's revocation; checked under the owner's
// lock, like CreateEndpoint.
func (s *Store) EnableEndpoint(ctx context.Context, ownerID string, id uuid.UUID, ciphertext, nonce []byte, issuedAt time.Time) (Endpoint, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Endpoint{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := lockOwner(ctx, tx, ownerID); err != nil {
		return Endpoint{}, err
	}
	if err := checkNotRevoked(ctx, tx, ownerID, issuedAt); err != nil {
		return Endpoint{}, err
	}
	e, err := scanEndpoint(tx.QueryRow(ctx, `
		UPDATE endpoints SET disabled_at = NULL, disabled_reason = NULL, secret_ciphertext = $3, secret_nonce = $4
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL AND disabled_at IS NOT NULL
		RETURNING `+endpointColumns, id, ownerID, ciphertext, nonce))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.GetEndpoint(ctx, ownerID, id); getErr != nil {
			return Endpoint{}, getErr
		}
		return Endpoint{}, ErrEndpointActive
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("enable endpoint: %w", err)
	}
	return e, tx.Commit(ctx)
}

// lockOwner serializes changes to one owner's endpoints (registrations, enables, revocations)
// until the transaction ends.
func lockOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", ownerID); err != nil {
		return fmt.Errorf("lock owner: %w", err)
	}
	return nil
}

// checkNotRevoked refuses a credential issued at or before the owner's newest revocation.
func checkNotRevoked(ctx context.Context, tx pgx.Tx, ownerID string, issuedAt time.Time) error {
	var revokedAt time.Time
	err := tx.QueryRow(ctx, "SELECT revoked_at FROM revocations WHERE owner_id = $1", ownerID).Scan(&revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("revoked at: %w", err)
	}
	if !issuedAt.After(revokedAt) {
		return ErrCredentialsRevoked
	}
	return nil
}
