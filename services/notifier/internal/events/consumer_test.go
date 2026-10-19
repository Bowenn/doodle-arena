package events_test

import (
	"context"
	"encoding/json"
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

func sale(eventID, seller, buyer string, royalties ...string) []byte {
	s := &eventsv1.SaleCompleted{
		Meta:   &eventsv1.EventMeta{EventId: eventID, EventType: "SaleCompleted", OccurredAt: timestamppb.New(time.Unix(1_800_000_000, 0))},
		SaleId: "sale-1", ListingId: "listing-1", DrawingId: "drawing-1", SellerId: seller, BuyerId: buyer, Price: 81,
		CompletedAt: timestamppb.New(time.Unix(1_800_000_000, 0)),
	}
	for _, r := range royalties {
		s.Royalties = append(s.Royalties, &eventsv1.RoyaltyPayment{UserId: r, Amount: 4})
	}
	b, _ := proto.Marshal(&eventsv1.SaleEvent{Event: &eventsv1.SaleEvent_SaleCompleted{SaleCompleted: s}})
	return b
}

func credit(eventID, user string) []byte {
	b, _ := proto.Marshal(&eventsv1.WalletEvent{Event: &eventsv1.WalletEvent_CoinsCredited{CoinsCredited: &eventsv1.CoinsCredited{
		Meta: &eventsv1.EventMeta{EventId: eventID}, UserId: user, Amount: 550,
		Reason: eventsv1.CreditReason_CREDIT_REASON_STRIPE_PURCHASE, Reference: "cs_test_1",
	}}})
	return b
}

func TestDecode(t *testing.T) {
	ev, err := events.Decode(events.SalesTopic, sale("evt-1", "u_ada", "u_zed", "u_bob", "u_ada"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "evt-1" || ev.Type != webhook.TypeSaleCompleted {
		t.Fatalf("event = %+v", ev)
	}
	if got := ev.Users; len(got) != 3 || got[0] != "u_ada" || got[1] != "u_zed" || got[2] != "u_bob" {
		t.Fatalf("users = %v (seller, buyer, royalty recipients, each once)", got)
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if data["price"] != float64(81) || data["seller_id"] != "u_ada" || data["completed_at"] != "2027-01-15T08:00:00Z" {
		t.Fatalf("data = %v (numbers as numbers, times as RFC 3339)", data)
	}

	ev, _ = events.Decode(events.WalletTopic, credit("evt-2", "u_zed"))
	_ = json.Unmarshal(ev.Data, &data)
	if ev.Type != webhook.TypeCoinsCredited || data["reason"] != "stripe_purchase" || data["amount"] != float64(550) {
		t.Fatalf("credit = %+v %v", ev, data)
	}

	empty, _ := proto.Marshal(&eventsv1.SaleEvent{})
	for name, bad := range map[string][]byte{"garbage": {0x0a, 0xff}, "empty": empty, "no id": sale("", "a", "b")} {
		if _, err := events.Decode(events.SalesTopic, bad); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestConsumerQueuesForOwnersOnly(t *testing.T) {
	st := pgtest.Store(t)
	ctx := t.Context()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, events.SalesTopic, events.WalletTopic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	brokers := cluster.ListenAddrs()

	register := func(owner string, types ...string) store.Endpoint {
		e, err := st.CreateEndpoint(ctx, store.Endpoint{ID: uuid.New(), OwnerID: owner, APIKeyID: ptr(uuid.New()), URL: "https://example.com/" + owner, EventTypes: types, SecretCiphertext: []byte("c"), SecretNonce: []byte("n")}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	sellerHook := register("u_ada", webhook.TypeSaleCompleted)
	royaltyHook := register("u_bob", webhook.TypeSaleCompleted, webhook.TypeCoinsCredited)
	creditsOnly := register("u_zed", webhook.TypeCoinsCredited)
	stranger := register("u_eve", webhook.TypeSaleCompleted, webhook.TypeCoinsCredited)

	consumer, err := events.NewConsumer(brokers, st, slog.New(slog.NewTextHandler(io.Discard, nil)),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { consumer.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	producer, _ := kgo.NewClient(kgo.SeedBrokers(brokers...))
	t.Cleanup(producer.Close)
	produce := func(topic string, value []byte) {
		if err := producer.ProduceSync(ctx, &kgo.Record{Topic: topic, Value: value}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	produce(events.SalesTopic, sale("evt-sale", "u_ada", "u_zed", "u_bob"))
	produce(events.SalesTopic, sale("evt-sale", "u_ada", "u_zed", "u_bob")) // redelivered
	produce(events.SalesTopic, []byte{0x0a, 0xff})                          // unreadable: skipped
	produce(events.WalletTopic, credit("evt-credit", "u_zed"))

	count := func(e store.Endpoint) int {
		ds, _ := st.ListDeliveries(ctx, e.OwnerID, e.ID, uuid.Nil, 50)
		return len(ds)
	}
	// Wait for every expected delivery: the two topics are consumed independently, so the
	// credit can be stored before the sale.
	deadline := time.Now().Add(20 * time.Second)
	for (count(sellerHook) == 0 || count(royaltyHook) == 0 || count(creditsOnly) == 0) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := []int{count(sellerHook), count(royaltyHook), count(creditsOnly), count(stranger)}; got[0] != 1 || got[1] != 1 || got[2] != 1 || got[3] != 0 {
		t.Fatalf("deliveries (seller, royalty recipient, buyer's credits-only, stranger) = %v, want [1 1 1 0]", got)
	}
	ds, _ := st.ListDeliveries(ctx, "u_ada", sellerHook.ID, uuid.Nil, 1)
	var env webhook.Envelope
	if err := json.Unmarshal([]byte(ds[0].Payload), &env); err != nil || env.ID != "evt-sale" || env.Type != webhook.TypeSaleCompleted {
		t.Fatalf("payload = %s", ds[0].Payload)
	}
}

func ptr[T any](v T) *T { return &v }
