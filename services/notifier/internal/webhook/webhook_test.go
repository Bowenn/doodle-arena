package webhook

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignatureRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body, _ := Body("evt_1", TypeSaleCompleted, now, json.RawMessage(`{"price":40}`))
	header := Signature("whsec_test", now, body)

	if !strings.HasPrefix(header, "t=1800000000,v1=") {
		t.Fatalf("header = %s", header)
	}
	if err := Verify("whsec_test", header, body, now.Add(time.Minute)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := `{"id":"evt_1"}`
	good := Signature("whsec_test", now, body)
	cases := map[string]struct {
		secret, header, body string
		at                   time.Time
	}{
		"wrong secret":    {"whsec_other", good, body, now},
		"changed body":    {"whsec_test", good, `{"id":"evt_2"}`, now},
		"too old":         {"whsec_test", good, body, now.Add(6 * time.Minute)},
		"from the future": {"whsec_test", good, body, now.Add(-6 * time.Minute)},
		"no header":       {"whsec_test", "", body, now},
		"no v1":           {"whsec_test", "t=1800000000", body, now},
		// A replayed body with a fresh timestamp: the MAC covers the timestamp.
		"new timestamp": {"whsec_test", strings.Replace(good, "t=1800000000", "t=1800000300", 1), body, now.Add(5 * time.Minute)},
	}
	for name, c := range cases {
		if err := Verify(c.secret, c.header, c.body, c.at); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestEnvelopeShape(t *testing.T) {
	body, _ := Body("evt_1", TypeCoinsCredited, time.Unix(1_800_000_000, 0), json.RawMessage(`{"amount":550}`))
	if body != `{"id":"evt_1","type":"coins.credited","created":1800000000,"data":{"amount":550}}` {
		t.Fatalf("body = %s", body)
	}
	s1, _ := NewSecret()
	s2, _ := NewSecret()
	if !strings.HasPrefix(s1, "whsec_") || len(s1) < 40 || s1 == s2 {
		t.Fatalf("secrets %q %q", s1, s2)
	}
}
