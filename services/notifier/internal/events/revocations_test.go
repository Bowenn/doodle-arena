package events_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/events"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/pgtest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

func revoked(user string, at time.Time) []byte {
	b, _ := proto.Marshal(&eventsv1.UserEvent{Event: &eventsv1.UserEvent_CredentialsRevoked{CredentialsRevoked: &eventsv1.CredentialsRevoked{
		Meta:   &eventsv1.EventMeta{EventId: uuid.NewString(), EventType: "CredentialsRevoked", Producer: "auth"},
		UserId: user, Reason: "refresh_token_reuse", RevokedAt: timestamppb.New(at),
	}}})
	return b
}

func TestDecodeRevocation(t *testing.T) {
	cr, err := events.DecodeRevocation(revoked("u_ada", time.Unix(1_800_000_000, 0)))
	if err != nil || cr.GetUserId() != "u_ada" || cr.GetReason() != "refresh_token_reuse" {
		t.Fatalf("DecodeRevocation = %v, %v", cr, err)
	}
	empty, _ := proto.Marshal(&eventsv1.UserEvent{})
	noUser, _ := proto.Marshal(&eventsv1.UserEvent{Event: &eventsv1.UserEvent_CredentialsRevoked{
		CredentialsRevoked: &eventsv1.CredentialsRevoked{RevokedAt: timestamppb.Now()}}})
	for name, bad := range map[string][]byte{"garbage": {0x0a, 0xff}, "empty": empty, "no user": noUser} {
		if _, err := events.DecodeRevocation(bad); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// Task 4.R2: a CredentialsRevoked, even one published before the notifier first subscribed,
// disables the user's endpoints, and a sale afterwards queues nothing for them.
func TestRevocationDisablesEndpointsAndStopsDeliveries(t *testing.T) {
	st := pgtest.Store(t)
	ctx := t.Context()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, events.UsersTopic, events.SalesTopic, events.WalletTopic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	brokers := cluster.ListenAddrs()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	victim, bystander := "u_vic_"+uuid.NewString()[:8], "u_by_"+uuid.NewString()[:8]
	register := func(owner string) store.Endpoint {
		e, err := st.CreateEndpoint(ctx, store.Endpoint{ID: uuid.New(), OwnerID: owner, URL: "https://example.com/" + owner,
			EventTypes: []string{webhook.TypeSaleCompleted}, SecretCiphertext: []byte("c"), SecretNonce: []byte("n")}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	thiefHook := register(victim)
	bystanderHook := register(bystander)

	producer, _ := kgo.NewClient(kgo.SeedBrokers(brokers...))
	t.Cleanup(producer.Close)
	produce := func(topic string, value []byte) {
		if err := producer.ProduceSync(ctx, &kgo.Record{Topic: topic, Value: value}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Second).UTC()
	// Published before the consumer starts: the group reads from the earliest offset.
	produce(events.UsersTopic, revoked(victim, at))
	produce(events.UsersTopic, []byte{0x0a, 0xff})  // unreadable: skipped
	produce(events.UsersTopic, revoked(victim, at)) // redelivered: nothing more

	revocations, err := events.NewRevocationConsumer(brokers, st, discard)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { revocations.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(20 * time.Second)
	for {
		e, _ := st.GetEndpoint(ctx, victim, thiefHook.ID)
		if e.DisabledAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the victim's endpoint was never disabled")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e, _ := st.GetEndpoint(ctx, bystander, bystanderHook.ID); e.DisabledAt != nil {
		t.Fatal("a bystander's endpoint was disabled")
	}
	if wm, ok, _ := st.RevokedAt(ctx, victim); !ok || !wm.Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("watermark = %v, %v", wm, ok)
	}

	sales, err := events.NewConsumer(brokers, st, discard, kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	salesCtx, stopSales := context.WithCancel(ctx)
	salesDone := make(chan struct{})
	go func() { sales.Run(salesCtx); close(salesDone) }()
	t.Cleanup(func() { stopSales(); <-salesDone })
	produce(events.SalesTopic, sale("evt-after-"+uuid.NewString(), victim, bystander))

	count := func(owner string, e store.Endpoint) int {
		ds, _ := st.ListDeliveries(ctx, owner, e.ID, uuid.Nil, 10)
		return len(ds)
	}
	deadline = time.Now().Add(20 * time.Second)
	for count(bystander, bystanderHook) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := []int{count(victim, thiefHook), count(bystander, bystanderHook)}; got[0] != 0 || got[1] != 1 {
		t.Fatalf("deliveries (victim's disabled endpoint, bystander) = %v, want [0 1]", got)
	}
}
