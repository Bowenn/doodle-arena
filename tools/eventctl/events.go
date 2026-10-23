package main

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
)

// errNoEvent means a wrapper's oneof is unset, or holds a case this build doesn't know
// (a newer producer). ADR 0004: consumers skip such messages instead of failing.
var errNoEvent = errors.New("wrapper has no known event case")

// topic describes one Kafka topic from the event catalog (design doc, section 6.2).
type topic struct {
	name string
	// newWrapper returns an empty wrapper message for this topic.
	newWrapper func() proto.Message
	// key returns the Kafka key of a wrapper, taken from its event.
	key func(proto.Message) (string, error)
}

// topics lists the catalog's topics (design doc 6.2). Keys follow the catalog's Key column.
var topics = map[string]topic{
	"game.rounds.v1":     {"game.rounds.v1", func() proto.Message { return &eventsv1.RoundEvent{} }, fieldKey("room_id")},
	"game.matches.v1":    {"game.matches.v1", func() proto.Message { return &eventsv1.MatchEvent{} }, fieldKey("room_id")},
	"assets.drawings.v1": {"assets.drawings.v1", func() proto.Message { return &eventsv1.DrawingEvent{} }, fieldKey("drawing_id")},
	"market.listings.v1": {"market.listings.v1", func() proto.Message { return &eventsv1.ListingEvent{} }, fieldKey("drawing_id")},
	"market.sales.v1":    {"market.sales.v1", func() proto.Message { return &eventsv1.SaleEvent{} }, fieldKey("drawing_id")},
	"market.wallet.v1":   {"market.wallet.v1", func() proto.Message { return &eventsv1.WalletEvent{} }, fieldKey("user_id")},
	"notifier.dlq.v1":    {"notifier.dlq.v1", func() proto.Message { return &eventsv1.DeadLetterEvent{} }, fieldKey("endpoint_id")},
	// Stage 6: live rooms and analytics (ADR 0017, ADR 0018).
	"live.guesses.v1":     {"live.guesses.v1", func() proto.Message { return &eventsv1.ViewerGuessEvent{} }, fieldKey("room_id")},
	"live.top-guesses.v1": {"live.top-guesses.v1", func() proto.Message { return &eventsv1.TopGuessesEvent{} }, fieldKey("room_id")},
	"analytics.flags.v1":  {"analytics.flags.v1", func() proto.Message { return &eventsv1.AnalyticsFlagEvent{} }, fieldKey("drawing_id")},
}

// eventOf returns the event inside a wrapper: the message in its single "event" oneof.
func eventOf(wrapper proto.Message) (protoreflect.Message, error) {
	m := wrapper.ProtoReflect()
	oneof := m.Descriptor().Oneofs().ByName("event")
	if oneof == nil {
		return nil, fmt.Errorf("%s is not an event wrapper", m.Descriptor().FullName())
	}
	field := m.WhichOneof(oneof)
	if field == nil {
		return nil, errNoEvent
	}
	return m.Get(field).Message(), nil
}

// setMeta fills in the EventMeta of the event inside a wrapper. event_type is set to the
// event's message name, so it always matches the oneof case.
func setMeta(wrapper proto.Message, meta *eventsv1.EventMeta) error {
	event, err := eventOf(wrapper)
	if err != nil {
		return err
	}
	field := event.Descriptor().Fields().ByName("meta")
	if field == nil {
		return fmt.Errorf("%s has no meta field", event.Descriptor().FullName())
	}
	meta = proto.Clone(meta).(*eventsv1.EventMeta)
	meta.EventType = string(event.Descriptor().Name())
	event.Set(field, protoreflect.ValueOfMessage(meta.ProtoReflect()))
	return nil
}

// fieldKey returns a key function that reads a string field of the wrapped event.
func fieldKey(name protoreflect.Name) func(proto.Message) (string, error) {
	return func(wrapper proto.Message) (string, error) {
		event, err := eventOf(wrapper)
		if err != nil {
			return "", err
		}
		field := event.Descriptor().Fields().ByName(name)
		if field == nil {
			return "", fmt.Errorf("%s has no %s field", event.Descriptor().FullName(), name)
		}
		key := event.Get(field).String()
		if key == "" {
			return "", fmt.Errorf("%s.%s is empty", event.Descriptor().Name(), name)
		}
		return key, nil
	}
}
