package nettransport

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

// New creates a private connection pool; routing never consults proxy environment variables.
func New(raw string, connections int) (*http.Transport, error) {
	if connections < 1 || connections > 2048 {
		return nil, errors.New("invalid HTTP connection limit")
	}
	transport := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: connections * 4, MaxIdleConnsPerHost: connections, MaxConnsPerHost: connections,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return nil, errors.New("invalid proxy address")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("unsupported proxy scheme")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return transport, nil
}
