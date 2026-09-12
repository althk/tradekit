-- tradekit shared schema, version 2: corporate actions.
--
-- This is a gap in the codebase, not a duplication. Every project except one
-- backtests on unadjusted series, which means a 1:10 split reads as a 90%
-- single-day crash -- firing stops, triggering breakout signals, and quietly
-- corrupting every statistic computed over a long lookback.
--
-- It is a library migration (1-499) rather than a project one because both
-- languages read it and because an adjusted series must be identical whichever
-- library computed it. See go/marketdata/reference/corpactions.go.

CREATE TABLE IF NOT EXISTS corporate_actions (
    exchange TEXT    NOT NULL,
    symbol   TEXT    NOT NULL,
    ex_date  TEXT    NOT NULL,          -- '2006-01-02'; the first day trading
                                        -- WITHOUT the entitlement
    kind     TEXT    NOT NULL,          -- 'split', 'bonus', 'dividend'
    -- The factor by which the share count multiplies: 10 for a 1:10 split,
    -- 2 for a 1:1 bonus, 1 for a dividend. Prices before the ex-date are
    -- divided by it and volumes multiplied.
    --
    -- REAL rather than an integer pair because a ratio is genuinely
    -- fractional -- a 3:2 bonus is 2.5 -- and because it is only ever used to
    -- scale a price, never to hold one. Money stays INTEGER everywhere.
    ratio    REAL    NOT NULL DEFAULT 1,
    -- Dividend amount per share, in minor units. Zero for splits and bonuses.
    amount   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (exchange, symbol, ex_date, kind),
    FOREIGN KEY (exchange, symbol) REFERENCES instruments(exchange, symbol)
);

CREATE INDEX IF NOT EXISTS idx_corporate_actions_date
    ON corporate_actions(ex_date);
