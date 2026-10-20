// Package kafkaauth gives a franz-go client its Kafka user (ADR 0013): SCRAM-SHA-512 with the
// service's own username and password, over TLS when a CA is given (SASL_SSL in AWS). The
// broker's ACLs decide what that user may do. With no username there is nothing to add, which
// is what the Testcontainers and in-memory brokers in tests expect.
package kafkaauth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Config is a service's Kafka credential.
type Config struct {
	Username string
	Password string
	CAPEM    string // the broker's CA certificate, PEM; empty: no TLS
}

// FromEnv reads KAFKA_USERNAME, KAFKA_PASSWORD and KAFKA_CA_PEM.
func FromEnv() Config {
	return Config{
		Username: os.Getenv("KAFKA_USERNAME"),
		Password: os.Getenv("KAFKA_PASSWORD"),
		CAPEM:    os.Getenv("KAFKA_CA_PEM"),
	}
}

// Opts returns the client options for c: none without a username.
func (c Config) Opts() ([]kgo.Opt, error) {
	if c.Username == "" {
		if c.CAPEM != "" {
			return nil, errors.New("kafka: KAFKA_CA_PEM is set without KAFKA_USERNAME")
		}
		return nil, nil
	}
	if c.Password == "" {
		return nil, errors.New("kafka: KAFKA_USERNAME is set without KAFKA_PASSWORD")
	}
	opts := []kgo.Opt{kgo.SASL(scram.Auth{User: c.Username, Pass: c.Password}.AsSha512Mechanism())}
	if c.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.CAPEM)) {
			return nil, errors.New("kafka: KAFKA_CA_PEM holds no certificate")
		}
		// franz-go sets ServerName to the broker's host for each connection.
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	}
	return opts, nil
}
