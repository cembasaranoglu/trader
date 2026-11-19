package store

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var ctx = context.Background()

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trader.db")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func appendN(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Append(ctx, "test_event", map[string]any{"v": 1, "i": i}); err != nil {
			t.Fatal(err)
		}
	}
}

// raw opens the database without the store's guards, as an attacker or a
// buggy tool would.
func raw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenCreatesSchemaAndAnchor(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	if seq, h := s.Tip(); seq != 0 || h != ([32]byte{}) {
		t.Fatalf("empty store tip %d %x", seq, h)
	}
	if b, err := os.ReadFile(path + ".anchor"); err != nil || !strings.HasPrefix(string(b), "v1 0 ") {
		t.Fatalf("anchor %q %v", b, err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q %v", mode, err)
	}
	for _, p := range []string{"foreign_keys", "recursive_triggers"} {
		var v int
		if err := s.db.QueryRow("PRAGMA " + p).Scan(&v); err != nil || v != 1 {
			t.Fatalf("%s = %d %v", p, v, err)
		}
	}
	var sync int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 { // 2 = FULL
		t.Fatalf("synchronous %d %v", sync, err)
	}
	appendN(t, s, 3)
	s.Close()
	s2, err := Open(ctx, path, Options{}) // migrations are idempotent, chain verifies
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if seq, _ := s2.Tip(); seq != 3 {
		t.Fatalf("tip after reopen %d", seq)
	}
}

func TestSchemaNewerThanBinaryRefused(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	s.Close()
	if _, err := raw(t, path).Exec(`INSERT INTO schema_migrations VALUES (99, '0099_future.sql', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("want refusal, got %v", err)
	}
}

func TestChainAndEvents(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e1, _ := s.Append(ctx, "a", map[string]int{"v": 1, "x": 1})
	e2, _ := s.Append(ctx, "b", map[string]int{"v": 1, "x": 2})
	if e1.Seq != 1 || e2.Seq != 2 || e2.PrevHash != e1.Hash || e1.PrevHash != ([32]byte{}) {
		t.Fatalf("chain links wrong: %+v %+v", e1, e2)
	}
	if e1.Hash != chainHash([32]byte{}, 1, clock.UnixNano(), "a", []byte(`{"v":1,"x":1}`)) {
		t.Fatal("hash does not match the documented formula")
	}
	evs, err := s.Events(ctx, 1, 0)
	if err != nil || len(evs) != 2 || string(evs[1].Payload) != `{"v":1,"x":2}` || !evs[0].Time.Equal(clock) {
		t.Fatalf("%v %+v", err, evs)
	}
	if err := s.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestChainHashVector pins the encoding: changing it would make existing
// databases unverifiable. Verified independently with Python hashlib.
func TestChainHashVector(t *testing.T) {
	t.Parallel()
	got := chainHash([32]byte{}, 1, 1795132800000000000, "order_submitted", []byte(`{"v":1,"x":"a"}`))
	if fmt.Sprintf("%x", got) != "9153f1baafde391830fd04ddea1e79145fb522f18fb587bc54a9061cc7cdf5c5" {
		t.Fatalf("hash encoding changed: %x", got)
	}
}

func TestChainHashFieldBoundaries(t *testing.T) {
	t.Parallel()
	// Length prefixes keep ("ab", "c") and ("a", "bc") apart.
	if chainHash([32]byte{}, 1, 1, "ab", []byte("c")) == chainHash([32]byte{}, 1, 1, "a", []byte("bc")) {
		t.Fatal("kind/payload boundary collision")
	}
	if chainHash([32]byte{}, 1, 2, "a", nil) == chainHash([32]byte{}, 2, 1, "a", nil) {
		t.Fatal("seq/ts swap collision")
	}
}

// Replacing record rows must fail even where recursive_triggers is off
// (sqlite3 CLI default).
func TestRecordTablesRejectReplace(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	seedRecords(t, s)
	s.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=recursive_triggers(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`INSERT OR REPLACE INTO fills VALUES ('BTCUSDT', 7, 'c', 'BUY', '90000.01', '999', '0', 'BTC', 0, 1, 1)`,
		`REPLACE INTO approvals VALUES (1, 'restart', 'kill', 'mallory', x'01', 0)`,
		`INSERT OR REPLACE INTO tax_ledger (symbol, trade_id, trade_time_ms, side, qty, price, commission, commission_asset) VALUES ('BTCUSDT', 7, 1, 'BUY', '999', '1', '0', 'BTC')`,
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: want immutable error, got %v", q, err)
		}
	}
	if err := VerifyFile(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func seedRecords(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.Update(ctx, func(tx *Tx) error {
		ev, err := tx.Append(ctx, "fill", map[string]int{"v": 1})
		if err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO intents (intent_id, created_seq, policy_version, symbol, side, qty, state, updated_ns) VALUES ('i', ?1, 'v0', 'BTCUSDT', 'BUY', '1', 'X', 0)`,
			`INSERT INTO orders (client_order_id, intent_id, symbol, side, type, qty, state, created_seq, updated_ns) VALUES ('c', 'i', 'BTCUSDT', 'BUY', 'LIMIT', '1', 'FILLED', ?1, 0)`,
			`INSERT INTO fills VALUES ('BTCUSDT', 7, 'c', 'BUY', '90000.01', '0.001', '0.000001', 'BTC', 0, 1, ?1)`,
			`INSERT INTO approvals VALUES (?1, 'restart', 'kill', 'cem', x'01', 0)`,
			`INSERT INTO tax_ledger (symbol, trade_id, trade_time_ms, side, qty, price, commission, commission_asset) VALUES ('BTCUSDT', 7, 1, 'BUY', '0.001', '90000.01', '0.000001', 'BTC')`,
		} {
			if _, err := tx.Exec(ctx, q, ev.Seq); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A guard trigger recreated under the same name but neutered, or moved to
// another table, is detected.
func TestNeuteredTriggerDetected(t *testing.T) {
	t.Parallel()
	for name, qs := range map[string][]string{
		"same name, WHEN 0": {`DROP TRIGGER events_no_delete`,
			`CREATE TRIGGER events_no_delete BEFORE DELETE ON events WHEN 0 BEGIN SELECT RAISE(ABORT, 'events: append-only'); END`},
		"other table": {`DROP TRIGGER events_no_update`,
			`CREATE TRIGGER events_no_update BEFORE UPDATE ON intents BEGIN SELECT RAISE(ABORT, 'events: append-only'); END`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, path := openTemp(t)
			appendN(t, s, 2)
			s.Close()
			db := raw(t, path)
			for _, q := range qs {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			if err := VerifyFile(ctx, path); !errors.Is(err, ErrSchema) {
				t.Fatalf("VerifyFile: want ErrSchema, got %v", err)
			}
			if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrSchema) {
				t.Fatalf("Open: want ErrSchema, got %v", err)
			}
		})
	}
}

func TestVerifyFileChecksMigrations(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]string{
		"changed checksum": `UPDATE schema_migrations SET sha256 = 'x' WHERE version = 1`,
		"newer version":    `INSERT INTO schema_migrations VALUES (2, '0002_future.sql', 'x', 0)`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, path := openTemp(t)
			appendN(t, s, 1)
			s.Close()
			if _, err := raw(t, path).Exec(q); err != nil {
				t.Fatal(err)
			}
			if err := VerifyFile(ctx, path); !errors.Is(err, ErrSchema) {
				t.Fatalf("want ErrSchema, got %v", err)
			}
		})
	}
}

