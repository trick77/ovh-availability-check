-- The state, as internal/watch.State holds it in memory. Times are unix seconds, NULL for "none". Only changes are
-- written: an open span or polled stretch has to_at NULL ("until meta.polled_at"), a variant in stock seen.last NULL
-- (same), so a poll that changes no stock writes meta alone.

CREATE TABLE meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    since     INTEGER,
    all_since INTEGER,
    polled_at INTEGER,
    digest_at INTEGER
);

CREATE TABLE polled (
    from_at INTEGER PRIMARY KEY,
    to_at   INTEGER
);

CREATE TABLE seen (
    fqn      TEXT PRIMARY KEY,
    since    INTEGER NOT NULL,
    last     INTEGER,
    in_stock INTEGER NOT NULL
) WITHOUT ROWID;

-- slow is part of the key: a 1H↔72H switch starts its span at the old span's to, which one poll in equals its from.
CREATE TABLE span (
    fqn     TEXT    NOT NULL,
    dc      TEXT    NOT NULL,
    from_at INTEGER NOT NULL,
    slow    INTEGER NOT NULL,
    to_at   INTEGER,
    gone_at INTEGER,
    quiet   INTEGER NOT NULL,
    PRIMARY KEY (fqn, dc, from_at, slow)
) WITHOUT ROWID;

CREATE TABLE price (
    fqn   TEXT    NOT NULL,
    at    INTEGER NOT NULL,
    rent  REAL    NOT NULL,
    setup REAL    NOT NULL,
    PRIMARY KEY (fqn, at)
) WITHOUT ROWID;

-- items: a restock's servers as JSON (watch.Item), NULL on a digest or an imported push.
CREATE TABLE event (
    at       INTEGER NOT NULL,
    kind     TEXT    NOT NULL,
    title    TEXT    NOT NULL,
    body     TEXT    NOT NULL,
    delivery TEXT    NOT NULL,
    retry    INTEGER NOT NULL,
    items    TEXT,
    PRIMARY KEY (at, kind)
) WITHOUT ROWID;

CREATE TABLE notified (
    pair TEXT PRIMARY KEY
) WITHOUT ROWID;

-- body: predict.Prediction as JSON, read and written whole.
CREATE TABLE prediction (
    fqn  TEXT PRIMARY KEY,
    body TEXT NOT NULL
) WITHOUT ROWID;

-- When each notification channel last took a start's test push, by a hash of its URL (the URL holds secrets).
CREATE TABLE hello (
    key TEXT PRIMARY KEY,
    at  INTEGER NOT NULL
) WITHOUT ROWID;
