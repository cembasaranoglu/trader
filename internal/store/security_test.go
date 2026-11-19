package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cembasaranoglu/trader/internal/pgtest"
)

// appSession runs statements as the application role in one session (temp
// objects live per session) and returns the first error.
func appSession(t *testing.T, dsn string, stmts ...string) error {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, st := range stmts {
		if _, err := c.Exec(ctx, st); err != nil {
			return classify(err)
		}
	}
	return nil
}

func superBool(t *testing.T, d *pgtest.DB, q string, args ...any) bool {
	t.Helper()
	c, err := pgx.Connect(ctx, d.Super)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var b bool
	if err := c.QueryRow(ctx, q, args...).Scan(&b); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return b
}

func TestApplicationRoleCannotCreateTemporaryObjects(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	err := appSession(t, d.App, `CREATE TEMP TABLE leases (name text, epoch bigint)`)
	if !errors.Is(err, ErrPrivileges) {
		t.Fatalf("temp table: %v", err)
	}
}

// The search_path attacks found in review, replayed with TEMPORARY granted
// back (a misconfiguration): they must have no effect because every
// function pins search_path with pg_temp last and qualifies names.
func TestSearchPathAttacksAreInert(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	d.Exec(t, fmt.Sprintf(`GRANT TEMPORARY ON DATABASE %s TO %s`, d.Name, d.AppRole))

	t.Run("escalation through a temp leases table", func(t *testing.T) {
		err := appSession(t, d.App,
			`CREATE TEMP TABLE leases (name text, epoch bigint, holder text, acquired_at timestamptz)`,
			`CREATE FUNCTION pg_temp.evil() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN EXECUTE 'GRANT ALL ON ALL TABLES IN SCHEMA public TO ' || quote_ident(session_user); RETURN NEW; END $$`,
			`CREATE TRIGGER evil BEFORE UPDATE ON pg_temp.leases FOR EACH ROW EXECUTE FUNCTION pg_temp.evil()`,
			`INSERT INTO pg_temp.leases VALUES ('executor', 0, NULL, NULL)`,
			`SELECT trader_acquire_lease('executor', 'attacker')`,
		)
		if err != nil {
			t.Fatalf("attack setup: %v", err)
		}
		if superBool(t, d, `SELECT has_table_privilege($1, 'events', 'DELETE')`, d.AppRole) {
			t.Fatal("the application role gained DELETE on events")
		}
		if !superBool(t, d, `SELECT epoch = 1 FROM leases WHERE name = 'executor'`) {
			t.Fatal("the real lease row was not the one updated")
		}
	})

	t.Run("fencing through a temp leases view", func(t *testing.T) {
		s := openAppUnchecked(t, d)
		err := appSession(t, d.App,
			`CREATE TEMP VIEW leases AS SELECT 'executor'::text AS name, 999::bigint AS epoch`,
		)
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.Append(ctx, "intent_created", ev{V: 1})
		if err != nil {
			t.Fatal(err)
		}
		err = appSession(t, d.App,
			`CREATE TEMP VIEW leases AS SELECT 'executor'::text AS name, 999::bigint AS epoch`,
			fmt.Sprintf(`INSERT INTO intents (intent_id, created_seq, epoch, policy_version, symbol, side, qty, state, updated_ns)
			 VALUES ('x', %d, 999, 'p', 'BTCUSDT', 'BUY', '1', 'PLANNED', 0)`, e.Seq),
		)
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("stale epoch through a shadow view: %v", err)
		}
	})

	t.Run("chain through a temp events table", func(t *testing.T) {
		err := appSession(t, d.App,
			`CREATE TEMP TABLE events (seq bigint, hash bytea)`,
			`INSERT INTO pg_temp.events VALUES (41, '\x0000000000000000000000000000000000000000000000000000000000000000')`,
			`INSERT INTO public.events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES (42, 0, 'k', '{"v":1}',
			 '\x0000000000000000000000000000000000000000000000000000000000000000', sha256('x'))`,
		)
		if !errors.Is(err, ErrChainBroken) {
			t.Fatalf("forged insert: %v", err)
		}
	})
}

func openAppUnchecked(t *testing.T, d *pgtest.DB) *Store {
	t.Helper()
	s, err := OpenUnchecked(ctx, d.App, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestPrivilegeRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d *pgtest.DB) string{
		"temporary": func(d *pgtest.DB) string {
			return fmt.Sprintf(`GRANT TEMPORARY ON DATABASE %s TO %s`, d.Name, d.AppRole)
		},
		"insert on migrations":     func(d *pgtest.DB) string { return fmt.Sprintf(`GRANT INSERT ON schema_migrations TO %s`, d.AppRole) },
		"insert on leases":         func(d *pgtest.DB) string { return fmt.Sprintf(`GRANT INSERT ON leases TO %s`, d.AppRole) },
		"delete on intents":        func(d *pgtest.DB) string { return fmt.Sprintf(`GRANT DELETE ON intents TO %s`, d.AppRole) },
		"member of the owner role": func(d *pgtest.DB) string { return fmt.Sprintf(`GRANT %s TO %s`, ownerOf(d), d.AppRole) },
	}
	for name, grant := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := migrated(t)
			d.Exec(t, grant(d))
			if _, err := Open(ctx, d.App, Options{}); !errors.Is(err, ErrPrivileges) {
				t.Fatalf("open: %v", err)
			}
			s := openAppUnchecked(t, d)
			if err := s.CheckPrivileges(ctx); !errors.Is(err, ErrPrivileges) {
				t.Fatalf("CheckPrivileges: %v", err)
			}
		})
	}
}

