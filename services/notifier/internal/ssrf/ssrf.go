// Package ssrf keeps webhook deliveries away from internal addresses (design doc 4.7). Endpoint
// URLs come from outside developers, so a URL pointing at the cloud metadata service, a
// database or anything else inside the network must never get a request from the notifier.
//
// Two checks: the URL's shape when an endpoint is registered, and the resolved addresses when a
// delivery is sent. The second one matters because DNS can change between the two: the dialer
// resolves the host itself, refuses if any address is internal, and connects to the address it
// checked, so a rebinding DNS server can't swap in another one.
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Policy says which hosts may be reached.
type Policy struct {
	// allowed are exact hostnames that may resolve to private addresses and use plain http:
	// only the local test receiver (NOTIFIER_ALLOWED_PRIVATE_HOSTS). Empty in production.
	allowed  map[string]bool
	resolver *net.Resolver
}

// NewPolicy takes the comma-separated allow-list.
func NewPolicy(allowedHosts string) *Policy {
	p := &Policy{allowed: map[string]bool{}, resolver: net.DefaultResolver}
	for _, h := range strings.Split(allowedHosts, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			p.allowed[h] = true
		}
	}
	return p
}

// MaxURLLength bounds registered URLs.
const MaxURLLength = 2048

// ErrBlocked means the address or URL is not allowed.
var ErrBlocked = errors.New("blocked by the SSRF policy")

// CheckURL validates a URL at registration: absolute, https (http only for an allow-listed
// host), no credentials, no fragment, and not an IP literal in a blocked range.
func (p *Policy) CheckURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > MaxURLLength {
		return nil, fmt.Errorf("%w: the URL must have 1 to %d characters", ErrBlocked, MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: not an absolute URL", ErrBlocked)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "%"):
		return nil, fmt.Errorf("%w: the host must not contain a zone", ErrBlocked)
	case u.User != nil:
		return nil, fmt.Errorf("%w: the URL must not contain credentials", ErrBlocked)
	case u.Fragment != "":
		return nil, fmt.Errorf("%w: the URL must not contain a fragment", ErrBlocked)
	case u.Scheme == "https":
	case u.Scheme == "http" && p.allowed[host]:
	default:
		return nil, fmt.Errorf("%w: the URL must use https", ErrBlocked)
	}
	if addr, err := netip.ParseAddr(host); err == nil && Blocked(addr) {
		return nil, fmt.Errorf("%w: %s is an internal address", ErrBlocked, host)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if !p.allowed[host] {
			return nil, fmt.Errorf("%w: localhost isn't reachable", ErrBlocked)
		}
	}
	return u, nil
}

// blockedPrefixes are the ranges a webhook must never reach.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",         // "this network", including 0.0.0.0
		"10.0.0.0/8",        // private
		"100.64.0.0/10",     // carrier-grade NAT, used inside some clouds
		"127.0.0.0/8",       // loopback
		"169.254.0.0/16",    // link-local, including the metadata service 169.254.169.254
		"172.16.0.0/12",     // private (Docker's default networks)
		"192.0.0.0/24",      // IETF protocol assignments
		"192.168.0.0/16",    // private
		"198.18.0.0/15",     // benchmarking
		"224.0.0.0/4",       // multicast
		"240.0.0.0/4",       // reserved, and the broadcast address
		"::/128", "::1/128", // unspecified, loopback
		"fc00::/7",                       // unique local
		"fe80::/10",                      // link-local
		"ff00::/8",                       // multicast
		"64:ff9b::/96", "64:ff9b:1::/48", // NAT64, which can reach IPv4 ranges above
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Blocked reports whether addr is in a range webhooks must not reach. IPv4-mapped IPv6
// addresses (::ffff:10.0.0.1) are judged as the IPv4 address they carry, and an IPv6 zone
// (fe80::1%eth0) is dropped first: netip.Prefix.Contains is false for any zoned address, which
// would otherwise let a zoned link-local address through.
func Blocked(addr netip.Addr) bool {
	addr = addr.WithZone("").Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// DialContext resolves the host, refuses internal addresses (unless the host is allow-listed),
// and connects to a checked address. Use it as the transport's DialContext.
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	allowed := p.allowed[strings.ToLower(host)]
	for _, a := range addrs {
		if a.Zone() != "" {
			return nil, fmt.Errorf("%w: %s resolves to a zoned address", ErrBlocked, host)
		}
		if Blocked(a) && !allowed {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlocked, host, a.Unmap())
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	var lastErr error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
