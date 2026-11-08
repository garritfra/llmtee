// Package keys manages proxy keys and the in-memory key cache used on the
// request path (the request path never queries the database).
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/garritfra/llmtee/internal/store"
)

// Prefix marks managed proxy keys.
const Prefix = "llmtee-"

// Kinds.
const (
	Managed     = "managed"
	Passthrough = "passthrough"
)

// Hash returns the hex SHA-256 of a secret.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// ID derives the stable key id logged with each request from a hash.
func ID(kind, hash string) string {
	if kind == Managed {
		return "mk-" + hash[:12]
	}
	return "pt-" + hash[:12]
}

// IsManaged reports whether a bearer token is a llmtee proxy key.
func IsManaged(token string) bool { return strings.HasPrefix(token, Prefix) }

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewSecret generates a new managed proxy key.
func NewSecret() string {
	b := make([]byte, 32)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return Prefix + string(b)
}

// New builds a key row for a secret.
func New(kind, name, secret string, day, month *float64) *store.Key {
	h := Hash(secret)
	return &store.Key{ID: ID(kind, h), Name: name, Hash: h, Kind: kind,
		BudgetDayUSD: day, BudgetMonthUSD: month, Created: time.Now().UnixMilli()}
}

// Cache is a snapshot of all keys, indexed by hash and id.
type Cache struct {
	mu     sync.RWMutex
	byHash map[string]*store.Key
	byID   map[string]*store.Key
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{byHash: map[string]*store.Key{}, byID: map[string]*store.Key{}}
}

// Reload replaces the cache content from the store.
func (c *Cache) Reload(ctx context.Context, s store.Store) error {
	ks, err := s.ListKeys(ctx)
	if err != nil {
		return err
	}
	c.Set(ks)
	return nil
}

// Set replaces the cache content.
func (c *Cache) Set(ks []*store.Key) {
	bh := make(map[string]*store.Key, len(ks))
	bi := make(map[string]*store.Key, len(ks))
	for _, k := range ks {
		bh[k.Hash] = k
		bi[k.ID] = k
	}
	c.mu.Lock()
	c.byHash, c.byID = bh, bi
	c.mu.Unlock()
}

// ByHash looks up a key by secret hash.
func (c *Cache) ByHash(h string) (*store.Key, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.byHash[h]
	return k, ok
}

// ByID looks up a key by id.
func (c *Cache) ByID(id string) (*store.Key, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.byID[id]
	return k, ok
}

// Name returns a display name for a key id.
func (c *Cache) Name(id string) string {
	if k, ok := c.ByID(id); ok {
		return k.Name
	}
	return ""
}
