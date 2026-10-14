package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type TikTok struct {
	cfg          core.Config
	assets       *core.Assets
	app          core.AppBackend
	proxies      []string
	next         atomic.Uint64
	transports   *transports
	resolveCache *memo[core.TikTokResolution]
	posts        *memo[core.TikTokExtraction]
	extractions  *memo[core.TikTokExtraction]
	music        *memo[core.TikTokMusic]
	gate         *core.Limiter
	resolveGate  *core.Limiter
}

func NewTikTok(cfg core.Config, assets *core.Assets, backend core.AppBackend, proxies []string) *TikTok {
	if len(proxies) == 0 {
		proxies = []string{""}
	}
	concurrency := cfg.ExtractionConcurrency
	if concurrency < 1 {
		concurrency = 32
	}
	cacheCapacity := core.CacheBudget(cfg.CacheMaxEntries).Responses
	t := &TikTok{cfg: cfg, assets: assets, app: backend, proxies: proxies, transports: newTransports(cfg.HTTPMaxConnections), resolveCache: newMemo[core.TikTokResolution](cfg.ResolutionTTL, cacheCapacity, cfg.CacheMaxEntries), posts: newMemo[core.TikTokExtraction](cfg.MetadataTTL, cacheCapacity, cfg.CacheMaxEntries), extractions: newMemo[core.TikTokExtraction](cfg.MetadataTTL, cacheCapacity, cfg.CacheMaxEntries), music: newMemo[core.TikTokMusic](cfg.MetadataTTL, cacheCapacity, cfg.CacheMaxEntries), gate: core.NewLimiter(concurrency, cfg.MaxQueuedRequests), resolveGate: core.NewLimiter(cfg.HTTPMaxConnections, cfg.MaxQueuedRequests)}
	t.music.expires = func(value core.TikTokMusic) time.Time {
		expires, _ := time.Parse(time.RFC3339Nano, value.ExpiresAt)
		return expires
	}
	return t
}
func (t *TikTok) Close()        { t.transports.close() }
func (t *TikTok) proxy() string { return t.proxies[(t.next.Add(1)-1)%uint64(len(t.proxies))] }
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
			client := newWebClient(transport)
			var resolved string
			if t.cfg.TikTokMethod == "app" {
				checkRedirect := client.CheckRedirect
				client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
					if err := checkRedirect(req, via); err != nil {
						return err
					}
					if videoID(req.URL.Path) != "" {
						resolved = req.URL.String()
						return http.ErrUseLastResponse
					}
					return nil
				}
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
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

func (t *TikTok) ExtractURL(ctx context.Context, raw string, refresh bool) (core.TikTokExtraction, error) {
	resolved, e := t.resolve(ctx, raw, refresh)
	if e != nil {
		return core.TikTokExtraction{}, e
	}
	return t.extractResolved(ctx, resolved, refresh)
}
func (t *TikTok) extractResolved(ctx context.Context, resolved core.TikTokResolution, refresh bool) (core.TikTokExtraction, error) {
	response, err := t.posts.do(ctx, resolved.SourceID, refresh, func() (core.TikTokExtraction, error) {
		release, err := t.gate.Acquire(ctx)
		if err != nil {
			return core.TikTokExtraction{}, err
		}
		defer release()
		ctx, cancel := context.WithTimeout(ctx, t.cfg.UpstreamTimeout)
		defer cancel()
		detail, asset, isApp, err := t.fetchMetadata(ctx, resolved)
		if err != nil {
			return core.TikTokExtraction{}, err
		}
		response, err := t.normalize(ctx, detail, asset, resolved, isApp)
		if err == nil {
			t.extractions.put(response.ExtractionID, response)
		}
		return response, err
	})
	if err == nil {
		response.SourceURL = resolved.SourceURL
		response.ResolvedURL = resolved.ResolvedURL
	}
	return response, err
}
func (t *TikTok) fetchMetadata(ctx context.Context, resolved core.TikTokResolution) (object, core.AssetContext, bool, error) {
	var err error
	var detail object
	var asset core.AssetContext
	isApp := t.cfg.TikTokMethod == "app"
	for attempt := 0; attempt < max(1, t.cfg.VideoInfoMaxRetries); attempt++ {
		if isApp {
			if t.app == nil {
				return nil, core.AssetContext{}, isApp, core.Upstream("TikTok app backend is unavailable")
			}
			var result core.AppDetail
			result, err = t.app.Detail(ctx, resolved.SourceID)
			if err == nil {
				detail = appDetail(result.Response, resolved.SourceID)
				if len(detail) == 0 {
					err = core.Upstream("TikTok app did not return the requested post")
				}
				asset = core.AssetContext{Platform: "tiktok", Referer: resolved.ResolvedURL, IdentityID: result.IdentityID, ProxyURL: result.ProxyURL, Headers: http.Header{"User-Agent": []string{result.UserAgent}}}
			}
		} else {
			detail, asset, err = t.fetchWeb(ctx, resolved.ResolvedURL, resolved.SourceID)
		}
		if err == nil {
			break
		}
		if !retryable(err) || ctx.Err() != nil {
			break
		}
		if attempt+1 < max(1, t.cfg.VideoInfoMaxRetries) {
			if waitErr := sleepContext(ctx, t.cfg.RetryDelay); waitErr != nil {
				return nil, core.AssetContext{}, isApp, waitErr
			}
		}
	}
	if err != nil {
		return nil, core.AssetContext{}, isApp, err
	}
	return detail, asset, isApp, nil
}

