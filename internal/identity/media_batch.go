package identity

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

// WithMedia owns one identity until all related requests and bodies finish.
// Signing and cookie changes remain in RAM; completion records one checkpoint.
func (p *Pool) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) (err error) {
	began := time.Now()
	l, err := p.acquire(ctx, id)
	core.DiagnosticLogger(ctx).Debug("media identity admission", "phase", "identity_wait", "elapsed_ms", time.Since(began).Milliseconds(), "acquired", err == nil)
	if err != nil {
		return err
	}
	var operationErr error
	defer func() {
		l.finish(operationErr)
		err = publicError(err)
	}()
	for attempt := 0; attempt < 2; attempt++ {
		client := l.client()
		batchCtx, cancel := context.WithCancel(l.ctx)
		batch := &mediaBatch{ctx: batchCtx, cancel: cancel, openMedia: func(ctx context.Context, address string, headers http.Header) (*http.Response, error) {
			return p.mediaRequest(ctx, client, address, headers)
		}, bodies: make(map[*leasedBody]struct{}), routeID: l.entry.proxy.id}
		func() { defer batch.close(); err = run(batch.open) }()
		client.SyncCookies()
		operationErr = err
		var failure *tiktokapp.UpstreamError
		if err != nil || batch.completed == 0 || (errors.As(batch.err, &failure) && failure.Recovery != tiktokapp.RecoveryNone) {
			if batch.err != nil {
				operationErr = batch.err
			}
		}
		if attempt != 0 || !l.recover(operationErr) {
			return err
		}
	}
	return err
}

type mediaBatch struct {
	ctx       context.Context
	cancel    context.CancelFunc
	openMedia core.OpenAppMedia
	mu        sync.Mutex
	requests  sync.WaitGroup
	bodies    map[*leasedBody]struct{}
	closed    bool
	err       error
	completed int
	routeID   int64
}

func (b *mediaBatch) recordLocked(err error) {
	if err == nil {
		return
	}
	var previous, next *tiktokapp.UpstreamError
	if b.err == nil || ((errors.Is(b.err, context.Canceled) || errors.Is(b.err, context.DeadlineExceeded)) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) || (errors.As(err, &next) && next.Recovery != tiktokapp.RecoveryNone && (!errors.As(b.err, &previous) || previous.Recovery == tiktokapp.RecoveryNone)) {
		b.err = err
	}
}

func (b *mediaBatch) open(ctx context.Context, address string, headers http.Header) (*http.Response, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, context.Canceled
	}
	b.requests.Add(1)
	b.mu.Unlock()
	defer b.requests.Done()
	requestCtx, cancel := context.WithCancel(ctx)
	unlink := context.AfterFunc(b.ctx, cancel)

	var direct atomic.Bool
	requestCtx = tiktokapp.WithMediaRouteObserver(requestCtx, func(directCDN bool) {
		direct.Store(directCDN)
		route := b.routeID
		if directCDN {
			route = 0
		}
		core.DiagnosticLogger(ctx).Debug("media route selected", "route_id", route, "direct_cdn", directCDN)
	})
	response, err := b.openMedia(requestCtx, address, headers)
	if err != nil {
		accountingErr := b.attemptError(requestCtx, err, direct.Load())
		unlink()
		cancel()
		b.mu.Lock()
		b.recordLocked(accountingErr)
		b.mu.Unlock()
		return nil, publicError(err)
	}
	body := &leasedBody{ReadCloser: response.Body}
	body.finish = func(err error) {
		completed := err == nil && requestCtx.Err() == nil
		err = b.attemptError(requestCtx, err, direct.Load())
		unlink()
		cancel()
		b.mu.Lock()
		if completed {
			b.completed++
		}
		delete(b.bodies, body)
		b.recordLocked(err)
		b.mu.Unlock()
	}
	b.mu.Lock()
	b.bodies[body] = struct{}{}
	b.mu.Unlock()
	response.Body = body
	return response, nil
}

func (b *mediaBatch) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.requests.Wait()
	b.mu.Lock()
	bodies := make([]*leasedBody, 0, len(b.bodies))
	for body := range b.bodies {
		bodies = append(bodies, body)
	}
	b.mu.Unlock()
	for _, body := range bodies {
		body.Close()
	}
}

// Preserve a proven per-request stall before closing the request context.
// Caller/total deadlines stay neutral, as do direct CDN failures for a metadata proxy.
func mediaAttemptError(ctx context.Context, err error, direct bool) error {
	var stall *core.DownloadAttemptTimeout
	if errors.As(context.Cause(ctx), &stall) {
		if direct {
			return context.Canceled
		}
		return stall
	}
	var network net.Error
	if err != nil && ctx.Err() == nil && errors.As(err, &network) {
		if direct {
			return context.Canceled
		}
		if network.Timeout() {
			return &core.DownloadAttemptTimeout{Phase: "transport"}
		}
	}
	return err
}

func (b *mediaBatch) attemptError(ctx context.Context, err error, direct bool) error {
	accounted := mediaAttemptError(ctx, err, direct)
	var stall *core.DownloadAttemptTimeout
	if errors.As(context.Cause(ctx), &stall) || errors.As(accounted, &stall) {
		route := b.routeID
		if direct {
			route = 0
		}
		core.DiagnosticLogger(ctx).Warn("media route stalled", "route_id", route, "direct_cdn", direct, "phase", stall.Phase)
	}
	return accounted
}
