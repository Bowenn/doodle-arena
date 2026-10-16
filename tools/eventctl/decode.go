package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type decodeConfig struct {
	brokers []string
	topic   string
	n       int
	timeout time.Duration
	out     io.Writer
}

// decode prints the last n messages of a topic as JSON. Raw Kafka tools only show protobuf
// bytes (ADR 0004), so this is the way to look at events while debugging.
func decode(ctx context.Context, cfg decodeConfig) error {
	t, ok := topics[cfg.topic]
	if !ok {
		return fmt.Errorf("unknown topic %q", cfg.topic)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// Find where each partition ends, then read the last n messages of every partition.
	admClient, err := kgo.NewClient(kgo.SeedBrokers(cfg.brokers...))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	ends, err := kadm.NewClient(admClient).ListEndOffsets(ctx, cfg.topic)
	admClient.Close()
	if err != nil {
		return fmt.Errorf("list offsets of %s: %w", cfg.topic, err)
	}
	starts := map[int32]kgo.Offset{}
	remaining := map[int32]int64{} // partition -> offset of its last message
	ends.Each(func(o kadm.ListedOffset) {
		if o.Err != nil || o.Offset == 0 {
			return
		}
		starts[o.Partition] = kgo.NewOffset().At(max(o.Offset-int64(cfg.n), 0))
		remaining[o.Partition] = o.Offset - 1
	})
	if err := ends.Error(); err != nil {
		return fmt.Errorf("list offsets of %s: %w", cfg.topic, err)
	}
	if len(remaining) == 0 {
		fmt.Fprintf(cfg.out, "%s is empty\n", cfg.topic)
		return nil
	}

	client, err := kgo.NewClient(kgo.SeedBrokers(cfg.brokers...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{cfg.topic: starts}))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()

	var records []*kgo.Record
	for len(remaining) > 0 {
		fetches := client.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reading %s: %w", cfg.topic, err)
		}
		for _, fe := range fetches.Errors() {
			return fmt.Errorf("fetch %s/%d: %w", fe.Topic, fe.Partition, fe.Err)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			records = append(records, r)
			if last, ok := remaining[r.Partition]; ok && r.Offset >= last {
				delete(remaining, r.Partition)
			}
		})
	}

	sort.Slice(records, func(i, j int) bool { return records[i].Timestamp.Before(records[j].Timestamp) })
	if len(records) > cfg.n {
		records = records[len(records)-cfg.n:]
	}
	for _, r := range records {
		fmt.Fprintln(cfg.out, describeRecord(t, r))
	}
	return nil
}

// describeRecord renders one record. A wrapper with an unknown case is reported, not an
// error: that is how a consumer must treat it (ADR 0004).
func describeRecord(t topic, r *kgo.Record) string {
	header := fmt.Sprintf("--- %s partition=%d offset=%d key=%s time=%s",
		r.Topic, r.Partition, r.Offset, r.Key, r.Timestamp.UTC().Format(time.RFC3339))
	for _, h := range r.Headers {
		header += fmt.Sprintf(" %s=%s", h.Key, h.Value)
	}
	w := t.newWrapper()
	if err := proto.Unmarshal(r.Value, w); err != nil {
		return fmt.Sprintf("%s\n(undecodable: %v)", header, err)
	}
	if _, err := eventOf(w); errors.Is(err, errNoEvent) {
		return header + "\n(unknown or unset event case: a newer producer? skipped)"
	}
	return header + "\n" + protojson.Format(w)
}
