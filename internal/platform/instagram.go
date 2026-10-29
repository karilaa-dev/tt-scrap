package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

const instagramHost = "instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com"

var instagramPath = regexp.MustCompile(`(?i)^/(?:(?:p|reels?|tv)/[A-Za-z0-9_-]+|stories/[A-Za-z0-9_.-]+(?:/[0-9]+)?)/?$`)

type InstagramAttemptRecorder interface {
	Attempt(context.Context, string, bool, func() (core.InstagramExtraction, error)) (core.InstagramExtraction, error)
}

type Instagram struct {
	Attempts    InstagramAttemptRecorder
	cfg         core.Config
	app         core.InstagramBackend
	assets      *core.Assets
	client      *http.Client
	posts       *memo[core.InstagramExtraction]
	extractions *memo[core.InstagramExtraction]
	gate        *core.Limiter
}

func NewInstagram(cfg core.Config, assets *core.Assets, backend ...core.InstagramBackend) *Instagram {
	var app core.InstagramBackend
	if len(backend) > 0 {
		app = backend[0]
	}
	limit := min(cfg.InstagramConcurrency, cfg.ExtractionConcurrency)
	if limit < 1 {
		limit = 4
	}
	cacheCapacity := core.CacheBudget(cfg.CacheMaxEntries).Responses
	return &Instagram{cfg: cfg, assets: assets, app: app, client: &http.Client{Transport: newTransport(cfg.HTTPMaxConnections), Timeout: cfg.InstagramTimeout}, posts: newMemo[core.InstagramExtraction](cfg.CacheTTL, cacheCapacity, cfg.CacheMaxEntries), extractions: newMemo[core.InstagramExtraction](cfg.CacheTTL, cacheCapacity, cfg.CacheMaxEntries), gate: core.NewLimiter(limit, cfg.MaxQueuedRequests)}
}
func (i *Instagram) Close() { i.client.CloseIdleConnections() }
func normalizeInstagram(raw string) (string, string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil {
		return "", "", core.InvalidLink("Invalid Instagram URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if u.Scheme != "https" || (host != "instagram.com" && host != "www.instagram.com") || u.User != nil {
		return "", "", core.InvalidLink("Only HTTPS Instagram URLs are accepted")
	}
	if !instagramPath.MatchString(u.Path) {
		return "", "", core.InvalidLink("Unsupported Instagram URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	id := parts[1]
	if strings.EqualFold(parts[0], "stories") && len(parts) > 2 {
		id = parts[2]
	}
	return "https://www.instagram.com" + strings.TrimRight(u.EscapedPath(), "/") + "/", id, nil
}
func (i *Instagram) ExtractURL(ctx context.Context, raw string, refresh bool) (core.InstagramExtraction, error) {
	normalized, id, e := normalizeInstagram(raw)
	if e != nil {
		return core.InstagramExtraction{}, e
	}
	// Canonical post routes are aliases for one shortcode. Sharing both the
	// cache and in-flight work avoids repeating authenticated requests when bots
	// receive different URL forms for the same post. Legacy stories stay URL-keyed.
	key := normalized
	if i.cfg.InstagramMethod != "legacy" && !strings.Contains(strings.ToLower(normalized), "/stories/") {
		key = "post:" + id
	}
	response, e := i.posts.do(ctx, key, refresh, func() (core.InstagramExtraction, error) {
		var result core.InstagramExtraction
		var err error
		if i.cfg.InstagramMethod == "legacy" {
			result, err = i.attempt(ctx, normalized, id, raw, true)
		} else {
			result, err = i.attempt(ctx, normalized, id, raw, false)
			var failure *core.Error
			policyFailure := errors.As(err, &failure) && (failure.Code == "content_too_long" || failure.Code == "instagram_database_unavailable")
			if err != nil && i.cfg.InstagramLegacyFallback && ctx.Err() == nil && !policyFailure && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				result, err = i.attempt(ctx, normalized, id, raw, true)
			}
		}
		if err == nil {
			i.extractions.put(result.ExtractionID, result)
		}
		return result, err
	})
	if e == nil {
		response.SourceURL = raw
		response.ResolvedURL = normalized
	}
	return response, e
}

func (i *Instagram) attempt(ctx context.Context, normalized, id, raw string, legacy bool) (core.InstagramExtraction, error) {
	run := func() (core.InstagramExtraction, error) {
		if legacy {
			return i.extractLegacy(ctx, normalized, id, raw)
		}
		return i.extractApp(ctx, normalized, id, raw)
	}
	if i.Attempts != nil {
		return i.Attempts.Attempt(ctx, id, legacy, run)
	}
	return run()
}

// Ready reports whether the selected method has an available credential source.
func (i *Instagram) Ready() bool {
	legacy := strings.TrimSpace(i.cfg.RapidAPIKey) != ""
	if i.cfg.InstagramMethod == "legacy" {
		return legacy
	}
	ready := i.app != nil
	if backend, ok := i.app.(interface{ Ready() bool }); ok {
		ready = backend.Ready()
	} else if backend, ok := i.app.(interface{ Count() int }); ok {
		ready = backend.Count() > 0
	}
	return ready || i.cfg.InstagramLegacyFallback && legacy
}

func (i *Instagram) extractLegacy(ctx context.Context, normalized, id, raw string) (core.InstagramExtraction, error) {
	data, e := i.fetch(ctx, normalized)
	if e != nil {
		return core.InstagramExtraction{}, e
	}
	extraction := core.NewID()
	expires := time.Now().UTC().Add(i.cfg.CacheTTL)
	response := core.InstagramExtraction{Platform: "instagram", ExtractionID: extraction, SourceID: id, SourceURL: raw, ResolvedURL: normalized, Method: "legacy", ExpiresAt: expires.Format(time.RFC3339Nano), Media: []core.InstagramMedia{}}
	response.CreatorUsername = instagramCreator(data)
	var usable []object
	for _, v := range arr(data["media"]) {
		media := obj(v)
		if str(media["url"]) != "" {
			usable = append(usable, media)
		}
	}
	for p, media := range usable {
		kind, ext, mime := "image", "jpg", ""
		if str(media["type"]) == "video" {
			kind, ext, mime = "video", "mp4", "video/mp4"
		}
		stem := id
		if len(usable) > 1 {
			stem = fmt.Sprintf("%s_%d", id, p+1)
		}
		asset := i.assets.RegisterUntil(core.AssetContext{Platform: "instagram", Kind: kind, Filename: stem + "." + ext, ContentType: mime, UpstreamURL: str(media["url"]), ExtractionID: extraction}, p, expires)
		item := core.InstagramMedia{Position: p, MediaType: kind, Asset: asset, Quality: nonempty(str(media["quality"]))}
		if u := str(media["thumbnail"]); u != "" {
			d := i.assets.RegisterUntil(core.AssetContext{Platform: "instagram", Kind: "thumbnail", Filename: stem + "_thumbnail.jpg", UpstreamURL: u, ExtractionID: extraction}, p, expires)
			item.Thumbnail = &d
		}
		response.Media = append(response.Media, item)
	}
	if len(response.Media) == 0 {
		return core.InstagramExtraction{}, core.NewError("content_deleted", "Instagram response contained no usable media", 404)
	}
	response.ContentType = response.Media[0].MediaType
	if len(response.Media) > 1 {
		response.ContentType = "carousel"
	}
	return response, nil
}

func (i *Instagram) GetExtraction(ctx context.Context, id string) (core.InstagramExtraction, error) {
	if e := ctx.Err(); e != nil {
		return core.InstagramExtraction{}, e
	}
	if response, ok := i.extractions.get(id); ok {
		return response, nil
	}
	return core.InstagramExtraction{}, core.MissingExtraction()
}
func (i *Instagram) fetch(ctx context.Context, source string) (object, error) {
	if i.cfg.RapidAPIKey == "" {
		return nil, core.Upstream("RAPIDAPI_KEY is not configured")
	}
	host := i.cfg.RapidAPIHost
	if host == "" {
		host = instagramHost
	}
	base := i.cfg.RapidAPIBaseURL
	if base == "" {
		base = "https://" + host
	}
	var last error
	for attempt := 0; attempt < max(1, i.cfg.InstagramMaxAttempts); attempt++ {
		release, e := i.gate.Acquire(ctx)
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/convert?url="+url.QueryEscape(source), nil)
		if e != nil {
			release()
			return nil, e
		}
		req.Header.Set("X-Rapidapi-Key", i.cfg.RapidAPIKey)
		req.Header.Set("X-Rapidapi-Host", host)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		res, e := i.client.Do(req)
		var body []byte
		status := 0
		retryDelay := i.cfg.InstagramRetryDelay * time.Duration(1<<attempt)
		if e == nil {
			status = res.StatusCode
			body, e = limitedRead(res.Body, metadataLimit)
			res.Body.Close()
			if status == 429 {
				retryDelay = max(retryDelay, time.Second)
				if seconds := integer(res.Header.Get("Retry-After")); seconds > 0 {
					retryDelay = max(retryDelay, time.Duration(seconds)*time.Second)
				}
			}
		}
		release()
		if e != nil {
			last = networkError(ctx, e)
		} else if status == http.StatusOK {
			payload, e := decode(body)
			if e != nil {
				return nil, core.Upstream("Instagram API returned an invalid payload")
			}
			return payload, nil
		} else {
			last = statusError(status, "Instagram API")
			if !retryable(last) {
				return nil, last
			}
		}
		if attempt+1 < max(1, i.cfg.InstagramMaxAttempts) {
			if e = sleepContext(ctx, min(retryDelay, 5*time.Second)); e != nil {
				return nil, e
			}
		}
	}
	return nil, last
}

func instagramCreator(data object) *string {
	for _, candidate := range []any{data["username"], obj(data["owner"])["username"], obj(data["author"])["username"], obj(data["user"])["username"]} {
		if value, ok := candidate.(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return &value
			}
		}
	}
	return nil
}
