package rooms

import (
	"context"
	"fmt"
	"sync"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

// Allocator asks a game server to own a room.
type Allocator interface {
	AllocateRoom(ctx context.Context, controlAddress string, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error)
	// Forget drops the connection to an address, so the next call dials again and looks the
	// name up afresh.
	Forget(controlAddress string)
}

// GRPCAllocator calls doodle.game.v1.ControlService, keeping one connection per game server.
// The control port is internal only; traffic stays on the private network, without TLS
// locally (mTLS between services is a later step).
type GRPCAllocator struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// NewGRPCAllocator returns an allocator with no connections yet.
func NewGRPCAllocator() *GRPCAllocator {
	return &GRPCAllocator{conns: make(map[string]*grpc.ClientConn)}
}

// AllocateRoom implements Allocator. The caller sets the deadline.
func (a *GRPCAllocator) AllocateRoom(ctx context.Context, addr string, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	conn, err := a.conn(addr)
	if err != nil {
		return nil, err
	}
	return gamev1.NewControlServiceClient(conn).AllocateRoom(ctx, req)
}

func (a *GRPCAllocator) conn(addr string) (*grpc.ClientConn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.conns[addr]; ok {
		return c, nil
	}
	// NewClient doesn't dial; the connection is made (and remade) lazily by the first call.
	c, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		// Keep reconnects fast (gRPC's default backoff grows to 2 minutes): a game server that
		// restarted must be reachable again by the time its heartbeats return.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 250 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 2 * time.Second},
			MinConnectTimeout: time.Second,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("game server %s: %w", addr, err)
	}
	a.conns[addr] = c
	return c, nil
}

// Forget implements Allocator. A cached connection keeps the IP address it resolved; after
// game-server containers are recreated and swap addresses, it can reach the wrong instance.
func (a *GRPCAllocator) Forget(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.conns[addr]; ok {
		_ = c.Close()
		delete(a.conns, addr)
	}
}

// Close closes every connection.
func (a *GRPCAllocator) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for addr, c := range a.conns {
		_ = c.Close()
		delete(a.conns, addr)
	}
	return nil
}
