// Package store keeps the state in SQLite (ncruces driver, no cgo): the in-memory watch.State is loaded at start,
// and after each poll only the rows that differ between the last saved state and the new one are written, in one
// transaction.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/trick77/ship/kit/migrate"
	"github.com/trick77/ship/kit/sqlitex"

	"github.com/trick77/ovh-restock/internal/predict"
	"github.com/trick77/ovh-restock/internal/watch"

	// registers the "sqlite3" database/sql driver; the SQLite build comes with it.
	_ "github.com/ncruces/go-sqlite3/driver"
)

// FileName is the database in the data directory. Never the product's name: a rename would start a deployment on
// an empty database next to its data.
const FileName = "app.db"

//go:embed migrations/*.sql
var migrationsFS embed.FS

func migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time
	}
	return sub
}

// Store is the database.
type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) dir's database, applies pending migrations and imports the JSON files of
// releases before 0.4 once.
func Open(ctx context.Context, dir string) (*Store, error) {
	path := filepath.Join(dir, FileName)
	// Migrations run on a pool of their own, closed before the app's opens (trakk's pattern): a failed
	// no-transaction file must not hand its connection state to the app.
	mdb, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	_, err = migrate.Up(ctx, mdb, migrations(), migrate.Options{Dialect: migrate.SQLite})
	_ = mdb.Close()
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{DB: db}
	if err := s.importLegacy(ctx, dir); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("import: %w", err)
	}
	return s, nil
}

// dsn is kit's DSN plus _txlock=immediate (a write transaction takes the lock at BEGIN: the poll and the predictions
// write from two goroutines) and temp_store in memory: the container's root is read-only.
func dsn(path string) string {
	return sqlitex.DSN(path, "_pragma=temp_store(memory)", "_txlock=immediate")
}

// Close closes the pool.
func (s *Store) Close() error { return s.DB.Close() }

