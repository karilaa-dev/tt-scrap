package core

import (
	"container/heap"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

type cacheItem[T any] struct {
	key         string
	value       T
	expires     time.Time
	recency     *list.Element
	expiryIndex int
}
type Cache[T any] struct {
	mu       sync.Mutex
	entries  map[string]*cacheItem[T]
	order    *list.List
	expiry   expiryHeap[T]
	capacity int
	ttl      time.Duration
}

func NewCache[T any](ttl time.Duration, capacity int) *Cache[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Cache[T]{entries: make(map[string]*cacheItem[T]), order: list.New(), capacity: capacity, ttl: ttl}
}
func (c *Cache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	entry, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !entry.expires.After(time.Now()) {
		c.remove(entry)
		return zero, false
	}
	c.order.MoveToFront(entry.recency)
	return entry.value, true
}
func (c *Cache[T]) Set(key string, value T) {
	c.SetUntil(key, value, time.Now().Add(c.ttl))
}
func (c *Cache[T]) SetUntil(key string, value T, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for len(c.expiry) > 0 && !c.expiry[0].expires.After(now) {
		c.remove(c.expiry[0])
	}
	if !expires.After(now) {
		if entry := c.entries[key]; entry != nil {
			c.remove(entry)
		}
		return
	}
	if entry := c.entries[key]; entry != nil {
		entry.value, entry.expires = value, expires
		c.order.MoveToFront(entry.recency)
		heap.Fix(&c.expiry, entry.expiryIndex)
		return
	}
	if len(c.entries) >= c.capacity {
		c.remove(c.order.Back().Value.(*cacheItem[T]))
	}
	entry := &cacheItem[T]{key: key, value: value, expires: expires}
	entry.recency = c.order.PushFront(entry)
	c.entries[key] = entry
	heap.Push(&c.expiry, entry)
}
func (c *Cache[T]) remove(entry *cacheItem[T]) {
	heap.Remove(&c.expiry, entry.expiryIndex)
	c.order.Remove(entry.recency)
	delete(c.entries, entry.key)
}
func (c *Cache[T]) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		c.remove(entry)
	}
}
func (c *Cache[T]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.order.Init()
	clear(c.expiry)
	c.expiry = c.expiry[:0]
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	var out [36]byte
	hex.Encode(out[:8], b[:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

type Assets struct {
	cache *Cache[AssetContext]
	ttl   time.Duration
}

func NewAssets(ttl time.Duration, capacity int) *Assets {
	return &Assets{cache: NewCache[AssetContext](ttl, capacity), ttl: ttl}
}
func (a *Assets) Register(value AssetContext, position int) AssetDescriptor {
	return a.RegisterUntil(value, position, time.Now().Add(a.ttl))
}
func (a *Assets) RegisterUntil(value AssetContext, position int, expires time.Time) AssetDescriptor {
	id := NewID()
	a.cache.SetUntil(id, value, expires)
	var mime *string
	if value.ContentType != "" {
		mime = Ptr(value.ContentType)
	}
	return AssetDescriptor{AssetID: id, Kind: value.Kind, Position: position, DownloadURL: "/v1/assets/" + id, Filename: value.Filename, ContentType: mime, ExpiresAt: expires.UTC().Format(time.RFC3339Nano)}
}
func (a *Assets) GetAsset(id string) (AssetContext, error) {
	value, ok := a.cache.Get(id)
	if !ok {
		return AssetContext{}, MissingAsset()
	}
	return value, nil
}
func (a *Assets) Clear() { a.cache.Clear() }

type Limiter struct {
	slots   chan struct{}
	waiting atomic.Int64
	limit   int64
}

func NewLimiter(capacity, maxQueue int) *Limiter {
	if capacity < 1 {
		capacity = 1
	}
	if maxQueue < 1 {
		maxQueue = capacity * 8
	}
	return &Limiter{slots: make(chan struct{}, capacity), limit: int64(maxQueue)}
}
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
		return l.release(), nil
	default:
	}
	if l.waiting.Add(1) > l.limit {
		l.waiting.Add(-1)
		return nil, NewError("service_busy", "The request queue is full", 503)
	}
	defer l.waiting.Add(-1)
	select {
	case l.slots <- struct{}{}:
		return l.release(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l *Limiter) release() func() {
	var once sync.Once
	return func() { once.Do(func() { <-l.slots }) }
}
