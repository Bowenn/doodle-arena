package delivery_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/delivery"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/kafkatest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/pgtest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/secretbox"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/ssrf"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

func TestBackoffStaysInItsBounds(t *testing.T) {
	cfg := delivery.Defaults()
	r := mrand.New(mrand.NewPCG(1, 2))
	for attempts := 1; attempts <= 40; attempts++ {
		ceiling := time.Duration(math.Min(float64(cfg.Cap), float64(cfg.Base)*math.Pow(2, float64(attempts-1))))
		for range 200 {
			d := delivery.Backoff(cfg, attempts, r)
			if d < 0 || d >= ceiling {
				t.Fatalf("attempts %d: %v not in [0, %v)", attempts, d, ceiling)
			}
		}
	}
}

type receiver struct {
	srv    *httptest.Server
	status atomic.Int32
	secret atomic.Value
	mu     sync.Mutex
	got    []time.Time
	bad    atomic.Int32
}

// newReceiver verifies every request's Doodle-Signature like a developer's endpoint would.
func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.status.Store(200)
	rc.secret.Store("")
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := webhook.Verify(rc.secret.Load().(string), r.Header.Get("Doodle-Signature"), string(body), time.Now()); err != nil || r.Header.Get("Doodle-Event-Id") == "" {
			rc.bad.Add(1)
		}
		rc.mu.Lock()
		rc.got = append(rc.got, time.Now())
		rc.mu.Unlock()
		w.WriteHeader(int(rc.status.Load()))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

type fixture struct {
	st  *store.Store
	box *secretbox.Box
	pol *ssrf.Policy
	log *slog.Logger
}

