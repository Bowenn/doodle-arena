// Package events turns the marketplace's Kafka events into webhook deliveries (design doc 4.7,
// ADR 0014): each event goes to the live endpoints, subscribed to its type, of the users it
// involves, and to nobody else.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

// Topics the notifier reads (design doc 6.2).
const (
	SalesTopic  = "market.sales.v1"
	WalletTopic = "market.wallet.v1"
)

// Event is one source event, ready to fan out.
type Event struct {
	ID      string    // meta.event_id: the dedupe key and the webhook's id
	Type    string    // webhook event type
	Users   []string  // whose endpoints get it
	Created time.Time // when it happened
	Data    json.RawMessage
}

// Reasons decode refuses a record; the consumer logs, counts and skips them (ADR 0004).
var (
	errMalformed = errors.New("malformed")
	errEmpty     = errors.New("empty event (no oneof case set)")
	errUnknown   = errors.New("unknown topic")
	errNoID      = errors.New("event has no meta.event_id")
)

// The webhook payloads: plain JSON with numbers as numbers and times as RFC 3339, a stable
// contract for developers rather than the protobuf JSON mapping (which writes int64 as strings).
type royalty struct {
	UserID string `json:"user_id"`
	Amount int64  `json:"amount"`
}

type saleCompleted struct {
	SaleID      string    `json:"sale_id"`
	ListingID   string    `json:"listing_id"`
	DrawingID   string    `json:"drawing_id"`
	SellerID    string    `json:"seller_id"`
	BuyerID     string    `json:"buyer_id"`
	Price       int64     `json:"price"`
	Royalties   []royalty `json:"royalties"`
	CompletedAt string    `json:"completed_at"`
}

type coinsCredited struct {
	UserID    string `json:"user_id"`
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason"`
	Reference string `json:"reference"`
}

// Decode reads a record from one of the two topics.
func Decode(topic string, value []byte) (Event, error) {
	switch topic {
	case SalesTopic:
		var w eventsv1.SaleEvent
		if err := proto.Unmarshal(value, &w); err != nil {
			return Event{}, fmt.Errorf("%w: %v", errMalformed, err)
		}
		s := w.GetSaleCompleted()
		if s == nil {
			return Event{}, errEmpty
		}
		data := saleCompleted{
			SaleID: s.GetSaleId(), ListingID: s.GetListingId(), DrawingID: s.GetDrawingId(),
			SellerID: s.GetSellerId(), BuyerID: s.GetBuyerId(), Price: s.GetPrice(),
			Royalties: []royalty{}, CompletedAt: s.GetCompletedAt().AsTime().UTC().Format(time.RFC3339Nano),
		}
		users := []string{s.GetSellerId(), s.GetBuyerId()}
		for _, r := range s.GetRoyalties() {
			data.Royalties = append(data.Royalties, royalty{UserID: r.GetUserId(), Amount: r.GetAmount()})
			users = append(users, r.GetUserId())
		}
		return event(s.GetMeta(), webhook.TypeSaleCompleted, users, data)
	case WalletTopic:
		var w eventsv1.WalletEvent
		if err := proto.Unmarshal(value, &w); err != nil {
			return Event{}, fmt.Errorf("%w: %v", errMalformed, err)
		}
		c := w.GetCoinsCredited()
		if c == nil {
			return Event{}, errEmpty
		}
		reason := map[eventsv1.CreditReason]string{
			eventsv1.CreditReason_CREDIT_REASON_REWARD:          "reward",
			eventsv1.CreditReason_CREDIT_REASON_STRIPE_PURCHASE: "stripe_purchase",
			eventsv1.CreditReason_CREDIT_REASON_TIP:             "tip",
			eventsv1.CreditReason_CREDIT_REASON_TIP_REFUND:      "tip_refund",
		}[c.GetReason()]
		if reason == "" {
			reason = "unknown"
		}
		data := coinsCredited{UserID: c.GetUserId(), Amount: c.GetAmount(), Reason: reason, Reference: c.GetReference()}
		return event(c.GetMeta(), webhook.TypeCoinsCredited, []string{c.GetUserId()}, data)
	default:
		return Event{}, errUnknown
	}
}

func event(meta *eventsv1.EventMeta, eventType string, users []string, data any) (Event, error) {
	if meta.GetEventId() == "" {
		return Event{}, errNoID
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %v", errMalformed, err)
	}
	created := time.Now()
	if meta.GetOccurredAt() != nil {
		created = meta.GetOccurredAt().AsTime()
	}
	return Event{ID: meta.GetEventId(), Type: eventType, Users: unique(users), Created: created, Data: raw}, nil
}

func unique(users []string) []string {
	seen := map[string]bool{}
	out := users[:0:0]
	for _, u := range users {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}
