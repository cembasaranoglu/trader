package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
	sum     [32]byte
}

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// migrations returns the embedded migrations in order.
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: bad migration file name %q", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b), sum: sha256.Sum256(b)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered 1..n without gaps (found %d at position %d)", m.version, i+1)
		}
	}
	return out, nil
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Migrate applies pending migrations as the owner role, each in its own
// transaction under a migration lock, then grants the application role its
// privileges. appRole must already exist (CREATE ROLE is an operator step,
// see the deploy runbook) and must differ from the owner. It returns the
// versions applied by this call.
func Migrate(ctx context.Context, dsn, appRole string) ([]int, error) {
	if !roleName.MatchString(appRole) {
		return nil, fmt.Errorf("store: invalid application role name %q", appRole)
	}
	cfg, err := ParseDSN(dsn, 0, 0)
	if err != nil {
		return nil, err
	}
	// Migrations can take longer than the per-statement default, and must
	// create objects in public whatever the owner's default search_path is.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "300000"
	cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err := checkServerVersion(ctx, conn); err != nil {
		return nil, err
	}
	var owner string
	var appExists bool
	if err := conn.QueryRow(ctx, `SELECT current_user, EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, appRole).Scan(&owner, &appExists); err != nil {
		return nil, fmt.Errorf("store: roles: %w", err)
	}
	if owner == appRole {
		return nil, errors.New("store: migrate must run as the owner role, not the application role")
	}
	if !appExists {
		return nil, fmt.Errorf("store: application role %q does not exist (create it first; see docs/runbook/deploy.md)", appRole)
	}
	ms, err := migrations()
	if err != nil {
		return nil, err
	}
	var applied []int
	for _, m := range ms {
		done, err := applyOne(ctx, conn, m)
		if err != nil {
			return applied, err
		}
		if done {
			applied = append(applied, m.version)
		}
	}
	if err := grant(ctx, conn, appRole); err != nil {
		return applied, err
	}
	return applied, nil
}

// applyOne applies m unless it is already recorded (with the same checksum).
func applyOne(ctx context.Context, conn *pgx.Conn, m migration) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('trader.migrate', 0))`); err != nil {
		return false, fmt.Errorf("store: migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		sha256     bytea NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return false, fmt.Errorf("store: schema_migrations: %w", err)
	}
	var sum []byte
	err = tx.QueryRow(ctx, `SELECT sha256 FROM schema_migrations WHERE version = $1`, m.version).Scan(&sum)
	switch {
	case err == nil:
		if string(sum) != string(m.sum[:]) {
			return false, fmt.Errorf("%w: migration %s was changed after it was applied", ErrSchema, m.name)
		}
		return false, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("store: read migrations: %w", err)
	}
	var maxV int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&maxV); err != nil {
		return false, err
	}
	if maxV >= m.version {
		return false, fmt.Errorf("%w: migration %d missing below applied version %d", ErrSchema, m.version, maxV)
	}
	// Simple protocol: the file holds several statements and function bodies.
	if _, err := tx.Exec(ctx, m.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		return false, fmt.Errorf("store: apply %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, sha256) VALUES ($1, $2, $3)`, m.version, m.name, m.sum[:]); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Tables and the privileges the application role gets on them.
var (
	recordTables  = []string{"events", "fills", "balances_snapshot", "approvals", "tax_ledger"}
	mutableTables = []string{"intents", "orders"}
	readTables    = []string{"leases", "schema_migrations"}
	leaseFuncs    = []string{"trader_acquire_lease(text, text)", "trader_release_lease(text)"}
)

func grant(ctx context.Context, conn *pgx.Conn, appRole string) error {
	r := pgx.Identifier{appRole}.Sanitize()
	var db string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return fmt.Errorf("store: grant: %w", err)
	}
	d := pgx.Identifier{db}.Sanitize()
	stmts := []string{
		// No temporary objects: a temporary table could shadow a store table
		// in a session's own queries.
		`REVOKE TEMPORARY ON DATABASE ` + d + ` FROM PUBLIC`,
		`REVOKE TEMPORARY ON DATABASE ` + d + ` FROM ` + r,
		`GRANT USAGE ON SCHEMA public TO ` + r,
		`REVOKE ALL ON ` + strings.Join(slices.Concat(recordTables, mutableTables, readTables), ", ") + ` FROM PUBLIC`,
		`REVOKE ALL ON ` + strings.Join(slices.Concat(recordTables, mutableTables, readTables), ", ") + ` FROM ` + r,
		`GRANT SELECT, INSERT ON ` + strings.Join(recordTables, ", ") + ` TO ` + r,
		`GRANT SELECT, INSERT, UPDATE ON ` + strings.Join(mutableTables, ", ") + ` TO ` + r,
		`GRANT SELECT ON ` + strings.Join(readTables, ", ") + ` TO ` + r,
	}
	for _, f := range leaseFuncs {
		stmts = append(stmts, `REVOKE ALL ON FUNCTION `+f+` FROM PUBLIC`, `GRANT EXECUTE ON FUNCTION `+f+` TO `+r)
	}
	for _, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			return fmt.Errorf("store: grant: %w", err)
		}
	}
	return nil
}

