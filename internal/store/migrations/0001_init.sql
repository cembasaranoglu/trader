-- 0001: append-only event log with a hash chain verified in the database,
-- the executor lease used for fencing, and the domain tables the order path
-- (W2b) writes. Money and quantities are exact decimal strings.
--
-- Runs as the owner role (traderd store migrate). The application role gets
-- only the privileges granted by the migrator after all migrations (see
-- grant in migrate.go): SELECT/INSERT on records, SELECT/INSERT/UPDATE on
-- intents and orders, SELECT on leases and schema_migrations, EXECUTE on the
-- two lease functions. The migrator also revokes TEMPORARY on the database
-- from PUBLIC.
--
-- Hardening against search_path attacks: every function pins
-- search_path = pg_catalog, public, pg_temp (pg_temp last, so a temporary
-- table cannot shadow a real one) and names every table and function with
-- its schema. Functions are not executable by PUBLIC.

ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- trader_forbid rejects UPDATE, DELETE and TRUNCATE on record tables.
CREATE FUNCTION trader_forbid() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '%: append-only (% rejected)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'TR002';
END
$$;

CREATE TABLE events (
    seq       bigint PRIMARY KEY CHECK (seq >= 1),   -- 1, 2, 3 ... contiguous (trigger below)
    ts_ns     bigint NOT NULL,                       -- UTC Unix nanoseconds
    kind      text   NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
    payload   bytea  NOT NULL CHECK (octet_length(payload) BETWEEN 2 AND 1048576), -- JSON object, stored exactly as hashed
    prev_hash bytea  NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash      bytea  NOT NULL CHECK (octet_length(hash) = 32)
);

-- trader_events_append accepts only the next event of the chain:
-- seq = last + 1, prev_hash = last hash (32 zero bytes before the first
-- event), hash = SHA-256(prev_hash || be64 seq || be64 ts_ns ||
-- be32 len(kind) || kind || be32 len(payload) || payload), and a payload
-- that is a JSON object with an integer "v" in [1, 10^18).
-- Two concurrent inserts of the same seq conflict on the primary key.
CREATE FUNCTION trader_events_append() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    last_seq  bigint;
    last_hash bytea;
    kind_b    bytea;
    want      bytea;
    doc       jsonb;
BEGIN
    BEGIN
        doc := convert_from(NEW.payload, 'UTF8')::jsonb;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'events: payload of seq % is not UTF-8 JSON', NEW.seq USING ERRCODE = 'TR005';
    END;
    IF jsonb_typeof(doc) IS DISTINCT FROM 'object'
       OR jsonb_typeof(doc -> 'v') IS DISTINCT FROM 'number'
       OR (doc ->> 'v') !~ '^[1-9][0-9]{0,17}$' THEN
        RAISE EXCEPTION 'events: payload of seq % needs an integer "v" >= 1', NEW.seq USING ERRCODE = 'TR005';
    END IF;
    SELECT e.seq, e.hash INTO last_seq, last_hash FROM public.events e ORDER BY e.seq DESC LIMIT 1;
    IF last_seq IS NULL THEN
        last_seq := 0;
        last_hash := '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea;
    END IF;
    IF NEW.seq IS DISTINCT FROM last_seq + 1 THEN
        RAISE EXCEPTION 'events: seq % is not the next seq %', NEW.seq, last_seq + 1 USING ERRCODE = 'TR003';
    END IF;
    IF NEW.prev_hash IS DISTINCT FROM last_hash THEN
        RAISE EXCEPTION 'events: prev_hash does not match the hash of seq %', last_seq USING ERRCODE = 'TR003';
    END IF;
    kind_b := convert_to(NEW.kind, 'UTF8');
    want := sha256(NEW.prev_hash || int8send(NEW.seq) || int8send(NEW.ts_ns)
                   || int4send(octet_length(kind_b)) || kind_b
                   || int4send(octet_length(NEW.payload)) || NEW.payload);
    IF NEW.hash IS DISTINCT FROM want THEN
        RAISE EXCEPTION 'events: hash of seq % does not match its content', NEW.seq USING ERRCODE = 'TR003';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER events_append BEFORE INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION trader_events_append();
