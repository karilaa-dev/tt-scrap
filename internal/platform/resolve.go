package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

var postIDPattern = regexp.MustCompile(`/(?:video|photo)/(\d+)`)

func normalizedHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// tt.site is TikTok's share-link domain; its links redirect to tiktok.com.
func shareHost(host string) bool { return host == "tt.site" || host == "www.tt.site" }

func validTikTokURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	host := normalizedHost(u)
	return u.Scheme == "https" && (host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com") || shareHost(host)) && u.User == nil
}

// Share links are served from www.tt.site; the apex host fails TLS handshakes.
func shareRequestURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || normalizedHost(u) != "tt.site" {
		return raw
	}
	u.Host = "www.tt.site"
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return u.String()
}
func videoID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if m := postIDPattern.FindStringSubmatch(u.Path); len(m) > 1 {
		return m[1]
	}
	return ""
}

func (t *TikTok) ResolveURL(ctx context.Context, raw string) (core.TikTokResolution, error) {
	return t.resolve(ctx, raw, false)
}
func (t *TikTok) resolve(ctx context.Context, raw string, refresh bool) (core.TikTokResolution, error) {
	raw = strings.TrimSpace(raw)
	if !validTikTokURL(raw) {
		return core.TikTokResolution{}, core.InvalidLink("Only HTTPS TikTok URLs are accepted")
	}
	ctx, cancel := context.WithTimeout(ctx, t.cfg.ResolveTimeout)
	defer cancel()
	return t.resolveCache.do(ctx, raw, refresh, func() (core.TikTokResolution, error) {
		if id := videoID(raw); id != "" {
			return core.TikTokResolution{Platform: "tiktok", SourceID: id, SourceURL: raw, ResolvedURL: raw}, nil
		}
		release, err := t.resolveGate.Acquire(ctx)
		if err != nil {
			return core.TikTokResolution{}, err
		}
		defer release()
		var last error
		for attempt := 0; attempt < max(1, t.cfg.ResolveRetries); attempt++ {
			transport, e := t.transports.get(t.proxy())
			if e != nil {
				return core.TikTokResolution{}, e
			}
			var resolved string
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("TikTok redirect limit exceeded")
				}
				if !validTikTokURL(req.URL.String()) {
					return errors.New("TikTok redirected outside its domain")
				}
				if videoID(req.URL.String()) != "" {
					resolved = req.URL.String()
					return http.ErrUseLastResponse
				}
				return nil
			}}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, shareRequestURL(raw), nil)
			req.Header.Set("User-Agent", browserAgent)
			res, e := client.Do(req)
			if e != nil {
				last = networkError(ctx, e)
				continue
			}
			res.Body.Close()
			if resolved != "" {
				return core.TikTokResolution{Platform: "tiktok", SourceID: videoID(resolved), SourceURL: raw, ResolvedURL: resolved}, nil
			}
			if res.StatusCode == 400 || res.StatusCode == 404 || res.StatusCode == 410 {
				return core.TikTokResolution{}, core.InvalidLink("Invalid or expired TikTok link")
			}
			if res.StatusCode >= 400 {
				last = statusError(res.StatusCode, "TikTok link resolution")
				continue
			}
			resolved = res.Request.URL.String()
			id := videoID(resolved)
			if id == "" {
				return core.TikTokResolution{}, core.InvalidLink("TikTok link does not identify a post")
			}
			return core.TikTokResolution{Platform: "tiktok", SourceID: id, SourceURL: raw, ResolvedURL: resolved}, nil
		}
		return core.TikTokResolution{}, last
	})
}