func unix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromUnix(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

// Load reads the whole state.
func (s *Store) Load(ctx context.Context) (watch.State, error) {
	var st watch.State
	var since, all, polled, digest sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT since, all_since, polled_at, digest_at FROM meta WHERE id = 1`).
		Scan(&since, &all, &polled, &digest)
	if err != nil && err != sql.ErrNoRows {
		return st, err
	}
	st.Since, st.AllSince, st.PolledAt, st.DigestAt = fromUnix(since), fromUnix(all), fromUnix(polled), fromUnix(digest)
	// NULL to/last: open at the last poll.
	open := func(n sql.NullInt64) time.Time {
		if !n.Valid {
			return st.PolledAt
		}
		return fromUnix(n)
	}

	if err := each(ctx, s.DB, `SELECT from_at, to_at FROM polled ORDER BY from_at`, func(r *sql.Rows) error {
		var from, to sql.NullInt64
		if err := r.Scan(&from, &to); err != nil {
			return err
		}
		st.Polled = append(st.Polled, watch.Span{From: fromUnix(from), To: open(to)})
		return nil
	}); err != nil {
		return st, err
	}

	st.Seen = map[string]watch.Seen{}
	if err := each(ctx, s.DB, `SELECT fqn, since, last, in_stock FROM seen`, func(r *sql.Rows) error {
		var fqn string
		var since, last sql.NullInt64
		var in bool
		if err := r.Scan(&fqn, &since, &last, &in); err != nil {
			return err
		}
		st.Seen[fqn] = watch.Seen{Since: fromUnix(since), Last: open(last), In: in}
		return nil
	}); err != nil {
		return st, err
	}

	st.Spans = map[string]map[string][]watch.Span{}
	if err := each(ctx, s.DB, `SELECT fqn, dc, from_at, slow, to_at, gone_at, quiet FROM span ORDER BY fqn, dc, from_at, to_at IS NULL, to_at`,
		func(r *sql.Rows) error {
			var fqn, dc string
			var from, to, gone sql.NullInt64
			var sp watch.Span
			if err := r.Scan(&fqn, &dc, &from, &sp.Slow, &to, &gone, &sp.Quiet); err != nil {
				return err
			}
			sp.From, sp.To, sp.Gone = fromUnix(from), open(to), fromUnix(gone)
			if st.Spans[fqn] == nil {
				st.Spans[fqn] = map[string][]watch.Span{}
			}
			st.Spans[fqn][dc] = append(st.Spans[fqn][dc], sp)
			return nil
		}); err != nil {
		return st, err
	}

	st.Prices = map[string][]watch.Price{}
	if err := each(ctx, s.DB, `SELECT fqn, at, rent, setup FROM price ORDER BY fqn, at`, func(r *sql.Rows) error {
		var fqn string
		var at sql.NullInt64
		var p watch.Price
		if err := r.Scan(&fqn, &at, &p.Rent, &p.Setup); err != nil {
			return err
		}
		p.At = fromUnix(at)
		st.Prices[fqn] = append(st.Prices[fqn], p)
		return nil
	}); err != nil {
		return st, err
	}

	if err := each(ctx, s.DB, `SELECT at, kind, title, body, delivery, retry, items FROM event ORDER BY at DESC, kind`,
		func(r *sql.Rows) error {
			var at sql.NullInt64
			var items sql.NullString
			var e watch.Event
			if err := r.Scan(&at, &e.Kind, &e.Title, &e.Body, &e.Delivery, &e.Retry, &items); err != nil {
				return err
			}
			e.At = fromUnix(at)
			if items.Valid {
				if err := json.Unmarshal([]byte(items.String), &e.Items); err != nil {
					return fmt.Errorf("event %d items: %w", at.Int64, err)
				}
			}
			st.Events = append(st.Events, e)
			return nil
		}); err != nil {
		return st, err
	}

	if err := each(ctx, s.DB, `SELECT pair FROM notified ORDER BY pair`, func(r *sql.Rows) error {
		var k string
		if err := r.Scan(&k); err != nil {
			return err
		}
		st.Notified = append(st.Notified, k)
		return nil
	}); err != nil {
		return st, err
	}
	return st, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func each(ctx context.Context, q querier, query string, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Save writes what differs between prev (what the database holds) and next, in one transaction. On an error
// nothing is written: the caller keeps prev, and the next Save carries this one's changes too.
func (s *Store) Save(ctx context.Context, prev, next watch.State) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := save(ctx, tx, prev, next); err != nil {
		return err
	}
	return tx.Commit()
}

// nullable is a time as a row holds it: NULL while it equals the last poll (open).
type nullable struct {
	t    int64
	open bool
}

func cell(t, polledAt time.Time, open bool) nullable {
	if open && t.Equal(polledAt) {
		return nullable{open: true}
	}
	return nullable{t: t.Unix()}
}

func (n nullable) arg() any {
	if n.open {
		return nil
	}
	return n.t
}

type spanKey struct {
	from int64
	slow bool
}

type spanRow struct {
	to    nullable
	gone  int64
	quiet bool
}

func spanRows(sp []watch.Span, polledAt time.Time) map[spanKey]spanRow {
	out := make(map[spanKey]spanRow, len(sp))
	for _, x := range sp {
		var gone int64
		if !x.Gone.IsZero() {
			gone = x.Gone.Unix()
		}
		out[spanKey{x.From.Unix(), x.Slow}] = spanRow{to: cell(x.To, polledAt, x.Gone.IsZero()), gone: gone, quiet: x.Quiet}
	}
	return out
}

// lastOpen tells whether sp's newest span is open at polledAt: its row's to is NULL then.
func lastOpen(sp []watch.Span, polledAt time.Time) bool {
	n := len(sp)
	return n > 0 && sp[n-1].Gone.IsZero() && sp[n-1].To.Equal(polledAt)
}

func save(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (id, since, all_since, polled_at, digest_at) VALUES (1, ?, ?, ?, ?)`,
		unix(next.Since), unix(next.AllSince), unix(next.PolledAt), unix(next.DigestAt)); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := savePolled(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("polled: %w", err)
	}
	if err := saveSeen(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("seen: %w", err)
	}
	if err := saveSpans(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("span: %w", err)
	}
	if err := savePrices(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("price: %w", err)
	}
	if err := saveEvents(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := saveNotified(ctx, tx, prev, next); err != nil {
		return fmt.Errorf("notified: %w", err)
	}
	return nil
}

func savePolled(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	rows := func(st watch.State) map[int64]nullable {
		out := map[int64]nullable{}
		for _, p := range st.Polled {
			out[p.From.Unix()] = cell(p.To, st.PolledAt, true)
		}
		return out
	}
	a, b := rows(prev), rows(next)
	for k := range a {
		if _, ok := b[k]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM polled WHERE from_at = ?`, k); err != nil {
				return err
			}
		}
	}
	for k, v := range b {
		if old, ok := a[k]; !ok || old != v {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO polled (from_at, to_at) VALUES (?, ?)`, k, v.arg()); err != nil {
				return err
			}
		}
	}
	return nil
}

