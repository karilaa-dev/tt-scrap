package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
)

// requestTrace records fixed phase names only. HTTP trace callbacks may run
// concurrently; no addresses, headers or raw errors enter these diagnostics.
type requestTrace struct {
	mu    sync.Mutex
	phase string
}

func (t *requestTrace) set(phase string) { t.mu.Lock(); t.phase = phase; t.mu.Unlock() }
func (t *requestTrace) get() string      { t.mu.Lock(); defer t.mu.Unlock(); return t.phase }
func (t *requestTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:           func(string) { t.set("connection_wait") },
		DNSStart:          func(httptrace.DNSStartInfo) { t.set("dns") },
		ConnectStart:      func(string, string) { t.set("connect") },
		ConnectDone:       func(string, string, error) { t.set("connect") },
		TLSHandshakeStart: func() { t.set("tls") },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { t.set("tls") },
		GotConn:           func(httptrace.GotConnInfo) { t.set("headers") },
		WroteRequest:      func(httptrace.WroteRequestInfo) { t.set("headers") },
	}
}

// stop joins an already-fired callback so it cannot cancel the next phase.
func stallTimer(timeout time.Duration, cancel context.CancelCauseFunc, phase func() string) func() {
	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		defer close(done)
		cancel(&core.DownloadAttemptTimeout{Phase: phase()})
	})
	return func() {
		if !timer.Stop() {
			<-done
		}
	}
}

func (d *Downloader) openTimed(ctx context.Context, asset core.AssetContext, source, byteRange, ifRange string, open core.OpenAppMedia) (*http.Response, error) {
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	trace := &requestTrace{phase: "headers"}
	ctx = httptrace.WithClientTrace(ctx, trace.hooks())
	started := time.Now()
	headerLimit := core.OperationStallLimit(ctx, d.cfg.DownloadHeaderTimeout)
	idleLimit := core.OperationStallLimit(ctx, d.cfg.DownloadIdleTimeout)
	// Backends that expose exchanges switch to body timing before inspecting
	// prefixes. Keep the outer guard for implementations without that hook.
	stop := sync.OnceFunc(stallTimer(headerLimit, cancel, trace.get))
	var guarded atomic.Bool
	ctx = nettransport.WithMediaExchange(ctx, func(next func() (*http.Response, error)) (*http.Response, error) {
		stop()
		guarded.Store(true)
		trace.set("headers")
		stopHeaders := stallTimer(headerLimit, cancel, trace.get)
		response, err := next()
		stopHeaders()
		if response != nil {
			trace.set("body")
			// Redirect bodies share this open context; closing one must not cancel
			// the subsequent exchange. The final outer body owns cancellation.
			response.Body = &idleBody{ReadCloser: response.Body, ctx: ctx, parent: parent, cancel: cancel, timeout: idleLimit, keepContext: true}
		}
		return response, err
	})
	res, err := d.open(ctx, asset, source, byteRange, ifRange, open)
	stop()
	if err != nil || ctx.Err() != nil {
		if res != nil {
			res.Body.Close()
		}
		if parent.Err() != nil {
			err = parent.Err()
		} else if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		cancel(nil)
		core.DiagnosticLogger(ctx).Warn("media request failed", "phase", trace.get(), "elapsed_ms", time.Since(started).Milliseconds(), "error_code", downloadErrorCode(err))
		return nil, err
	}
	core.DiagnosticLogger(ctx).Debug("media open completed", "phase", trace.get(), "elapsed_ms", time.Since(started).Milliseconds(), "status", res.StatusCode, "ranged", byteRange != "")
	if guarded.Load() {
		res.Body = &cancelBody{ReadCloser: res.Body, cancel: cancel}
	} else {
		res.Body = &idleBody{ReadCloser: res.Body, ctx: ctx, parent: parent, cancel: cancel, timeout: idleLimit}
	}
	return res, nil
}

type idleBody struct {
	io.ReadCloser
	ctx, parent context.Context
	cancel      context.CancelCauseFunc
	timeout     time.Duration
	keepContext bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	stop := stallTimer(b.timeout, b.cancel, func() string { return "body" })
	var n int
	var err error
	for n == 0 && err == nil && b.ctx.Err() == nil {
		n, err = b.ReadCloser.Read(p)
	}
	stop()
	if b.parent.Err() != nil {
		err = b.parent.Err()
	} else if b.ctx.Err() != nil {
		err = context.Cause(b.ctx)
	}
	if err != nil && err != io.EOF {
		core.DiagnosticLogger(b.ctx).Warn("media body read failed", "phase", "body", "error_code", downloadErrorCode(err))
	}
	return n, err
}
func (b *idleBody) Close() error {
	// Close the leased body before cancelling a successful request, so the
	// identity batch can distinguish completed responses from abandoned ones.
	err := b.ReadCloser.Close()
	if !b.keepContext {
		b.cancel(nil)
	}
	return err
}
func downloadErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "request_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	var api *core.Error
	if errors.As(err, &api) {
		return api.Code
	}
	return "upstream_network_error"
}

// cancelBody closes the lease first, preserving successful completion accounting.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(nil); return err }
