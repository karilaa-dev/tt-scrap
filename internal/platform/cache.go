package platform

import (
	"context"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type flight[T any] struct {
	done  chan struct{}
	value T
	err   error
}
type memo[T any] struct {
	mu      sync.Mutex
	cache   *core.Cache[T]
	flights map[string]*flight[T]
	maximum int
}

func newMemo[T any](ttl time.Duration, maximum int) *memo[T] {
	if ttl <= 0 {
		ttl = time.Minute
	}
	if maximum <= 0 {
		maximum = 8192
	}
	return &memo[T]{cache: core.NewCache[T](ttl, maximum), flights: make(map[string]*flight[T]), maximum: maximum}
}
func (m *memo[T]) get(key string) (T, bool) { return m.cache.Get(key) }
func (m *memo[T]) put(key string, value T)  { m.cache.Set(key, value) }
func (m *memo[T]) do(ctx context.Context, key string, refresh bool, fn func() (T, error)) (T, error) {
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
	m.mu.Lock()
	if f.err == nil {
		m.cache.Set(key, f.value)
	}
	delete(m.flights, key)
	close(f.done)
	m.mu.Unlock()
	return f.value, f.err
}
