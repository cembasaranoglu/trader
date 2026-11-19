// Package store is the durable record of the trading system (ADR-002): an
// append-only, hash-chained event log plus domain tables, in one SQLite
// database in WAL mode.
//
// Integrity model:
//   - Every event row is chained: hash = SHA-256(prev_hash ‖ seq ‖ ts_ns ‖
//     kind ‖ payload), with fixed-width integers and length-prefixed strings
//     (see chainHash). The first event's prev_hash is 32 zero bytes.
//   - Triggers reject UPDATE and DELETE on events and any INSERT whose seq is
//     not max(seq)+1; recursive_triggers is on so INSERT OR REPLACE cannot
//     delete rows silently.
//   - Fills, approvals and the tax ledger have UPDATE/DELETE triggers and
//     BEFORE INSERT guards against existing keys (INSERT OR REPLACE).
//   - After each commit the chain tip (seq, hash) is written to an anchor
//     file next to the database (atomic replace + fsync). Verify fails when
//     the database ends before the anchor (truncation or an older copy) or
//     the hash at the anchor's seq differs (rewrite). The anchor may lag by
//     the events of a crash between commit and anchor write; Open refreshes
//     it after verifying the chain.
//   - Verify compares the SQL of every guard trigger and of the events table
//     with the SQL the migrations create, and checks applied migration
//     checksums, so a neutered trigger under the same name is detected.
//
// Threat model: this detects accidents (truncation, restoring the wrong or
// an older copy, edits by tools that bypass the store) and corruption. It
// does not stop a malicious user who can write both the database and the
// anchor file; off-host copies of backups and anchors (W7) are the defence
// against that.
//
// One process writes: Open takes an exclusive lock file (path + ".lock").
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Limits.
const (
	MaxPayloadBytes = 1 << 20
	hashLen         = 32
	busyTimeoutMs   = 5000
)

// Errors.
var (
	ErrChainBroken   = errors.New("store: hash chain broken")
	ErrAnchorMissing = errors.New("store: anchor file missing for a non-empty event log")
	ErrAnchorAhead   = errors.New("store: event log ends before the anchor (truncated or older copy)")
	ErrInvalidEvent  = errors.New("store: invalid event")
	ErrClosed        = errors.New("store: closed")
	ErrSchema        = errors.New("store: schema integrity check failed")
	// ErrAnchorNotUpdated: the events were committed but the anchor file
	// could not be written. The data is durable; the anchor lags.
	ErrAnchorNotUpdated = errors.New("store: events committed but anchor not updated")
)

// guardObjects are the schema objects whose SQL must match the migrations
// exactly: the events table and every guard trigger.
var guardObjects = []string{
	"events",
	"events_append_contiguous", "events_no_update", "events_no_delete",
	"fills_no_update", "fills_no_delete", "fills_no_replace",
	"approvals_no_update", "approvals_no_delete", "approvals_no_replace",
	"tax_ledger_no_update", "tax_ledger_no_delete", "tax_ledger_no_replace",
}

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Event is one row of the event log.
type Event struct {
	Seq      int64
	Time     time.Time
	Kind     string
	Payload  json.RawMessage
	PrevHash [hashLen]byte
	Hash     [hashLen]byte
}

// Store is safe for concurrent use.
type Store struct {
	path   string
	anchor string
	db     *sql.DB // single writer connection
	now    func() time.Time

	mu     sync.Mutex // serializes writes, anchor updates and backups
	tipSeq int64
	tip    [hashLen]byte
	closed bool
	lock   *os.File
}

// Options configures Open.
type Options struct {
	Now func() time.Time // nil: time.Now
}

var ofdOnce sync.Once

// enableOFD turns on OFD locks on Linux before the first connection so an
// unrelated os.File close cannot drop SQLite's locks (modernc doc.go).
func enableOFD() {
	ofdOnce.Do(func() {
		_, _ = sqlite.OFDLocking(true) // unavailable on non-Linux; too late only if a DB was opened earlier
	})
}

