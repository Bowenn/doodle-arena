// Package webhook builds what developers receive (design doc 4.7): a JSON envelope and its
// Doodle-Signature header.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event types an endpoint can subscribe to; ping is sent only by POST .../test.
const (
	TypeSaleCompleted = "sale.completed"
	TypeCoinsCredited = "coins.credited"
	TypePing          = "ping"
)

// SourceEventType is the Kafka event type behind a webhook type (SaleCompleted for
// sale.completed), as DeliveryDead.source_event_type carries it; "" for ping, which has none.
func SourceEventType(webhookType string) string {
	switch webhookType {
	case TypeSaleCompleted:
		return "SaleCompleted"
	case TypeCoinsCredited:
		return "CoinsCredited"
	}
	return ""
}

// Subscribable lists the event types endpoints may choose.
var Subscribable = []string{TypeSaleCompleted, TypeCoinsCredited}

// Envelope is the body of every webhook. Data is the event itself, in snake_case JSON.
type Envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

// Body renders the envelope.
func Body(id, eventType string, created time.Time, data json.RawMessage) (string, error) {
	b, err := json.Marshal(Envelope{ID: id, Type: eventType, Created: created.Unix(), Data: data})
	return string(b), err
}

// NewSecret returns a fresh signing secret: whsec_ and 32 random bytes.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secret: %w", err)
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// Signature is the Doodle-Signature header for body sent at t:
// t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>" with the endpoint's secret>.
// The timestamp is inside the MAC, so a captured request can't be replayed with a new one.
func Signature(secret string, t time.Time, body string) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

func mac(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

// Tolerance is how old a signature receivers should accept (design doc 4.7).
const Tolerance = 5 * time.Minute

// ErrSignature: the header is missing, malformed, too old, or doesn't match.
var ErrSignature = errors.New("signature doesn't verify")

// Verify is what a receiver does: parse the header, refuse an old timestamp, and compare the
// MAC in constant time. The test receiver uses it; developers can copy it.
func Verify(secret, header, body string, now time.Time) error {
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || v1 == "" {
		return ErrSignature
	}
	age := now.Sub(time.Unix(sec, 0))
	if age > Tolerance || age < -Tolerance {
		return fmt.Errorf("%w: timestamp out of tolerance", ErrSignature)
	}
	if !hmac.Equal([]byte(v1), []byte(mac(secret, ts, body))) {
		return ErrSignature
	}
	return nil
}
