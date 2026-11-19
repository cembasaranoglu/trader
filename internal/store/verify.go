package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tipInfo struct {
	seq       int64
	hash      [hashLen]byte
	anchorSeq int64 // 0 when there is no anchor
}

// Verify checks the schema, re-hashes the whole chain in one consistent
// snapshot (constant memory) and compares it with the anchor.
func (s *Store) Verify(ctx context.Context) error {
	if s.isClosed() {
		return ErrClosed
	}
	_, err := s.verify(ctx)
	return err
}

func (s *Store) verify(ctx context.Context) (tipInfo, error) {
	var ti tipInfo
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ti, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only
	if err := checkMigrations(ctx, tx); err != nil {
		return ti, err
	}
	if err := checkGuards(ctx, tx); err != nil {
		return ti, err
	}
	var aSeq int64
	var aHash [hashLen]byte
	missing := true
	if s.anchor != "" {
		var aerr error
		aSeq, aHash, aerr = readAnchor(s.anchor)
		missing = errors.Is(aerr, fs.ErrNotExist)
		if aerr != nil && !missing {
			return ti, aerr
		}
	}
	tipSeq, tipHash, atAnchor, err := walkChain(ctx, tx, aSeq)
	if err != nil {
		return ti, err
	}
	ti.seq, ti.hash = tipSeq, tipHash
	if s.anchor == "" {
		return ti, nil
	}
	if missing {
		if tipSeq == 0 {
			return ti, nil
		}
		return ti, ErrAnchorMissing
	}
	if aSeq > tipSeq {
		return ti, fmt.Errorf("%w: anchor seq %d, last event %d", ErrAnchorAhead, aSeq, tipSeq)
	}
	if aSeq > 0 && atAnchor != aHash {
		return ti, fmt.Errorf("%w: hash at anchor seq %d differs", ErrChainBroken, aSeq)
	}
	ti.anchorSeq = aSeq
	return ti, nil
}

// walkChain re-hashes the chain page by page and returns the last seq and
// hash and the hash at seq target (zero if target is 0 or beyond the end).
func walkChain(ctx context.Context, tx pgx.Tx, target int64) (int64, [hashLen]byte, [hashLen]byte, error) {
	var prev, atTarget [hashLen]byte
	var last int64
	for {
		rows, err := tx.Query(ctx, `SELECT seq, ts_ns, kind, payload, prev_hash, hash FROM events WHERE seq > $1 ORDER BY seq LIMIT $2`, last, verifyPageSize)
		if err != nil {
			return 0, prev, atTarget, fmt.Errorf("store: read events: %w", err)
		}
		n := 0
		for rows.Next() {
			var seq, ts int64
			var kind string
			var payload, prevHash, hash []byte
			if err := rows.Scan(&seq, &ts, &kind, &payload, &prevHash, &hash); err != nil {
				rows.Close()
				return 0, prev, atTarget, fmt.Errorf("store: scan event: %w", err)
			}
			if seq != last+1 {
				rows.Close()
				return 0, prev, atTarget, fmt.Errorf("%w: expected seq %d, found %d", ErrChainBroken, last+1, seq)
			}
			if len(prevHash) != hashLen || [hashLen]byte(prevHash) != prev {
				rows.Close()
				return 0, prev, atTarget, fmt.Errorf("%w: prev_hash mismatch at seq %d", ErrChainBroken, seq)
			}
			got := chainHash(prev, seq, ts, kind, payload)
			if len(hash) != hashLen || [hashLen]byte(hash) != got {
				rows.Close()
				return 0, prev, atTarget, fmt.Errorf("%w: hash mismatch at seq %d", ErrChainBroken, seq)
			}
			if seq == target {
				atTarget = got
			}
			prev, last = got, seq
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, prev, atTarget, fmt.Errorf("store: read events: %w", err)
		}
		if n < verifyPageSize {
			return last, prev, atTarget, nil
		}
	}
}

// InitAnchor writes the anchor from the current tip after verifying the
// chain. It refuses when the anchor already exists. Use it once on a host
// that has none (a new or rebuilt server), after checking the chain against
// an off-host copy.
func (s *Store) InitAnchor(ctx context.Context) (int64, error) {
	if s.anchor == "" {
		return 0, errors.New("store: no anchor path configured")
	}
	if _, _, err := readAnchor(s.anchor); err == nil {
		return 0, ErrAnchorExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only
	if err := checkGuards(ctx, tx); err != nil {
		return 0, err
	}
	seq, h, _, err := walkChain(ctx, tx, 0)
	if err != nil {
		return 0, err
	}
	// An empty log gets an anchor at seq 0; the first append advances it.
	if err := writeAnchor(s.anchor, seq, h); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAnchorNotUpdated, err)
	}
	s.mu.Lock()
	s.anchorSeq = max(s.anchorSeq, seq)
	s.mu.Unlock()
	return seq, nil
}

// OpenUnchecked connects and checks only the server version: no role check,
// no schema or chain verification, no anchor refresh. It is for operator
// commands: Verify, Export and InitAnchor check the schema and guards
// themselves, and CheckPrivileges confirms the application role.
func OpenUnchecked(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := ParseDSN(dsn, opts.StatementTimeout, opts.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 2
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
	if err := checkServerVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}
