-- tradekit shared schema, version 4: one canonical timestamp form.
--
-- Timestamps are compared as strings in every range query, which is only
-- correct when every row is written in the same zone at the same width. Until
-- now Go wrote RFC 3339 in the caller's zone with trailing zeros trimmed and
-- Python wrote isoformat() with "+00:00" and six digits, so "...T10:00:00Z"
-- sorted before "...T15:15:00+05:30" although it is the later instant, and
-- one bar could be stored twice under two spellings.
--
-- From this version both libraries write UTC, microsecond precision, fixed
-- width: 2006-01-02T15:04:05.000000Z. This migration rewrites what is already
-- stored. SQLite's own date functions do the conversion (they understand both
-- offset forms); they keep milliseconds, so sub-millisecond digits of existing
-- rows are dropped, which no bar start and no order time depends on. A value
-- the date functions cannot read is left as it was.

-- canonical(x) is spelled out per column below:
--   CASE WHEN x IS NULL OR x = '' THEN x
--        ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', x) || '000Z', x) END

-- Candles are keyed by start, so the same instant stored under two spellings
-- collides once both are canonical. The first spelling converted wins; the
-- duplicate it collides with is removed, keeping the bar that was stored first
-- in the same sense UpsertCandles keeps the first value read.
UPDATE OR IGNORE candles SET start =
    CASE WHEN start IS NULL OR start = '' THEN start
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', start) || '000Z', start) END;
DELETE FROM candles
 WHERE start NOT LIKE '____-__-__T__:__:__.______Z'
   AND EXISTS (
       SELECT 1 FROM candles AS c
        WHERE c.exchange = candles.exchange AND c.symbol = candles.symbol
          AND c.timeframe = candles.timeframe
          AND c.start = strftime('%Y-%m-%dT%H:%M:%f', candles.start) || '000Z');

UPDATE instruments SET updated_at =
    CASE WHEN updated_at IS NULL OR updated_at = '' THEN updated_at
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', updated_at) || '000Z', updated_at) END;

UPDATE signals SET at =
    CASE WHEN at IS NULL OR at = '' THEN at
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', at) || '000Z', at) END;

UPDATE orders SET
    placed_at = CASE WHEN placed_at IS NULL OR placed_at = '' THEN placed_at
                     ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', placed_at) || '000Z', placed_at) END,
    updated_at = CASE WHEN updated_at IS NULL OR updated_at = '' THEN updated_at
                      ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', updated_at) || '000Z', updated_at) END;

UPDATE trades SET
    entry_at = CASE WHEN entry_at IS NULL OR entry_at = '' THEN entry_at
                    ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', entry_at) || '000Z', entry_at) END,
    exit_at = CASE WHEN exit_at IS NULL OR exit_at = '' THEN exit_at
                   ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', exit_at) || '000Z', exit_at) END;

UPDATE equity_snapshots SET at =
    CASE WHEN at IS NULL OR at = '' THEN at
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', at) || '000Z', at) END;

UPDATE runs SET
    started_at = CASE WHEN started_at IS NULL OR started_at = '' THEN started_at
                      ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', started_at) || '000Z', started_at) END,
    ended_at = CASE WHEN ended_at IS NULL OR ended_at = '' THEN ended_at
                    ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', ended_at) || '000Z', ended_at) END;

UPDATE kv_state SET updated_at =
    CASE WHEN updated_at IS NULL OR updated_at = '' THEN updated_at
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', updated_at) || '000Z', updated_at) END;

UPDATE decisions SET at =
    CASE WHEN at IS NULL OR at = '' THEN at
         ELSE COALESCE(strftime('%Y-%m-%dT%H:%M:%f', at) || '000Z', at) END;

-- The recent-orders and pending-orders queries filter on paper and order by
-- placed_at; idx_orders_status (status, paper) serves neither.
CREATE INDEX IF NOT EXISTS idx_orders_paper_placed ON orders(paper, placed_at);
