package platform

import (
	"context"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type flight[T any] struct {
	done     chan struct{}
	value    T
	err      error
	canceled bool
}
type memo[T any] struct {
	mu      sync.Mutex
	cache   *core.Cache[T]
	flights map[string]*flight[T]
	maximum int
	ttl     time.Duration
	expires func(T) time.Time
}

func newMemo[T any](ttl time.Duration, capacity, maximum int) *memo[T] {
	if ttl <= 0 {
		ttl = time.Minute
	}
	if maximum <= 0 {
		maximum = 8192
	}
	return &memo[T]{cache: core.NewCache[T](ttl, capacity), flights: make(map[string]*flight[T]), maximum: maximum, ttl: ttl}
}
func (m *memo[T]) get(key string) (T, bool) { return m.cache.Get(key) }
func (m *memo[T]) put(key string, value T) {
	expires := time.Now().Add(m.ttl)
	if m.expires != nil {
		if deadline := m.expires(value); deadline.Before(expires) {
			expires = deadline
		}
	}
	m.cache.SetUntil(key, value, expires)
}
func (m *memo[T]) do(ctx context.Context, key string, refresh bool, fn func() (T, error)) (T, error) {
	for {
		if err := ctx.Err(); err != nil {
			var zero T
			return zero, err
		}

		m.mu.Lock()
		if !refresh {
			if v, ok := m.cache.Get(key); ok {
				m.mu.Unlock()
				return v, nil
			}
		}
		if f := m.flights[key]; f != nil {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				var zero T
				return zero, ctx.Err()
			case <-f.done:
				if f.canceled {
					continue
				}
				return f.value, f.err
			}
		}
		if len(m.flights) >= m.maximum {
			m.mu.Unlock()
			var zero T
			return zero, core.NewError("service_busy", "The metadata request queue is full", 503)
		}
		f := &flight[T]{done: make(chan struct{})}
		m.flights[key] = f
		m.mu.Unlock()
		f.value, f.err = fn()
		f.canceled = f.err != nil && ctx.Err() != nil
		m.mu.Lock()
		if f.err == nil {
			m.put(key, f.value)
		}
		delete(m.flights, key)
		close(f.done)
		m.mu.Unlock()
		return f.value, f.err
	}
}
