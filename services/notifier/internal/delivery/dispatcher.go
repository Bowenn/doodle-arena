// Package delivery sends queued webhooks (design doc 4.7, ADR 0014): signed POSTs with retries
// on a full-jitter exponential backoff, through a client that can't reach internal addresses,
// until a 2xx answer or the last attempt, after which the delivery is DEAD.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Bowenn/doodle-arena/services/notifier/internal/secretbox"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/ssrf"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/store"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/webhook"
)

// Config tunes the dispatcher. Defaults() is what the service runs with (design doc 4.7).
type Config struct {
	Workers     int           // deliveries sent at the same time
	Batch       int           // deliveries claimed per poll
	Poll        time.Duration // how often to look for due deliveries when idle
	Lease       time.Duration // how long a claim lasts before another worker may retry it
	Base        time.Duration // backoff base
	Cap         time.Duration // backoff cap
	MaxAttempts int
	Timeout     time.Duration // per attempt
}

// Defaults: base 2 s, cap 30 min, 8 attempts, 5 s timeouts.
func Defaults() Config {
	return Config{Workers: 8, Batch: 32, Poll: 500 * time.Millisecond, Lease: 60 * time.Second,
		Base: 2 * time.Second, Cap: 30 * time.Minute, MaxAttempts: 8, Timeout: 5 * time.Second}
}

// Backoff is the wait before the next attempt after `attempts` failed ones: full jitter, a
// uniformly random time in [0, min(cap, base·2^(attempts-1))). The randomness spreads retries
// of a recovering endpoint instead of hitting it in waves.
func Backoff(cfg Config, attempts int, r *rand.Rand) time.Duration {
	ceiling := cfg.Cap
	if attempts-1 < 32 {
		if d := cfg.Base << (attempts - 1); d > 0 && d < cfg.Cap {
			ceiling = d
		}
	}
	return time.Duration(r.Int64N(int64(ceiling)))
}

// Dispatcher claims due deliveries and sends them.
type Dispatcher struct {
	store  *store.Store
	box    *secretbox.Box
	client *http.Client
	cfg    Config
	log    *slog.Logger
	tracer trace.Tracer
	now    func() time.Time

	mu  sync.Mutex
	rnd *rand.Rand

	results  metric.Int64Counter
	duration metric.Float64Histogram
}

// NewDispatcher returns a dispatcher whose HTTP client dials through the SSRF policy, never
// follows redirects (a redirect could point inside), and sends no proxy or trace headers.
func NewDispatcher(st *store.Store, box *secretbox.Box, policy *ssrf.Policy, cfg Config, log *slog.Logger) (*Dispatcher, error) {
	m := otel.Meter("notifier")
	results, err := m.Int64Counter("notifier.deliveries",
		metric.WithDescription("Webhook attempts by result: success, retry (failed, will try again) or dead (failed for the last time)"),
		metric.WithUnit("{attempt}"))
	if err != nil {
		return nil, err
	}
	duration, err := m.Float64Histogram("notifier.delivery.duration",
		metric.WithDescription("Time from sending a webhook to its answer (or failure)"), metric.WithUnit("ms"))
	if err != nil {
		return nil, err
	}
	// The queue's own health, for the WebhookQueueStuck alert: unlike the dead-letter count, it
	// grows only when the notifier itself falls behind, not when a developer's server is down.
	_, err = m.Float64ObservableGauge("notifier.queue.oldest_age",
		metric.WithDescription("Seconds the oldest due delivery has waited for a worker (0 when none is due)"),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			age, err := st.OldestDueAge(ctx)
			if err != nil {
				return nil // no value rather than a wrong one
			}
			o.Observe(age)
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("notifier.queue.oldest_age: %w", err)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a 3xx counts as a failed attempt
		},
	}
	return &Dispatcher{store: st, box: box, client: client, cfg: cfg, log: log, tracer: otel.Tracer("notifier"),
		now: time.Now, rnd: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), results: results, duration: duration}, nil
}