// After a crash left the anchor behind, Open brings it up to the tip, so a
// later truncation of those events is detected.
func TestOpenRefreshesLaggingAnchor(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 2)
	old, _ := os.ReadFile(path + ".anchor")
	appendN(t, s, 3)
	s.Close()
	os.WriteFile(path+".anchor", old, 0o600)
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if b, _ := os.ReadFile(path + ".anchor"); !strings.HasPrefix(string(b), "v1 5 ") {
		t.Fatalf("anchor not refreshed: %q", b)
	}
	tamper(t, path, `DELETE FROM events WHERE seq >= 3`)
	if err := VerifyFile(ctx, path); !errors.Is(err, ErrAnchorAhead) {
		t.Fatalf("truncation after refresh: want ErrAnchorAhead, got %v", err)
	}
}

func TestPanicInUpdateDoesNotWedgeStore(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	func() {
		defer func() { recover() }()
		s.Update(ctx, func(tx *Tx) error {
			tx.Append(ctx, "a", map[string]int{"v": 1})
			panic("boom")
		})
	}()
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := s.Append(cctx, "a", map[string]int{"v": 1}); err != nil {
		t.Fatalf("store wedged after a panic: %v", err)
	}
	if seq, _ := s.Tip(); seq != 1 {
		t.Fatalf("tip %d", seq)
	}
}

func TestSecondWriterRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are Unix-only")
	}
	_, path := openTemp(t)
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
}

func TestStaleAnchorTempsRemoved(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	s.Close()
	stray := filepath.Join(filepath.Dir(path), ".anchor-12345")
	os.WriteFile(stray, []byte("x"), 0o600)
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stray anchor temp file not removed")
	}
}

func TestBackupFileWhileWriting(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 5)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Append(ctx, "w", map[string]int{"v": 1, "i": i}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	dst := filepath.Join(filepath.Dir(path), "bk", "hot.db")
	if _, err := BackupFile(ctx, path, dst); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	// Check the checksum first: opening a database for writing changes it.
	if err := VerifyChecksum(dst); err != nil {
		t.Fatal(err)
	}
	// The backup's anchor is its own tip: no unanchored tail.
	b, _ := os.ReadFile(dst + ".anchor")
	bs, err := Open(ctx, dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := bs.Tip()
	bs.Close()
	if !strings.HasPrefix(string(b), fmt.Sprintf("v1 %d ", seq)) {
		t.Fatalf("backup anchor %q, backup tip %d", b, seq)
	}
}

func TestBackupFailureLeavesNoFiles(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 3)
	s.Close()
	// A source anchor that does not match the data makes the backup fail.
	os.WriteFile(path+".anchor", []byte("v1 2 "+strings.Repeat("ab", 32)+"\n"), 0o600)
	dst := filepath.Join(filepath.Dir(path), "bk", "bad.db")
	if _, err := BackupFile(ctx, path, dst); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("want ErrChainBroken, got %v", err)
	}
	for _, p := range []string{dst, dst + ".anchor", dst + ".sha256"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind", filepath.Base(p))
		}
	}
}

func TestVerifyChecksumDetectsCorruption(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 2)
	dst := filepath.Join(filepath.Dir(path), "bk", "c.db")
	if _, err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("x"))
	f.Close()
	if err := VerifyChecksum(dst); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
}

func TestMissingTriggerDetected(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 1)
	s.Close()
	db := raw(t, path)
	if _, err := db.Exec(`DROP TRIGGER events_no_delete`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := VerifyFile(ctx, path); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}
}

func TestMigrationChangeDetected(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	s.Close()
	if _, err := raw(t, path).Exec(`UPDATE schema_migrations SET sha256 = 'x' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	s, path := openTemp(t)
	appendN(t, s, 1)
	for _, p := range []string{path, path + ".anchor", path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has permissions %v", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

func TestImmutableRecordTables(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	seedRecords(t, s)
	s.Close()
	db := raw(t, path)
	for _, q := range []string{
		`UPDATE fills SET qty = '1' WHERE trade_id = 7`, `DELETE FROM fills`,
		`UPDATE approvals SET signer = 'x'`, `DELETE FROM approvals`,
		`UPDATE tax_ledger SET qty = '1'`, `DELETE FROM tax_ledger`,
		`INSERT INTO tax_ledger (symbol, trade_id, trade_time_ms, side, qty, price, commission, commission_asset, usd_try_rate) VALUES ('BTCUSDT', 8, 1, 'BUY', '1', '1', '0', 'BTC', '42.1')`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: succeeded", q)
		}
	}
}

func TestStrictTypes(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 1)
	s.Close()
	db := raw(t, path)
	if _, err := db.Exec(`DROP TRIGGER events_append_contiguous`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events VALUES (2, 'not-an-int', 'k', x'7b7d', zeroblob(32), zeroblob(32))`); err == nil {
		t.Fatal("STRICT table accepted a text timestamp")
	}
}

