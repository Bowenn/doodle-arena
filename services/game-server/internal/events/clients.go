package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
)

// NewKafkaClient returns a producer for the game topics. franz-go's idempotent producer
// (on by default) keeps its own retries from writing duplicates, acks=all waits for the
// in-sync replicas, records that were never sent fail after 30 s (so the publisher can retry
// them), and the tracing hook writes each record's traceparent header from the context it is
// produced with.
// Extra options carry the service's Kafka user (internal/kafkaauth).
func NewKafkaClient(brokers []string, extra ...kgo.Opt) (*kgo.Client, error) {
	tracer := kotel.NewTracer() // the global tracer provider and propagator
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(30 * time.Second),
		kgo.ProducerLinger(0),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(tracer)).Hooks()...),
	}, extra...)
	c, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return c, nil
}

// S3Config reaches the object store: SeaweedFS locally, S3 in AWS.
type S3Config struct {
	Endpoint  string // empty: AWS's own endpoint
	Region    string
	PathStyle bool   // SeaweedFS needs path-style addressing; S3 prefers virtual-hosted
	AccessKey string // empty (with SecretKey): the default chain, such as an ECS task role
	SecretKey string
}

// NewS3Client returns a traced S3 client. Without static keys it takes credentials from the
// SDK's default chain: in ECS, the task role (ADR 0003), so no key is ever configured there.
func NewS3Client(ctx context.Context, cfg S3Config) (*s3.Client, error) {
	opts := s3.Options{
		Region:       cfg.Region,
		UsePathStyle: cfg.PathStyle,
		// Only send checksums S3 requires; some S3-compatible stores reject the newer
		// default checksum headers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}
	switch {
	case cfg.AccessKey != "" && cfg.SecretKey != "":
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	case cfg.AccessKey != "" || cfg.SecretKey != "":
		return nil, errors.New("set both S3_ACCESS_KEY and S3_SECRET_KEY, or neither (to use the task role)")
	default:
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
		if err != nil {
			return nil, fmt.Errorf("aws credentials: %w", err)
		}
		opts.Credentials = awsCfg.Credentials
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	otelaws.AppendMiddlewares(&opts.APIOptions)
	return s3.New(opts), nil
}
