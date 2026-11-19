package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cembasaranoglu/trader/internal/pgtest"
)

// migrated returns a test database with all migrations applied.
func migrated(t *testing.T) *pgtest.DB {
	t.Helper()
	d := pgtest.New(t)
	if _, err := Migrate(context.Background(), d.Owner, d.AppRole); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// execErr runs SQL with the given connection string and returns its error,
// classified like the store's own errors.
func execErr(t *testing.T, dsn, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, sql, args...)
	return classify(err)
}
