package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
)

// guessRecords builds live.guesses.v1 records: wrong guesses by viewers of a live room, as the
// relays will publish them (task 6.2), for the analytics job (ADR 0018) to count. Each text is
// normalized the way the game compares guesses: lowercased, letters a-z only.
func guessRecords(roomID string, roundNo uint32, texts []string, now time.Time, newID func() string) ([]*kgo.Record, error) {
	var out []*kgo.Record
	for _, text := range texts {
		norm := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' {
				return r
			}
			return -1
		}, strings.ToLower(text))
		if norm == "" {
			return nil, fmt.Errorf("guess %q has no letters a-z", text)
		}
		wrapper := &eventsv1.ViewerGuessEvent{Event: &eventsv1.ViewerGuessEvent_ViewerGuess{ViewerGuess: &eventsv1.ViewerGuess{
			RoomId: roomID, RoundNo: roundNo, Text: norm, GuessedAt: timestamppb.New(now),
		}}}
		if err := setMeta(wrapper, &eventsv1.EventMeta{EventId: newID(), SchemaVersion: 1, OccurredAt: timestamppb.New(now), Producer: "eventctl"}); err != nil {
			return nil, err
		}
		value, err := proto.Marshal(wrapper)
		if err != nil {
			return nil, err
		}
		out = append(out, &kgo.Record{Topic: "live.guesses.v1", Key: []byte(roomID), Value: value})
	}
	return out, nil
}

// publishGuesses sends the guesses, all with the time now.
func publishGuesses(ctx context.Context, brokers []string, auth []kgo.Opt, roomID string, roundNo uint32, texts []string, out io.Writer) error {
	records, err := guessRecords(roomID, roundNo, texts, time.Now().UTC(), newUUID)
	if err != nil {
		return err
	}
	client, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(brokers...), kgo.RequiredAcks(kgo.AllISRAcks())}, auth...)...)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return fmt.Errorf("produce to live.guesses.v1: %w", err)
	}
	fmt.Fprintf(out, "published %d guesses for room %s, round %d\n", len(records), roomID, roundNo)
	return nil
}
