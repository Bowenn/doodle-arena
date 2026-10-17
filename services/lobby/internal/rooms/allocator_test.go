package rooms

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/grpc"
)

// echoGame answers every allocation as the server it was meant for.
type echoGame struct {
	gamev1.UnimplementedControlServiceServer
}

func (echoGame) AllocateRoom(_ context.Context, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	return &gamev1.AllocateRoomResponse{ServerId: req.GetServerId()}, nil
}

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

// A freshly started lobby must reach a game server within the first call's deadline. gRPC's
// DNS resolver also looks up a TXT record for a service config by default, and reports the
// addresses only after that lookup ends; behind a cold upstream it took 5-8 s, so the first
// AllocateRoom calls to every server ran out of time.
func TestFirstAllocationDoesNotWaitForATXTLookup(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	gamev1.RegisterControlServiceServer(gs, echoGame{})
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	dns := &slowTXTDNS{name: dnsmessage.MustNewName("gs-cold.test.")}
	go dns.serve(udp)

	a := NewGRPCAllocator()
	defer a.Close()
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	// dns://<server>/<name>: gRPC's DNS resolver asks that server instead of the system's.
	target := "dns://" + udp.LocalAddr().String() + "/gs-cold.test:" + port

	ctx, cancel := context.WithTimeout(t.Context(), allocateTimeout)
	defer cancel()
	resp, err := a.AllocateRoom(ctx, target, &gamev1.AllocateRoomRequest{RoomId: "r1", ServerId: "gs-cold"})
	if err != nil {
		t.Fatalf("first AllocateRoom: %v", err)
	}
	if resp.GetServerId() != "gs-cold" {
		t.Fatalf("server_id = %q", resp.GetServerId())
	}
	if n := dns.txt.Load(); n != 0 {
		t.Fatalf("%d TXT lookups, want none", n)
	}
}
