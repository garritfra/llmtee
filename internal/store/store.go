// Package store persists request logs and keys in SQLite (default) or
// PostgreSQL behind one narrow interface, using handwritten SQL.
package store

import (
	"context"
	"errors"
	"time"
)

// Request is one logged proxy request.
type Request struct {
	ID               string
	TS               int64 // unix ms
	Upstream         string
	Path             string
	Model            string
	KeyID            string
	UserID           string
	SessionID        string
	Status           int
	Streamed         bool
	TokensIn         *int64
	TokensOut        *int64
	TokensCacheRead  *int64
	TokensCacheWrite *int64
	CostUSD          *float64
	LatencyMS        int64
	TTFTMS           *int64
	Error            string
	Properties       map[string]string

	// Bodies are only set when body logging is enabled (Insert) or
	// explicitly requested (GetRequest). Stored zstd-compressed.
	ReqBody  []byte
	RespBody []byte
	HasBody  bool
}

// Time returns the request timestamp.
func (r *Request) Time() time.Time { return time.UnixMilli(r.TS) }

// Filter selects requests.
type Filter struct {
	Since, Until int64 // unix ms, 0 = open
	Upstream     string
	Model        string
	KeyID        string
	UserID       string
	SessionID    string
	Status       string // "", "ok", "error"
	PropName     string
	PropValue    string
	Before       string // cursor: only ids < Before
	Limit        int
}

// Group dimensions for spend aggregation.
const (
	ByNone     = ""
	ByDay      = "day"
	ByModel    = "model"
	ByKey      = "key"
	ByUser     = "user"
	ByUpstream = "upstream"
	BySession  = "session"
)

// SpendRow is one aggregation bucket.
type SpendRow struct {
	Group     string
	Requests  int64
	CostUSD   float64
	TokensIn  int64
	TokensOut int64
	CacheRead int64
	Unpriced  int64 // requests with known usage but no known price
	Errors    int64
}

// SeriesRow is the spend of one group in one time bucket.
type SeriesRow struct {
	Start    int64 // bucket start, unix ms
	Group    string
	Other    bool // merged groups that were not in the keep list
	Requests int64
	CostUSD  float64
}

// Key is a proxy key or a labeled passthrough key.
type Key struct {
	ID             string
	Name           string
	Hash           string
	Kind           string // "managed" or "passthrough"
	BudgetDayUSD   *float64
	BudgetMonthUSD *float64
	Created        int64
}

// KeySpend is the spend of one key in the current day and month.
type KeySpend struct {
	Day, Month float64
}

// ErrNotFound is returned for missing rows.
var ErrNotFound = errors.New("not found")

// Store is the storage interface shared by both backends.
type Store interface {
	Insert(ctx context.Context, reqs []*Request) error
	ListRequests(ctx context.Context, f Filter) ([]*Request, error)
	GetRequest(ctx context.Context, id string, withBodies bool) (*Request, error)
	Spend(ctx context.Context, f Filter, groupBy string) ([]SpendRow, error)
	// Series buckets spend by time (buckets of bucketMS, aligned to the
	// Unix epoch) and optionally by a dimension. With keep set, groups not
	// in keep are merged into one row per bucket with Other set.
	Series(ctx context.Context, f Filter, bucketMS int64, groupBy string, keep []string) ([]SeriesRow, error)
	BudgetSpend(ctx context.Context, dayStart, monthStart int64) (map[string]KeySpend, KeySpend, error)
	Distinct(ctx context.Context, column string, since int64) ([]string, error)

	ListKeys(ctx context.Context) ([]*Key, error)
	CreateKey(ctx context.Context, k *Key) error
	UpdateKey(ctx context.Context, k *Key) error
	DeleteKey(ctx context.Context, id string) error

	Retention(ctx context.Context, bodiesBefore, requestsBefore int64) (int64, error)
	Ping(ctx context.Context) error
	Dialect() string
	Close() error
}