// Run claims and sends until ctx is cancelled; attempts in flight finish first.
func (d *Dispatcher) Run(ctx context.Context) {
	slots := make(chan struct{}, d.cfg.Workers)
	var inflight sync.WaitGroup
	defer inflight.Wait()
	for {
		free := d.cfg.Workers - len(slots)
		claimed := 0
		if free > 0 {
			batch, err := d.store.Claim(ctx, min(free, d.cfg.Batch), d.cfg.Lease)
			if err != nil && ctx.Err() == nil {
				d.log.WarnContext(ctx, "claiming deliveries failed", "err", err)
			}
			claimed = len(batch)
			for _, c := range batch {
				slots <- struct{}{}
				inflight.Go(func() {
					defer func() { <-slots }()
					// Not ctx: an attempt that started finishes and is recorded during shutdown.
					d.Deliver(context.WithoutCancel(ctx), c)
				})
			}
		}
		if claimed > 0 {
			continue // more may be due right away
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.cfg.Poll):
		}
	}
}

// Deliver sends one claimed delivery and records the attempt.
func (d *Dispatcher) Deliver(ctx context.Context, c store.Claimed) {
	parent := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": c.Traceparent})
	attempt := c.Attempts + 1
	ctx, span := d.tracer.Start(parent, "notifier.deliver", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("delivery.id", c.ID.String()),
			attribute.String("endpoint.id", c.EndpointID.String()),
			attribute.String("event.type", c.EventType),
			attribute.Int("delivery.attempt", attempt),
		))
	defer span.End()

	started := d.now()
	status, sendErr := d.send(ctx, c)
	elapsed := d.now().Sub(started)

	result := store.AttemptResult{StatusCode: status, Duration: elapsed}
	label := "success"
	switch {
	case sendErr == nil && status >= 200 && status < 300:
		result.Outcome = store.Succeeded
	case attempt >= d.cfg.MaxAttempts:
		result.Outcome, label = store.Dead, "dead"
	default:
		d.mu.Lock()
		wait := Backoff(d.cfg, attempt, d.rnd)
		d.mu.Unlock()
		result.Outcome, label, result.RetryAt = store.Retry, "retry", d.now().Add(wait)
	}
	if result.Outcome != store.Succeeded {
		result.Error = describe(status, sendErr)
		span.SetStatus(codes.Error, result.Error)
	}
	span.SetAttributes(attribute.Int("http.response.status_code", status), attribute.String("result", label))
	d.duration.Record(ctx, float64(elapsed.Microseconds())/1000)
	d.results.Add(ctx, 1, metric.WithAttributes(attribute.String("result", label)))

	if err := d.store.RecordAttempt(ctx, c.ID, attempt, result); err != nil {
		if errors.Is(err, store.ErrNotClaimed) {
			return // its endpoint was deleted meanwhile
		}
		// The lease runs out and the delivery is tried again: at least once.
		d.log.ErrorContext(ctx, "recording an attempt failed", "delivery_id", c.ID, "err", err)
		return
	}
	logArgs := []any{"delivery_id", c.ID, "endpoint_id", c.EndpointID, "event_id", c.SourceEventID,
		"attempt", attempt, "status_code", status, "duration_ms", elapsed.Milliseconds()}
	switch result.Outcome {
	case store.Succeeded:
		d.log.InfoContext(ctx, "webhook delivered", logArgs...)
	case store.Retry:
		d.log.InfoContext(ctx, "webhook failed, will retry", append(logArgs, "error", result.Error, "retry_at", result.RetryAt)...)
	case store.Dead:
		d.log.WarnContext(ctx, "webhook dead after the last attempt", append(logArgs, "error", result.Error)...)
	}
}

// send makes one POST; it returns the status code (0 without a response).
func (d *Dispatcher) send(ctx context.Context, c store.Claimed) (int, error) {
	secret, err := d.box.Open(c.SecretCiphertext, c.SecretNonce, []byte(c.EndpointID.String()))
	if err != nil {
		return 0, fmt.Errorf("signing secret: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, strings.NewReader(c.Payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DoodleArena-Webhooks/1")
	req.Header.Set("Doodle-Signature", webhook.Signature(string(secret), d.now(), c.Payload))
	req.Header.Set("Doodle-Event-Id", c.SourceEventID)
	req.Header.Set("Doodle-Event-Type", c.EventType)
	req.Header.Set("Doodle-Delivery-Id", c.ID.String())
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // reuse the connection; never stored
	return resp.StatusCode, nil
}

// describe says why an attempt failed, briefly and without response bodies or secrets.
func describe(status int, err error) string {
	var nerr net.Error
	switch {
	case err == nil:
		return "HTTP " + strconv.Itoa(status)
	case errors.Is(err, ssrf.ErrBlocked):
		return "blocked: the address is internal"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()):
		return "timeout"
	case errors.Is(err, secretbox.ErrOpen):
		return "signing secret unavailable"
	default:
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return msg
	}
}
