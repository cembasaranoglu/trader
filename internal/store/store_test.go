package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cembasaranoglu/trader/internal/pgtest"
)

var ctx = context.Background()

type ev struct {
	V int    `json:"v"`
	N int    `json:"n"`
	S string `json:"s,omitempty"`
}

func openApp(t *testing.T, d *pgtest.DB, anchor string) *Store {
	t.Helper()
	s, err := Open(ctx, d.App, Options{AnchorPath: anchor})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestParseDSN(t *testing.T) {
	t.Parallel()
	ca := pgtest.CAFile(t)
	ok := []string{
		"postgres://u:secret@localhost:5432/db?sslmode=disable",
		"postgres://u:secret@127.0.0.1/db?sslmode=disable",
		"postgres://u:secret@postgres:5432/db?sslmode=disable", // compose service
		"postgres://u:secret@localhost:5432,127.0.0.1:5433/db?sslmode=disable",
		"host=/var/run/postgresql user=u dbname=db",
		"postgres://u:secret@db.example.com/db?sslmode=verify-full&sslrootcert=" + ca,
		"postgres://u:secret@db1.example.com,db2.example.com/db?sslmode=verify-full&sslrootcert=" + ca,
	}
	for _, d := range ok {
		if _, err := ParseDSN(d, 0, 0); err != nil {
			t.Errorf("%s: %v", RedactDSN(d), err)
		}
	}
	bad := map[string]error{
		"postgres://u:secret@db.example.com/db?sslmode=disable":                             ErrInsecureDSN,
		"postgres://u:secret@db.example.com/db?sslmode=require":                             ErrInsecureDSN,
		"postgres://u:secret@db.example.com/db?sslmode=verify-ca&sslrootcert=" + ca:         ErrInsecureDSN,
		"postgres://u:secret@db.example.com/db":                                             ErrInsecureDSN, // default prefer
		"postgres://u:secret@db.example.com/db?sslmode=verify-full":                         ErrInsecureDSN, // no explicit CA
		"postgres://u:secret@10.0.0.5/db?sslmode=disable":                                   ErrInsecureDSN,
		"postgres://u:secret@localhost:1,db.example.com:5432/db?sslmode=disable":            ErrInsecureDSN, // remote fallback host
		"postgres://u:secret@localhost:1,db.example.com:5432/db?sslmode=prefer":             ErrInsecureDSN,
		"postgres://u:secret@db.example.com,localhost/db?sslmode=require&sslrootcert=" + ca: ErrInsecureDSN,
	}
	for d, want := range bad {
		_, err := ParseDSN(d, 0, 0)
		if !errors.Is(err, want) {
			t.Errorf("%s: err %v, want %v", RedactDSN(d), err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
	for _, d := range []string{"", "postgres://%zz", "postgres://u@localhost/"} {
		_, err := ParseDSN(d, 0, 0)
		if err == nil {
			t.Errorf("%q: expected error", d)
		}
	}
	if _, err := ParseDSN("postgres://u:hunter2@[bad", 0, 0); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("malformed DSN error must not echo it: %v", err)
	}
	cfg, err := ParseDSN("postgres://u:p@localhost/db?sslmode=disable", 3*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "3000" || rp["timezone"] != "UTC" || rp["search_path"] != "public, pg_temp" {
		t.Fatalf("runtime params %v", rp)
	}
	local, _ := ParseDSN("postgres://u:p@localhost:1,127.0.0.1:2/db?sslmode=disable", 0, 0)
	remote, _ := ParseDSN("postgres://u:p@db.example.com/db?sslmode=verify-full&sslrootcert="+ca, 0, 0)
	if !AllHostsLocal(local) || AllHostsLocal(remote) {
		t.Fatal("AllHostsLocal")
	}
}

func TestIsLocalHost(t *testing.T) {
	t.Parallel()
	for h, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true, "/tmp": true, "postgres": true,
		"db.example.com": false, "10.0.0.5": false, "2001:db8::1": false, "": false,
	} {
		if got := IsLocalHost(h); got != want {
			t.Errorf("IsLocalHost(%q) = %v", h, got)
		}
	}
}

func TestMigrateIdempotentAndChecked(t *testing.T) {
	t.Parallel()
	d := pgtest.New(t)
	got, err := Migrate(ctx, d.Owner, d.AppRole)
	if err != nil || len(got) != 1 || got[0] != 1 {
		t.Fatalf("first migrate: %v %v", got, err)
	}
	got, err = Migrate(ctx, d.Owner, d.AppRole)
	if err != nil || len(got) != 0 {
		t.Fatalf("second migrate: %v %v", got, err)
	}
	if _, err := Migrate(ctx, d.Owner, "no_such_role"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing app role: %v", err)
	}
	if _, err := Migrate(ctx, d.Owner, "Bad-Role"); err == nil {
		t.Fatal("invalid role name accepted")
	}
	d.Exec(t, `UPDATE schema_migrations SET sha256 = '\x00' WHERE version = 1`)
	if _, err := Migrate(ctx, d.Owner, d.AppRole); !errors.Is(err, ErrSchema) {
		t.Fatalf("changed migration: %v", err)
	}
	if _, err := Open(ctx, d.App, Options{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("open with changed migration: %v", err)
	}
}

func TestOpenRefusesWrongRoleAndOldSchema(t *testing.T) {
	t.Parallel()
	d := pgtest.New(t)
	if _, err := Open(ctx, d.App, Options{}); !errors.Is(err, ErrSchemaOld) {
		t.Fatalf("before migrate: %v", err)
	}
	if _, err := Migrate(ctx, d.Owner, d.AppRole); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, d.Owner, Options{}); !errors.Is(err, ErrPrivileges) {
		t.Fatalf("open as owner: %v", err)
	}
	if _, err := Open(ctx, d.Super, Options{}); !errors.Is(err, ErrPrivileges) {
		t.Fatalf("open as superuser: %v", err)
	}
	d.Exec(t, fmt.Sprintf(`GRANT UPDATE ON events TO %s`, d.AppRole))
	if _, err := Open(ctx, d.App, Options{}); !errors.Is(err, ErrPrivileges) {
		t.Fatalf("app role with UPDATE on events: %v", err)
	}
	d.Exec(t, fmt.Sprintf(`REVOKE UPDATE ON events FROM %s`, d.AppRole))
	s := openApp(t, d, "")
	if err := s.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAppendAndRead(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	var prev [hashLen]byte
	for i := 1; i <= 3; i++ {
		e, err := s.Append(ctx, "tick", ev{V: 1, N: i})
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq != int64(i) || e.PrevHash != prev || e.Hash != chainHash(prev, e.Seq, e.Time.UnixNano(), "tick", e.Payload) {
			t.Fatalf("event %d: %+v", i, e)
		}
		prev = e.Hash
	}
	seq, tip, err := s.Tip(ctx)
	if err != nil || seq != 3 || tip != prev {
		t.Fatalf("tip %d %x %v", seq, tip, err)
	}
	evs, err := s.Events(ctx, 2, 0, 0)
	if err != nil || len(evs) != 2 || evs[0].Seq != 2 || string(evs[1].Payload) != `{"v":1,"n":3}` {
		t.Fatalf("events %+v %v", evs, err)
	}
	// Multiple events and domain rows in one transaction, all or nothing.
	_, err = s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Append(ctx, "a", ev{V: 1}); err != nil {
			return err
		}
		return errors.New("abort")
	})
	if err == nil || err.Error() != "abort" {
		t.Fatal(err)
	}
	if seq, _, _ := s.Tip(ctx); seq != 3 {
		t.Fatalf("aborted transaction left seq %d", seq)
	}
}

func TestPayloadContract(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	cases := map[string]struct {
		kind    string
		payload any
	}{
		"bad kind":      {"Bad", ev{V: 1}},
		"empty kind":    {"", ev{V: 1}},
		"not object":    {"k", []int{1}},
		"no v":          {"k", map[string]int{"n": 1}},
		"v string":      {"k", map[string]string{"v": "1"}},
		"v zero":        {"k", map[string]int{"v": 0}},
		"v fractional":  {"k", map[string]float64{"v": 1.5}},
		"too large":     {"k", ev{V: 1, S: strings.Repeat("x", MaxPayloadBytes)}},
		"unmarshalable": {"k", map[string]any{"v": 1, "c": make(chan int)}},
	}
	for name, c := range cases {
		if _, err := s.Append(ctx, c.kind, c.payload); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDatabaseRejectsTampering(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	e1, err := s.Append(ctx, "tick", ev{V: 1, N: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The application role has no UPDATE/DELETE/TRUNCATE privilege.
	for _, q := range []string{`UPDATE events SET kind = 'x'`, `DELETE FROM events`, `TRUNCATE events`, `UPDATE leases SET epoch = 99`} {
		if err := execErr(t, d.App, q); !errors.Is(err, ErrPrivileges) {
			t.Errorf("app %s: %v", q, err)
		}
	}
	// Even a role with privileges (the superuser) is stopped by the triggers.
	for _, q := range []string{`UPDATE events SET kind = 'x'`, `DELETE FROM events`, `TRUNCATE events CASCADE`} {
		if err := execErr(t, d.Super, q); !errors.Is(err, ErrAppendOnly) {
			t.Errorf("super %s: %v", q, err)
		}
	}
	// Inserts that would break the chain, written directly as the app role.
	ts := time.Now().UnixNano()
	good := chainHash(e1.Hash, 2, ts, "k", []byte(`{"v":1}`))
	wrongPrev := chainHash([hashLen]byte{}, 2, ts, "k", []byte(`{"v":1}`))
	gapHash := chainHash(e1.Hash, 3, ts, "k", []byte(`{"v":1}`))
	for name, args := range map[string][]any{
		"gap":        {int64(3), ts, "k", []byte(`{"v":1}`), e1.Hash[:], gapHash[:]},
		"wrong prev": {int64(2), ts, "k", []byte(`{"v":1}`), make([]byte, 32), wrongPrev[:]},
		"wrong hash": {int64(2), ts, "k", []byte(`{"v":2}`), e1.Hash[:], good[:]},
		"duplicate":  {int64(1), ts, "k", []byte(`{"v":1}`), make([]byte, 32), good[:]},
	} {
		err := execErr(t, d.App, `INSERT INTO events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES ($1, $2, $3, $4, $5, $6)`, args...)
		if !errors.Is(err, ErrChainBroken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := execErr(t, d.App, `INSERT INTO events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES ($1, $2, $3, $4, $5, $6)`,
		int64(2), ts, "k", []byte(`{"v":1}`), e1.Hash[:], good[:]); err != nil {
		t.Fatalf("a correct direct insert must pass: %v", err)
	}
	if err := s.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentAppendersNeverFork(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	a := openApp(t, d, "")
	b := openApp(t, d, "") // a second process
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := range workers {
		s := a
		if w%2 == 1 {
			s = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := s.Append(ctx, "tick", ev{V: 1, N: w*1000 + i}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seq, _, err := a.Tip(ctx)
	if err != nil || seq != workers*each {
		t.Fatalf("tip %d, want %d (%v)", seq, workers*each, err)
	}
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsSchemaAndRowTampering(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		sql  string
		want error
	}{
		"disabled trigger": {`ALTER TABLE events DISABLE TRIGGER events_forbid`, ErrSchema},
		"dropped trigger":  {`DROP TRIGGER fills_forbid ON fills`, ErrSchema},
		"function body": {`CREATE OR REPLACE FUNCTION trader_forbid() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
			ErrSchema},
		"row rewritten": {`ALTER TABLE events DISABLE TRIGGER USER; UPDATE events SET payload = '{"v":1,"n":9}' WHERE seq = 1; ALTER TABLE events ENABLE TRIGGER USER`,
			ErrChainBroken},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := migrated(t)
			s := openApp(t, d, "")
			for i := range 3 {
				if _, err := s.Append(ctx, "tick", ev{V: 1, N: i}); err != nil {
					t.Fatal(err)
				}
			}
			d.Exec(t, c.sql)
			if err := s.Verify(ctx); !errors.Is(err, c.want) {
				t.Fatalf("verify: %v, want %v", err, c.want)
			}
			if _, err := Open(ctx, d.App, Options{}); !errors.Is(err, c.want) {
				t.Fatalf("open: %v, want %v", err, c.want)
			}
		})
	}
}

func TestAnchor(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	anchor := filepath.Join(t.TempDir(), "trader.anchor")
	s := openApp(t, d, anchor)
	var last Event
	for i := range 5 {
		e, err := s.Append(ctx, "tick", ev{V: 1, N: i})
		if err != nil {
			t.Fatal(err)
		}
		last = e
	}
	b, err := os.ReadFile(anchor)
	if err != nil || string(b) != fmt.Sprintf("v1 5 %s\n", hex.EncodeToString(last.Hash[:])) {
		t.Fatalf("anchor %q %v", b, err)
	}
	if fi, _ := os.Stat(anchor); fi.Mode().Perm() != 0o600 {
		t.Fatalf("anchor mode %v", fi.Mode())
	}
	s.Close()

	// An older snapshot restored: the last two events are gone.
	d.Exec(t, `ALTER TABLE events DISABLE TRIGGER USER; DELETE FROM events WHERE seq > 3; ALTER TABLE events ENABLE TRIGGER USER`)
	if _, err := Open(ctx, d.App, Options{AnchorPath: anchor}); !errors.Is(err, ErrAnchorAhead) {
		t.Fatalf("restored older snapshot: %v", err)
	}
	// A rewritten history with the same length: the hash at the anchor differs.
	d2 := migrated(t)
	anchor2 := filepath.Join(t.TempDir(), "trader.anchor")
	s2 := openApp(t, d2, anchor2)
	for i := range 3 {
		if _, err := s2.Append(ctx, "tick", ev{V: 1, N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAnchor(anchor2, 2, [hashLen]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Verify(ctx); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("anchor hash mismatch: %v", err)
	}
	// Missing anchor for a non-empty log: refused until InitAnchor.
	os.Remove(anchor2)
	if _, err := Open(ctx, d2.App, Options{AnchorPath: anchor2}); !errors.Is(err, ErrAnchorMissing) {
		t.Fatalf("missing anchor: %v", err)
	}
	u, err := OpenUnchecked(ctx, d2.App, Options{AnchorPath: anchor2})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if n, err := u.InitAnchor(ctx); err != nil || n != 3 {
		t.Fatalf("init anchor: %d %v", n, err)
	}
	if _, err := u.InitAnchor(ctx); !errors.Is(err, ErrAnchorExists) {
		t.Fatalf("second init: %v", err)
	}
	s3 := openApp(t, d2, anchor2)
	if err := s3.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	// Another process appends; Open refreshes this host's anchor.
	if _, err := openApp(t, d2, "").Append(ctx, "tick", ev{V: 1}); err != nil {
		t.Fatal(err)
	}
	openApp(t, d2, anchor2)
	if seq, _, _ := readAnchor(anchor2); seq != 4 {
		t.Fatalf("anchor not refreshed: %d", seq)
	}
}

// insertIntent writes an intent stamped with epoch.
func insertIntent(ctx context.Context, s *Store, id string, epoch int64) error {
	_, err := s.Update(ctx, func(tx *Tx) error {
		e, err := tx.Append(ctx, "intent_created", ev{V: 1})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO intents (intent_id, created_seq, epoch, policy_version, symbol, side, qty, state, updated_ns)
			VALUES ($1, $2, $3, 'p1', 'BTCUSDT', 'BUY', '0.001', 'PLANNED', $4)`, id, e.Seq, epoch, time.Now().UnixNano())
		return err
	})
	return err
}

func TestLeaseAndFencing(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	fast := LeaseOptions{PingInterval: 50 * time.Millisecond, PingTimeout: time.Second}
	l1, err := s.AcquireLease(ctx, "executor", "host-a", fast)
	if err != nil {
		t.Fatal(err)
	}
	if l1.Epoch() != 1 {
		t.Fatalf("first epoch %d", l1.Epoch())
	}
	if _, err := s.AcquireLease(ctx, "executor", "host-b", fast); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second holder: %v", err)
	}
	if err := insertIntent(ctx, s, "i1", 1); err != nil {
		t.Fatalf("write with the current epoch: %v", err)
	}
	if err := insertIntent(ctx, s, "i0", 0); !errors.Is(err, ErrFenced) {
		t.Fatalf("write with an old epoch: %v", err)
	}
	// The holder's session dies (crash, network): the lock is released.
	// Only this test's database: parallel tests hold their own leases.
	d.Exec(t, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'traderd-lease-executor' AND datname = current_database()`)
	select {
	case <-l1.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("lease loss not detected")
	}
	if !errors.Is(l1.Err(), ErrLeaseLost) {
		t.Fatalf("lost error %v", l1.Err())
	}
	l2, err := s.AcquireLease(ctx, "executor", "host-b", fast)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Epoch() != 2 {
		t.Fatalf("second epoch %d", l2.Epoch())
	}
	// The old holder's late writes are rejected.
	if err := insertIntent(ctx, s, "i2", l1.Epoch()); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale insert: %v", err)
	}
	_, err = s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec(ctx, `UPDATE intents SET state = 'SENT', epoch = $1 WHERE intent_id = 'i1'`, l1.Epoch())
		return err
	})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale update: %v", err)
	}
	if err := insertIntent(ctx, s, "i3", l2.Epoch()); err != nil {
		t.Fatal(err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l2.Lost():
	default:
		t.Fatal("Lost not closed after Release")
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release after loss: %v", err)
	}
	l3, err := s.AcquireLease(ctx, "executor", "host-a", fast)
	if err != nil || l3.Epoch() != 3 {
		t.Fatalf("third: %v %v", l3, err)
	}
	l3.Release(ctx)
	if _, err := s.AcquireLease(ctx, "Bad", "x", fast); err == nil {
		t.Fatal("invalid lease name accepted")
	}
	if _, err := s.AcquireLease(ctx, "nosuch", "x", fast); err == nil {
		t.Fatal("unknown lease accepted")
	}
}

// TestFenceSerialisesWithEpochChange: an intent write that started under
// epoch 1 holds the lease row; the next holder's epoch increment waits for
// it, so the write is either committed before epoch 2 exists (and visible
// to the new holder) or rejected.
func TestFenceSerialisesWithEpochChange(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	l1, err := s.AcquireLease(ctx, "executor", "a", LeaseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inTx := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	var wg sync.WaitGroup
	// Whatever happens below, let the open transaction end before the
	// cleanup closes the pool (which waits for its connections).
	defer wg.Wait()
	defer release()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.Update(ctx, func(tx *Tx) error {
			e, err := tx.Append(ctx, "intent_created", ev{V: 1})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO intents (intent_id, created_seq, epoch, policy_version, symbol, side, qty, state, updated_ns)
				VALUES ('slow', $1, $2, 'p1', 'BTCUSDT', 'BUY', '0.001', 'PLANNED', 0)`, e.Seq, l1.Epoch()); err != nil {
				return err
			}
			close(inTx)
			<-finish
			return nil
		})
		if err != nil {
			t.Errorf("slow write: %v", err)
		}
	}()
	select {
	case <-inTx:
	case <-time.After(10 * time.Second):
		t.Fatal("slow write did not start")
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		l   *Lease
		err error
	}
	acquired := make(chan result, 1)
	go func() {
		l, err := s.AcquireLease(ctx, "executor", "b", LeaseOptions{})
		acquired <- result{l, err}
	}()
	select {
	case r := <-acquired:
		if r.l != nil {
			r.l.Release(ctx)
		}
		t.Fatalf("acquire returned while a write under the old epoch was in flight: lease=%v err=%v", r.l, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	wg.Wait()
	r := <-acquired
	if r.err != nil || r.l.Epoch() != 2 {
		t.Fatalf("new holder %v %v", r.l, r.err)
	}
	defer r.l.Release(ctx)
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM intents WHERE intent_id = 'slow'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the earlier write must be visible to the new holder: %d %v", n, err)
	}
}

func TestExportRoundTrip(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	s := openApp(t, d, "")
	for i := range 2500 { // more than two pages
		if _, err := s.Append(ctx, "tick", ev{V: 1, N: i}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	n, tip, err := s.Export(ctx, &buf)
	if err != nil || n != 2500 {
		t.Fatalf("export %d %v", n, err)
	}
	m, tip2, err := VerifyExport(bytes.NewReader(buf.Bytes()))
	if err != nil || m != n || tip2 != tip {
		t.Fatalf("verify export %d %v", m, err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	tamper := func(i int, old, repl string) string {
		cp := append([]string(nil), lines...)
		cp[i] = strings.Replace(cp[i], old, repl, 1)
		return strings.Join(cp, "\n") + "\n"
	}
	bad := map[string]string{
		"header":       tamper(0, "trader-events", "other"),
		"dropped line": strings.Join(append(append([]string(nil), lines[:5]...), lines[6:]...), "\n") + "\n",
		"payload":      tamper(3, `"payload":"`, `"payload":"e`),
		"unknown key":  tamper(2, `{"seq"`, `{"x":1,"seq"`),
	}
	for name, in := range bad {
		if _, _, err := VerifyExport(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := VerifyExport(strings.NewReader("")); err == nil {
		t.Error("empty export accepted")
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	if classify(nil) != nil {
		t.Fatal("nil")
	}
	other := errors.New("x")
	if classify(other) != other {
		t.Fatal("non-pg error changed")
	}
	if !errors.Is(classify(pgx.ErrNoRows), pgx.ErrNoRows) {
		t.Fatal("ErrNoRows")
	}
}

func TestExpectedFunctionsCoverMigration(t *testing.T) {
	t.Parallel()
	f, err := expectedFunctions()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trader_forbid", "trader_events_append", "trader_lease_key", "trader_acquire_lease", "trader_release_lease", "trader_fence"} {
		if _, ok := f[name]; !ok {
			t.Errorf("function %s not extracted from the migration", name)
		}
	}
}