// Open opens or creates the database at path, applies migrations, and
// verifies the hash chain against the anchor file (path + ".anchor").
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	enableOFD()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create directory: %w", err)
	}
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	removeStaleTemps(filepath.Dir(path))
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("store: open: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
			lock.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, anchor: path + ".anchor", db: db, now: now, lock: lock}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// SQLite creates -wal and -shm with the main file's permissions; the
	// first ones were created before the chmod, so fix them too.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("store: chmod %s: %w", filepath.Base(p), err)
		}
	}
	if err := s.loadTip(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.verifyAnchor(ctx, s.db, s.anchor); err != nil {
		db.Close()
		return nil, err
	}
	// Create the anchor for a new database, or bring a lagging one (crash
	// between commit and anchor write) up to the verified tip.
	if aSeq, _, err := readAnchor(s.anchor); errors.Is(err, fs.ErrNotExist) || (err == nil && aSeq < s.tipSeq) {
		if err := writeAnchor(s.anchor, s.tipSeq, s.tip); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

// removeStaleTemps deletes anchor temp files left by a crash during
// writeAnchor.
func removeStaleTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".anchor-*"))
	for _, m := range matches {
		os.Remove(m)
	}
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeoutMs)+")")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "recursive_triggers(ON)")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Close closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.db.Close()
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// Tip returns the sequence and hash of the last event (0 and zero hash when empty).
func (s *Store) Tip() (int64, [hashLen]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tipSeq, s.tip
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, sha256 TEXT NOT NULL, applied_ns INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("store: migrations table: %w", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	var applied int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("store: read migration version: %w", err)
	}
	if max := int64(len(names)); applied > max {
		return fmt.Errorf("store: database schema version %d is newer than this binary (%d)", applied, max)
	}
	for i, name := range names {
		version := int64(i + 1)
		if want := fmt.Sprintf("%04d_", version); !strings.HasPrefix(name, want) {
			return fmt.Errorf("store: migration %q out of sequence, want prefix %q", name, want)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		// BEGIN IMMEDIATE (_txlock) serializes concurrent migrators; the
		// version is re-read inside the transaction.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration: %w", err)
		}
		var have sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT sha256 FROM schema_migrations WHERE version = ?`, version).Scan(&have)
		if err == nil {
			tx.Rollback()
			if have.String != digest {
				return fmt.Errorf("%w: migration %s was changed after it was applied", ErrSchema, name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, sha256, applied_ns) VALUES (?, ?, ?, ?)`,
			version, name, digest, s.now().UTC().UnixNano()); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) loadTip(ctx context.Context) error {
	var seq sql.NullInt64
	var hash []byte
	err := s.db.QueryRowContext(ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read tip: %w", err)
	}
	if len(hash) != hashLen {
		return fmt.Errorf("%w: tip hash length %d", ErrChainBroken, len(hash))
	}
	s.tipSeq = seq.Int64
	copy(s.tip[:], hash)
	return nil
}

// chainHash computes an event hash. Integers are fixed-width big-endian and
// strings are length-prefixed, so different field splits never collide.
func chainHash(prev [hashLen]byte, seq, tsNs int64, kind string, payload []byte) [hashLen]byte {
	h := sha256.New()
	h.Write(prev[:])
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seq))
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], uint64(tsNs))
	h.Write(b[:])
	writeLP(h, []byte(kind))
	writeLP(h, payload)
	var out [hashLen]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeLP(h interface{ Write([]byte) (int, error) }, v []byte) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(v)))
	h.Write(b[:])
	h.Write(v)
}

// Tx is a write transaction. Events appended in it and its other statements
// commit atomically.
type Tx struct {
	tx      *sql.Tx
	s       *Store
	tipSeq  int64
	tip     [hashLen]byte
	pending []Event
}

