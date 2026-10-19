package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Delivery is one event for one endpoint.
type Delivery struct {
	ID             uuid.UUID
	EndpointID     uuid.UUID
	SourceEventID  string
	EventType      string
	Payload        string
	Traceparent    string // W3C trace context of the queueing span; empty without one
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode *int
	LastError      *string
	FirstAttemptAt *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	AttemptLog     []Attempt
}

// Attempt is one HTTP try of a delivery.
type Attempt struct {
	No         int
	StatusCode int
	Error      *string
	DurationMs int
	At         time.Time
}

// Claimed is a delivery a worker holds, with what it needs to send it.
type Claimed struct {
	Delivery
	URL              string
	SecretCiphertext []byte
	SecretNonce      []byte
}

// InsertDeliveries queues deliveries, skipping any (endpoint, source event) already queued: a
// redelivered Kafka record changes nothing. Returns how many were new.
func (s *Store) InsertDeliveries(ctx context.Context, ds []Delivery) (int, error) {
	if len(ds) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, d := range ds {
		batch.Queue(`INSERT INTO deliveries (id, endpoint_id, source_event_id, event_type, payload, traceparent)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')) ON CONFLICT (endpoint_id, source_event_id) DO NOTHING`,
			d.ID, d.EndpointID, d.SourceEventID, d.EventType, d.Payload, d.Traceparent)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	results := tx.SendBatch(ctx, batch)
	inserted := 0
	for range ds {
		tag, err := results.Exec()
		if err != nil {
			results.Close()
			return 0, fmt.Errorf("insert delivery: %w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	if err := results.Close(); err != nil {
		return 0, fmt.Errorf("insert deliveries: %w", err)
	}
	return inserted, tx.Commit(ctx)
}

// Claim takes up to limit due deliveries of live, enabled endpoints and pushes their next_attempt_at
// forward by lease, so another worker skips them now and takes them again if this one dies
// before recording the attempt. SKIP LOCKED lets several workers (and instances) claim side by
// side without waiting on each other.
func (s *Store) Claim(ctx context.Context, limit int, lease time.Duration) ([]Claimed, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH due AS (
			SELECT d.id FROM deliveries d JOIN endpoints e ON e.id = d.endpoint_id
			WHERE d.status = 'PENDING' AND d.next_attempt_at <= now() AND e.deleted_at IS NULL AND e.disabled_at IS NULL
			ORDER BY d.next_attempt_at
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE deliveries d SET next_attempt_at = now() + make_interval(secs => $2)
		FROM due, endpoints e
		WHERE d.id = due.id AND e.id = d.endpoint_id
		RETURNING d.id, d.endpoint_id, d.source_event_id, d.event_type, d.payload, COALESCE(d.traceparent, ''),
			d.attempts, d.first_attempt_at, d.created_at, e.url, e.secret_ciphertext, e.secret_nonce`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Claimed, error) {
		var c Claimed
		err := r.Scan(&c.ID, &c.EndpointID, &c.SourceEventID, &c.EventType, &c.Payload, &c.Traceparent,
			&c.Attempts, &c.FirstAttemptAt, &c.CreatedAt, &c.URL, &c.SecretCiphertext, &c.SecretNonce)
		c.Status = "PENDING"
		return c, err
	})
}

// Outcome is what an attempt leads to.
type Outcome int

const (
	// Succeeded: a 2xx answer; the delivery is done.
	Succeeded Outcome = iota
	// Retry: try again at the given time.
	Retry
	// Dead: the last attempt failed; the delivery goes to the dead-letter queue.
	Dead
)

// AttemptResult is one finished HTTP try.
type AttemptResult struct {
	StatusCode int // 0 without a response
	Error      string
	Duration   time.Duration
	Outcome    Outcome
	RetryAt    time.Time // for Retry
}

// ErrNotClaimed: the delivery isn't pending any more (its endpoint was deleted meanwhile).
var ErrNotClaimed = errors.New("delivery is no longer pending")

// RecordAttempt logs the attempt and moves the delivery on, in one transaction.
func (s *Store) RecordAttempt(ctx context.Context, id uuid.UUID, attemptNo int, r AttemptResult) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	var errText *string
	if r.Error != "" {
		errText = &r.Error
	}
	var status string
	var next *time.Time
	switch r.Outcome {
	case Succeeded:
		status = "SUCCEEDED"
	case Dead:
		status = "DEAD"
	default:
		status = "PENDING"
		next = &r.RetryAt
	}
	tag, err := tx.Exec(ctx, `
		UPDATE deliveries SET attempts = $2, status = $3, last_status_code = $4, last_error = $5,
			first_attempt_at = COALESCE(first_attempt_at, now()),
			next_attempt_at = COALESCE($6, next_attempt_at),
			finished_at = CASE WHEN $3 = 'PENDING' THEN NULL ELSE now() END
		WHERE id = $1 AND status = 'PENDING'`,
		id, attemptNo, status, r.StatusCode, errText, next)
	if err != nil {
		return fmt.Errorf("update delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotClaimed
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delivery_attempts (delivery_id, attempt_no, status_code, error, duration_ms)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, id, attemptNo, r.StatusCode, errText, r.Duration.Milliseconds()); err != nil {
		return fmt.Errorf("insert attempt: %w", err)
	}
	return tx.Commit(ctx)
}

// ListDeliveries pages an endpoint's deliveries, newest first, with their attempts; before is
// the last id of the previous page (or uuid.Nil).
func (s *Store) ListDeliveries(ctx context.Context, ownerID string, endpointID uuid.UUID, before uuid.UUID, limit int) ([]Delivery, error) {
	if _, err := s.GetEndpoint(ctx, ownerID, endpointID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, endpoint_id, source_event_id, event_type, payload, status, attempts, next_attempt_at,
			last_status_code, last_error, first_attempt_at, finished_at, created_at
		FROM deliveries
		WHERE endpoint_id = $1 AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid
			OR (created_at, id) < (SELECT created_at, id FROM deliveries WHERE id = $2))
		ORDER BY created_at DESC, id DESC LIMIT $3`, endpointID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := r.Scan(&d.ID, &d.EndpointID, &d.SourceEventID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
			&d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.FirstAttemptAt, &d.FinishedAt, &d.CreatedAt)
		return d, err
	})
	if err != nil || len(ds) == 0 {
		return ds, err
	}
	ids := make([]uuid.UUID, len(ds))
	index := make(map[uuid.UUID]int, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
		index[d.ID] = i
	}
	arows, err := s.Pool.Query(ctx, `SELECT delivery_id, attempt_no, status_code, error, duration_ms, at
		FROM delivery_attempts WHERE delivery_id = ANY($1) ORDER BY delivery_id, attempt_no`, ids)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var id uuid.UUID
		var a Attempt
		if err := arows.Scan(&id, &a.No, &a.StatusCode, &a.Error, &a.DurationMs, &a.At); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		i := index[id]
		ds[i].AttemptLog = append(ds[i].AttemptLog, a)
	}
	return ds, arows.Err()
}

// UnpublishedDead returns DEAD deliveries not yet acknowledged on notifier.dlq.v1.
func (s *Store) UnpublishedDead(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, endpoint_id, source_event_id, event_type, attempts, last_status_code, last_error,
			first_attempt_at, finished_at
		FROM deliveries WHERE status = 'DEAD' AND dead_published_at IS NULL AND event_type <> 'ping'
		ORDER BY finished_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("unpublished dead: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := r.Scan(&d.ID, &d.EndpointID, &d.SourceEventID, &d.EventType, &d.Attempts, &d.LastStatusCode,
			&d.LastError, &d.FirstAttemptAt, &d.FinishedAt)
		d.Status = "DEAD"
		return d, err
	})
}

