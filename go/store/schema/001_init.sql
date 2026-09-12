-- tradekit shared schema, version 1.
--
-- Both go/store and tradekit.store embed and apply this file. Projects add
-- their own tables in migrations numbered 500 and above; the library owns
-- 001-499. Never edit an applied migration -- add a new numbered file.
--
-- All monetary columns are INTEGER minor units (paise / cents). All timestamps
-- are TEXT in RFC3339 with an explicit offset, so they sort lexicographically
-- and carry their timezone.

CREATE TABLE IF NOT EXISTS instruments (
    exchange     TEXT    NOT NULL,
    symbol       TEXT    NOT NULL,
    name         TEXT    NOT NULL DEFAULT '',
    isin         TEXT    NOT NULL DEFAULT '',
    segment      TEXT    NOT NULL DEFAULT 'equity',
    lot_size     INTEGER NOT NULL DEFAULT 1,
    tick_size    INTEGER NOT NULL DEFAULT 5,
    expiry       TEXT,
    strike       INTEGER,
    option_type  TEXT    NOT NULL DEFAULT '',
    active       INTEGER NOT NULL DEFAULT 1,
    updated_at   TEXT    NOT NULL,
    PRIMARY KEY (exchange, symbol)
);

CREATE INDEX IF NOT EXISTS idx_instruments_isin   ON instruments(isin);
CREATE INDEX IF NOT EXISTS idx_instruments_active ON instruments(active, segment);

-- Broker-specific identifiers for an instrument, kept out of the instrument
-- row so that adding a broker is not a schema change.
CREATE TABLE IF NOT EXISTS instrument_ids (
    exchange  TEXT NOT NULL,
    symbol    TEXT NOT NULL,
    broker    TEXT NOT NULL,          -- 'zerodha', 'upstox', 'alpaca'
    broker_id TEXT NOT NULL,          -- '738561', 'NSE_EQ|INE002A01018'
    PRIMARY KEY (exchange, symbol, broker),
    FOREIGN KEY (exchange, symbol) REFERENCES instruments(exchange, symbol)
);

CREATE INDEX IF NOT EXISTS idx_instrument_ids_lookup ON instrument_ids(broker, broker_id);

-- One row per bar. `start` is the bucket's opening timestamp, so an
-- incremental sync is "SELECT max(start) WHERE key AND timeframe".
CREATE TABLE IF NOT EXISTS candles (
    exchange      TEXT    NOT NULL,
    symbol        TEXT    NOT NULL,
    timeframe     TEXT    NOT NULL,
    start         TEXT    NOT NULL,
    open          INTEGER NOT NULL,
    high          INTEGER NOT NULL,
    low           INTEGER NOT NULL,
    close         INTEGER NOT NULL,
    volume        INTEGER NOT NULL DEFAULT 0,
    open_interest INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (exchange, symbol, timeframe, start)
);

CREATE INDEX IF NOT EXISTS idx_candles_scan ON candles(timeframe, start);

