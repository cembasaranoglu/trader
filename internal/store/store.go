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
//   - After each commit the chain tip (seq, hash) is written to an anchor
//     file next to the database (atomic replace + fsync). Verify fails when
//     the database ends before the anchor (truncation or an older copy) or
//     the hash at the anchor's seq differs (rewrite). The anchor may lag the
//     database by the events of a crash between commit and anchor write.
//
// There is one writer connection; the process is the only writer.
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
)

// guardTriggers must exist in every database; Verify fails without them.
var guardTriggers = []string{
	"events_append_contiguous", "events_no_update", "events_no_delete",
	"fills_no_update", "fills_no_delete", "approvals_no_update", "approvals_no_delete",
	"tax_ledger_no_update", "tax_ledger_no_delete",
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
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, anchor: path + ".anchor", db: db, now: now}
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
	if s.tipSeq == 0 {
		if _, err := os.Stat(s.anchor); errors.Is(err, fs.ErrNotExist) {
			if err := writeAnchor(s.anchor, 0, [hashLen]byte{}); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return s, nil
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
	return s.db.Close()
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
	tx := &Tx{tx: sqlTx, s: s, tipSeq: s.tipSeq, tip: s.tip}
	if err := fn(tx); err != nil {
		sqlTx.Rollback()
		return nil, err
	}
	if err := sqlTx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}
	s.tipSeq, s.tip = tx.tipSeq, tx.tip
	if len(tx.pending) > 0 {
		if err := writeAnchor(s.anchor, s.tipSeq, s.tip); err != nil {
			return tx.pending, fmt.Errorf("store: events committed but anchor not updated: %w", err)
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

// Verify re-hashes the whole chain and checks it against the anchor file.
func (s *Store) Verify(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.verifyAnchor(ctx, s.db, s.anchor)
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
	return (&Store{}).verifyAnchor(ctx, db, path+".anchor")
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// verifyAnchor checks the guard triggers, reads the anchor, walks the whole
// chain and compares them. Memory use is constant.
func (s *Store) verifyAnchor(ctx context.Context, db querier, anchorPath string) error {
	if err := checkTriggers(ctx, db); err != nil {
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

func checkTriggers(ctx context.Context, db querier) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		return fmt.Errorf("store: read triggers: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("store: read triggers: %w", err)
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read triggers: %w", err)
	}
	for _, n := range guardTriggers {
		if !have[n] {
			return fmt.Errorf("%w: guard trigger %s is missing", ErrSchema, n)
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

// Backup writes a consistent copy of the database to dst (which must not
// exist) with VACUUM INTO, writes its anchor (dst + ".anchor") and SHA-256
// (dst + ".sha256"), and verifies the copy. Writes are blocked meanwhile.
func (s *Store) Backup(ctx context.Context, dst string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrClosed
	}
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("store: backup target %s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("store: backup directory: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return "", fmt.Errorf("store: VACUUM INTO: %w", err)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return "", fmt.Errorf("store: backup chmod: %w", err)
	}
	if err := writeAnchor(dst+".anchor", s.tipSeq, s.tip); err != nil {
		return "", err
	}
	sum, err := fileSHA256(dst)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst+".sha256", []byte(sum+"  "+filepath.Base(dst)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("store: write checksum: %w", err)
	}
	if err := VerifyFile(ctx, dst); err != nil {
		return "", fmt.Errorf("store: backup verification failed: %w", err)
	}
	return sum, nil
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
