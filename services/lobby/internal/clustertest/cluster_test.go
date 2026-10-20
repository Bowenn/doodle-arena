// Package clustertest runs the lobby's Redis code against Redis Cluster. Redis is one node today,
// but every multi-key MULTI and Lua script must keep working when it isn't: Redis Cluster refuses
// a transaction or script whose keys hash to different slots (CROSSSLOT). A one-node cluster that
// owns every slot enforces that rule as a real one does, so these tests catch an untagged key.
package clustertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/libs/go/tcretry"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/leaderboard"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/registry"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/rooms"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image matches the Redis in infra/docker-compose.yml.
const image = "redis:8.10.2-trixie"

// cluster starts a one-node Redis Cluster that owns all 16384 slots and returns a cluster
// client. The node announces its address inside Docker, so the client dials the mapped port
// for every node address it learns.
func cluster(t *testing.T) *redis.ClusterClient {
	t.Helper()
	ctx := t.Context()
	c, err := tcretry.Run("redis cluster", func() (*testcontainers.DockerContainer, error) {
		return testcontainers.Run(ctx, image,
			testcontainers.WithExposedPorts("6379/tcp"),
			testcontainers.WithCmd("redis-server", "--cluster-enabled", "yes", "--appendonly", "no"),
			testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
		)
	})
	if err != nil {
		t.Fatalf("start redis cluster: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	if code, _, err := c.Exec(ctx, []string{"redis-cli", "cluster", "addslotsrange", "0", "16383"}); err != nil || code != 0 {
		t.Fatalf("cluster addslotsrange: exit %d, %v", code, err)
	}
	endpoint, err := c.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: []string{endpoint},
		Dialer: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, endpoint)
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := client.ClusterInfo(ctx).Result()
		if err == nil && strings.Contains(info, "cluster_state:ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster not ok: %q, %v", info, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return client
}

func TestLobbyRedisWorksOnRedisCluster(t *testing.T) {
	ctx := t.Context()
	rdb := cluster(t)

	// The control: keys in different slots in one transaction are refused, so the cluster
	// really enforces the rule the lobby's keys are tagged for.
	_, err := rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, "lobby:room:x", "1", time.Minute)
		p.ZAdd(ctx, "lobby:rooms:public", redis.Z{Score: 1, Member: "x"})
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "CROSSSLOT") {
		t.Fatalf("an untagged two-key transaction: err = %v, want CROSSSLOT", err)
	}

	store := rooms.NewStore(rdb)
	now := time.Now().UTC()
	r := rooms.Room{ID: "r1", HostID: "u1", Mode: "classic", Public: true, ServerID: "gs-1", CreatedAt: now, PlacedAt: now}
	steps := []struct {
		name string
		run  func() error
	}{
		{"rooms.Save", func() error { return store.Save(ctx, r) }},
		{"rooms.Touch", func() error { return store.Touch(ctx, "gs-1", []string{"r1"}) }},
		{"rooms.Get", func() error { _, err := store.Get(ctx, "r1"); return err }},
		{"rooms.Hosts", func() error { _, err := store.Hosts(ctx); return err }},
		{"rooms.RoomsOf", func() error { _, err := store.RoomsOf(ctx, "gs-1"); return err }},
		{"rooms.PublicPage", func() error { _, _, err := store.PublicPage(ctx, nil, 10); return err }},
		{"rooms.Move", func() error {
			moved := r
			moved.ServerID, moved.PlacedAt = "gs-2", time.Now().UTC()
			if err := store.Move(ctx, moved, "gs-1"); err != nil {
				return err
			}
			r = moved
			return nil
		}},
		{"rooms.Unindex", func() error { return store.Unindex(ctx, "gs-1", "r1") }},
		{"rooms.Remove", func() error { return store.Remove(ctx, r) }},
	}
	reg := registry.New(rdb, registry.DefaultTTL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	steps = append(steps,
		struct {
			name string
			run  func() error
		}{"registry.Record", func() error {
			_, err := reg.Record(ctx, registry.Server{ID: "gs-1", WSAddress: "/ws/gs-1", ControlAddress: "gs-1:9090"})
			return err
		}},
		struct {
			name string
			run  func() error
		}{"registry.Servers", func() error {
			servers, err := reg.Servers(ctx)
			if err == nil && len(servers) != 1 {
				err = fmt.Errorf("%d servers, want 1", len(servers))
			}
			return err
		}},
	)
	board := leaderboard.NewStore(rdb)
	match := leaderboard.Match{ID: "m1", EndedAt: now, Results: []leaderboard.Result{{UserID: "u1", Score: 10}, {UserID: "u2", Score: 5}}}
	steps = append(steps,
		struct {
			name string
			run  func() error
		}{"leaderboard.Apply", func() error { _, err := board.Apply(ctx, match); return err }},
		struct {
			name string
			run  func() error
		}{"leaderboard.Apply again", func() error {
			counted, err := board.Apply(ctx, match)
			if err == nil && counted {
				err = errors.New("a redelivered match counted twice")
			}
			return err
		}},
		struct {
			name string
			run  func() error
		}{"leaderboard.Board", func() error {
			_, err := board.Board(ctx, leaderboard.Week(now), 10, "u1")
			return err
		}},
	)
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Errorf("%s on Redis Cluster: %v", step.name, err)
		}
	}
}