CREATE TRIGGER events_forbid BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER events_forbid_truncate BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();

-- Leases: one row per singleton role. epoch is the fencing token; it only
-- grows, and only through trader_acquire_lease, which first takes the
-- session-level advisory lock for the lease. A session that dies releases
-- the advisory lock; the next holder's epoch is larger, and writes stamped
-- with the old epoch are rejected (trader_fence).
CREATE TABLE leases (
    name        text   PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]{0,31}$'),
    epoch       bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    holder      text,
    acquired_at timestamptz
);
INSERT INTO leases (name) VALUES ('executor');

CREATE FUNCTION trader_lease_key(p_name text) RETURNS bigint
LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, public, pg_temp AS $$ SELECT pg_catalog.hashtextextended('trader.lease.' || p_name, 0) $$;

-- Returns the new epoch, or NULL when another session holds the lease.
CREATE FUNCTION trader_acquire_lease(p_name text, p_holder text) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    e bigint;
BEGIN
    IF NOT pg_catalog.pg_try_advisory_lock(public.trader_lease_key(p_name)) THEN
        RETURN NULL;
    END IF;
    UPDATE public.leases SET epoch = epoch + 1, holder = left(p_holder, 200), acquired_at = now()
        WHERE name = p_name RETURNING epoch INTO e;
    IF e IS NULL THEN
        PERFORM pg_catalog.pg_advisory_unlock(public.trader_lease_key(p_name));
        RAISE EXCEPTION 'unknown lease %', p_name USING ERRCODE = 'TR004';
    END IF;
    RETURN e;
END
$$;

CREATE FUNCTION trader_release_lease(p_name text) RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT pg_catalog.pg_advisory_unlock(public.trader_lease_key(p_name))
$$;

-- trader_fence rejects a write whose epoch is not the executor's current
-- epoch. FOR SHARE serialises with a concurrent epoch increment, so a write
-- either commits before the new holder's epoch (and the new holder sees it
-- when it reconciles) or is rejected.
CREATE FUNCTION trader_fence() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    cur bigint;
BEGIN
    SELECT l.epoch INTO cur FROM public.leases l WHERE l.name = 'executor' FOR SHARE;
    IF cur IS NULL OR NEW.epoch IS DISTINCT FROM cur THEN
        RAISE EXCEPTION '%: fenced (epoch % is not the current executor epoch %)', TG_TABLE_NAME, NEW.epoch, cur
            USING ERRCODE = 'TR001';
    END IF;
    RETURN NEW;
END
$$;

-- Intents are written before an order is sent (E-1, E-2).
CREATE TABLE intents (
    intent_id         text   PRIMARY KEY,
    created_seq       bigint NOT NULL REFERENCES events (seq),
    epoch             bigint NOT NULL,
    policy_version    text   NOT NULL,
    symbol            text   NOT NULL,
    side              text   NOT NULL CHECK (side IN ('BUY', 'SELL')),
    qty               text   NOT NULL,
    stop_price        text,
    take_profit_price text,
    state             text   NOT NULL,
    updated_ns        bigint NOT NULL
);

CREATE TABLE orders (
    client_order_id text   PRIMARY KEY,
    intent_id       text   NOT NULL REFERENCES intents (intent_id),
    epoch           bigint NOT NULL,
    exchange_id     bigint,
    order_list_id   bigint,
    symbol          text   NOT NULL,
    side            text   NOT NULL CHECK (side IN ('BUY', 'SELL')),
    type            text   NOT NULL,
    qty             text   NOT NULL,
    price           text,
    stop_price      text,
    state           text   NOT NULL,
    created_seq     bigint NOT NULL REFERENCES events (seq),
    updated_ns      bigint NOT NULL
);

