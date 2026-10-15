package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

type lease struct {
	pool         *Pool
	entry        *entry
	ctx          context.Context
	cancel       context.CancelFunc
	unlink       func() bool
	proxyVersion uint64
	once         sync.Once
}

func (l *lease) client() *tiktokapp.Client {
	if l.entry.client == nil {
		l.entry.client = l.pool.clientFactory(l.entry.session, l.entry.proxy.client)
		if l.pool.mediaTransport != nil {
			l.entry.client.SetMediaTransport(&http.Client{Transport: l.pool.mediaTransport})
		}
	}
	return l.entry.client
}

func (p *Pool) acquire(ctx context.Context, id string) (*lease, error) {
	p.mu.Lock()
	if !p.ready || p.closing {
		p.mu.Unlock()
		return nil, core.NewError("service_not_configured", "The identity pool is unavailable.", 503)
	}
	p.workers.Add(1)
	p.mu.Unlock()
	acquired := false
	// reserved is the busy identity this pinned request waits for.
	var reserved *entry
	defer func() {
		if reserved != nil {
			p.mu.Lock()
			reserved.waiting--
			if reserved.waiting == 0 && !reserved.busy {
				p.wakeLocked()
			}
			p.mu.Unlock()
		}
		if !acquired {
			p.workers.Done()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if reserved != nil {
			reserved.waiting--
			reserved = nil
		}
		if !p.ready || p.closing {
			p.mu.Unlock()
			return nil, core.NewError("service_not_configured", "The identity pool is unavailable.", 503)
		}
		// Without its connection the pool cannot know it still owns the
		// identities; another process may have taken the lock.
		if !p.connected {
			p.mu.Unlock()
			return nil, core.NewError("service_not_configured", "The identity pool is reconnecting to its database.", 503)
		}
		now := time.Now().UnixMilli()
		var selected *entry
		var selectedProxy *proxyState
		ordinal := 0
		eligible := false
		if id != "" {
			selected = p.entries[id]
			if selected == nil {
				p.mu.Unlock()
				return nil, core.Upstream("The requested identity is unavailable.")
			}
			if selected.health.CooldownUntil <= now && selected.proxy.health.CooldownUntil <= now {
				eligible = true
				if selected.busy {
					selected.waiting++
					reserved = selected
					selected = nil
				}
			} else {
				selected = nil
			}
		} else {
			var count int64
			var sequence uint64
			for slot := 0; slot < 10; slot++ {
				for _, proxy := range p.proxies {
					if proxy.health.CooldownUntil > now {
						continue
					}
					e := proxy.slots[slot]
					if e != nil && e.health.CooldownUntil > now {
						continue
					}
					eligible = true
					if e != nil && (e.busy || e.waiting > 0) {
						continue
					}
					var assignments int64
					var last uint64
					if e != nil {
						assignments = e.assignments
						last = e.selected
					}
					if selectedProxy == nil || assignments < count || (assignments == count && last < sequence) {
						selectedProxy = proxy
						ordinal = slot
						selected = e
						count = assignments
						sequence = last
					}
				}
			}
			if selectedProxy != nil && selected == nil {
				session, err := tiktokapp.NewSession(nil)
				if err != nil {
					p.mu.Unlock()
					return nil, err
				}
				selected = &entry{id: core.NewID(), proxy: selectedProxy, ordinal: ordinal, session: session}
				p.entries[selected.id] = selected
				selectedProxy.slots[ordinal] = selected
			}
		}
		if selected != nil {
			selected.busy = true
			if id == "" {
				selected.assignments++
				p.sequence++
				selected.selected = p.sequence
			}
			operationCtx, cancel := context.WithCancel(ctx)
			l := &lease{pool: p, entry: selected, ctx: operationCtx, cancel: cancel, proxyVersion: selected.proxy.healthVersion}
			l.unlink = context.AfterFunc(p.lifetime, cancel)
			p.mu.Unlock()
			acquired = true
			return l, nil
		}
		if !eligible {
			p.mu.Unlock()
			return nil, core.NewError("upstream_rate_limited", "All eligible identities are temporarily cooling down.", 429)
		}
		if p.queued >= p.queueLimit {
			p.mu.Unlock()
			return nil, core.NewError("service_busy", "The identity request queue is full.", 503)
		}
		p.queued++
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
		case <-p.lifetime.Done():
		}
		p.mu.Lock()
		p.queued--
		p.mu.Unlock()
	}
}