CREATE TABLE IF NOT EXISTS signals (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    INTEGER,
    exchange  TEXT    NOT NULL,
    symbol    TEXT    NOT NULL,
    kind      TEXT    NOT NULL,
    at        TEXT    NOT NULL,
    price     INTEGER NOT NULL,
    stop      INTEGER NOT NULL DEFAULT 0,
    target    INTEGER NOT NULL DEFAULT 0,
    strategy  TEXT    NOT NULL DEFAULT '',
    metadata  TEXT    NOT NULL DEFAULT '{}',
    paper     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_signals_at  ON signals(at);
CREATE INDEX IF NOT EXISTS idx_signals_run ON signals(run_id);

CREATE TABLE IF NOT EXISTS orders (
    id              TEXT    PRIMARY KEY,
    exchange        TEXT    NOT NULL,
    symbol          TEXT    NOT NULL,
    side            TEXT    NOT NULL,
    quantity        INTEGER NOT NULL,
    type            TEXT    NOT NULL,
    product         TEXT    NOT NULL,
    limit_price     INTEGER NOT NULL DEFAULT 0,
    trigger_price   INTEGER NOT NULL DEFAULT 0,
    time_in_force   TEXT    NOT NULL DEFAULT 'day',
    tag             TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL,
    filled_quantity INTEGER NOT NULL DEFAULT 0,
    average_price   INTEGER NOT NULL DEFAULT 0,
    placed_at       TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    message         TEXT    NOT NULL DEFAULT '',
    protective_id   TEXT    NOT NULL DEFAULT '',
    paper           INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status, paper);
CREATE INDEX IF NOT EXISTS idx_orders_symbol ON orders(exchange, symbol);

-- Closed round trips. `key` is caller-supplied and unique per matched lot
-- (e.g. "<exit_order_id>:<lot_index>") so re-running a reconciler over the
-- same broker fills is idempotent.
CREATE TABLE IF NOT EXISTS trades (
    key         TEXT    PRIMARY KEY,
    run_id      INTEGER,
    exchange    TEXT    NOT NULL,
    symbol      TEXT    NOT NULL,
    strategy    TEXT    NOT NULL DEFAULT '',
    side        TEXT    NOT NULL,
    quantity    INTEGER NOT NULL,
    entry_price INTEGER NOT NULL,
    exit_price  INTEGER NOT NULL,
    entry_at    TEXT    NOT NULL,
    exit_at     TEXT    NOT NULL,
    gross_pnl   INTEGER NOT NULL,
    charges     INTEGER NOT NULL DEFAULT 0,
    net_pnl     INTEGER NOT NULL,
    exit_reason TEXT    NOT NULL,
    paper       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_trades_exit ON trades(exit_at, paper);
CREATE INDEX IF NOT EXISTS idx_trades_run  ON trades(run_id);

CREATE TABLE IF NOT EXISTS equity_snapshots (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         INTEGER,
    at             TEXT    NOT NULL,
    balance        INTEGER NOT NULL,
    realized_pnl   INTEGER NOT NULL DEFAULT 0,
    unrealized_pnl INTEGER NOT NULL DEFAULT 0,
    open_positions INTEGER NOT NULL DEFAULT 0,
    paper          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_equity_at ON equity_snapshots(at, paper);

-- A named execution: a live session, a sync pass, or a backtest. Gives every
-- other table a foreign key to "which run produced this".
CREATE TABLE IF NOT EXISTS runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,            -- 'live', 'paper', 'backtest', 'sync'
    name       TEXT NOT NULL DEFAULT '',
    params     TEXT NOT NULL DEFAULT '{}',
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    status     TEXT NOT NULL DEFAULT 'running',
    message    TEXT NOT NULL DEFAULT ''
);

-- Statutory and broker charge rates, effective-dated so a historical backtest
-- prices trades with the schedule that was actually in force.
CREATE TABLE IF NOT EXISTS charge_rates (
    broker         TEXT    NOT NULL,     -- 'zerodha', 'upstox', ...
    segment        TEXT    NOT NULL,     -- 'equity_delivery', 'equity_intraday', 'futures', 'options'
    kind           TEXT    NOT NULL,     -- 'stt_buy', 'stt_sell', 'exchange', 'sebi', 'stamp', 'gst', 'dp', 'brokerage'
    effective_from TEXT    NOT NULL,
    rate           REAL    NOT NULL,     -- fraction of turnover, or a flat amount in minor units
    flat           INTEGER NOT NULL DEFAULT 0,   -- 1 when `rate` is a flat per-order amount
    PRIMARY KEY (broker, segment, kind, effective_from)
);

-- Opaque JSON state, keyed by string. Replaces the ad-hoc kv_store tables.
CREATE TABLE IF NOT EXISTS kv_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Applied migrations. The runner refuses to re-apply a recorded version.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
