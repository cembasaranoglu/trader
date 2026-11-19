// Package store is the durable record of the trading system (ADR-002,
// ADR-007): an append-only, hash-chained event log plus domain tables in
// PostgreSQL (16 or later).
//
// Integrity model:
//   - Every event row is chained: hash = SHA-256(prev_hash ‖ seq ‖ ts_ns ‖
//     kind ‖ payload), with fixed-width big-endian integers and
//     length-prefixed strings (see chainHash). The first event's prev_hash is
//     32 zero bytes. A database trigger recomputes the hash and accepts only
//     seq = last+1 with prev_hash = last hash and a payload that meets the
//     contract, so a client holding the application credentials cannot write
//     a broken chain.
//   - Every function pins search_path (pg_temp last) and names objects with
//     their schema, PUBLIC cannot execute them, and the application role
//     cannot create temporary objects, so temporary tables cannot shadow
//     store tables (search_path attacks on SECURITY DEFINER functions).
//   - Record tables (events, fills, balance snapshots, approvals, tax ledger)
//     reject UPDATE, DELETE and TRUNCATE by trigger, and the application role
//     is granted only SELECT and INSERT on them (role separation: the owner
//     role runs migrations; traderd connects as the application role and
//     refuses to run as the owner or a superuser).
//   - Intents and orders carry the executor's fencing epoch (see Lease); a
//     trigger rejects writes whose epoch is not the current one.
//   - Optionally, after each commit the chain tip (seq, hash) is written to
//     an anchor file on the local disk (atomic replace + fsync). Verify fails
//     when the database ends before the anchor (a restore of an older
//     snapshot) or the hash at the anchor's seq differs.
//   - Verify checks applied migration checksums, that every guard trigger
//     exists, is enabled and calls the expected function, and that each
//     guard function's source equals the migration's.
//
// Threat model: this detects application bugs, misuse of the application
// credentials, accidents (restoring an older snapshot, edits that bypass the
// store) and corruption. It does not stop someone holding the owner
// credentials, who can drop triggers; off-host copies of exports and anchors
// (W7) are the defence against that.
//
// Concurrency: any number of processes may append. Appends serialise on a
// transaction-level advisory lock, so the chain has no gaps or forks; the
// primary key and the trigger reject a conflicting insert even from a client
// that skips the lock.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits.
const (
	MaxPayloadBytes   = 1 << 20
	hashLen           = 32
	minServerVersion  = 160000
	defaultMaxConns   = 4
	defaultStmtTO     = 10 * time.Second
	defaultConnectTO  = 10 * time.Second
	verifyPageSize    = 1000
	chainLockSQL      = `SELECT pg_advisory_xact_lock(hashtextextended('trader.events', 0))`
	idleInTxTimeoutMs = "60000"
)

// Errors.
var (
	ErrChainBroken      = errors.New("store: hash chain broken")
	ErrAnchorMissing    = errors.New("store: anchor file missing for a non-empty event log (verify, then run 'traderd store anchor-init')")
	ErrAnchorExists     = errors.New("store: anchor file already exists")
	ErrAnchorAhead      = errors.New("store: event log ends before the anchor (an older snapshot was restored, or events were removed)")
	ErrInvalidEvent     = errors.New("store: invalid event")
	ErrClosed           = errors.New("store: closed")
	ErrSchema           = errors.New("store: schema integrity check failed")
	ErrSchemaOld        = errors.New("store: database schema is older than this binary (run 'traderd store migrate')")
	ErrSchemaNewer      = errors.New("store: database schema is newer than this binary")
	ErrPrivileges       = errors.New("store: database role has more privileges than the application role may have")
	ErrServerVersion    = errors.New("store: PostgreSQL 16 or later required")
	ErrFenced           = errors.New("store: write rejected by fencing (stale executor epoch)")
	ErrAppendOnly       = errors.New("store: record tables are append-only")
	ErrAnchorNotUpdated = errors.New("store: events committed but anchor not updated")
)

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

// Options configures Open.
type Options struct {
	// AnchorPath is the local anchor file; empty disables the anchor (for
	// processes on hosts that keep no anchor, such as workers).
	AnchorPath       string
	Now              func() time.Time // nil: time.Now
	MaxConns         int32            // pool size; 0: 4
	StatementTimeout time.Duration    // per statement; 0: 10s
	ConnectTimeout   time.Duration    // 0: 10s
	Logger           *slog.Logger
}

// Store is safe for concurrent use.
type Store struct {
	pool   *pgxpool.Pool
	cfg    *pgxpool.Config
	anchor string
	now    func() time.Time
	log    *slog.Logger

	mu        sync.Mutex // anchor writes
	anchorSeq int64
	closed    bool
}

