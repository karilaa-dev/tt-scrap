package identity

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

const preparationInterval = 10 * time.Second

type preparationRun struct {
	status core.IdentityPreparation
	done   chan struct{}
}

// StartPreparation coalesces callers into one pool-owned job. HTTP cancellation
// cannot abandon registration midway; Close cancels and drains the job.
func (p *Pool) StartPreparation() (core.IdentityPreparation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || p.closing {
		return core.IdentityPreparation{}, core.NewError("service_not_configured", "The identity pool is unavailable.", 503)
	}
	status := p.preparationStatusLocked()
	if status.State == "running" || status.State == "completed" && status.Ready == status.Target {
		return status, nil
	}
	now := time.Now().UTC()
	job := &preparationRun{status: core.IdentityPreparation{State: "running", StartedAt: &now}, done: make(chan struct{})}
	p.preparation = job
	p.workers.Add(1)
	go p.prepareIdentities(job)
	return p.preparationStatusLocked(), nil
}

func (p *Pool) PreparationStatus() core.IdentityPreparation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preparationStatusLocked()
}

func (p *Pool) preparationStatusLocked() core.IdentityPreparation {
	status := core.IdentityPreparation{State: "idle"}
	if p.preparation != nil {
		status = p.preparation.status
	}
	status.Proxies, status.Target = len(p.proxies), len(p.proxies)*10
	now := time.Now()
	for _, e := range p.entries {
		if e.prepared(now) {
			status.Ready++
		}
	}
	return status
}

func (e *entry) prepared(now time.Time) bool {
	return e.registered && !e.signedAt.IsZero() && !e.signedAt.After(now) && now.Sub(e.signedAt) < tiktokapp.SigningTTL
}

// Prepare blocks startup until every slot has durable registration and a fresh seed.
func (p *Pool) Prepare(ctx context.Context) error {
	if _, err := p.StartPreparation(); err != nil {
		return err
	}
	p.mu.Lock()
	job := p.preparation
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-job.done:
	}
	status := p.PreparationStatus()
	if status.State != "completed" || status.Ready != status.Target {
		return errors.New("identity preparation failed; saved progress will be reused on retry")
	}
	return nil
}

func (p *Pool) prepareIdentities(job *preparationRun) {
	defer p.workers.Done()
	var workers sync.WaitGroup
	for _, proxy := range p.proxies {
		workers.Go(func() {
			for ordinal := range proxy.slots {
				if p.lifetime.Err() != nil {
					return
				}
				for attempt := 0; attempt < 3; attempt++ {
					if err := p.prepareSlot(proxy, ordinal); err == nil {
						break
					}
					if p.lifetime.Err() != nil {
						return
					}
				}
			}
		})
	}
	workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	status := p.preparationStatusLocked()
	job.status.Failed = status.Target - status.Ready
	job.status.State = "completed"
	if job.status.Failed > 0 || !p.ready || p.closing {
		job.status.State = "failed"
	}
	now := time.Now().UTC()
	job.status.FinishedAt = &now
	close(job.done)
}

func (p *Pool) prepareSlot(proxy *proxyState, ordinal int) error {
	p.mu.Lock()
	if !p.ready || p.closing {
		p.mu.Unlock()
		return context.Canceled
	}
	e := proxy.slots[ordinal]
	if e != nil && e.prepared(time.Now()) {
		p.mu.Unlock()
		return nil
	}
	if e == nil {
		session, err := tiktokapp.NewSession(nil)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		e = &entry{id: core.NewID(), proxy: proxy, ordinal: ordinal, session: session, revision: -1}
		proxy.slots[ordinal], p.entries[e.id] = e, e
	}
	id := e.id
	p.mu.Unlock()
	if err := proxy.preparationPace.wait(p.lifetime, p.preparationDelay); err != nil {
		return err
	}
	l, err := p.acquire(p.lifetime, id)
	if err != nil {
		return err
	}
	// A busy lease may have delayed the start beyond the reserved time.
	proxy.preparationPace.next = time.Now().Add(p.preparationDelay)
	client := l.client()
	ctx, cancel := context.WithTimeout(l.ctx, p.preparationTimeout)
	defer cancel()
	err = client.Bootstrap(ctx)
	client.SyncCookies()
	if err != nil {
		l.recover(err)
	}
	if saved := l.finish(err); saved != nil {
		return saved
	}
	return err
}

// Each proxy has one preparation worker. Its clock survives repeated jobs,
// preventing retries or repeated POSTs from resetting the creation rate.
type preparationPacer struct{ next time.Time }

func (p *preparationPacer) wait(ctx context.Context, interval time.Duration) error {
	if delay := time.Until(p.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.next = time.Now().Add(interval)
	return nil
}