func TestTriggersBlockMutation(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 3)
	db := raw(t, path)
	db.Exec(`PRAGMA recursive_triggers = ON`)
	// x'7b7d' is "{}" as a BLOB, so only the trigger or CHECK can reject these.
	for name, tc := range map[string]struct{ q, msg string }{
		"update":            {`UPDATE events SET kind = 'x' WHERE seq = 2`, "events: append-only"},
		"delete":            {`DELETE FROM events WHERE seq = 3`, "events: append-only"},
		"insert gap":        {`INSERT INTO events VALUES (10, 1, 'k', x'7b7d', zeroblob(32), zeroblob(32))`, "seq must be max(seq)+1"},
		"insert past":       {`INSERT INTO events VALUES (2, 1, 'k', x'7b7d', zeroblob(32), zeroblob(32))`, "seq must be max(seq)+1"},
		"insert or replace": {`INSERT OR REPLACE INTO events VALUES (2, 1, 'k', x'7b7d', zeroblob(32), zeroblob(32))`, "seq must be max(seq)+1"},
		"replace into":      {`REPLACE INTO events VALUES (3, 1, 'k', x'7b7d', zeroblob(32), zeroblob(32))`, "seq must be max(seq)+1"},
		"short hash":        {`INSERT INTO events VALUES (4, 1, 'k', x'7b7d', zeroblob(32), zeroblob(31))`, "CHECK constraint failed"},
	} {
		_, err := db.Exec(tc.q)
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.msg, err)
		}
	}
	if err := s.Verify(ctx); err != nil {
		t.Fatalf("chain changed: %v", err)
	}
}

// tamper drops the event guard triggers on a closed store's file, runs qs
// and recreates the triggers, as a careful attacker would.
func tamper(t *testing.T, path string, qs ...string) {
	t.Helper()
	db := raw(t, path)
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'events'`)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]string{}
	for rows.Next() {
		var n, q string
		rows.Scan(&n, &q)
		saved[n] = q
	}
	rows.Close()
	for n := range saved {
		qs = append([]string{"DROP TRIGGER " + n}, qs...)
	}
	for _, q := range saved {
		qs = append(qs, q)
	}
	for _, q := range qs {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
}

func TestTamperDetection(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		qs   []string
		want error
	}{
		"payload":        {[]string{`UPDATE events SET payload = CAST('{"i":9}' AS BLOB) WHERE seq = 2`}, ErrChainBroken},
		"kind":           {[]string{`UPDATE events SET kind = 'other' WHERE seq = 2`}, ErrChainBroken},
		"timestamp":      {[]string{`UPDATE events SET ts_ns = ts_ns + 1 WHERE seq = 2`}, ErrChainBroken},
		"middle deleted": {[]string{`DELETE FROM events WHERE seq = 2`}, ErrChainBroken},
		"tail truncated": {[]string{`DELETE FROM events WHERE seq >= 4`}, ErrAnchorAhead},
		"all deleted":    {[]string{`DELETE FROM events`}, ErrAnchorAhead},
		"hash rewritten": {[]string{`UPDATE events SET hash = zeroblob(32) WHERE seq = 5`}, ErrChainBroken},
		"prev rewritten": {[]string{`UPDATE events SET prev_hash = zeroblob(32) WHERE seq = 3`}, ErrChainBroken},
		"seq renumbered": {[]string{`UPDATE events SET seq = 7 WHERE seq = 5`}, ErrChainBroken},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, path := openTemp(t)
			appendN(t, s, 5)
			s.Close()
			tamper(t, path, tc.qs...)
			if err := VerifyFile(ctx, path); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyFile: want %v, got %v", tc.want, err)
			}
			if _, err := Open(ctx, path, Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("Open: want %v, got %v", tc.want, err)
			}
		})
	}
}

// A consistently recomputed chain (attacker rewrote history and hashes) is
// caught by the anchor.
func TestRecomputedChainCaughtByAnchor(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 3)
	s.Close()
	db := raw(t, path)
	rows, _ := db.Query(`SELECT seq, ts_ns, kind FROM events ORDER BY seq`)
	type row struct {
		seq, ts int64
		kind    string
	}
	var rs []row
	for rows.Next() {
		var r row
		rows.Scan(&r.seq, &r.ts, &r.kind)
		rs = append(rs, r)
	}
	rows.Close()
	db.Close()
	var prev [32]byte
	qs := []string{}
	for _, r := range rs {
		payload := fmt.Sprintf(`{"forged":%d}`, r.seq)
		h := chainHash(prev, r.seq, r.ts, r.kind, []byte(payload))
		qs = append(qs, fmt.Sprintf(`UPDATE events SET payload = CAST('%s' AS BLOB), prev_hash = x'%x', hash = x'%x' WHERE seq = %d`, payload, prev, h, r.seq))
		prev = h
	}
	tamper(t, path, qs...)
	if err := VerifyFile(ctx, path); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("recomputed chain must fail the anchor check, got %v", err)
	}
}

func TestAnchorFileProblems(t *testing.T) {
	t.Parallel()
	for name, mut := range map[string]func(path string){
		"missing":   func(p string) { os.Remove(p + ".anchor") },
		"malformed": func(p string) { os.WriteFile(p+".anchor", []byte("garbage"), 0o600) },
		"bad hash":  func(p string) { os.WriteFile(p+".anchor", []byte("v1 2 zz\n"), 0o600) },
		"ahead":     func(p string) { os.WriteFile(p+".anchor", []byte("v1 9 "+strings.Repeat("00", 32)+"\n"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, path := openTemp(t)
			appendN(t, s, 2)
			s.Close()
			mut(path)
			if _, err := Open(ctx, path, Options{}); err == nil {
				t.Fatal("open must fail")
			}
		})
	}
}

// The anchor may lag the database by events committed just before a crash.
func TestAnchorLagIsAccepted(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 2)
	old, _ := os.ReadFile(path + ".anchor")
	appendN(t, s, 2)
	s.Close()
	os.WriteFile(path+".anchor", old, 0o600)
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("lagging anchor must be accepted: %v", err)
	}
	s2.Close()
}

func TestUpdateIsAtomic(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	boom := errors.New("boom")
	_, err := s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Append(ctx, "a", map[string]int{"v": 1}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A foreign-key violation in a domain table rolls back the event too.
	_, err = s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Append(ctx, "a", map[string]int{"v": 1}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO orders (client_order_id, intent_id, symbol, side, type, qty, state, created_seq, updated_ns)
			VALUES ('x', 'no-such-intent', 'BTCUSDT', 'BUY', 'LIMIT', '1', 'NEW', 1, 0)`)
		return err
	})
	if err == nil {
		t.Fatal("foreign key violation must fail")
	}
	if seq, _ := s.Tip(); seq != 0 {
		t.Fatalf("rolled-back events were kept: tip %d", seq)
	}
	if evs, _ := s.Events(ctx, 1, 0); len(evs) != 0 {
		t.Fatal("rolled-back event visible")
	}
	appendN(t, s, 1) // seq continues at 1
	if seq, _ := s.Tip(); seq != 1 {
		t.Fatalf("tip %d", seq)
	}
}

