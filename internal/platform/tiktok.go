package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
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
		detail, asset, err := t.fetchMetadata(ctx, resolved)
		if err != nil {
			return core.TikTokExtraction{}, err
		}
		response, err := t.normalize(ctx, detail, asset, resolved)
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
func (t *TikTok) fetchMetadata(ctx context.Context, resolved core.TikTokResolution) (object, core.AssetContext, error) {
	if t.app == nil {
		return nil, core.AssetContext{}, core.Upstream("TikTok app backend is unavailable")
	}
	var err error
	for attempt := 0; attempt < max(1, t.cfg.VideoInfoMaxRetries); attempt++ {
		var result core.AppDetail
		result, err = t.app.Detail(ctx, resolved.SourceID)
		if err == nil {
			detail := appDetail(result.Response, resolved.SourceID)
			if len(detail) > 0 {
				asset := core.AssetContext{Platform: "tiktok", Referer: resolved.ResolvedURL, IdentityID: result.IdentityID, Headers: http.Header{"User-Agent": []string{result.UserAgent}}}
				return detail, asset, nil
			}
			err = core.Upstream("TikTok app did not return the requested post")
		}
		if !retryable(err) || ctx.Err() != nil {
			break
		}
		if attempt+1 < max(1, t.cfg.VideoInfoMaxRetries) {
			if waitErr := sleepContext(ctx, t.cfg.RetryDelay); waitErr != nil {
				return nil, core.AssetContext{}, waitErr
			}
		}
	}
	return nil, core.AssetContext{}, err
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
		detail, asset, e := t.fetchMetadata(ctx, core.TikTokResolution{SourceID: id, SourceURL: raw, ResolvedURL: raw})
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
