package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationsTable records applied migrations. It is namespaced so it cannot
// collide with another tool's table in a shared database.
const migrationsTable = "biga_schema_migrations"

// Migrate brings the schema up to date. It is safe to run from several
// instances at once: golang-migrate holds an advisory lock while it works.
func Migrate(ctx context.Context, databaseURL string, log *slog.Logger) error {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	target, err := migrateURL(databaseURL)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	defer m.Close()

	stop := context.AfterFunc(ctx, func() { m.GracefulStop <- true })
	defer stop()

	before, _, _ := m.Version()
	switch err := m.Up(); {
	case errors.Is(err, migrate.ErrNoChange):
		log.Info("database schema is up to date", "version", before)
		return nil
	case err != nil:
		return fmt.Errorf("apply migrations: %w", err)
	}
	after, _, _ := m.Version()
	log.Info("migrated database schema", "from", before, "to", after)
	return nil
}

// migrateURL converts a postgres:// URL for golang-migrate's pgx driver,
// dropping pgxpool-only settings that the plain driver would forward to the
// server as unknown runtime parameters.
func migrateURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("DATABASE_URL must be a postgres:// URL to run migrations")
	}
	u.Scheme = "pgx5"
	q := u.Query()
	for key := range q {
		if strings.HasPrefix(key, "pool_") {
			q.Del(key)
		}
	}
	q.Set("x-migrations-table", migrationsTable)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