func (t *TikTok) GetExtraction(ctx context.Context, id string) (core.TikTokExtraction, error) {
	if e := ctx.Err(); e != nil {
		return core.TikTokExtraction{}, e
	}
	if response, ok := t.extractions.get(id); ok {
		return response, nil
	}
	return core.TikTokExtraction{}, core.MissingExtraction()
}
func (t *TikTok) ExtractMusic(ctx context.Context, id string, refresh bool) (core.TikTokMusic, error) {
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return core.TikTokMusic{}, core.InvalidLink("A numeric TikTok video ID is required")
	}
	return t.music.do(ctx, id, refresh, func() (core.TikTokMusic, error) {
		if !refresh {
			if post, ok := t.posts.get(id); ok && post.Music != nil && post.Music.Audio != nil {
				if expires, err := time.Parse(time.RFC3339Nano, post.ExpiresAt); err == nil && expires.After(time.Now()) {
					return musicResponse(post.ExtractionID, id, post.ExpiresAt, post.Music), nil
				}
			}
		}
		release, e := t.gate.Acquire(ctx)
		if e != nil {
			return core.TikTokMusic{}, e
		}
		defer release()
		ctx, cancel := context.WithTimeout(ctx, t.cfg.UpstreamTimeout)
		defer cancel()
		raw := "https://www.tiktok.com/@_/video/" + id
		detail, asset, _, e := t.fetchMetadata(ctx, core.TikTokResolution{SourceID: id, SourceURL: raw, ResolvedURL: raw})
		if e != nil {
			return core.TikTokMusic{}, e
		}
		extraction := core.NewID()
		asset.ExtractionID = extraction
		expires := time.Now().UTC().Add(t.cfg.CacheTTL)
		music := t.musicMetadata(obj(detail["music"]), asset, id, expires)
		if music == nil || music.Audio == nil {
			return core.TikTokMusic{}, core.Upstream("TikTok post has no music audio")
		}
		return musicResponse(extraction, id, expires.Format(time.RFC3339Nano), music), nil
	})
}

