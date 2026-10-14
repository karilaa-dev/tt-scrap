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

var postIDPattern = regexp.MustCompile(`/(?:video|photo)/(\d+)|^/v/(\d+)\.html$`)
var sharePathPattern = regexp.MustCompile(`^/t/[A-Za-z0-9_-]+/?$`)

func normalizedHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// tt.site is TikTok's share-link domain; its /t/<code> links redirect to
// tiktok.com. Only that single-segment form is accepted, so a tt.site URL never
// carries a post ID that could be trusted without following the redirect.
func shareLink(host, path string) bool {
	return (host == "tt.site" || host == "www.tt.site") && sharePathPattern.MatchString(path)
}

func validTikTokURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	host := normalizedHost(u)
	return u.Scheme == "https" && (host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com") || shareLink(host, u.Path)) && u.User == nil
}

// Share links are served from www.tt.site; the apex host fails TLS handshakes.
func shareRequestURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || normalizedHost(u) != "tt.site" {
		return raw
	}
	port := u.Port()
	u.Host = "www.tt.site"
	if port != "" {
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return u.String()
}
func videoID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if m := postIDPattern.FindStringSubmatch(u.Path); m != nil {
		return m[1] + m[2]
	}
	return ""
}

// Redirects to these TikTok pages cannot lead to a post, so their often large
// HTML is never requested. Expired share links redirect to the home page.
func terminalPage(u *url.URL) bool {
	path := strings.TrimSuffix(u.Path, "/")
	return path == "" || strings.HasPrefix(path, "/@") || path == "/login" || path == "/foryou" || path == "/explore"
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
			var resolved string
			resolved, last = t.followShareLink(ctx, raw)
			if last == nil {
				return core.TikTokResolution{Platform: "tiktok", SourceID: videoID(resolved), SourceURL: raw, ResolvedURL: resolved}, nil
			}
			var failure *core.Error
			if errors.As(last, &failure) && failure.Code == "invalid_link" {
				return core.TikTokResolution{}, last
			}
		}
		return core.TikTokResolution{}, last
	})
}

// followShareLink reads each redirect's Location without requesting it. The
// first location that names a post resolves the link.
func (t *TikTok) followShareLink(ctx context.Context, raw string) (string, error) {
	transport, err := t.transports.get(t.proxy())
	if err != nil {
		return "", err
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	current := shareRequestURL(raw)
	for hop := 0; hop < 10; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", core.InvalidLink("Invalid or expired TikTok link")
		}
		req.Header.Set("User-Agent", browserAgent)
		res, err := client.Do(req)
		if err != nil {
			return "", networkError(ctx, err)
		}
		// Share responses are redirects or error pages; their bodies are never needed.
		res.Body.Close()
		switch res.StatusCode {
		case 301, 302, 303, 307, 308:
		case 400, 404, 410:
			return "", core.InvalidLink("Invalid or expired TikTok link")
		default:
			if res.StatusCode >= 400 {
				return "", statusError(res.StatusCode, "TikTok link resolution")
			}
			return "", core.InvalidLink("TikTok link does not identify a post")
		}
		location, err := res.Location()
		if err != nil {
			return "", core.NewError("upstream_network_error", "TikTok redirect lacks a location", 502)
		}
		next := location.String()
		if !validTikTokURL(next) {
			return "", core.NewError("upstream_network_error", "TikTok redirected outside its domain", 502)
		}
		if videoID(next) != "" {
			return next, nil
		}
		if terminalPage(location) {
			return "", core.InvalidLink("TikTok link does not identify a post")
		}
		current = next
	}
	return "", core.NewError("upstream_network_error", "TikTok redirect limit exceeded", 502)
}
