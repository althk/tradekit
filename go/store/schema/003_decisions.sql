-- tradekit shared schema, version 3: the decision journal.
--
-- A log line is prose; a journal entry is a row you can query alongside the
-- trade it produced. This is the table that answers "why did it do that" three
-- weeks later, and "why did nothing happen today", which is the harder
-- question -- rejections are recorded as deliberately as actions.
--
-- Library migration (1-499): a Go project and a Python project pointed at the
-- same database must write rows the other can read. See go/harness/journal.go
-- and py/src/tradekit/harness/journal.py.

CREATE TABLE IF NOT EXISTS decisions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER NOT NULL,
    at          TEXT    NOT NULL,           -- ISO-8601 with offset
    exchange    TEXT    NOT NULL,
    symbol      TEXT    NOT NULL,
    action      TEXT    NOT NULL,           -- 'enter', 'skip', 'exit', 'resize', ...
    reason      TEXT    NOT NULL DEFAULT '', -- risk.ErrBlocked's reason, or the strategy's
    -- Free-form context, as JSON with sorted keys so the same decision
    -- written by either language produces the same bytes.
    detail_json TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_decisions_run_at
    ON decisions(run_id, at);
