package heartbeat

import (
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"

	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/grpc"
)

// slowTXTDNS is a DNS server that answers A queries for one name with 127.0.0.1 at once and
// never answers TXT queries, like a cold upstream resolver behind Docker's embedded DNS.
type slowTXTDNS struct {
	name dnsmessage.Name
	txt  atomic.Int32 // TXT queries seen
}

func (d *slowTXTDNS) serve(conn net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return // closed
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		if q.Type == dnsmessage.TypeTXT {
			d.txt.Add(1)
			continue // never answered
		}
		resp := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true},
			Questions: []dnsmessage.Question{q},
		}
		switch {
		case q.Name != d.name:
			resp.Header.RCode = dnsmessage.RCodeNameError
		case q.Type == dnsmessage.TypeA:
			resp.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			}}
		}
		// AAAA: no records, no error.
		out, err := resp.Pack()
		if err != nil {
			continue
		}
		_, _ = conn.WriteTo(out, from)
	}
}

// A freshly started game server must reach the lobby with its first heartbeat. gRPC's DNS
// resolver also looks up a TXT record for a service config by default, and reports the
// addresses only after that lookup ends; behind a cold upstream it took seconds, so the first
// beats (each with a 1-second deadline) failed and the server registered late.
func TestFirstHeartbeatDoesNotWaitForATXTLookup(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lobby := &fakeLobby{}
	gs := grpc.NewServer()
	lobbyv1.RegisterRegistryServiceServer(gs, lobby)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	dns := &slowTXTDNS{name: dnsmessage.MustNewName("lobby-cold.test.")}
	go dns.serve(udp)

	_, port, _ := net.SplitHostPort(lis.Addr().String())
	// dns://<server>/<name>: gRPC's DNS resolver asks that server instead of the system's.
	conn, err := Dial("dns://" + udp.LocalAddr().String() + "/lobby-cold.test:" + port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	id := Identity{ServerID: "gs-cold", WSAddress: "/ws/gs-cold", ControlAddress: "gs-cold:9090"}
	sender := NewSender(lobbyv1.NewRegistryServiceClient(conn), id, fixedStatus{}, Interval,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := sender.beat(t.Context()); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if lobby.count() != 1 {
		t.Fatalf("lobby got %d heartbeats, want 1", lobby.count())
	}
	if n := dns.txt.Load(); n != 0 {
		t.Fatalf("%d TXT lookups, want none", n)
	}
}
