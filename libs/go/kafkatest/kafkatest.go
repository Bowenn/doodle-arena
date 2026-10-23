// Package kafkatest starts a real single-node Kafka (KRaft) for integration tests, with the
// same official image as infra/docker-compose.yml. The game server and the notifier share it.
package kafkatest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/tcretry"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaImage matches infra/docker-compose.yml.
const KafkaImage = "apache/kafka:4.2.2"

const advertisedFile = "/tmp/advertised-listeners"

// Kafka starts a broker and returns its bootstrap address. A client outside Docker must be
// told the broker's mapped host port, which is known only after the container starts, so
// the container waits for that address in a file before it starts Kafka.
func Kafka(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	var addr string
	c, err := tcretry.Run("kafka", func() (*testcontainers.DockerContainer, error) {
		return testcontainers.Run(ctx, KafkaImage,
			testcontainers.WithExposedPorts("9092/tcp"),
			testcontainers.WithEnv(map[string]string{
				"KAFKA_NODE_ID":                                  "1",
				"KAFKA_PROCESS_ROLES":                            "broker,controller",
				"KAFKA_CONTROLLER_QUORUM_VOTERS":                 "1@localhost:9093",
				"KAFKA_LISTENERS":                                "EXTERNAL://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093,INTERNAL://0.0.0.0:19092",
				"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP":           "EXTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT,INTERNAL:PLAINTEXT",
				"KAFKA_INTER_BROKER_LISTENER_NAME":               "INTERNAL",
				"KAFKA_CONTROLLER_LISTENER_NAMES":                "CONTROLLER",
				"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
				"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
				"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
				"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
				"KAFKA_AUTO_CREATE_TOPICS_ENABLE":                "false", // like the stack: topics are created on purpose
			}),
			testcontainers.WithEntrypoint("sh", "-c"),
			testcontainers.WithCmd(fmt.Sprintf(
				"while [ ! -f %[1]s ]; do sleep 0.1; done; export KAFKA_ADVERTISED_LISTENERS=$(cat %[1]s); exec /etc/kafka/docker/run",
				advertisedFile)),
			testcontainers.WithLifecycleHooks(testcontainers.ContainerLifecycleHooks{
				PostStarts: []testcontainers.ContainerHook{func(ctx context.Context, c testcontainers.Container) error {
					if err := wait.ForMappedPort("9092/tcp").WaitUntilReady(ctx, c); err != nil {
						return err
					}
					host, err := c.Host(ctx)
					if err != nil {
						return err
					}
					port, err := c.MappedPort(ctx, "9092/tcp")
					if err != nil {
						return err
					}
					addr = fmt.Sprintf("%s:%d", host, port.Num())
					listeners := fmt.Sprintf("EXTERNAL://%s,INTERNAL://localhost:19092", addr)
					if err := c.CopyToContainer(ctx, []byte(listeners), advertisedFile, 0o644); err != nil {
						return fmt.Errorf("copy advertised listeners: %w", err)
					}
					return wait.ForLog("Kafka Server started").WithStartupTimeout(time.Minute).WaitUntilReady(ctx, c)
				}},
			}),
		)
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start kafka: %v", err)
	}
	return addr
}

// CreateTopics creates topics with one partition each.
func CreateTopics(t *testing.T, addr string, topics ...string) {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	resp, err := kadm.NewClient(cl).CreateTopics(t.Context(), 1, 1, nil, topics...)
	if err == nil {
		err = resp.Error()
	}
	if err != nil {
		t.Fatalf("create topics: %v", err)
	}
}