// MarkDeadPublished records that Kafka acknowledged these deliveries' DeliveryDead.
func (s *Store) MarkDeadPublished(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, "UPDATE deliveries SET dead_published_at = now() WHERE id = ANY($1) AND dead_published_at IS NULL", ids)
	if err != nil {
		return fmt.Errorf("mark dead published: %w", err)
	}
	return nil
}

// DeadSince counts deliveries that went DEAD after t, test pings aside (the notifier.dlq.size
// gauge).
func (s *Store) DeadSince(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE status = 'DEAD' AND finished_at > $1 AND event_type <> 'ping'", t).Scan(&n)
	return n, err
}

// OldestDueAge is how long the oldest due delivery has waited for a worker, in seconds: PENDING,
// due, and for a live, enabled endpoint (the deliveries Claim would take). 0 when none is due.
// The notifier.queue.oldest_age gauge: it grows when the workers stop or fall behind.
func (s *Store) OldestDueAge(ctx context.Context) (float64, error) {
	var age float64
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(d.next_attempt_at)), 0)::float8
		FROM deliveries d JOIN endpoints e ON e.id = d.endpoint_id
		WHERE d.status = 'PENDING' AND d.next_attempt_at <= now() AND e.deleted_at IS NULL AND e.disabled_at IS NULL`).Scan(&age)
	return age, err
}
