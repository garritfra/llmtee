package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	"github.com/klauspost/compress/zstd"
	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // sqlite driver (CGO-free)
)

//go:embed migrations
var migrations embed.FS

// Dialects.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

type sqlStore struct {
	db      *sql.DB
	dialect string
	enc     *zstd.Encoder
	dec     *zstd.Decoder
}

// Open connects to the database given by url and runs migrations.
//
//	""                      -> sqlite file llmtee.db
//	sqlite:///data/x.db     -> sqlite file /data/x.db
//	sqlite://x.db, file:x.db, x.db
//	postgres://... postgresql://...
func Open(ctx context.Context, url string) (Store, error) {
	dialect, dsn, err := parseURL(url)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	switch dialect {
	case SQLite:
		if dir := filepath.Dir(dsn); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create data dir: %w", err)
			}
		}
		db, err = sql.Open("sqlite", "file:"+dsn+
			"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate")
		if err == nil {
			db.SetMaxOpenConns(8)
		}
	case Postgres:
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			db.SetMaxOpenConns(16)
			db.SetConnMaxIdleTime(5 * time.Minute)
		}
	}
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s: %w", dialect, err)
	}
	if err := migrate(pctx, db, dialect); err != nil {
		db.Close()
		return nil, err
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	return &sqlStore{db: db, dialect: dialect, enc: enc, dec: dec}, nil
}

func parseURL(url string) (dialect, dsn string, err error) {
	switch {
	case url == "":
		return SQLite, "llmtee.db", nil
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		return Postgres, url, nil
	case strings.HasPrefix(url, "sqlite://"):
		return SQLite, strings.TrimPrefix(url, "sqlite://"), nil
	case strings.HasPrefix(url, "sqlite:"):
		return SQLite, strings.TrimPrefix(url, "sqlite:"), nil
	case strings.HasPrefix(url, "file:"):
		return SQLite, strings.TrimPrefix(url, "file:"), nil
	case !strings.Contains(url, "://"):
		return SQLite, url, nil
	}
	return "", "", fmt.Errorf("unsupported DATABASE_URL %q (use sqlite://path or postgres://...)", url)
}