CREATE TRIGGER intents_fence BEFORE INSERT OR UPDATE ON intents
    FOR EACH ROW EXECUTE FUNCTION trader_fence();
CREATE TRIGGER orders_fence BEFORE INSERT OR UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION trader_fence();
CREATE TRIGGER intents_forbid BEFORE DELETE ON intents
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER orders_forbid BEFORE DELETE ON orders
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER intents_forbid_truncate BEFORE TRUNCATE ON intents
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER orders_forbid_truncate BEFORE TRUNCATE ON orders
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();

CREATE TABLE fills (
    symbol           text    NOT NULL,
    trade_id         bigint  NOT NULL,
    client_order_id  text    NOT NULL REFERENCES orders (client_order_id),
    side             text    NOT NULL CHECK (side IN ('BUY', 'SELL')),
    price            text    NOT NULL,
    qty              text    NOT NULL,
    commission       text    NOT NULL,
    commission_asset text    NOT NULL,
    is_maker         boolean NOT NULL,
    trade_time_ms    bigint  NOT NULL,
    seq              bigint  NOT NULL REFERENCES events (seq),
    PRIMARY KEY (symbol, trade_id)
);

CREATE TABLE balances_snapshot (
    seq    bigint NOT NULL REFERENCES events (seq),
    asset  text   NOT NULL,
    free   text   NOT NULL,
    locked text   NOT NULL,
    PRIMARY KEY (seq, asset)
);

CREATE TABLE approvals (
    seq         bigint PRIMARY KEY REFERENCES events (seq),
    action      text   NOT NULL,
    subject     text   NOT NULL,
    signer      text   NOT NULL,
    signature   bytea  NOT NULL,
    approved_ns bigint NOT NULL
);

CREATE TABLE tax_ledger (
    symbol           text   NOT NULL,
    trade_id         bigint NOT NULL,
    trade_time_ms    bigint NOT NULL,
    side             text   NOT NULL CHECK (side IN ('BUY', 'SELL')),
    qty              text   NOT NULL,
    price            text   NOT NULL,
    commission       text   NOT NULL,
    commission_asset text   NOT NULL,
    usd_try_rate     text,
    usd_try_source   text,
    tx_tax_rate      text,
    PRIMARY KEY (symbol, trade_id),
    FOREIGN KEY (symbol, trade_id) REFERENCES fills (symbol, trade_id),
    CHECK ((usd_try_rate IS NULL) = (usd_try_source IS NULL)) -- a rate always names its source
);

-- Fills, balance snapshots, approvals and the tax ledger are records of
-- fact. INSERT ... ON CONFLICT DO UPDATE needs UPDATE (not granted) and
-- would fire the UPDATE trigger.
CREATE TRIGGER fills_forbid BEFORE UPDATE OR DELETE ON fills
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER balances_snapshot_forbid BEFORE UPDATE OR DELETE ON balances_snapshot
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER approvals_forbid BEFORE UPDATE OR DELETE ON approvals
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER tax_ledger_forbid BEFORE UPDATE OR DELETE ON tax_ledger
    FOR EACH ROW EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER fills_forbid_truncate BEFORE TRUNCATE ON fills
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER balances_snapshot_forbid_truncate BEFORE TRUNCATE ON balances_snapshot
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER approvals_forbid_truncate BEFORE TRUNCATE ON approvals
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();
CREATE TRIGGER tax_ledger_forbid_truncate BEFORE TRUNCATE ON tax_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION trader_forbid();

-- No function is executable by PUBLIC; grant() gives the application role
-- EXECUTE on the two lease functions only.
REVOKE ALL ON FUNCTION trader_forbid(), trader_events_append(), trader_lease_key(text),
    trader_acquire_lease(text, text), trader_release_lease(text), trader_fence() FROM PUBLIC;
