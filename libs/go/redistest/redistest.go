// Package redistest starts one real Redis per test package with Testcontainers.
package redistest

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Bowenn/doodle-arena/libs/go/tcretry"
	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// Image matches the Redis in infra/docker-compose.yml.
const Image = "redis:8.10.2-trixie"

var addr string

// Main starts Redis, runs the package's tests and stops it. Call it from TestMain.
func Main(m *testing.M) {
	ctx := context.Background()
	c, err := tcretry.Run("redis", func() (*tcredis.RedisContainer, error) { return tcredis.Run(ctx, Image) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		os.Exit(1)
	}
	endpoint, err := c.Endpoint(ctx, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis endpoint:", err)
		os.Exit(1)
	}
	addr = endpoint
	code := m.Run()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

// New returns a client to the package's Redis, emptied first. Tests that use it must not run
// in parallel with each other.
func New(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.FlushAll(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}