// checkMigrations compares the recorded migrations with the embedded ones.
func checkMigrations(ctx context.Context, q pgxQuerier) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("store: read schema: %w", err)
	}
	if !exists {
		return ErrSchemaOld
	}
	rows, err := q.Query(ctx, `SELECT version, sha256 FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var v int
		var sum []byte
		if err := rows.Scan(&v, &sum); err != nil {
			return err
		}
		if i >= len(ms) {
			return fmt.Errorf("%w (migration %d unknown to this binary)", ErrSchemaNewer, v)
		}
		if v != ms[i].version || string(sum) != string(ms[i].sum[:]) {
			return fmt.Errorf("%w: migration %d does not match %s", ErrSchema, v, ms[i].name)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if i < len(ms) {
		return ErrSchemaOld
	}
	return nil
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// guard is a trigger that must exist, be enabled and call fn.
type guard struct{ table, trigger, fn string }

var guards = []guard{
	{"events", "events_append", "trader_events_append"},
	{"events", "events_forbid", "trader_forbid"},
	{"events", "events_forbid_truncate", "trader_forbid"},
	{"intents", "intents_fence", "trader_fence"},
	{"orders", "orders_fence", "trader_fence"},
	{"intents", "intents_forbid", "trader_forbid"},
	{"orders", "orders_forbid", "trader_forbid"},
	{"intents", "intents_forbid_truncate", "trader_forbid"},
	{"orders", "orders_forbid_truncate", "trader_forbid"},
	{"fills", "fills_forbid", "trader_forbid"},
	{"balances_snapshot", "balances_snapshot_forbid", "trader_forbid"},
	{"approvals", "approvals_forbid", "trader_forbid"},
	{"tax_ledger", "tax_ledger_forbid", "trader_forbid"},
	{"fills", "fills_forbid_truncate", "trader_forbid"},
	{"balances_snapshot", "balances_snapshot_forbid_truncate", "trader_forbid"},
	{"approvals", "approvals_forbid_truncate", "trader_forbid"},
	{"tax_ledger", "tax_ledger_forbid_truncate", "trader_forbid"},
}

// Functions the migrations define, and whether each is SECURITY DEFINER.
// The set in the database must equal this one (extra or missing functions
// in public are refused).
var functionSecDef = map[string]bool{
	"trader_forbid":        false,
	"trader_events_append": false,
	"trader_lease_key":     false,
	"trader_acquire_lease": true,
	"trader_release_lease": true,
	"trader_fence":         true,
}

// pinnedSearchPath is the search_path every function must set.
const pinnedSearchPath = "search_path=pg_catalog, public, pg_temp"

var createFunction = regexp.MustCompile(`(?i)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+(?:public\.)?(\w+)\s*\(`)
var dollarOpen = regexp.MustCompile(`(?i)\bAS\s+\$(\w*)\$`)

// parseFunctions returns the body (text between the dollar quotes, as
// pg_proc.prosrc stores it) of every function a migration defines. A later
// definition of the same name replaces an earlier one.
func parseFunctions(sql string, out map[string]string) error {
	locs := createFunction.FindAllStringSubmatchIndex(sql, -1)
	for i, loc := range locs {
		name := strings.ToLower(sql[loc[2]:loc[3]])
		rest := sql[loc[1]:]
		// The body's opening quote must come before the next definition.
		head := rest
		if i+1 < len(locs) {
			head = sql[loc[1]:locs[i+1][0]]
		}
		m := dollarOpen.FindStringSubmatchIndex(head)
		if m == nil {
			return fmt.Errorf("store: function %s: no dollar-quoted body", name)
		}
		tag := "$" + rest[m[2]:m[3]] + "$"
		body := rest[m[1]:]
		end := strings.Index(body, tag)
		if end < 0 {
			return fmt.Errorf("store: function %s: unterminated body", name)
		}
		out[name] = body[:end]
	}
	return nil
}

// expectedFunctions returns the source of every function the migrations
// define.
func expectedFunctions() (map[string]string, error) {
	ms, err := migrations()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range ms {
		if err := parseFunctions(m.sql, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkGuards verifies that every guard trigger exists, is enabled and calls
// its function, and that every function's source equals the migration's.
func checkGuards(ctx context.Context, q pgxQuerier) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname, t.tgname, p.proname, t.tgenabled
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE NOT t.tgisinternal AND n.nspname = 'public'`)
	if err != nil {
		return fmt.Errorf("store: read triggers: %w", err)
	}
	type trig struct{ fn, enabled string }
	found := map[string]trig{}
	for rows.Next() {
		var table, name, fn, enabled string
		if err := rows.Scan(&table, &name, &fn, &enabled); err != nil {
			rows.Close()
			return err
		}
		found[table+"."+name] = trig{fn, enabled}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range guards {
		t, ok := found[g.table+"."+g.trigger]
		switch {
		case !ok:
			return fmt.Errorf("%w: trigger %s on %s missing", ErrSchema, g.trigger, g.table)
		case t.fn != g.fn:
			return fmt.Errorf("%w: trigger %s calls %s, want %s", ErrSchema, g.trigger, t.fn, g.fn)
		case t.enabled != "O" && t.enabled != "A":
			return fmt.Errorf("%w: trigger %s is disabled", ErrSchema, g.trigger)
		}
	}
	want, err := expectedFunctions()
	if err != nil {
		return err
	}
	if len(want) != len(functionSecDef) {
		return fmt.Errorf("store: migrations define %d functions, expected %d", len(want), len(functionSecDef))
	}
	frows, err := q.Query(ctx, `SELECT p.proname, p.prosrc, p.prosecdef, COALESCE(p.proconfig, '{}')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'`)
	if err != nil {
		return fmt.Errorf("store: read functions: %w", err)
	}
	seen := map[string]bool{}
	for frows.Next() {
		var name, src string
		var secdef bool
		var conf []string
		if err := frows.Scan(&name, &src, &secdef, &conf); err != nil {
			frows.Close()
			return err
		}
		body, known := want[name]
		switch {
		case !known:
			frows.Close()
			return fmt.Errorf("%w: unexpected function %s in schema public", ErrSchema, name)
		case seen[name]:
			frows.Close()
			return fmt.Errorf("%w: function %s is overloaded", ErrSchema, name)
		case src != body:
			frows.Close()
			return fmt.Errorf("%w: function %s differs from its migration", ErrSchema, name)
		case secdef != functionSecDef[name]:
			frows.Close()
			return fmt.Errorf("%w: function %s SECURITY DEFINER is %v, want %v", ErrSchema, name, secdef, functionSecDef[name])
		case len(conf) != 1 || conf[0] != pinnedSearchPath:
			frows.Close()
			return fmt.Errorf("%w: function %s must set %q, has %v", ErrSchema, name, pinnedSearchPath, conf)
		}
		seen[name] = true
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return err
	}
	for name := range want {
		if !seen[name] {
			return fmt.Errorf("%w: function %s missing", ErrSchema, name)
		}
	}
	return nil
}

// checkPrivileges refuses a role that could rewrite records: a superuser,
// the owner (or a member of the owner role) of a store table, or a role with
// UPDATE, DELETE or TRUNCATE where the application role has none.
func checkPrivileges(ctx context.Context, q pgxQuerier) error {
	var user string
	var super, createRole, temp bool
	if err := q.QueryRow(ctx, `SELECT current_user, rolsuper, rolcreaterole, has_database_privilege(current_user, current_database(), 'TEMPORARY')
		FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super, &createRole, &temp); err != nil {
		return fmt.Errorf("store: read role: %w", err)
	}
	if super || createRole {
		return fmt.Errorf("%w: role %s is a superuser or can create roles", ErrPrivileges, user)
	}
	if temp {
		return fmt.Errorf("%w: role %s can create temporary tables (run 'traderd store migrate', which revokes TEMPORARY)", ErrPrivileges, user)
	}
	all := slices.Concat(recordTables, mutableTables, readTables)
	for _, t := range all {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, t).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrSchemaOld
		}
		var owns bool
		if err := q.QueryRow(ctx, `SELECT pg_has_role(current_user, c.relowner, 'MEMBER') FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = $1`, t).Scan(&owns); err != nil {
			return err
		}
		if owns {
			return fmt.Errorf("%w: role %s owns table %s (connect as the application role)", ErrPrivileges, user, t)
		}
		forbidden := []string{"UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
		switch {
		case slices.Contains(mutableTables, t):
			forbidden = []string{"DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
		case slices.Contains(readTables, t):
			forbidden = append(forbidden, "INSERT")
		}
		for _, priv := range forbidden {
			var has bool
			if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, t, priv).Scan(&has); err != nil {
				return err
			}
			if has {
				return fmt.Errorf("%w: role %s has %s on %s", ErrPrivileges, user, priv, t)
			}
		}
	}
	return nil
}