func setup(t *testing.T) fixture {
	k := make([]byte, 16)
	_, _ = rand.Read(k)
	box, _ := secretbox.New(hex.EncodeToString(k))
	return fixture{st: pgtest.Store(t), box: box, pol: ssrf.NewPolicy("127.0.0.1"), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// register stores an endpoint for url with a fresh secret and queues one delivery to it.
func (f fixture) register(t *testing.T, url string) (store.Endpoint, string, store.Delivery) {
	t.Helper()
	ctx := t.Context()
	secret, _ := webhook.NewSecret()
	id := uuid.New()
	ct, nonce, _ := f.box.Seal([]byte(secret), []byte(id.String()))
	e, err := f.st.CreateEndpoint(ctx, store.Endpoint{ID: id, OwnerID: "u_dev", APIKeyID: uuid.New(), URL: url,
		EventTypes: []string{webhook.TypeSaleCompleted}, SecretCiphertext: ct, SecretNonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := webhook.Body("evt-"+id.String(), webhook.TypeSaleCompleted, time.Now(), []byte(`{"price":40}`))
	d := store.Delivery{ID: uuid.New(), EndpointID: e.ID, SourceEventID: "evt-" + id.String(), EventType: webhook.TypeSaleCompleted, Payload: body}
	if _, err := f.st.InsertDeliveries(ctx, []store.Delivery{d}); err != nil {
		t.Fatal(err)
	}
	return e, secret, d
}

func (f fixture) run(t *testing.T, cfg delivery.Config) {
	t.Helper()
	disp, err := delivery.NewDispatcher(f.st, f.box, f.pol, cfg, f.log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { disp.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (f fixture) status(t *testing.T, e store.Endpoint) store.Delivery {
	ds, err := f.st.ListDeliveries(t.Context(), e.OwnerID, e.ID, uuid.Nil, 1)
	if err != nil || len(ds) != 1 {
		t.Fatalf("deliveries: %v %v", ds, err)
	}
	return ds[0]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fast() delivery.Config {
	cfg := delivery.Defaults()
	cfg.Poll, cfg.Base, cfg.Cap, cfg.Timeout = 10*time.Millisecond, 20*time.Millisecond, 160*time.Millisecond, 2*time.Second
	return cfg
}

func TestDelivery(t *testing.T) {
	f := setup(t)
	f.run(t, fast())

	t.Run("a healthy receiver gets the event once and verifies its signature", func(t *testing.T) {
		rc := newReceiver(t)
		e, secret, _ := f.register(t, rc.srv.URL+"/hooks")
		rc.secret.Store(secret)

		waitFor(t, "SUCCEEDED", func() bool { return f.status(t, e).Status == "SUCCEEDED" })
		time.Sleep(100 * time.Millisecond)
		if rc.count() != 1 || rc.bad.Load() != 0 {
			t.Fatalf("received %d, bad signatures %d", rc.count(), rc.bad.Load())
		}
	})

	t.Run("a failing receiver is retried with backoff until DEAD", func(t *testing.T) {
		rc := newReceiver(t)
		rc.status.Store(500)
		e, secret, _ := f.register(t, rc.srv.URL+"/hooks")
		rc.secret.Store(secret)

		waitFor(t, "DEAD", func() bool { return f.status(t, e).Status == "DEAD" })
		d := f.status(t, e)
		if d.Attempts != 8 || len(d.AttemptLog) != 8 || rc.count() != 8 || rc.bad.Load() != 0 {
			t.Fatalf("attempts %d, logged %d, received %d, bad %d", d.Attempts, len(d.AttemptLog), rc.count(), rc.bad.Load())
		}
		if *d.LastStatusCode != 500 || *d.LastError != "HTTP 500" {
			t.Fatalf("last = %v %v", *d.LastStatusCode, *d.LastError)
		}
	})

	t.Run("a receiver that recovers ends SUCCEEDED", func(t *testing.T) {
		rc := newReceiver(t)
		rc.status.Store(503)
		e, secret, _ := f.register(t, rc.srv.URL+"/hooks")
		rc.secret.Store(secret)
		waitFor(t, "two failed attempts", func() bool { return rc.count() >= 2 })
		rc.status.Store(204)
		waitFor(t, "SUCCEEDED", func() bool { return f.status(t, e).Status == "SUCCEEDED" })
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		var reached atomic.Int32
		inside := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
		defer inside.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, inside.URL, http.StatusFound)
		}))
		defer redirector.Close()
		e, _, _ := f.register(t, redirector.URL)
		waitFor(t, "a recorded 302", func() bool { d := f.status(t, e); return d.LastStatusCode != nil })
		if code := *f.status(t, e).LastStatusCode; code != 302 || reached.Load() != 0 {
			t.Fatalf("status %d, redirect target reached %d times", code, reached.Load())
		}
	})

	t.Run("a host resolving inside is never contacted", func(t *testing.T) {
		rc := newReceiver(t)
		// localhost isn't on the allow-list (only 127.0.0.1 is, for the other tests).
		url := strings.Replace(rc.srv.URL, "127.0.0.1", "localhost", 1)
		e, _, _ := f.register(t, url)
		waitFor(t, "a blocked attempt", func() bool { d := f.status(t, e); return d.LastError != nil })
		if msg := *f.status(t, e).LastError; msg != "blocked: the address is internal" || rc.count() != 0 {
			t.Fatalf("last error %q, received %d", msg, rc.count())
		}
	})
}

// M4: a failing endpoint ends in the dead-letter queue, on a real Kafka.
func TestDeadDeliveryReachesTheDeadLetterQueue(t *testing.T) {
	f := setup(t)
	addr := kafkatest.Kafka(t)
	kafkatest.CreateTopics(t, addr, delivery.DLQTopic)
	f.run(t, fast())

	rc := newReceiver(t)
	rc.status.Store(500)
	e, _, d := f.register(t, rc.srv.URL)
	waitFor(t, "DEAD", func() bool { return f.status(t, e).Status == "DEAD" })

	producer, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.RecordDeliveryTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	dl, err := delivery.NewDeadLetters(f.st, producer, f.log, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := dl.PublishOnce(t.Context()); n != 1 || err != nil {
		t.Fatalf("published %d, %v", n, err)
	}
	if n, _ := dl.PublishOnce(t.Context()); n != 0 {
		t.Fatalf("published %d again", n)
	}

	consumer, _ := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ConsumeTopics(delivery.DLQTopic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	defer consumer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	fetches := consumer.PollRecords(ctx, 1)
	if fetches.NumRecords() != 1 {
		t.Fatalf("records on %s: %d (%v)", delivery.DLQTopic, fetches.NumRecords(), fetches.Errors())
	}
	r := fetches.Records()[0]
	var ev eventsv1.DeadLetterEvent
	if err := proto.Unmarshal(r.Value, &ev); err != nil {
		t.Fatal(err)
	}
	dd := ev.GetDeliveryDead()
	if string(r.Key) != e.ID.String() || dd.GetDeliveryId() != d.ID.String() || dd.GetAttempts() != 8 ||
		dd.GetLastStatusCode() != 500 || dd.GetSourceEventId() != d.SourceEventID || dd.GetMeta().GetEventType() != "DeliveryDead" {
		t.Fatalf("DeliveryDead = %v (key %s)", dd, r.Key)
	}
}
