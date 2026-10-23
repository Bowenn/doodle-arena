package main

import (
	"errors"
	"os"

	"github.com/Bowenn/doodle-arena/libs/go/kafkaauth"
	"github.com/twmb/franz-go/pkg/kgo"
)

// devToolsAuth is the host tools' Kafka user (ADR 0013): dev-tools, which may read every topic
// and write game.rounds.v1, game.matches.v1 and live.guesses.v1, on the broker's EXTERNAL listener (127.0.0.1:9094).
// It exists only locally and in CI. KAFKA_USERNAME and KAFKA_PASSWORD override it.
func devToolsAuth() ([]kgo.Opt, error) {
	cfg := kafkaauth.FromEnv()
	if cfg.Username == "" {
		cfg.Username = "dev-tools"
	}
	if cfg.Password == "" && cfg.Username == "dev-tools" {
		cfg.Password = os.Getenv("KAFKA_DEV_TOOLS_PASSWORD")
	}
	if cfg.Password == "" {
		return nil, errors.New("no Kafka password: set KAFKA_DEV_TOOLS_PASSWORD (it's in .env; scripts/fake-events.sh reads it from there)")
	}
	return cfg.Opts()
}
