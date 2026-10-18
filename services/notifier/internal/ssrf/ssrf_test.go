package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBlockedRanges(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "127.255.255.254": true, "10.1.2.3": true, "172.16.0.1": true,
		"172.31.255.255": true, "192.168.1.1": true, "169.254.169.254": true, "0.0.0.0": true,
		"100.64.0.1": true, "::1": true, "::": true, "fc00::1": true, "fd12:3456::1": true,
		"fe80::1": true, "::ffff:10.0.0.1": true, "::ffff:169.254.169.254": true,
		"64:ff9b::a00:1": true, "224.0.0.1": true, "255.255.255.255": true,
		// A zone must not hide a link-local or loopback address.
		"fe80::1%eth0": true, "fe80::1%25": true, "::1%lo0": true,
		// Public addresses go through.
		"8.8.8.8": false, "1.1.1.1": false, "172.32.0.1": false, "2606:4700::1111": false,
		"::ffff:8.8.8.8": false,
	}
	for s, want := range cases {
		if got := Blocked(netip.MustParseAddr(s)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	p := NewPolicy("webhook-receiver")
	ok := []string{
		"https://example.com/hooks",
		"https://example.com:8443/a?b=c",
		"http://webhook-receiver:9000/hooks/demo", // allow-listed: http allowed
		"https://webhook-receiver/x",
	}
	for _, u := range ok {
		if _, err := p.CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
	bad := []string{
		"", "example.com/hooks", "ftp://example.com", "http://example.com/hooks",
		"https://user:pass@example.com/", "https://example.com/#frag",
		"https://127.0.0.1/", "https://[::1]/", "https://169.254.169.254/latest/meta-data",
		"https://10.0.0.5:8080/", "https://localhost/", "https://api.localhost/",
		"https://[fe80::1%25eth0]/", "https://[fe80::1%25en0]:8443/x",
		"https://example.com/" + strings.Repeat("a", MaxURLLength),
		"mailto:dev@example.com",
	}
	for _, u := range bad {
		if _, err := p.CheckURL(u); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckURL(%q) accepted", u)
		}
	}
}

func TestDialRefusesHostsThatResolveInside(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// "localhost" resolves to a loopback address, like a DNS name pointed at an internal IP.
	p := NewPolicy("")
	if _, err := p.DialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port)); !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial localhost: err = %v, want ErrBlocked", err)
	}
	if _, err := p.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port)); !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial 127.0.0.1: err = %v, want ErrBlocked", err)
	}

	// Only an exact allow-listed name gets through.
	allowed := NewPolicy("localhost")
	conn, err := allowed.DialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("allow-listed dial: %v", err)
	}
	conn.Close()
}
