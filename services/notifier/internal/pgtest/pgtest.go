// Package pgtest starts a real Postgres for integration tests, the same image as
// infra/docker-compose.yml, and migrates it with the notifier's own migrations. It starts a
// container per test, in packages go test runs in parallel, so it goes through tcretry like the
// other Go helpers: two packages starting their first container at once can race on
// Testcontainers' reaper.
package pgtest

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Bowenn/doodle-arena/libs/go/tcretry"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/store"
)

// Image matches infra/docker-compose.yml.
const Image = "postgres:18.6-trixie"

// Store returns a migrated store on a fresh database, closed when the test ends.
func Store(t *testing.T) *store.Store {
	t.Helper()
	ctx := t.Context()
	c, err := tcretry.Run("postgres", func() (*postgres.PostgresContainer, error) {
		return postgres.Run(ctx, Image,
			postgres.WithDatabase("notifier"),
			postgres.WithUsername("notifier"),
			postgres.WithPassword("notifier"),
			postgres.BasicWaitStrategies(),
		)
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres url: %v", err)
	}
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
