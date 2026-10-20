package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type publishConfig struct {
	brokers    []string
	kafka      []kgo.Opt // the Kafka user (kafka.go)
	s3         s3Config
	bucket     string
	matchID    string
	dryRun     bool
	out        io.Writer
	newID      func() string
	traceHdr   func() string
	now        func() time.Time
	reqTimeout time.Duration
}

type s3Config struct {
	endpoint  string
	accessKey string
	secretKey string
}

// publish uploads the fake stroke log and publishes RoundFinished and MatchFinished.
func publish(ctx context.Context, cfg publishConfig) error {
	matchID := cfg.matchID
	if matchID == "" {
		matchID = cfg.newID()
	}
	b, err := buildBatch(matchID, cfg.now().UTC(), cfg.newID)
	if err != nil {
		return err
	}
	logBytes, err := gzipStrokeLog(b.strokeLog)
	if err != nil {
		return err
	}
	records, err := recordsFor(b, cfg.traceHdr)
	if err != nil {
		return err
	}

	if cfg.dryRun {
		fmt.Fprintf(cfg.out, "dry run: would upload s3://%s/%s (%d bytes)\n", cfg.bucket, b.strokeLogKey, len(logBytes))
		for i, r := range records {
			w := []proto.Message{b.round, b.match}[i]
			fmt.Fprintf(cfg.out, "dry run: would publish to %s key=%s\n%s\n", r.Topic, r.Key, protojson.Format(w))
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.reqTimeout)
	defer cancel()

	if err := uploadStrokeLog(ctx, cfg.s3, cfg.bucket, b.strokeLogKey, logBytes); err != nil {
		return err
	}
	fmt.Fprintf(cfg.out, "uploaded s3://%s/%s (%d strokes)\n", cfg.bucket, b.strokeLogKey, len(b.strokeLog.GetStrokes()))

	client, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(cfg.brokers...), kgo.RequiredAcks(kgo.AllISRAcks())}, cfg.kafka...)...)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()

	for _, res := range client.ProduceSync(ctx, records...) {
		if res.Err != nil {
			return fmt.Errorf("produce to %s: %w", res.Record.Topic, res.Err)
		}
		fmt.Fprintf(cfg.out, "published %s partition=%d offset=%d key=%s\n",
			res.Record.Topic, res.Record.Partition, res.Record.Offset, res.Record.Key)
	}
	fmt.Fprintf(cfg.out, "match_id=%s\n", matchID)
	return nil
}

// recordsFor turns a batch into Kafka records: the serialized wrapper as the value, the
// catalog key, and a traceparent header.
func recordsFor(b *batch, traceparent func() string) ([]*kgo.Record, error) {
	var records []*kgo.Record
	for _, item := range []struct {
		topic   string
		wrapper proto.Message
	}{{"game.rounds.v1", b.round}, {"game.matches.v1", b.match}} {
		key, err := topics[item.topic].key(item.wrapper)
		if err != nil {
			return nil, err
		}
		value, err := proto.Marshal(item.wrapper)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", item.topic, err)
		}
		records = append(records, &kgo.Record{
			Topic:   item.topic,
			Key:     []byte(key),
			Value:   value,
			Headers: []kgo.RecordHeader{{Key: "traceparent", Value: []byte(traceparent())}},
		})
	}
	return records, nil
}

func uploadStrokeLog(ctx context.Context, cfg s3Config, bucket, key string, body []byte) error {
	if cfg.accessKey == "" || cfg.secretKey == "" {
		return fmt.Errorf("S3_ACCESS_KEY and S3_SECRET_KEY must be set (scripts/fake-events.sh loads them from .env)")
	}
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(cfg.endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.accessKey, cfg.secretKey, ""),
		// Only send checksums S3 requires; some S3-compatible stores reject the newer
		// default checksum headers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("upload s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}
