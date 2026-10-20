// Command eventctl is a developer tool for Doodle Arena's Kafka events.
//
//	eventctl publish [-match-id ID] [-dry-run]   publish a fake round and match from fixtures
//	eventctl decode -topic T [-n N]              print the last N messages of a topic as JSON
//
// scripts/fake-events.sh runs `publish` with settings from .env.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "eventctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: eventctl publish|decode [flags]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	brokers := strings.Split(envOr("KAFKA_BROKERS", "localhost:9094"), ",")
	auth, err := devToolsAuth()
	if err != nil {
		return err
	}

	switch args[0] {
	case "publish":
		fs := flag.NewFlagSet("publish", flag.ContinueOnError)
		matchID := fs.String("match-id", "", "reuse this match id (to test idempotent consumers); default: a new one")
		dryRun := fs.Bool("dry-run", false, "print the events instead of uploading and publishing")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return publish(ctx, publishConfig{
			brokers: brokers,
			kafka:   auth,
			s3: s3Config{
				endpoint:  envOr("S3_ENDPOINT", "http://localhost:8333"),
				accessKey: os.Getenv("S3_ACCESS_KEY"),
				secretKey: os.Getenv("S3_SECRET_KEY"),
			},
			bucket:     envOr("ROUND_LOGS_BUCKET", "round-logs"),
			matchID:    *matchID,
			dryRun:     *dryRun,
			out:        os.Stdout,
			newID:      newUUID,
			traceHdr:   newTraceparent,
			now:        time.Now,
			reqTimeout: 15 * time.Second,
		})
	case "decode":
		fs := flag.NewFlagSet("decode", flag.ContinueOnError)
		topicName := fs.String("topic", "", "topic to read, for example game.rounds.v1")
		n := fs.Int("n", 5, "number of most recent messages to print")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *n < 1 {
			return fmt.Errorf("-n must be at least 1")
		}
		return decode(ctx, decodeConfig{brokers: brokers, kafka: auth, topic: *topicName, n: *n, timeout: 15 * time.Second, out: os.Stdout})
	default:
		return fmt.Errorf("unknown command %q (want publish or decode)", args[0])
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
