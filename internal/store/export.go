package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

// Export format (JSON lines): a header {"format":"trader-events","v":1},
// one line per event with the payload base64-encoded (so the exact hashed
// bytes survive), and a trailer {"end":true,"count":N,"tip":"<hex>"}.
// VerifyExport checks a file offline; the trailer makes a file cut short
// detectable (any prefix of a chain is itself a valid chain).

const exportFormat = "trader-events"

type exportHeader struct {
	Format string `json:"format"`
	V      int    `json:"v"`
}

type exportTrailer struct {
	End   bool   `json:"end"`
	Count int64  `json:"count"`
	Tip   string `json:"tip"`
}

type exportLine struct {
	Seq      int64  `json:"seq"`
	TsNs     int64  `json:"ts_ns"`
	Kind     string `json:"kind"`
	Payload  string `json:"payload"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// Export writes every event in one consistent snapshot, verifying the chain
// while it writes. It returns the number of events and the tip hash.
func (s *Store) Export(ctx context.Context, w io.Writer) (int64, [hashLen]byte, error) {
	var tip [hashLen]byte
	if s.isClosed() {
		return 0, tip, ErrClosed
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, tip, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only
	if err := checkMigrations(ctx, tx); err != nil {
		return 0, tip, err
	}
	if err := checkGuards(ctx, tx); err != nil {
		return 0, tip, err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(exportHeader{Format: exportFormat, V: 1}); err != nil {
		return 0, tip, err
	}
	var last int64
	for {
		rows, err := tx.Query(ctx, `SELECT seq, ts_ns, kind, payload, prev_hash, hash FROM events WHERE seq > $1 ORDER BY seq LIMIT $2`, last, verifyPageSize)
		if err != nil {
			return 0, tip, err
		}
		evs, err := scanEvents(rows)
		if err != nil {
			return 0, tip, err
		}
		for _, ev := range evs {
			if ev.Seq != last+1 || ev.PrevHash != tip || chainHash(tip, ev.Seq, ev.Time.UnixNano(), ev.Kind, ev.Payload) != ev.Hash {
				return 0, tip, fmt.Errorf("%w at seq %d", ErrChainBroken, ev.Seq)
			}
			if err := enc.Encode(exportLine{Seq: ev.Seq, TsNs: ev.Time.UnixNano(), Kind: ev.Kind,
				Payload: base64.StdEncoding.EncodeToString(ev.Payload), PrevHash: hex.EncodeToString(ev.PrevHash[:]),
				Hash: hex.EncodeToString(ev.Hash[:])}); err != nil {
				return 0, tip, err
			}
			last, tip = ev.Seq, ev.Hash
		}
		if len(evs) < verifyPageSize {
			break
		}
	}
	if err := enc.Encode(exportTrailer{End: true, Count: last, Tip: hex.EncodeToString(tip[:])}); err != nil {
		return 0, tip, err
	}
	return last, tip, bw.Flush()
}

// maxExportLine bounds one line: a base64 payload of MaxPayloadBytes plus
// the other fields.
const maxExportLine = MaxPayloadBytes*4/3 + 4096

// VerifyExport verifies an export offline: header, contiguous seqs from 1,
// prev_hash links, hashes, and a trailer whose count and tip match and
// after which nothing follows. The payload contract is a write-time rule
// (Go and the database trigger) and is not re-checked here; the hash
// protects the bytes. It returns the number of events and the tip hash.
func VerifyExport(r io.Reader) (int64, [hashLen]byte, error) {
	var tip [hashLen]byte
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxExportLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return 0, tip, err
		}
		return 0, tip, errors.New("store: empty export")
	}
	var hdr exportHeader
	if err := strictUnmarshal(sc.Bytes(), &hdr); err != nil || hdr.Format != exportFormat || hdr.V != 1 {
		return 0, tip, errors.New("store: not a trader-events v1 export")
	}
	var last int64
	ended := false
	for sc.Scan() {
		if ended {
			return 0, tip, fmt.Errorf("%w: data after the trailer", ErrChainBroken)
		}
		var probe struct {
			End *bool `json:"end"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) == nil && probe.End != nil {
			var tr exportTrailer
			if err := strictUnmarshal(sc.Bytes(), &tr); err != nil || !tr.End {
				return 0, tip, fmt.Errorf("%w: malformed trailer", ErrChainBroken)
			}
			if tr.Count != last || tr.Tip != hex.EncodeToString(tip[:]) {
				return 0, tip, fmt.Errorf("%w: trailer says %d events ending in %s, file has %d", ErrChainBroken, tr.Count, tr.Tip, last)
			}
			ended = true
			continue
		}
		var ln exportLine
		if err := strictUnmarshal(sc.Bytes(), &ln); err != nil {
			return 0, tip, fmt.Errorf("%w: line %d: %v", ErrChainBroken, last+2, err)
		}
		payload, err := base64.StdEncoding.DecodeString(ln.Payload)
		if err != nil {
			return 0, tip, fmt.Errorf("%w: seq %d payload encoding", ErrChainBroken, ln.Seq)
		}
		prev, err1 := hex.DecodeString(ln.PrevHash)
		hash, err2 := hex.DecodeString(ln.Hash)
		switch {
		case ln.Seq != last+1:
			return 0, tip, fmt.Errorf("%w: expected seq %d, found %d", ErrChainBroken, last+1, ln.Seq)
		case err1 != nil || err2 != nil || len(prev) != hashLen || len(hash) != hashLen:
			return 0, tip, fmt.Errorf("%w: seq %d hash encoding", ErrChainBroken, ln.Seq)
		case [hashLen]byte(prev) != tip:
			return 0, tip, fmt.Errorf("%w: prev_hash mismatch at seq %d", ErrChainBroken, ln.Seq)
		case !kindPattern.MatchString(ln.Kind):
			return 0, tip, fmt.Errorf("%w: seq %d kind", ErrChainBroken, ln.Seq)
		}
		got := chainHash(tip, ln.Seq, ln.TsNs, ln.Kind, payload)
		if got != [hashLen]byte(hash) {
			return 0, tip, fmt.Errorf("%w: hash mismatch at seq %d", ErrChainBroken, ln.Seq)
		}
		last, tip = ln.Seq, got
	}
	if err := sc.Err(); err != nil {
		return 0, tip, fmt.Errorf("store: read export: %w", err)
	}
	if !ended {
		return 0, tip, fmt.Errorf("%w: no trailer (the export is incomplete)", ErrChainBroken)
	}
	return last, tip, nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
