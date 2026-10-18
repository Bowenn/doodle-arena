// Package store owns the notifier's Postgres database (design doc, section 5): endpoints,
// deliveries and their attempts. Migrations are embedded and applied at startup.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// scheme
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store wraps the connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres (url like postgres://user:pass@host:5432/notifier) and applies any
// pending migration. golang-migrate takes an advisory lock, so two instances starting together
// don't both migrate.
func Open(ctx context.Context, url string) (*Store, error) {
	if err := Migrate(url); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Migrate applies the embedded migrations up to the latest.
func Migrate(url string) error {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, "pgx5://"+trimScheme(url))
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

func trimScheme(url string) string {
	for _, p := range []string{"postgres://", "postgresql://"} {
		if len(url) > len(p) && url[:len(p)] == p {
			return url[len(p):]
		}
	}
	return url
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }
