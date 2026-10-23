package relay

import (
	"context"
	"errors"
	"time"

	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Topics a relay uses, with its own Kafka user, relay (ADR 0019): it may write the first and
// read the second, nothing else.
const (
	TopicGuesses    = "live.guesses.v1"
	TopicTopGuesses = "live.top-guesses.v1"
)

// KafkaOpts are what a relay's Kafka client needs on top of the game server's: it reads the
// top guesses from the end, without a consumer group (every relay needs every room's lists,
// and old lists are of no use).
func KafkaOpts() []kgo.Opt {
	return []kgo.Opt{
		kgo.ConsumeTopics(TopicTopGuesses),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
	}
}

// kafkaPublisher sends viewers' wrong guesses to live.guesses.v1 (keyed by room, like every
// live topic). It never waits: with the client's buffer full, a guess is dropped and counted,
// because the top-guesses list loses nothing from one guess among thousands.
type kafkaPublisher struct {
	client  *kgo.Client
	metrics *metrics
}

func (p kafkaPublisher) PublishGuess(ctx context.Context, roomID string, roundNo int, text string, at time.Time) {
	value, err := proto.Marshal(&eventsv1.ViewerGuessEvent{Event: &eventsv1.ViewerGuessEvent_ViewerGuess{
		ViewerGuess: &eventsv1.ViewerGuess{
			Meta: &eventsv1.EventMeta{EventId: uuid.NewString(), EventType: "ViewerGuess", SchemaVersion: 1,
				OccurredAt: timestamppb.New(at), Producer: "game-server-relay"},
			RoomId: roomID, RoundNo: uint32(roundNo), Text: text, GuessedAt: timestamppb.New(at),
		},
	}})
	if err != nil {
		p.metrics.published(ctx, "failed")
		return
	}
	p.client.TryProduce(ctx, &kgo.Record{Topic: TopicGuesses, Key: []byte(roomID), Value: value}, func(_ *kgo.Record, err error) {
		switch {
		case err == nil:
			p.metrics.published(context.Background(), "success")
		case errors.Is(err, kgo.ErrMaxBuffered):
			p.metrics.published(context.Background(), "dropped")
		default:
			p.metrics.published(context.Background(), "failed")
		}
	})
}

// ConsumeTopGuesses reads live.top-guesses.v1 until ctx ends and shows each list to its room's
// viewers on this relay (most rooms aren't watched here: those are skipped). A record it
// can't read is counted and skipped (ADR 0004).
func (r *Relay) ConsumeTopGuesses(ctx context.Context) {
	if r.cfg.Kafka == nil {
		return
	}
	for {
		fetches := r.cfg.Kafka.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			r.cfg.Log.WarnContext(ctx, "reading top guesses failed", "topic", topic, "partition", partition, "err", err)
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			var e eventsv1.TopGuessesEvent
			if err := proto.Unmarshal(rec.Value, &e); err != nil || e.GetTopGuesses() == nil {
				r.metrics.topGuesses(ctx, "unreadable")
				return
			}
			t := e.GetTopGuesses()
			top := TopGuesses{RoundNo: int(t.GetRoundNo()), WindowEndMS: t.GetWindowEnd().AsTime().UnixMilli()}
			for _, g := range t.GetGuesses() {
				top.Guesses = append(top.Guesses, GuessCount{Text: g.GetText(), Count: int(g.GetCount())})
			}
			r.ShowTopGuesses(t.GetRoomId(), top)
		})
	}
}