func migrate(ctx context.Context, db *sql.DB, dialect string) error {
	sub, err := fs.Sub(migrations, "migrations/"+dialect)
	if err != nil {
		return err
	}
	gd := goose.DialectSQLite3
	if dialect == Postgres {
		gd = goose.DialectPostgres
	}
	p, err := goose.NewProvider(gd, db, sub)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

func (s *sqlStore) Dialect() string { return s.dialect }
func (s *sqlStore) Close() error    { return s.db.Close() }
func (s *sqlStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// q rewrites ? placeholders to $n for PostgreSQL.
func (s *sqlStore) q(query string) string {
	if s.dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func ns(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *sqlStore) Insert(ctx context.Context, reqs []*Request) error {
	if len(reqs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insReq, err := tx.PrepareContext(ctx, s.q(`INSERT INTO requests
		(id, ts, upstream, path, model, key_id, user_id, session_id, status, streamed,
		 tokens_in, tokens_out, tokens_cache_read, tokens_cache_write, cost_usd, latency_ms, ttft_ms, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`))
	if err != nil {
		return err
	}
	defer insReq.Close()
	var insProp, insBody *sql.Stmt
	for _, r := range reqs {
		if _, err := insReq.ExecContext(ctx, r.ID, r.TS, r.Upstream, ns(r.Path), ns(r.Model), ns(r.KeyID),
			ns(r.UserID), ns(r.SessionID), r.Status, b2i(r.Streamed), r.TokensIn, r.TokensOut,
			r.TokensCacheRead, r.TokensCacheWrite, r.CostUSD, r.LatencyMS, r.TTFTMS, ns(r.Error)); err != nil {
			return err
		}
		for k, v := range r.Properties {
			if insProp == nil {
				if insProp, err = tx.PrepareContext(ctx, s.q(`INSERT INTO request_properties (request_id, name, value) VALUES (?,?,?)`)); err != nil {
					return err
				}
				defer insProp.Close()
			}
			if _, err := insProp.ExecContext(ctx, r.ID, k, v); err != nil {
				return err
			}
		}
		if r.ReqBody != nil || r.RespBody != nil {
			if insBody == nil {
				if insBody, err = tx.PrepareContext(ctx, s.q(`INSERT INTO request_bodies (request_id, req, resp) VALUES (?,?,?)`)); err != nil {
					return err
				}
				defer insBody.Close()
			}
			if _, err := insBody.ExecContext(ctx, r.ID, s.compress(r.ReqBody), s.compress(r.RespBody)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *sqlStore) compress(b []byte) []byte {
	if b == nil {
		return nil
	}
	return s.enc.EncodeAll(b, make([]byte, 0, len(b)/3))
}

func (s *sqlStore) decompress(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out, err := s.dec.DecodeAll(b, nil)
	if err != nil {
		return []byte(fmt.Sprintf("<corrupt body: %v>", err))
	}
	return out
}

// where builds the WHERE clause for a filter.
func where(f Filter) (string, []any) {
	var c []string
	var a []any
	add := func(cond string, args ...any) { c = append(c, cond); a = append(a, args...) }
	if f.Since > 0 {
		add("ts >= ?", f.Since)
	}
	if f.Until > 0 {
		add("ts < ?", f.Until)
	}
	if f.Upstream != "" {
		add("upstream = ?", f.Upstream)
	}
	if f.Model != "" {
		add("model = ?", f.Model)
	}
	if f.KeyID != "" {
		add("key_id = ?", f.KeyID)
	}
	if f.UserID != "" {
		add("user_id = ?", f.UserID)
	}
	if f.SessionID != "" {
		add("session_id = ?", f.SessionID)
	}
	switch f.Status {
	case "ok":
		add("status < 400 AND error IS NULL")
	case "error":
		add("(status >= 400 OR error IS NOT NULL)")
	}
	if f.PropName != "" {
		if f.PropValue != "" {
			add("id IN (SELECT request_id FROM request_properties WHERE name = ? AND value = ?)", f.PropName, f.PropValue)
		} else {
			add("id IN (SELECT request_id FROM request_properties WHERE name = ?)", f.PropName)
		}
	}
	if f.Before != "" {
		add("id < ?", f.Before)
	}
	if len(c) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(c, " AND "), a
}

const requestCols = `id, ts, upstream, path, model, key_id, user_id, session_id, status, streamed,
	tokens_in, tokens_out, tokens_cache_read, tokens_cache_write, cost_usd, latency_ms, ttft_ms, error`

type scanner interface{ Scan(...any) error }

func scanRequest(sc scanner) (*Request, error) {
	var r Request
	var path, model, key, user, sess, errStr sql.NullString
	var status, streamed, latency sql.NullInt64
	var tin, tout, tcr, tcw, ttft sql.NullInt64
	var cost sql.NullFloat64
	if err := sc.Scan(&r.ID, &r.TS, &r.Upstream, &path, &model, &key, &user, &sess, &status, &streamed,
		&tin, &tout, &tcr, &tcw, &cost, &latency, &ttft, &errStr); err != nil {
		return nil, err
	}
	r.Path, r.Model, r.KeyID, r.UserID, r.SessionID, r.Error = path.String, model.String, key.String, user.String, sess.String, errStr.String
	r.Status = int(status.Int64)
	r.Streamed = streamed.Int64 != 0
	r.LatencyMS = latency.Int64
	ip := func(n sql.NullInt64) *int64 {
		if !n.Valid {
			return nil
		}
		v := n.Int64
		return &v
	}
	r.TokensIn, r.TokensOut, r.TokensCacheRead, r.TokensCacheWrite, r.TTFTMS = ip(tin), ip(tout), ip(tcr), ip(tcw), ip(ttft)
	if cost.Valid {
		c := cost.Float64
		r.CostUSD = &c
	}
	return &r, nil
}

func (s *sqlStore) ListRequests(ctx context.Context, f Filter) ([]*Request, error) {
	if f.Limit <= 0 || f.Limit > 10000 {
		f.Limit = 100
	}
	w, args := where(f)
	rows, err := s.db.QueryContext(ctx, s.q("SELECT "+requestCols+" FROM requests"+w+" ORDER BY id DESC LIMIT ?"), append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Request
	byID := map[string]*Request{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	return out, s.loadProps(ctx, byID)
}

func (s *sqlStore) loadProps(ctx context.Context, byID map[string]*Request) error {
	ids := make([]any, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.db.QueryContext(ctx, s.q("SELECT request_id, name, value FROM request_properties WHERE request_id IN ("+ph+")"), ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var val sql.NullString
		if err := rows.Scan(&id, &name, &val); err != nil {
			return err
		}
		r := byID[id]
		if r.Properties == nil {
			r.Properties = map[string]string{}
		}
		r.Properties[name] = val.String
	}
	return rows.Err()
}

func (s *sqlStore) GetRequest(ctx context.Context, id string, withBodies bool) (*Request, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx, s.q("SELECT "+requestCols+" FROM requests WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadProps(ctx, map[string]*Request{r.ID: r}); err != nil {
		return nil, err
	}
	var req, resp []byte
	q := "SELECT req, resp FROM request_bodies WHERE request_id = ?"
	if !withBodies {
		q = "SELECT NULL, NULL FROM request_bodies WHERE request_id = ?"
	}
	err = s.db.QueryRowContext(ctx, s.q(q), id).Scan(&req, &resp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		r.HasBody = true
		if withBodies {
			r.ReqBody, r.RespBody = s.decompress(req), s.decompress(resp)
		}
	}
	return r, nil
}

var groupExpr = map[string]string{
	ByDay:      "ts / 86400000",
	ByModel:    "COALESCE(model, '')",
	ByKey:      "COALESCE(key_id, '')",
	ByUser:     "COALESCE(user_id, '')",
	ByUpstream: "upstream",
	BySession:  "COALESCE(session_id, '')",
}

func (s *sqlStore) Spend(ctx context.Context, f Filter, groupBy string) ([]SpendRow, error) {
	w, args := where(f)
	aggs := `COUNT(*), COALESCE(SUM(cost_usd), 0), COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0),
		COALESCE(SUM(tokens_cache_read), 0),
		COALESCE(SUM(CASE WHEN cost_usd IS NULL AND tokens_in IS NOT NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 400 OR error IS NOT NULL THEN 1 ELSE 0 END), 0)`
	var query string
	if groupBy == ByNone {
		query = "SELECT '', " + aggs + " FROM requests" + w
	} else {
		g, ok := groupExpr[groupBy]
		if !ok {
			return nil, fmt.Errorf("invalid group %q", groupBy)
		}
		order := "3 DESC"
		if groupBy == ByDay {
			order = "1"
		}
		query = "SELECT " + g + ", " + aggs + " FROM requests" + w + " GROUP BY " + g + " ORDER BY " + order
		if f.Limit > 0 {
			query += " LIMIT " + strconv.Itoa(f.Limit)
		}
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpendRow
	for rows.Next() {
		var r SpendRow
		var g any
		if err := rows.Scan(&g, &r.Requests, &r.CostUSD, &r.TokensIn, &r.TokensOut, &r.CacheRead, &r.Unpriced, &r.Errors); err != nil {
			return nil, err
		}
		switch v := g.(type) {
		case int64:
			if groupBy == ByDay {
				r.Group = time.UnixMilli(v * 86400000).UTC().Format("2006-01-02")
			} else {
				r.Group = strconv.FormatInt(v, 10)
			}
		case []byte:
			r.Group = string(v)
		case string:
			r.Group = v
		case nil:
		default:
			r.Group = fmt.Sprint(v)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqlStore) BudgetSpend(ctx context.Context, dayStart, monthStart int64) (map[string]KeySpend, KeySpend, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT COALESCE(key_id, ''),
		COALESCE(SUM(CASE WHEN ts >= ? THEN cost_usd ELSE 0 END), 0), COALESCE(SUM(cost_usd), 0)
		FROM requests WHERE ts >= ? GROUP BY COALESCE(key_id, '')`), dayStart, monthStart)
	if err != nil {
		return nil, KeySpend{}, err
	}
	defer rows.Close()
	out := map[string]KeySpend{}
	var total KeySpend
	for rows.Next() {
		var id string
		var ks KeySpend
		if err := rows.Scan(&id, &ks.Day, &ks.Month); err != nil {
			return nil, KeySpend{}, err
		}
		total.Day += ks.Day
		total.Month += ks.Month
		if id != "" {
			out[id] = ks
		}
	}
	return out, total, rows.Err()
}

var distinctCols = map[string]bool{"model": true, "upstream": true, "user_id": true, "key_id": true}

func (s *sqlStore) Distinct(ctx context.Context, column string, since int64) ([]string, error) {
	if !distinctCols[column] {
		return nil, fmt.Errorf("invalid column %q", column)
	}
	rows, err := s.db.QueryContext(ctx, s.q("SELECT DISTINCT "+column+" FROM requests WHERE ts >= ? AND "+column+" IS NOT NULL ORDER BY 1 LIMIT 500"), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *sqlStore) ListKeys(ctx context.Context) ([]*Key, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, hash, kind, budget_day_usd, budget_month_usd, created FROM keys ORDER BY created")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Key
	for rows.Next() {
		var k Key
		var bd, bm sql.NullFloat64
		if err := rows.Scan(&k.ID, &k.Name, &k.Hash, &k.Kind, &bd, &bm, &k.Created); err != nil {
			return nil, err
		}
		if bd.Valid {
			k.BudgetDayUSD = &bd.Float64
		}
		if bm.Valid {
			k.BudgetMonthUSD = &bm.Float64
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

func (s *sqlStore) CreateKey(ctx context.Context, k *Key) error {
	_, err := s.db.ExecContext(ctx, s.q("INSERT INTO keys (id, name, hash, kind, budget_day_usd, budget_month_usd, created) VALUES (?,?,?,?,?,?,?)"),
		k.ID, k.Name, k.Hash, k.Kind, k.BudgetDayUSD, k.BudgetMonthUSD, k.Created)
	return err
}

func (s *sqlStore) UpdateKey(ctx context.Context, k *Key) error {
	res, err := s.db.ExecContext(ctx, s.q("UPDATE keys SET name = ?, budget_day_usd = ?, budget_month_usd = ? WHERE id = ?"),
		k.Name, k.BudgetDayUSD, k.BudgetMonthUSD, k.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) DeleteKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q("DELETE FROM keys WHERE id = ?"), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// idBound returns the smallest ULID for a timestamp; ULIDs sort by time, so
// "id < idBound(ts)" selects requests older than ts without a join.
func idBound(ms int64) string {
	var u ulid.ULID
	_ = u.SetTime(uint64(ms))
	return u.String()
}

func (s *sqlStore) Retention(ctx context.Context, bodiesBefore, requestsBefore int64) (int64, error) {
	var deleted int64
	exec := func(q string, args ...any) error {
		res, err := s.db.ExecContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted += n
		return nil
	}
	if bodiesBefore > 0 {
		if err := exec("DELETE FROM request_bodies WHERE request_id < ?", idBound(bodiesBefore)); err != nil {
			return deleted, err
		}
	}
	if requestsBefore > 0 {
		b := idBound(requestsBefore)
		for _, q := range []string{
			"DELETE FROM request_bodies WHERE request_id < ?",
			"DELETE FROM request_properties WHERE request_id < ?",
			"DELETE FROM requests WHERE id < ?",
		} {
			if err := exec(q, b); err != nil {
				return deleted, err
			}
		}
	}
	if deleted > 10000 && s.dialect == SQLite {
		if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
			return deleted, fmt.Errorf("vacuum: %w", err)
		}
	}
	return deleted, nil
}

// compile-time check
var _ Store = (*sqlStore)(nil)