// Open connects as the application role, checks the server version, the
// schema (migrations, guards) and the role's privileges, and verifies the
// whole chain against the anchor.
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := ParseDSN(dsn, opts.StatementTimeout, opts.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = opts.MaxConns
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = defaultMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s := &Store{pool: pool, cfg: cfg, anchor: opts.AnchorPath, now: opts.Now, log: opts.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if err := s.open(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) open(ctx context.Context) error {
	if err := checkServerVersion(ctx, s.pool); err != nil {
		return err
	}
	// An unmigrated database is reported as such before the role check
	// (whose result depends on grants that migrate applies).
	if err := checkMigrations(ctx, s.pool); err != nil {
		return err
	}
	if err := s.CheckPrivileges(ctx); err != nil {
		return err
	}
	tip, err := s.verify(ctx)
	if err != nil {
		return err
	}
	s.anchorSeq = tip.anchorSeq
	// Refresh the anchor: it may lag by the events of a crash between a
	// commit and the anchor write, or by appends from other processes.
	if s.anchor != "" && tip.seq > tip.anchorSeq {
		if err := writeAnchor(s.anchor, tip.seq, tip.hash); err != nil {
			return fmt.Errorf("%w: %v", ErrAnchorNotUpdated, err)
		}
		s.anchorSeq = tip.seq
	}
	return nil
}

// CheckPrivileges refuses a connection role that could rewrite records (see
// checkPrivileges). Open runs it; read-only operator commands that use
// OpenUnchecked may run it to confirm a connection string names the
// application role.
func (s *Store) CheckPrivileges(ctx context.Context) error {
	if s.isClosed() {
		return ErrClosed
	}
	return checkPrivileges(ctx, s.pool)
}

// Close closes the pool. Leases must be released first.
func (s *Store) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.pool.Close()
}

func (s *Store) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Tip returns the last event's seq and hash (0 and zero hash when empty).
func (s *Store) Tip(ctx context.Context) (int64, [hashLen]byte, error) {
	if s.isClosed() {
		return 0, [hashLen]byte{}, ErrClosed
	}
	return readTip(ctx, s.pool)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readTip(ctx context.Context, q queryRower) (int64, [hashLen]byte, error) {
	var seq int64
	var h []byte
	var out [hashLen]byte
	err := q.QueryRow(ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq, &h)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, out, nil
	}
	if err != nil {
		return 0, out, fmt.Errorf("store: read tip: %w", err)
	}
	if len(h) != hashLen {
		return 0, out, fmt.Errorf("%w: tip hash length %d", ErrChainBroken, len(h))
	}
	copy(out[:], h)
	return seq, out, nil
}

// chainHash computes an event hash. Integers are fixed-width big-endian and
// strings are length-prefixed, so different field splits never collide. The
// database trigger trader_events_append computes the same function.
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

// maxSchemaVersion is the largest payload "v" (18 digits); the database
// trigger enforces the same bound.
const maxSchemaVersion = 999999999999999999

// validatePayload enforces the event payload contract at write time: a JSON
// object of at most MaxPayloadBytes with an integer schema version "v" in
// [1, maxSchemaVersion]. The database trigger enforces the same contract.
func validatePayload(b []byte) error {
	if len(b) > MaxPayloadBytes {
		return fmt.Errorf("%w: payload larger than %d bytes", ErrInvalidEvent, MaxPayloadBytes)
	}
	if len(b) < 2 || b[0] != '{' || !json.Valid(b) {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidEvent)
	}
	var head struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(b, &head); err != nil || len(head.V) == 0 {
		return fmt.Errorf("%w: payload needs a schema version field \"v\"", ErrInvalidEvent)
	}
	if v, err := strconv.ParseInt(string(head.V), 10, 64); err != nil || v < 1 || v > maxSchemaVersion {
		return fmt.Errorf("%w: payload \"v\" must be an integer >= 1", ErrInvalidEvent)
	}
	return nil
}

