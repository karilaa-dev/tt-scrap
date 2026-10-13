package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const browserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
const metadataLimit = 16 << 20

type transports struct {
	mu    sync.Mutex
	items map[string]*http.Transport
	max   int
}

func newTransports(max int) *transports {
	if max <= 0 {
		max = 128
	}
	return &transports{items: make(map[string]*http.Transport), max: max}
}
func (p *transports) get(proxy string) (*http.Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.items[proxy]; t != nil {
		return t, nil
	}
	t := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: p.max * 2, MaxIdleConnsPerHost: p.max, MaxConnsPerHost: p.max, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 25 * time.Second, ExpectContinueTimeout: time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if proxy != "" {
		u, e := url.Parse(proxy)
		if e != nil || u.Hostname() == "" {
			return nil, errors.New("invalid proxy URL")
		}
		t.Proxy = http.ProxyURL(u)
	}
	p.items[proxy] = t
	return t, nil
}
func (p *transports) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.items {
		t.CloseIdleConnections()
	}
}
func limitedRead(r io.Reader, max int64) ([]byte, error) {
	data, e := io.ReadAll(io.LimitReader(r, max+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > max {
		return nil, errors.New("upstream response exceeds byte limit")
	}
	return data, nil
}
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