// finish releases the identity and records its state in RAM. PostgreSQL saves
// the checkpoint later, so database problems never fail the operation.
func (l *lease) finish(operationErr error) {
	l.once.Do(func() {
		p, e := l.pool, l.entry
		// Cancellation is not an identity failure; check before ending the context.
		cancelled := l.ctx.Err() != nil
		l.unlink()
		l.cancel()
		// The lease still owns the session, so it cannot change while encoding.
		session, err := json.Marshal(e.session)
		p.mu.Lock()
		defer func() {
			e.busy = false
			p.wakeLocked()
			p.mu.Unlock()
			p.workers.Done()
		}()
		if operationErr == nil {
			e.health.Failures = 0
			e.health.CooldownUntil = 0
			if e.proxy.healthVersion == l.proxyVersion && e.proxy.health.Failures > 0 {
				e.proxy.health.Failures = 0
				e.proxy.health.CooldownUntil = 0
				e.proxy.healthVersion++
			}
		} else if !cancelled {
			p.failedLocked(e, operationErr)
		}
		e.registered = e.session.Identity.Kind == "registered"
		if err != nil {
			p.log.Error("identity checkpoint skipped", "identity_id", e.id, "error", err)
			return
		}
		p.checkpointLocked(e, session)
	})
}
func (p *Pool) failedLocked(e *entry, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var typed *tiktokapp.UpstreamError
	var network net.Error
	isNetwork := errors.As(err, &network)
	if !errors.As(err, &typed) && !isNetwork {
		return
	}
	if typed != nil && (typed.Code == "content_deleted" || tiktokapp.IsContentRestricted(typed)) {
		return
	}
	h := &e.health
	code := "upstream_network_error"
	base := time.Second
	if isNetwork {
		h = &e.proxy.health
		e.proxy.healthVersion++
	} else {
		code = typed.Code
		base = 5 * time.Second
		if code == "upstream_unavailable" {
			base = time.Second
		}
	}
	h.Failures++
	now := time.Now()
	stamp := now.UTC().Format(time.RFC3339Nano)
	h.LastFailureAt = &stamp
	h.LastFailureCode = &code
	delay := min(5*time.Minute, base*time.Duration(1<<min(h.Failures-1, 8)))
	if typed != nil && typed.RetryAfter > 0 {
		delay = typed.RetryAfter
	}
	h.CooldownUntil = max(h.CooldownUntil, now.Add(delay).UnixMilli())
}
func (l *lease) recover(err error) bool {
	var failure *tiktokapp.UpstreamError
	if l.ctx.Err() != nil || !errors.As(err, &failure) || failure.Recovery == tiktokapp.RecoveryNone {
		return false
	}
	var session *tiktokapp.Session
	if failure.Recovery == tiktokapp.RecoveryRecreate {
		var newErr error
		session, newErr = tiktokapp.NewSession(nil)
		if newErr != nil {
			return false
		}
	} else if failure.Recovery != tiktokapp.RecoveryRefresh {
		return false
	}
	p, e := l.pool, l.entry
	p.mu.Lock()
	defer p.mu.Unlock()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	recovery := string(failure.Recovery)
	e.health.LastRecoveryAt = &stamp
	e.health.LastRecovery = &recovery
	e.health.LastFailureAt = &stamp
	e.health.LastFailureCode = &failure.Code
	if session != nil {
		e.session = session
		e.client = nil
		e.registered = false
	} else {
		e.session.SDK = nil
	}
	return true
}
