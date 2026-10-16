package rooms

import (
	"context"
	"fmt"
	"sync"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Allocator asks a game server to own a room.
type Allocator interface {
	AllocateRoom(ctx context.Context, controlAddress string, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error)
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
	)
	if err != nil {
		return nil, fmt.Errorf("game server %s: %w", addr, err)
	}
	a.conns[addr] = c
	return c, nil
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
