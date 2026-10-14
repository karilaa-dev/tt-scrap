package identity

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func publicError(err error) error {
	if err == nil {
		return nil
	}
	var existing *core.Error
	if errors.As(err, &existing) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewError("upstream_timeout", "The upstream request timed out.", 504)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var upstream *tiktokapp.UpstreamError
	if errors.As(err, &upstream) {
		code, status := upstream.Code, 502
		switch code {
		case "content_deleted":
			status = 404
		case "content_private", "content_restricted":
			status = 403
		case "region_blocked":
			status = 451
		case "upstream_rate_limited":
			status = 429
		default:
			code = "upstream_extraction_error"
		}
		return core.NewError(code, upstream.Error(), status)
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return core.NewError("upstream_timeout", "The upstream request timed out.", 504)
		}
		return core.NewError("upstream_network_error", "The upstream network request failed.", 502)
	}
	return core.Upstream("The TikTok app request failed.")
}
func (p *Pool) Detail(ctx context.Context, id string) (result core.AppDetail, err error) {
	l, err := p.acquire(ctx, "")
	if err != nil {
		return result, err
	}
	defer func() {
		if saved := l.finish(err); saved != nil {
			err = saved
		}
		err = publicError(err)
	}()
	account := tiktokapp.Account{}
	var restriction error
	for attempt := 0; attempt < 2; attempt++ {
		client := l.client()
		result.Response, err = client.DetailWithAccount(l.ctx, id, account)
		if tiktokapp.IsContentRestricted(err) && !account.Valid() && p.account.Valid() {
			restriction = err
			account = p.account
			result.Response, err = client.DetailWithAccount(l.ctx, id, account)
		}
		err = accountFallbackError(restriction, err)
		client.SyncCookies()
		if err == nil {
			result.IdentityID = l.entry.id
			result.ProxyURL = l.entry.proxy.address
			result.UserAgent = tiktokapp.UserAgent(l.entry.session.Profile)
			return result, nil
		}
		if attempt > 0 || !l.recover(err) {
			return result, err
		}
	}
	return result, err
}
func (p *Pool) Feed(ctx context.Context) (result map[string]any, err error) {
	l, err := p.acquire(ctx, "")
	if err != nil {
		return nil, err
	}
	defer func() {
		if saved := l.finish(err); saved != nil {
			err = saved
		}
		err = publicError(err)
	}()
	for attempt := 0; attempt < 2; attempt++ {
		client := l.client()
		result, err = client.Feed(l.ctx)
		client.SyncCookies()
		if err == nil {
			return result, nil
		}
		if attempt > 0 || !l.recover(err) {
			return nil, err
		}
	}
	return nil, err
}
func (p *Pool) OpenMedia(ctx context.Context, identityID, address string, headers http.Header) (*http.Response, error) {
	l, err := p.acquire(ctx, identityID)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		client := l.client()
		response, err := p.mediaRequest(l.ctx, client, address, headers)
		client.SyncCookies()
		if err == nil && response.StatusCode >= 400 {
			response.Body.Close()
			err = tiktokapp.ClassifyFailure(response.StatusCode, 0, "", false, 0)
		}
		if err == nil {
			response.Body = &leasedBody{ReadCloser: response.Body, finish: l.finish}
			return response, nil
		}
		if attempt > 0 || !l.recover(err) {
			if saved := l.finish(err); saved != nil {
				return nil, saved
			}
			return nil, publicError(err)
		}
	}
	panic("unreachable identity retry")
}

type leasedBody struct {
	io.ReadCloser
	finish   func(error) error
	once     sync.Once
	mu       sync.Mutex
	readErr  error
	closeErr error
}

func (b *leasedBody) Read(buffer []byte) (int, error) {
	n, err := b.ReadCloser.Read(buffer)
	if err != nil && err != io.EOF {
		b.mu.Lock()
		b.readErr = err
		b.mu.Unlock()
	}
	return n, err
}
func (b *leasedBody) Close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.mu.Lock()
		err := b.readErr
		b.mu.Unlock()
		if err == nil {
			err = b.closeErr
		}
		if saved := b.finish(err); saved != nil {
			b.closeErr = saved
		}
	})
	return b.closeErr
}
