package identity

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

// WithMedia owns one identity until all related requests and bodies finish.
// Signing and cookie changes remain in RAM; completion saves one checkpoint.
func (p *Pool) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) (err error) {
	l, err := p.acquire(ctx, id)
	if err != nil {
		return err
	}
	var operationErr error
	defer func() {
		if saved := l.finish(operationErr); saved != nil {
			err = saved
		}
		err = publicError(err)
	}()
	for attempt := 0; attempt < 2; attempt++ {
		client := l.client()
		batchCtx, cancel := context.WithCancel(l.ctx)
		batch := &mediaBatch{ctx: batchCtx, cancel: cancel, openMedia: func(ctx context.Context, address string, headers http.Header) (*http.Response, error) {
			return p.mediaRequest(ctx, client, address, headers)
		}, bodies: make(map[*leasedBody]struct{})}
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
}

func (b *mediaBatch) recordLocked(err error) {
	if err == nil {
		return
	}
	var previous, next *tiktokapp.UpstreamError
	if b.err == nil || (errors.As(err, &next) && next.Recovery != tiktokapp.RecoveryNone && (!errors.As(b.err, &previous) || previous.Recovery == tiktokapp.RecoveryNone)) {
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
	response, err := b.openMedia(requestCtx, address, headers)
	if err != nil {
		unlink()
		cancel()
		b.mu.Lock()
		b.recordLocked(err)
		b.mu.Unlock()
		return nil, publicError(err)
	}
	body := &leasedBody{ReadCloser: response.Body}
	body.finish = func(err error) error {
		completed := err == nil && requestCtx.Err() == nil
		unlink()
		cancel()
		b.mu.Lock()
		if completed {
			b.completed++
		}
		delete(b.bodies, body)
		b.recordLocked(err)
		b.mu.Unlock()
		return nil
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
