package main

import (
	"errors"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// devToolsAuth is the host tools' Kafka user (ADR 0013): dev-tools, which may read every topic
// and write game.rounds.v1 and game.matches.v1, on the broker's EXTERNAL listener (127.0.0.1:9094).
// It exists only locally and in CI. KAFKA_USERNAME and KAFKA_PASSWORD override it.
func devToolsAuth() ([]kgo.Opt, error) {
	user := envOr("KAFKA_USERNAME", "dev-tools")
	pass := os.Getenv("KAFKA_PASSWORD")
	if pass == "" && user == "dev-tools" {
		pass = os.Getenv("KAFKA_DEV_TOOLS_PASSWORD")
	}
	if pass == "" {
		return nil, errors.New("no Kafka password: set KAFKA_DEV_TOOLS_PASSWORD (it's in .env; scripts/fake-events.sh reads it from there)")
	}
	return []kgo.Opt{kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha512Mechanism())}, nil
}
