package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

func TestReceiverVerifiesAndObeysItsMode(t *testing.T) {
	srv := httptest.NewServer((&receiver{hooks: map[string]*hook{}}).routes())
	defer srv.Close()
	put := func(path, body string) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %v %v", path, resp, err)
		}
	}
	send := func(secret string) int {
		body := `{"id":"evt-1"}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/demo", strings.NewReader(body))
		req.Header.Set("Doodle-Signature", webhook.Signature(secret, time.Now(), body))
		req.Header.Set("Doodle-Event-Id", "evt-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	put("/admin/demo/secret", "whsec_right")
	if got := send("whsec_right"); got != 200 {
		t.Fatalf("signed right: %d", got)
	}
	if got := send("whsec_wrong"); got != 401 {
		t.Fatalf("signed wrong: %d", got)
	}
	put("/admin/demo/mode", "fail")
	if got := send("whsec_right"); got != 500 {
		t.Fatalf("fail mode: %d", got)
	}

	resp, _ := http.Get(srv.URL + "/admin/demo")
	var h hook
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Mode != "fail" || len(h.Received) != 3 || !h.Received[0].Verified || h.Received[1].Verified {
		t.Fatalf("hook = %+v", h)
	}
}

func TestReceiverKeepsUnsignedRequestsOnHooksWithoutASecret(t *testing.T) {
	srv := httptest.NewServer((&receiver{hooks: map[string]*hook{}}).routes())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/hooks/alerts", "application/json", strings.NewReader(`{"content":"[FIRING] LedgerImbalance"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("unsigned POST: %v %v", resp, err)
	}

	resp, _ = http.Get(srv.URL + "/admin/alerts")
	var h hook
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if len(h.Received) != 1 || h.Received[0].Verified || !strings.Contains(h.Received[0].Body, "LedgerImbalance") {
		t.Fatalf("hook = %+v", h)
	}
}