// Append adds an event to the transaction. payload must marshal to a JSON
// object with an integer schema version "v" >= 1. The marshalled bytes are
// stored and hashed exactly as produced.
func (t *Tx) Append(ctx context.Context, kind string, payload any) (Event, error) {
	if !kindPattern.MatchString(kind) {
		return Event{}, fmt.Errorf("%w: kind %q", ErrInvalidEvent, kind)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("%w: payload: %v", ErrInvalidEvent, err)
	}
	if len(b) > MaxPayloadBytes {
		return Event{}, fmt.Errorf("%w: payload larger than %d bytes", ErrInvalidEvent, MaxPayloadBytes)
	}
	if len(b) == 0 || b[0] != '{' {
		return Event{}, fmt.Errorf("%w: payload must be a JSON object", ErrInvalidEvent)
	}
	var head struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(b, &head); err != nil || len(head.V) == 0 {
		return Event{}, fmt.Errorf("%w: payload needs a schema version field \"v\"", ErrInvalidEvent)
	}
	if v, err := strconv.ParseInt(string(head.V), 10, 64); err != nil || v < 1 {
		return Event{}, fmt.Errorf("%w: payload \"v\" must be an integer >= 1", ErrInvalidEvent)
	}
	ev := Event{Seq: t.tipSeq + 1, Time: t.s.now().UTC(), Kind: kind, Payload: b, PrevHash: t.tip}
	ev.Hash = chainHash(ev.PrevHash, ev.Seq, ev.Time.UnixNano(), kind, b)
	if _, err := t.tx.ExecContext(ctx, `INSERT INTO events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?)`,
		ev.Seq, ev.Time.UnixNano(), kind, b, ev.PrevHash[:], ev.Hash[:]); err != nil {
		return Event{}, fmt.Errorf("store: insert event: %w", err)
	}
	t.tipSeq, t.tip = ev.Seq, ev.Hash
	t.pending = append(t.pending, ev)
	return ev, nil
}

// Exec runs a statement in the transaction (domain tables).
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryRow runs a single-row query in the transaction.
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// Update runs fn in a write transaction. If fn returns an error or the commit
// fails nothing is written. After a successful commit the anchor is updated;
// an anchor write error is returned with the committed events.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	committed := false
	// Roll back on any exit without commit, including a panic in fn, so the
	// single connection is never left inside a transaction.
	defer func() {
		if !committed {
			sqlTx.Rollback()
		}
	}()
	tx := &Tx{tx: sqlTx, s: s, tipSeq: s.tipSeq, tip: s.tip}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if err := sqlTx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	s.tipSeq, s.tip = tx.tipSeq, tx.tip
	if len(tx.pending) > 0 {
		if err := writeAnchor(s.anchor, s.tipSeq, s.tip); err != nil {
			return tx.pending, fmt.Errorf("%w: %v", ErrAnchorNotUpdated, err)
		}
	}
	return tx.pending, nil
}

// Append adds one event in its own transaction.
func (s *Store) Append(ctx context.Context, kind string, payload any) (Event, error) {
	var ev Event
	evs, err := s.Update(ctx, func(tx *Tx) error {
		var err error
		ev, err = tx.Append(ctx, kind, payload)
		return err
	})
	if len(evs) == 1 {
		ev = evs[0]
	}
	return ev, err
}

// Verify checks the schema, re-hashes the whole chain and compares it with
// the anchor. It reads through a separate read-only connection and does not
// block writes.
func (s *Store) Verify(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return VerifyFile(ctx, s.path)
}