func ownerOf(d *pgtest.DB) string {
	return "o_" + strings.TrimPrefix(d.AppRole, "a_")
}

func TestVerifyDetectsFunctionAttributeChanges(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"search_path reset":     `ALTER FUNCTION trader_fence() RESET search_path`,
		"security definer":      `ALTER FUNCTION trader_forbid() SECURITY DEFINER`,
		"security invoker":      `ALTER FUNCTION trader_acquire_lease(text, text) SECURITY INVOKER`,
		"extra function":        `CREATE FUNCTION public.helper() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`,
		"overloaded function":   `CREATE FUNCTION public.trader_lease_key(p int) RETURNS bigint LANGUAGE sql SET search_path = pg_catalog, public, pg_temp AS $$ SELECT 1::bigint $$`,
		"dropped function body": `CREATE OR REPLACE FUNCTION trader_lease_key(p_name text) RETURNS bigint LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, public, pg_temp AS $$ SELECT 1::bigint $$`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := migrated(t)
			s := openAppUnchecked(t, d)
			d.Exec(t, sql)
			if err := s.Verify(ctx); !errors.Is(err, ErrSchema) {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}

func TestParseFunctions(t *testing.T) {
	t.Parallel()
	out := map[string]string{}
	src := `CREATE FUNCTION a() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE OR REPLACE FUNCTION public.b(x int) RETURNS int LANGUAGE sql AS $body$ SELECT $$x$$ $body$;
create function c() returns int language sql set search_path = pg_catalog as $f$ SELECT 3 $f$;
CREATE FUNCTION a() RETURNS int LANGUAGE sql AS $$ SELECT 4 $$;`
	if err := parseFunctions(src, out); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": " SELECT 4 ", "b": " SELECT $$x$$ ", "c": " SELECT 3 "}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s: %q, want %q", k, out[k], v)
		}
	}
	if err := parseFunctions(`CREATE FUNCTION x() RETURNS int LANGUAGE sql AS 'SELECT 1';`, map[string]string{}); err == nil {
		t.Error("non-dollar body accepted")
	}
	if err := parseFunctions(`CREATE FUNCTION x() RETURNS int LANGUAGE sql AS $q$ SELECT 1`, map[string]string{}); err == nil {
		t.Error("unterminated body accepted")
	}
}

func TestPayloadContractInDatabase(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	ts := time.Now().UnixNano()
	for name, payload := range map[string]string{
		"not json":     `not json`,
		"array":        `[1]`,
		"no v":         `{"n":1}`,
		"v string":     `{"v":"1"}`,
		"v zero":       `{"v":0}`,
		"v fractional": `{"v":1.5}`,
		"v too large":  `{"v":1000000000000000000}`,
	} {
		p := []byte(payload)
		h := chainHash([hashLen]byte{}, 1, ts, "k", p)
		err := execErr(t, d.App, `INSERT INTO events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES (1, $1, 'k', $2, $3, $4)`,
			ts, p, make([]byte, 32), h[:])
		if !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLeaseSessionSettingsAndConcurrentRelease(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	l, err := s.AcquireLease(ctx, "executor", "a", LeaseOptions{PingInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// The monitor's first ping is an hour away, so the connection is free.
	for name, want := range map[string]string{"tcp_keepalives_idle": "10", "tcp_keepalives_interval": "5", "tcp_keepalives_count": "3", "tcp_user_timeout": "30000"} {
		var got string
		if err := l.conn.QueryRow(ctx, `SELECT setting FROM pg_settings WHERE name = $1`, name).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Release(context.Background()); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestExportTrailer(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	for i := range 3 {
		if _, err := s.Append(ctx, "tick", ev{V: 1, N: i}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if _, _, err := s.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], `{"end":true,"count":3,`) {
		t.Fatalf("trailer %q", lines[len(lines)-1])
	}
	join := func(ls []string) string { return strings.Join(ls, "\n") + "\n" }
	bad := map[string]string{
		"no trailer":         join(lines[:len(lines)-1]),
		"cut before trailer": join(append(append([]string(nil), lines[:2]...), lines[len(lines)-1])),
		"data after trailer": join(append(append([]string(nil), lines...), lines[1])),
		"wrong count":        join(append(append([]string(nil), lines[:len(lines)-1]...), strings.Replace(lines[len(lines)-1], `"count":3`, `"count":2`, 1))),
	}
	for name, in := range bad {
		if _, _, err := VerifyExport(strings.NewReader(in)); !errors.Is(err, ErrChainBroken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An empty log exports a header and a trailer.
	e := migrated(t)
	var empty bytes.Buffer
	if n, _, err := openApp(t, e, "").Export(ctx, &empty); err != nil || n != 0 {
		t.Fatalf("empty export %d %v", n, err)
	}
	if n, _, err := VerifyExport(&empty); err != nil || n != 0 {
		t.Fatalf("verify empty export %d %v", n, err)
	}
}

func TestInitAnchorOnEmptyLog(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	anchor := t.TempDir() + "/a"
	u, err := OpenUnchecked(ctx, d.App, Options{AnchorPath: anchor})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if n, err := u.InitAnchor(ctx); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if seq, _, err := readAnchor(anchor); err != nil || seq != 0 {
		t.Fatalf("anchor %d %v", seq, err)
	}
	s := openApp(t, d, anchor)
	if _, err := s.Append(ctx, "tick", ev{V: 1}); err != nil {
		t.Fatal(err)
	}
	if seq, _, _ := readAnchor(anchor); seq != 1 {
		t.Fatalf("anchor not advanced: %d", seq)
	}
}