func (t *TikTok) fetchWeb(ctx context.Context, raw, id string) (object, core.AssetContext, error) {
	proxy := t.proxy()
	transport, e := t.transports.get(proxy)
	if e != nil {
		return nil, core.AssetContext{}, e
	}
	client := newWebClient(transport)
	detail, asset, err := t.fetchWebWithClient(ctx, client, proxy, raw, id, nil)
	if restriction := webContentRestriction(err); restriction != nil && t.cfg.TikTokSessionID != "" {
		u := &url.URL{Scheme: "https", Host: "www.tiktok.com", Path: "/"}
		for _, name := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
			client.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: t.cfg.TikTokSessionID, Domain: ".tiktok.com", Path: "/", Secure: true, HttpOnly: true}})
		}
		detail, asset, err = t.fetchWebWithClient(ctx, client, proxy, raw, id, restriction)
		if webContentRestriction(err) != nil {
			err = restriction
		}
	}
	return detail, asset, err
}
func (t *TikTok) fetchWebWithClient(ctx context.Context, client *http.Client, proxy, raw, id string, restriction *core.Error) (object, core.AssetContext, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		req.Header.Set("User-Agent", browserAgent)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		res, e := client.Do(req)
		if e != nil {
			return nil, core.AssetContext{}, networkError(ctx, e)
		}
		body, e := limitedRead(res.Body, metadataLimit)
		res.Body.Close()
		if e != nil {
			return nil, core.AssetContext{}, core.Upstream("TikTok page could not be read")
		}
		if res.StatusCode >= 400 {
			accessDenied := res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden
			if restriction != nil && accessDenied {
				return nil, core.AssetContext{}, restriction
			}
			if accessDenied && res.Header.Get("tt_block") == "" && !webBotChallengePattern.Match(body) && !challengeElementPattern.Match(body) {
				_, status, parseErr := ParseWebDetail(body, id)
				var failure *core.Error
				if errors.As(parseErr, &failure) {
					return nil, core.AssetContext{}, failure
				}
				switch status {
				case 10216, 10222, 10204:
					return nil, core.AssetContext{}, webStatus(status)
				}
				if res.StatusCode == http.StatusUnauthorized {
					return nil, core.AssetContext{}, core.NewError("content_restricted", "TikTok requires login for this content", 403)
				}
			}
			return nil, core.AssetContext{}, statusError(res.StatusCode, "TikTok webpage")
		}
		if res.Request.URL.Path == "/login" {
			return nil, core.AssetContext{}, core.NewError("content_private", "TikTok requires login for this content", 403)
		}
		detail, status, parseErr := ParseWebDetail(body, id)
		var parseFailure *core.Error
		if errors.As(parseErr, &parseFailure) {
			return nil, core.AssetContext{}, parseFailure
		}
		if status != 0 {
			return nil, core.AssetContext{}, webStatus(status)
		}
		if len(detail) > 0 {
			if str(detail["id"]) != "" && str(detail["id"]) != id {
				return nil, core.AssetContext{}, core.Upstream("TikTok returned a different post")
			}
			return detail, core.AssetContext{Platform: "tiktok", Referer: raw, ProxyURL: proxy, CookieJar: client.Jar, Headers: http.Header{"User-Agent": []string{browserAgent}}}, nil
		}
		if attempt == 0 {
			solved, e := solveWebChallenge(ctx, body, client.Jar)
			if e != nil {
				return nil, core.AssetContext{}, core.Upstream("TikTok webpage challenge failed")
			}
			if solved {
				continue
			}
		}
		_ = parseErr
		return nil, core.AssetContext{}, core.Upstream("TikTok page contains no post metadata")
	}
	return nil, core.AssetContext{}, core.Upstream("TikTok page contains no post metadata")
}
func appDetail(response object, id string) object {
	if detail := obj(response["aweme_detail"]); str(detail["aweme_id"]) == id {
		return detail
	}
	for _, v := range arr(response["aweme_details"]) {
		detail := obj(v)
		if str(detail["aweme_id"]) == id {
			return detail
		}
	}
	if str(response["aweme_id"]) == id {
		return response
	}
	return nil
}
func webContentRestriction(err error) *core.Error {
	var failure *core.Error
	if errors.As(err, &failure) {
		switch failure.Code {
		case "content_private", "content_restricted", "region_blocked":
			return failure
		}
	}
	return nil
}

func webStatus(status int) error {
	switch status {
	case 10216, 10222:
		return core.NewError("content_private", "TikTok content is private or requires login", 403)
	case 10204:
		return core.NewError("region_blocked", "TikTok content is not available in this region", 451)
	case 10201, 10202:
		return core.NewError("content_deleted", "TikTok post was not found", 404)
	default:
		return core.Upstream("TikTok did not return this post")
	}
}
func statusError(status int, operation string) error {
	switch status {
	case 404, 410:
		return core.NewError("content_deleted", operation+" was not found", 404)
	case 429:
		return core.NewError("upstream_rate_limited", operation+" rate limit exceeded", 429)
	case 401, 403:
		return core.NewError("upstream_network_error", operation+" was rejected", 502)
	default:
		return core.NewError("upstream_network_error", operation+" failed", 502)
	}
}
func networkError(ctx context.Context, e error) error {
	var existing *core.Error
	if errors.As(e, &existing) {
		return e
	}
	if errors.Is(e, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	var timeout net.Error
	if errors.Is(e, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(e, &timeout) && timeout.Timeout()) {
		return core.NewError("upstream_timeout", "Upstream request timed out", 504)
	}
	return core.NewError("upstream_network_error", "Upstream network request failed", 502)
}
func retryable(e error) bool {
	var api *core.Error
	if errors.As(e, &api) {
		return (api.Status >= 500 || api.Status == 429) && api.Code != "service_not_configured" && api.Code != "service_busy"
	}
	return false
}

func musicResponse(extraction, id, expires string, music *core.MusicMetadata) core.TikTokMusic {
	return core.TikTokMusic{ExtractionID: extraction, Platform: "tiktok", SourceID: id, Title: music.Title, Author: music.Author, DurationSeconds: music.DurationSeconds, Cover: music.Cover, Audio: *music.Audio, ExpiresAt: expires}
}