// VerifyFile verifies a database file and its anchor without modifying them
// (for example a backup copy).
func VerifyFile(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	enableOFD()
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := checkMigrations(ctx, db); err != nil {
		return err
	}
	return (&Store{}).verifyAnchor(ctx, db, path+".anchor")
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// verifyAnchor checks the guard objects' SQL, reads the anchor, walks the
// whole chain and compares them. Memory use is constant.
func (s *Store) verifyAnchor(ctx context.Context, db querier, anchorPath string) error {
	if err := checkGuards(ctx, db); err != nil {
		return err
	}
	aSeq, aHash, aerr := readAnchor(anchorPath)
	missing := errors.Is(aerr, fs.ErrNotExist)
	if aerr != nil && !missing {
		return aerr
	}
	tipSeq, atAnchor, err := walkChain(ctx, db, aSeq)
	if err != nil {
		return err
	}
	if missing {
		if tipSeq == 0 {
			return nil
		}
		return ErrAnchorMissing
	}
	if aSeq > tipSeq {
		return fmt.Errorf("%w: anchor seq %d, last event %d", ErrAnchorAhead, aSeq, tipSeq)
	}
	if atAnchor != aHash {
		return fmt.Errorf("%w: hash at anchor seq %d differs", ErrChainBroken, aSeq)
	}
	return nil
}

// migrationFiles returns the embedded migrations in order with their SHA-256.
func migrationFiles() ([]string, [][]byte, []string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, nil, nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	bodies := make([][]byte, len(names))
	sums := make([]string, len(names))
	for i, n := range names {
		b, err := migrationFS.ReadFile("migrations/" + n)
		if err != nil {
			return nil, nil, nil, err
		}
		sum := sha256.Sum256(b)
		bodies[i], sums[i] = b, hex.EncodeToString(sum[:])
	}
	return names, bodies, sums, nil
}

// checkMigrations requires the applied migrations to be a prefix of the
// embedded ones with identical checksums.
func checkMigrations(ctx context.Context, db querier) error {
	_, _, sums, err := migrationFiles()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT version, sha256 FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("%w: read schema_migrations: %v", ErrSchema, err)
	}
	defer rows.Close()
	var want int64 = 1
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return fmt.Errorf("%w: read schema_migrations: %v", ErrSchema, err)
		}
		if v != want {
			return fmt.Errorf("%w: migration versions not contiguous at %d", ErrSchema, v)
		}
		if v > int64(len(sums)) {
			return fmt.Errorf("%w: database schema version %d is newer than this binary (%d)", ErrSchema, v, len(sums))
		}
		if sums[v-1] != sum {
			return fmt.Errorf("%w: migration %d was changed after it was applied", ErrSchema, v)
		}
		want++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: read schema_migrations: %v", ErrSchema, err)
	}
	if want == 1 {
		return fmt.Errorf("%w: no migrations applied", ErrSchema)
	}
	return nil
}

type schemaObject struct{ table, sql string }

var (
	expectedOnce   sync.Once
	expectedSchema map[string]schemaObject
	expectedErr    error
)

// expected builds the guard objects' SQL by applying the embedded
// migrations to an in-memory database.
func expected() (map[string]schemaObject, error) {
	expectedOnce.Do(func() {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(ON)")
		if err != nil {
			expectedErr = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		_, bodies, _, err := migrationFiles()
		if err != nil {
			expectedErr = err
			return
		}
		for _, b := range bodies {
			if _, err := db.Exec(string(b)); err != nil {
				expectedErr = err
				return
			}
		}
		expectedSchema, expectedErr = readSchema(context.Background(), db)
	})
	return expectedSchema, expectedErr
}

func readSchema(ctx context.Context, db querier) (map[string]schemaObject, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_master WHERE type IN ('table', 'trigger')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]schemaObject{}
	for rows.Next() {
		var name, table string
		var q sql.NullString
		if err := rows.Scan(&name, &table, &q); err != nil {
			return nil, err
		}
		out[name] = schemaObject{table: table, sql: q.String}
	}
	return out, rows.Err()
}