type seenRow struct {
	since int64
	last  nullable
	in    bool
}

func saveSeen(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	row := func(e watch.Seen, polledAt time.Time) seenRow {
		return seenRow{since: e.Since.Unix(), last: cell(e.Last, polledAt, e.In), in: e.In}
	}
	var put, del *sql.Stmt
	for k := range prev.Seen {
		if _, ok := next.Seen[k]; ok {
			continue
		}
		if del == nil {
			var err error
			if del, err = tx.PrepareContext(ctx, `DELETE FROM seen WHERE fqn = ?`); err != nil {
				return err
			}
			defer func() { _ = del.Close() }()
		}
		if _, err := del.ExecContext(ctx, k); err != nil {
			return err
		}
	}
	for k, e := range next.Seen {
		v := row(e, next.PolledAt)
		if old, ok := prev.Seen[k]; ok && row(old, prev.PolledAt) == v {
			continue
		}
		if put == nil {
			var err error
			if put, err = tx.PrepareContext(ctx, `INSERT OR REPLACE INTO seen (fqn, since, last, in_stock) VALUES (?, ?, ?, ?)`); err != nil {
				return err
			}
			defer func() { _ = put.Close() }()
		}
		if _, err := put.ExecContext(ctx, k, v.since, v.last.arg(), v.in); err != nil {
			return err
		}
	}
	return nil
}