func TestIntentUniqueness(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	insert := func() error {
		_, err := s.Update(ctx, func(tx *Tx) error {
			ev, err := tx.Append(ctx, "intent_persisted", map[string]any{"v": 1, "intent_id": "abc"})
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO intents (intent_id, created_seq, policy_version, symbol, side, qty, state, updated_ns)
				VALUES ('abc', ?, 'v0000', 'BTCUSDT', 'BUY', '0.001', 'PERSISTED', 0)`, ev.Seq)
			return err
		})
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err == nil {
		t.Fatal("duplicate intent id must be rejected (E-1)")
	}
	if seq, _ := s.Tip(); seq != 1 {
		t.Fatalf("the failed duplicate must not leave an event: tip %d", seq)
	}
}

func TestInvalidEvents(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	for name, tc := range map[string]struct {
		kind    string
		payload any
	}{
		"empty kind":     {"", map[string]int{"v": 1}},
		"upper kind":     {"Order", map[string]int{"v": 1}},
		"long kind":      {strings.Repeat("a", 65), map[string]int{"v": 1}},
		"no version":     {"a", map[string]int{"x": 1}},
		"zero version":   {"a", map[string]int{"v": 0}},
		"float version":  {"a", map[string]float64{"v": 1.5}},
		"string version": {"a", map[string]string{"v": "1"}},
		"array payload":  {"a", []int{1}},
		"string payload": {"a", "x"},
		"null payload":   {"a", nil},
		"unmarshalable":  {"a", map[string]any{"f": func() {}}},
		"too large":      {"a", map[string]any{"v": 1, "x": strings.Repeat("x", MaxPayloadBytes)}},
	} {
		if _, err := s.Append(ctx, tc.kind, tc.payload); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: want ErrInvalidEvent, got %v", name, err)
		}
	}
	if seq, _ := s.Tip(); seq != 0 {
		t.Fatal("invalid events were written")
	}
}

func TestConcurrentAppends(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := s.Append(ctx, "c", map[string]int{"v": 1, "i": i}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if seq, _ := s.Tip(); seq != 200 {
		t.Fatalf("tip %d", seq)
	}
	if err := s.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBackup(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	appendN(t, s, 4)
	dst := filepath.Join(filepath.Dir(path), "backups", "b1.db")
	sum, err := s.Backup(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst + ".sha256"); !strings.HasPrefix(string(b), sum+"  b1.db") {
		t.Fatalf("checksum file %q", b)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions %v", fi.Mode().Perm())
	}
	if err := VerifyFile(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, dst); err == nil {
		t.Fatal("existing target must be refused")
	}
	// Restoring = opening the copy with its anchor.
	restored, err := Open(ctx, dst, Options{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if seq, _ := restored.Tip(); seq != 4 {
		t.Fatalf("restored tip %d", seq)
	}
	restored.Close()
	// An old backup restored without its own anchor, next to a newer anchor, is detected.
	old := filepath.Join(filepath.Dir(path), "old.db")
	if _, err := s.Backup(ctx, old); err != nil {
		t.Fatal(err)
	}
	appendN(t, s, 2)
	cur, _ := os.ReadFile(path + ".anchor")
	os.WriteFile(old+".anchor", cur, 0o600)
	if err := VerifyFile(ctx, old); !errors.Is(err, ErrAnchorAhead) {
		t.Fatalf("older copy with newer anchor: want ErrAnchorAhead, got %v", err)
	}
}

func TestClosedStore(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	s.Close()
	if _, err := s.Append(ctx, "a", map[string]int{"v": 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Verify(ctx); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("double close must be a no-op")
	}
}

// TestCrashConsistency kills a child process (SIGKILL) while it appends and
// checks that the database reopens with a valid chain, keeps every event the
// child reported as committed, and passes SQLite's integrity check.
func TestCrashConsistency(t *testing.T) {
	if os.Getenv("STORE_CRASH_CHILD") != "" {
		s, err := Open(ctx, os.Getenv("STORE_CRASH_CHILD"), Options{})
		if err != nil {
			os.Exit(3)
		}
		for i := 0; ; i++ {
			ev, err := s.Append(ctx, "crash", map[string]int{"v": 1, "i": i})
			if err != nil {
				os.Exit(4)
			}
			fmt.Println(ev.Seq) // committed
		}
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "crash.db")
	for round := 0; round < 3; round++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashConsistency$")
		cmd.Env = append(os.Environ(), "STORE_CRASH_CHILD="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var last int64
		var mu sync.Mutex
		done := make(chan struct{})
		first := make(chan struct{})
		go func() {
			defer close(done)
			var once sync.Once
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if n, err := strconv.ParseInt(sc.Text(), 10, 64); err == nil {
					mu.Lock()
					last = n
					mu.Unlock()
					once.Do(func() { close(first) })
				}
			}
		}()
		// Kill only after the child has committed at least once, then let it
		// keep appending so the kill lands in the middle of a write.
		select {
		case <-first:
		case <-done:
		case <-time.After(30 * time.Second):
		}
		time.Sleep(time.Duration(20+round*40) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		<-done
		err = cmd.Wait()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ProcessState.ExitCode() != -1 {
			t.Fatalf("round %d: child must be killed by the signal, got %v", round, err)
		}
		s, err := Open(ctx, path, Options{})
		if err != nil {
			t.Fatalf("round %d: reopen after SIGKILL: %v", round, err)
		}
		seq, _ := s.Tip()
		var integrity string
		if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("round %d: integrity_check %q %v", round, integrity, err)
		}
		s.Close()
		mu.Lock()
		reported := last
		mu.Unlock()
		if seq < reported {
			t.Fatalf("round %d: committed event %d lost (tip %d)", round, reported, seq)
		}
		if reported == 0 {
			t.Fatalf("round %d: child never committed; the crash was not exercised", round)
		}
		t.Logf("round %d: child reported %d, tip %d", round, reported, seq)
	}
}