// Tx is a write transaction. Events appended in it and its other statements
// commit atomically.
type Tx struct {
	tx      pgx.Tx
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
	if err := validatePayload(b); err != nil {
		return Event{}, err
	}
	ev := Event{Seq: t.tipSeq + 1, Time: t.s.now().UTC(), Kind: kind, Payload: b, PrevHash: t.tip}
	ev.Hash = chainHash(ev.PrevHash, ev.Seq, ev.Time.UnixNano(), kind, b)
	if _, err := t.tx.Exec(ctx, `INSERT INTO events (seq, ts_ns, kind, payload, prev_hash, hash) VALUES ($1, $2, $3, $4, $5, $6)`,
		ev.Seq, ev.Time.UnixNano(), kind, b, ev.PrevHash[:], ev.Hash[:]); err != nil {
		return Event{}, fmt.Errorf("store: insert event: %w", classify(err))
	}
	t.tipSeq, t.tip = ev.Seq, ev.Hash
	t.pending = append(t.pending, ev)
	return ev, nil
}

// Exec runs a statement in the transaction (domain tables). Database errors
// raised by the guards are mapped to ErrFenced, ErrAppendOnly, ErrPrivileges.
func (t *Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	return tag, classify(err)
}

// QueryRow runs a single-row query in the transaction.
func (t *Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// Query runs a query in the transaction.
func (t *Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	return rows, classify(err)
}

// Update runs fn in a write transaction that holds the chain lock. If fn
// returns an error or the commit fails nothing is written. After a
// successful commit that appended events the anchor is updated; an anchor
// write error is returned together with the committed events.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) ([]Event, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	pgTx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback errors are irrelevant here: the transaction did not
			// commit, and a broken connection is discarded by the pool.
			_ = pgTx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err := pgTx.Exec(ctx, chainLockSQL); err != nil {
		return nil, fmt.Errorf("store: chain lock: %w", err)
	}
	seq, h, err := readTip(ctx, pgTx)
	if err != nil {
		return nil, err
	}
	tx := &Tx{tx: pgTx, s: s, tipSeq: seq, tip: h}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if err := pgTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit: %w", classify(err))
	}
	committed = true
	if len(tx.pending) > 0 && s.anchor != "" {
		if err := s.advanceAnchor(tx.tipSeq, tx.tip); err != nil {
			return tx.pending, err
		}
	}
	return tx.pending, nil
}

// advanceAnchor writes the anchor when seq is beyond the last one written by
// this process (concurrent commits may finish out of order).
func (s *Store) advanceAnchor(seq int64, h [hashLen]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.anchorSeq {
		return nil
	}
	if err := writeAnchor(s.anchor, seq, h); err != nil {
		return fmt.Errorf("%w: %v", ErrAnchorNotUpdated, err)
	}
	s.anchorSeq = seq
	return nil
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

// Events returns events with seq in [from, to] in order (to <= 0: until the
// end), at most limit of them (limit <= 0: verifyPageSize).
func (s *Store) Events(ctx context.Context, from, to int64, limit int) ([]Event, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	if to <= 0 {
		to = 1<<63 - 1
	}
	if limit <= 0 {
		limit = verifyPageSize
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, ts_ns, kind, payload, prev_hash, hash FROM events WHERE seq BETWEEN $1 AND $2 ORDER BY seq LIMIT $3`, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var ts int64
		var payload, prev, hash []byte
		if err := rows.Scan(&ev.Seq, &ts, &ev.Kind, &payload, &prev, &hash); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		if len(prev) != hashLen || len(hash) != hashLen {
			return nil, fmt.Errorf("%w: hash length at seq %d", ErrChainBroken, ev.Seq)
		}
		ev.Time = time.Unix(0, ts).UTC()
		ev.Payload = payload
		copy(ev.PrevHash[:], prev)
		copy(ev.Hash[:], hash)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	return out, nil
}

// Error codes raised by the migration's guard functions.
const (
	codeFenced     = "TR001"
	codeAppendOnly = "TR002"
	codeChain      = "TR003"
	codeInvalid    = "TR005"
	codeNoPriv     = "42501" // insufficient_privilege
)

// classify maps guard errors to package errors, keeping the original error
// in the chain.
func classify(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case codeFenced:
		return fmt.Errorf("%w: %w", ErrFenced, err)
	case codeAppendOnly:
		return fmt.Errorf("%w: %w", ErrAppendOnly, err)
	case codeChain:
		return fmt.Errorf("%w: %w", ErrChainBroken, err)
	case codeInvalid:
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	case codeNoPriv:
		return fmt.Errorf("%w: %w", ErrPrivileges, err)
	}
	return err
}

func checkServerVersion(ctx context.Context, q queryRower) error {
	var v string
	if err := q.QueryRow(ctx, `SHOW server_version_num`).Scan(&v); err != nil {
		return fmt.Errorf("store: server version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("store: server version %q: %w", v, err)
	}
	if n < minServerVersion {
		return fmt.Errorf("%w (server reports %d)", ErrServerVersion, n)
	}
	return nil
}