func saveSpans(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	var put, del *sql.Stmt
	prep := func(st **sql.Stmt, q string) error {
		if *st != nil {
			return nil
		}
		var err error
		*st, err = tx.PrepareContext(ctx, q)
		return err
	}
	defer func() {
		if put != nil {
			_ = put.Close()
		}
		if del != nil {
			_ = del.Close()
		}
	}()
	pair := func(fqn, dc string, a, b []watch.Span) error {
		// Most pairs didn't change: same spans, and the newest is open in both or in neither (an open span
		// extended by a poll is the same row, to NULL).
		if slices.Equal(a, b) && !lastOpen(a, prev.PolledAt) && !lastOpen(b, next.PolledAt) {
			return nil
		}
		ra, rb := spanRows(a, prev.PolledAt), spanRows(b, next.PolledAt)
		for k := range ra {
			if _, ok := rb[k]; ok {
				continue
			}
			if err := prep(&del, `DELETE FROM span WHERE fqn = ? AND dc = ? AND from_at = ? AND slow = ?`); err != nil {
				return err
			}
			if _, err := del.ExecContext(ctx, fqn, dc, k.from, k.slow); err != nil {
				return err
			}
		}
		for k, v := range rb {
			if old, ok := ra[k]; ok && old == v {
				continue
			}
			if err := prep(&put, `INSERT OR REPLACE INTO span (fqn, dc, from_at, slow, to_at, gone_at, quiet) VALUES (?, ?, ?, ?, ?, ?, ?)`); err != nil {
				return err
			}
			var gone any
			if v.gone != 0 {
				gone = v.gone
			}
			if _, err := put.ExecContext(ctx, fqn, dc, k.from, k.slow, v.to.arg(), gone, v.quiet); err != nil {
				return err
			}
		}
		return nil
	}
	for fqn, nm := range next.Spans {
		pm := prev.Spans[fqn]
		for dc, b := range nm {
			if err := pair(fqn, dc, pm[dc], b); err != nil {
				return err
			}
		}
		for dc, a := range pm {
			if _, ok := nm[dc]; !ok {
				if err := pair(fqn, dc, a, nil); err != nil {
					return err
				}
			}
		}
	}
	for fqn, pm := range prev.Spans {
		if _, ok := next.Spans[fqn]; ok {
			continue
		}
		for dc, a := range pm {
			if err := pair(fqn, dc, a, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func savePrices(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	rows := func(ps []watch.Price) map[int64]watch.Price {
		out := make(map[int64]watch.Price, len(ps))
		for _, p := range ps {
			out[p.At.Unix()] = p
		}
		return out
	}
	diff := func(fqn string, a, b []watch.Price) error {
		if slices.Equal(a, b) {
			return nil
		}
		ra, rb := rows(a), rows(b)
		for k := range ra {
			if _, ok := rb[k]; !ok {
				if _, err := tx.ExecContext(ctx, `DELETE FROM price WHERE fqn = ? AND at = ?`, fqn, k); err != nil {
					return err
				}
			}
		}
		for k, p := range rb {
			if old, ok := ra[k]; ok && old.Rent == p.Rent && old.Setup == p.Setup {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO price (fqn, at, rent, setup) VALUES (?, ?, ?, ?)`,
				fqn, k, p.Rent, p.Setup); err != nil {
				return err
			}
		}
		return nil
	}
	for fqn, b := range next.Prices {
		if err := diff(fqn, prev.Prices[fqn], b); err != nil {
			return err
		}
	}
	for fqn, a := range prev.Prices {
		if _, ok := next.Prices[fqn]; !ok {
			if err := diff(fqn, a, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

type eventKey struct {
	at   int64
	kind string
}

func saveEvents(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	rows := func(es []watch.Event) map[eventKey]watch.Event {
		out := make(map[eventKey]watch.Event, len(es))
		for _, e := range es {
			out[eventKey{e.At.Unix(), e.Kind}] = e
		}
		return out
	}
	a, b := rows(prev.Events), rows(next.Events)
	for k := range a {
		if _, ok := b[k]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM event WHERE at = ? AND kind = ?`, k.at, k.kind); err != nil {
				return err
			}
		}
	}
	for k, e := range b {
		if old, ok := a[k]; ok && reflect.DeepEqual(old, e) {
			continue
		}
		var items any
		if len(e.Items) > 0 {
			j, err := json.Marshal(e.Items)
			if err != nil {
				return err
			}
			items = string(j)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO event (at, kind, title, body, delivery, retry, items) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			k.at, k.kind, e.Title, e.Body, e.Delivery, e.Retry, items); err != nil {
			return err
		}
	}
	return nil
}

func saveNotified(ctx context.Context, tx *sql.Tx, prev, next watch.State) error {
	for _, k := range prev.Notified {
		if !slices.Contains(next.Notified, k) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM notified WHERE pair = ?`, k); err != nil {
				return err
			}
		}
	}
	for _, k := range next.Notified {
		if !slices.Contains(prev.Notified, k) {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO notified (pair) VALUES (?)`, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// Predictions are the kept predictions by fqn.
func (s *Store) Predictions(ctx context.Context) (map[string]predict.Prediction, error) {
	out := map[string]predict.Prediction{}
	err := each(ctx, s.DB, `SELECT fqn, body FROM prediction`, func(r *sql.Rows) error {
		var fqn, body string
		if err := r.Scan(&fqn, &body); err != nil {
			return err
		}
		var p predict.Prediction
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			return fmt.Errorf("prediction %s: %w", fqn, err)
		}
		out[fqn] = p
		return nil
	})
	return out, err
}

// SavePredictions writes put and deletes del, one transaction.
func (s *Store) SavePredictions(ctx context.Context, put map[string]predict.Prediction, del []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := savePredictions(ctx, tx, put, del); err != nil {
		return err
	}
	return tx.Commit()
}

func savePredictions(ctx context.Context, tx *sql.Tx, put map[string]predict.Prediction, del []string) error {
	for _, k := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM prediction WHERE fqn = ?`, k); err != nil {
			return err
		}
	}
	for k, p := range put {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO prediction (fqn, body) VALUES (?, ?)`, k, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// Hello is when each channel last took a start's test push, by URL hash.
func (s *Store) Hello(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := each(ctx, s.DB, `SELECT key, at FROM hello`, func(r *sql.Rows) error {
		var k string
		var at sql.NullInt64
		if err := r.Scan(&k, &at); err != nil {
			return err
		}
		out[k] = fromUnix(at)
		return nil
	})
	return out, err
}

// SaveHello replaces the test push marks with took.
func (s *Store) SaveHello(ctx context.Context, took map[string]time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveHello(ctx, tx, took); err != nil {
		return err
	}
	return tx.Commit()
}

func saveHello(ctx context.Context, tx *sql.Tx, took map[string]time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM hello`); err != nil {
		return err
	}
	for k, t := range took {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hello (key, at) VALUES (?, ?)`, k, t.Unix()); err != nil {
			return err
		}
	}
	return nil
}
