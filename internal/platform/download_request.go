package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
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
	stop := stallTimer(core.OperationStallLimit(ctx, d.cfg.DownloadHeaderTimeout), cancel, trace.get)
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
	core.DiagnosticLogger(ctx).Debug("media headers received", "phase", "headers", "elapsed_ms", time.Since(started).Milliseconds(), "status", res.StatusCode, "ranged", byteRange != "")
	res.Body = &idleBody{ReadCloser: res.Body, ctx: ctx, parent: parent, cancel: cancel, timeout: core.OperationStallLimit(ctx, d.cfg.DownloadIdleTimeout)}
	return res, nil
}

type idleBody struct {
	io.ReadCloser
	ctx, parent context.Context
	cancel      context.CancelCauseFunc
	timeout     time.Duration
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
	b.cancel(nil)
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
