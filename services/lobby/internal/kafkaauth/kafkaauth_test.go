package kafkaauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestNoUsernameAddsNothing(t *testing.T) {
	opts, err := Config{}.Opts()
	if err != nil || len(opts) != 0 {
		t.Fatalf("Opts() = %d options, %v; want none", len(opts), err)
	}
}

func TestUsernameAddsSASL(t *testing.T) {
	opts, err := Config{Username: "lobby", Password: "secret"}.Opts()
	if err != nil || len(opts) != 1 {
		t.Fatalf("Opts() = %d options, %v; want SASL only", len(opts), err)
	}
}

func TestCAAddsTLS(t *testing.T) {
	opts, err := Config{Username: "lobby", Password: "secret", CAPEM: testCA(t)}.Opts()
	if err != nil || len(opts) != 2 {
		t.Fatalf("Opts() = %d options, %v; want SASL and TLS", len(opts), err)
	}
}

func TestIncompleteConfigIsAnError(t *testing.T) {
	for name, c := range map[string]Config{
		"no password":       {Username: "lobby"},
		"CA without a user": {CAPEM: testCA(t)},
		"CA that isn't PEM": {Username: "lobby", Password: "secret", CAPEM: "not a certificate"},
	} {
		if _, err := c.Opts(); err == nil {
			t.Errorf("%s: Opts() returned no error", name)
		}
	}
}

func testCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
