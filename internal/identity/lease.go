package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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
	finishErr    error
}

func (l *lease) client() *tiktokapp.Client {
	if l.entry.client == nil {
		l.entry.client = l.pool.clientFactory(l.entry.session, l.entry.proxy.client)
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
	defer func() {
		if !acquired {
			p.workers.Done()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if !p.ready || p.closing {
			p.mu.Unlock()
			return nil, core.NewError("service_not_configured", "The identity pool is unavailable.", 503)
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
					if e != nil && e.busy {
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
				selected = &entry{id: core.NewID(), proxy: selectedProxy, ordinal: ordinal, session: session, revision: -1}
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
func (l *lease) finish(operationErr error) error {
	l.once.Do(func() {
		p, e := l.pool, l.entry
		defer func() {
			l.unlink()
			l.cancel()
			p.mu.Lock()
			e.registered = e.session.Identity.Kind == "registered"
			e.busy = false
			p.wakeLocked()
			p.mu.Unlock()
			p.workers.Done()
		}()
		p.mu.Lock()
		if operationErr == nil {
			e.health.Failures = 0
			e.health.CooldownUntil = 0
			if e.proxy.healthVersion == l.proxyVersion && e.proxy.health.Failures > 0 {
				e.proxy.health.Failures = 0
				e.proxy.health.CooldownUntil = 0
				e.proxy.healthVersion++
			}
		} else if l.ctx.Err() == nil {
			p.failedLocked(e, operationErr)
		}
		p.mu.Unlock()
		if err := p.save(e); err != nil {
			p.fail()
			l.finishErr = core.NewError("service_not_configured", "Identity state could not be saved.", 503)
		}
	})
	return l.finishErr
}
func (p *Pool) failedLocked(e *entry, err error) {
	var typed *tiktokapp.UpstreamError
	var network net.Error
	isNetwork := errors.As(err, &network)
	if !errors.As(err, &typed) && !isNetwork {
		return
	}
	if typed != nil && (typed.Code == "content_private" || typed.Code == "content_deleted" || typed.Code == "region_blocked") {
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
func (p *Pool) save(e *entry) error {
	raw, err := json.Marshal(e.session)
	if err != nil {
		return err
	}
	p.mu.Lock()
	rawHealth, err := json.Marshal(e.health)
	proxyVersion := e.proxy.healthVersion
	dirtyProxy := proxyVersion > e.proxy.savedHealthVersion
	rawProxy, proxyErr := json.Marshal(e.proxy.health)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if proxyErr != nil {
		return proxyErr
	}
	p.dbMu.Lock()
	defer p.dbMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	next := e.revision + 1
	var changed int64
	if e.revision < 0 {
		tag, err := p.db.Exec(ctx, `INSERT INTO `+p.identitiesTable+`(id,proxy_id,ordinal,session,assignment_count,revision,health,last_used_at) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7::jsonb,clock_timestamp()) ON CONFLICT DO NOTHING`, e.id, e.proxy.id, e.ordinal, raw, e.assignments, next, rawHealth)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected()
	} else {
		tag, err := p.db.Exec(ctx, `UPDATE `+p.identitiesTable+` SET session=$2::jsonb,assignment_count=$3,revision=$4,last_used_at=clock_timestamp(),updated_at=clock_timestamp(),health=$8::jsonb WHERE id=$1 AND revision=$5 AND proxy_id=$6 AND ordinal=$7`, e.id, raw, e.assignments, next, e.revision, e.proxy.id, e.ordinal, rawHealth)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected()
	}
	p.writes.Add(1)
	if changed != 1 {
		return errors.New("identity revision conflict")
	}
	e.revision = next
	if dirtyProxy {
		// A previous writer may already have persisted a newer proxy version.
		p.mu.Lock()
		stale := proxyVersion <= e.proxy.savedHealthVersion
		p.mu.Unlock()
		if !stale {
			if _, err = p.db.Exec(ctx, `UPDATE `+p.proxiesTable+` SET health=$2::jsonb,updated_at=clock_timestamp() WHERE id=$1`, e.proxy.id, rawProxy); err != nil {
				return err
			}
			p.writes.Add(1)
			p.mu.Lock()
			e.proxy.savedHealthVersion = max(e.proxy.savedHealthVersion, proxyVersion)
			p.mu.Unlock()
		}
	}
	return nil
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
	} else {
		e.session.SDK = nil
	}
	return true
}