// checkGuards compares the SQL of the events table and of every guard
// trigger with what the migrations create.
func checkGuards(ctx context.Context, db querier) error {
	want, err := expected()
	if err != nil {
		return fmt.Errorf("store: build expected schema: %w", err)
	}
	have, err := readSchema(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: read schema: %v", ErrSchema, err)
	}
	for _, name := range guardObjects {
		w, ok := want[name]
		if !ok {
			return fmt.Errorf("%w: %s not defined by the migrations", ErrSchema, name)
		}
		h, ok := have[name]
		if !ok {
			return fmt.Errorf("%w: %s is missing", ErrSchema, name)
		}
		if h != w {
			return fmt.Errorf("%w: %s differs from its migration", ErrSchema, name)
		}
	}
	return nil
}

// walkChain verifies every row and returns the tip seq and the hash at seq
// target (the zero hash for target 0).
func walkChain(ctx context.Context, db querier, target int64) (int64, [hashLen]byte, error) {
	var atTarget [hashLen]byte
	rows, err := db.QueryContext(ctx, `SELECT seq, ts_ns, kind, payload, prev_hash, hash FROM events ORDER BY seq`)
	if err != nil {
		return 0, atTarget, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	var prev [hashLen]byte
	var expect int64 = 1
	for rows.Next() {
		var seq, ts int64
		var kind string
		var payload, prevHash, hash []byte
		if err := rows.Scan(&seq, &ts, &kind, &payload, &prevHash, &hash); err != nil {
			return 0, atTarget, fmt.Errorf("store: scan event: %w", err)
		}
		if seq != expect {
			return 0, atTarget, fmt.Errorf("%w: expected seq %d, found %d", ErrChainBroken, expect, seq)
		}
		if len(prevHash) != hashLen || [hashLen]byte(prevHash) != prev {
			return 0, atTarget, fmt.Errorf("%w: prev_hash mismatch at seq %d", ErrChainBroken, seq)
		}
		got := chainHash(prev, seq, ts, kind, payload)
		if len(hash) != hashLen || [hashLen]byte(hash) != got {
			return 0, atTarget, fmt.Errorf("%w: hash mismatch at seq %d", ErrChainBroken, seq)
		}
		if seq == target {
			atTarget = got
		}
		prev = got
		expect++
	}
	if err := rows.Err(); err != nil {
		return 0, atTarget, fmt.Errorf("store: read events: %w", err)
	}
	return expect - 1, atTarget, nil
}

// anchor file format: "v1 <seq> <hex hash>\n".
func writeAnchor(path string, seq int64, hash [hashLen]byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".anchor-*")
	if err != nil {
		return fmt.Errorf("store: anchor temp: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := fmt.Fprintf(f, "v1 %d %s\n", seq, hex.EncodeToString(hash[:])); err != nil {
		f.Close()
		return fmt.Errorf("store: anchor write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: anchor sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: anchor close: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("store: anchor chmod: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: anchor rename: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("store: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

func readAnchor(path string) (int64, [hashLen]byte, error) {
	var h [hashLen]byte
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, h, err
	}
	if len(b) > 256 {
		return 0, h, fmt.Errorf("%w: anchor file too large", ErrChainBroken)
	}
	parts := strings.Fields(string(b))
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, h, fmt.Errorf("%w: malformed anchor file", ErrChainBroken)
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || seq < 0 {
		return 0, h, fmt.Errorf("%w: malformed anchor seq", ErrChainBroken)
	}
	raw, err := hex.DecodeString(parts[2])
	if err != nil || len(raw) != hashLen {
		return 0, h, fmt.Errorf("%w: malformed anchor hash", ErrChainBroken)
	}
	copy(h[:], raw)
	return seq, h, nil
}

// Backup writes a consistent copy of the database to dst through the
// store's own connection (writes wait meanwhile). See backupFrom.
func (s *Store) Backup(ctx context.Context, dst string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrClosed
	}
	return backupFrom(ctx, s.db, s.anchor, dst)
}

// BackupFile backs up the database at src through a read-only connection:
// it never migrates or writes the source, so it is safe while another
// process (traderd) is writing.
func BackupFile(ctx context.Context, src, dst string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	enableOFD()
	db, err := sql.Open("sqlite", dsn(src, true))
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := checkMigrations(ctx, db); err != nil {
		return "", err
	}
	return backupFrom(ctx, db, src+".anchor", dst)
}

type execQuerier interface {
	querier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// backupFrom copies the database with VACUUM INTO into dst, which must not
// exist (it is created first with mode 0600 and O_EXCL). The backup's anchor
// is written from the backup's own verified chain, and the source anchor
// (read before the copy) must match the backup at its seq. dst + ".anchor"
// and dst + ".sha256" are written; on any error every partial file is removed.
func backupFrom(ctx context.Context, src execQuerier, srcAnchor, dst string) (sum string, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("store: backup directory: %w", err)
	}
	aSeq, aHash, aerr := readAnchor(srcAnchor)
	if aerr != nil && !errors.Is(aerr, fs.ErrNotExist) {
		return "", aerr
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("store: backup target: %w", err)
	}
	f.Close()
	defer func() {
		if err != nil {
			for _, p := range []string{dst, dst + ".anchor", dst + ".sha256"} {
				os.Remove(p)
			}
		}
	}()
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return "", fmt.Errorf("store: VACUUM INTO: %w", err)
	}
	bdb, err := sql.Open("sqlite", dsn(dst, true))
	if err != nil {
		return "", err
	}
	bdb.SetMaxOpenConns(1)
	tipSeq, atAnchor, werr := walkChain(ctx, bdb, aSeq)
	var tip [hashLen]byte
	if werr == nil && tipSeq > 0 {
		_, tip, werr = walkChain(ctx, bdb, tipSeq)
	}
	bdb.Close()
	if werr != nil {
		return "", fmt.Errorf("store: backup chain: %w", werr)
	}
	if aerr == nil {
		if aSeq > tipSeq {
			return "", fmt.Errorf("%w: source anchor seq %d, backup ends at %d", ErrAnchorAhead, aSeq, tipSeq)
		}
		if atAnchor != aHash {
			return "", fmt.Errorf("%w: backup differs from the source anchor at seq %d", ErrChainBroken, aSeq)
		}
	}
	if err := writeAnchor(dst+".anchor", tipSeq, tip); err != nil {
		return "", err
	}
	sum, err = fileSHA256(dst)
	if err != nil {
		return "", err
	}
	if err := writeFileSync(dst+".sha256", []byte(sum+"  "+filepath.Base(dst)+"\n")); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return "", err
	}
	if err := VerifyFile(ctx, dst); err != nil {
		return "", fmt.Errorf("store: backup verification failed: %w", err)
	}
	return sum, nil
}

// VerifyChecksum compares path's SHA-256 with path + ".sha256".
func VerifyChecksum(path string) error {
	b, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return fmt.Errorf("store: checksum file: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 || len(fields[0]) != 64 {
		return fmt.Errorf("store: malformed checksum file")
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if sum != fields[0] {
		return fmt.Errorf("%w: SHA-256 of %s does not match its .sha256 file", ErrChainBroken, filepath.Base(path))
	}
	return nil
}

func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: write %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("store: write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: sync %s: %w", filepath.Base(path), err)
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("store: checksum: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Events returns events with seq in [from, to] in order (to <= 0: until the end).
func (s *Store) Events(ctx context.Context, from, to int64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if to <= 0 {
		to = 1<<63 - 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, ts_ns, kind, payload, prev_hash, hash FROM events WHERE seq BETWEEN ? AND ? ORDER BY seq`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var ts int64
		var prev, hash []byte
		if err := rows.Scan(&ev.Seq, &ts, &ev.Kind, &ev.Payload, &prev, &hash); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev.Time = time.Unix(0, ts).UTC()
		copy(ev.PrevHash[:], prev)
		copy(ev.Hash[:], hash)
		out = append(out, ev)
	}
	return out, rows.Err()
}
