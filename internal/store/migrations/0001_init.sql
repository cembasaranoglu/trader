-- 0001: append-only event log with hash chain, and the domain tables the
-- order path (W2b) writes. Money and quantities are exact decimal strings.

CREATE TABLE events (
    seq       INTEGER PRIMARY KEY,           -- 1, 2, 3 ... contiguous (trigger below)
    ts_ns     INTEGER NOT NULL,              -- UTC Unix nanoseconds
    kind      TEXT    NOT NULL,
    payload   BLOB    NOT NULL,              -- JSON object, stored exactly as hashed
    prev_hash BLOB    NOT NULL CHECK (length(prev_hash) = 32),
    hash      BLOB    NOT NULL CHECK (length(hash) = 32)
) STRICT;

-- Only seq = max+1 may be inserted. This also blocks INSERT OR REPLACE of an
-- existing row (its seq is not max+1).
CREATE TRIGGER events_append_contiguous BEFORE INSERT ON events
WHEN NEW.seq IS NOT (SELECT COALESCE(MAX(seq), 0) + 1 FROM events)
BEGIN
    SELECT RAISE(ABORT, 'events: seq must be max(seq)+1');
END;

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events: append-only');
END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events: append-only');
END;

-- Intents are written before an order is sent (E-1, E-2).
CREATE TABLE intents (
    intent_id         TEXT    PRIMARY KEY,
    created_seq       INTEGER NOT NULL REFERENCES events (seq),
    policy_version    TEXT    NOT NULL,
    symbol            TEXT    NOT NULL,
    side              TEXT    NOT NULL CHECK (side IN ('BUY', 'SELL')),
    qty               TEXT    NOT NULL,
    stop_price        TEXT,
    take_profit_price TEXT,
    state             TEXT    NOT NULL,
    updated_ns        INTEGER NOT NULL
) STRICT;

CREATE TABLE orders (
    client_order_id TEXT    PRIMARY KEY,
    intent_id       TEXT    NOT NULL REFERENCES intents (intent_id),
    exchange_id     INTEGER,
    order_list_id   INTEGER,
    symbol          TEXT    NOT NULL,
    side            TEXT    NOT NULL CHECK (side IN ('BUY', 'SELL')),
    type            TEXT    NOT NULL,
    qty             TEXT    NOT NULL,
    price           TEXT,
    stop_price      TEXT,
    state           TEXT    NOT NULL,
    created_seq     INTEGER NOT NULL REFERENCES events (seq),
    updated_ns      INTEGER NOT NULL
) STRICT;

CREATE TABLE fills (
    symbol           TEXT    NOT NULL,
    trade_id         INTEGER NOT NULL,
    client_order_id  TEXT    NOT NULL REFERENCES orders (client_order_id),
    side             TEXT    NOT NULL CHECK (side IN ('BUY', 'SELL')),
    price            TEXT    NOT NULL,
    qty              TEXT    NOT NULL,
    commission       TEXT    NOT NULL,
    commission_asset TEXT    NOT NULL,
    is_maker         INTEGER NOT NULL CHECK (is_maker IN (0, 1)),
    trade_time_ms    INTEGER NOT NULL,
    seq              INTEGER NOT NULL REFERENCES events (seq),
    PRIMARY KEY (symbol, trade_id)
) STRICT;

CREATE TABLE balances_snapshot (
    seq    INTEGER NOT NULL REFERENCES events (seq),
    asset  TEXT    NOT NULL,
    free   TEXT    NOT NULL,
    locked TEXT    NOT NULL,
    PRIMARY KEY (seq, asset)
) STRICT;

CREATE TABLE approvals (
    seq          INTEGER PRIMARY KEY REFERENCES events (seq),
    action       TEXT    NOT NULL,
    subject      TEXT    NOT NULL,
    signer       TEXT    NOT NULL,
    signature    BLOB    NOT NULL,
    approved_ns  INTEGER NOT NULL
) STRICT;

CREATE TABLE tax_ledger (
    symbol           TEXT    NOT NULL,
    trade_id         INTEGER NOT NULL,
    trade_time_ms    INTEGER NOT NULL,
    side             TEXT    NOT NULL CHECK (side IN ('BUY', 'SELL')),
    qty              TEXT    NOT NULL,
    price            TEXT    NOT NULL,
    commission       TEXT    NOT NULL,
    commission_asset TEXT    NOT NULL,
    usd_try_rate     TEXT,
    usd_try_source   TEXT,
    tx_tax_rate      TEXT,
    PRIMARY KEY (symbol, trade_id),
    FOREIGN KEY (symbol, trade_id) REFERENCES fills (symbol, trade_id),
    CHECK ((usd_try_rate IS NULL) = (usd_try_source IS NULL)) -- a rate always names its source
) STRICT;

-- Fills, approvals and the tax ledger are records of fact: never changed.
CREATE TRIGGER fills_no_update BEFORE UPDATE ON fills BEGIN SELECT RAISE(ABORT, 'fills: immutable'); END;
CREATE TRIGGER fills_no_delete BEFORE DELETE ON fills BEGIN SELECT RAISE(ABORT, 'fills: immutable'); END;
CREATE TRIGGER approvals_no_update BEFORE UPDATE ON approvals BEGIN SELECT RAISE(ABORT, 'approvals: immutable'); END;
CREATE TRIGGER approvals_no_delete BEFORE DELETE ON approvals BEGIN SELECT RAISE(ABORT, 'approvals: immutable'); END;
CREATE TRIGGER tax_ledger_no_update BEFORE UPDATE ON tax_ledger BEGIN SELECT RAISE(ABORT, 'tax_ledger: immutable'); END;
CREATE TRIGGER tax_ledger_no_delete BEFORE DELETE ON tax_ledger BEGIN SELECT RAISE(ABORT, 'tax_ledger: immutable'); END;
