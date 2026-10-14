package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func token() string { return core.NewID() }

func (t *TikTok) normalize(ctx context.Context, data object, base core.AssetContext, resolved core.TikTokResolution, app bool) (core.TikTokExtraction, error) {
	if err := ctx.Err(); err != nil {
		return core.TikTokExtraction{}, err
	}
	id := resolved.SourceID
	extraction := token()
	expiresAt := time.Now().UTC().Add(t.cfg.CacheTTL)
	expires := expiresAt.Format(time.RFC3339Nano)
	base.ExtractionID = extraction
	register := func(source []string, kind, name, contentType string, position int) *core.AssetDescriptor {
		if len(source) == 0 {
			return nil
		}
		asset := base
		asset.Kind = kind
		asset.Filename = name
		asset.UpstreamURL = source[0]
		asset.AlternateURLs = source[1:]
		asset.ContentType = contentType
		d := t.assets.RegisterUntil(asset, position, expiresAt)
		return &d
	}
	author := obj(data["author"])
	stats := obj(data["stats"])
	if app {
		stats = obj(data["statistics"])
	}
	result := core.TikTokExtraction{ExtractionID: extraction, Platform: "tiktok", SourceID: id, SourceURL: resolved.SourceURL, ResolvedURL: resolved.ResolvedURL, CreatorUsername: nonempty(strings.TrimSpace(str(first(author["uniqueId"], author["unique_id"])))), Media: []core.AssetDescriptor{}, ExpiresAt: expires, Likes: numeric(first(stats["diggCount"], stats["digg_count"])), Views: numeric(first(stats["playCount"], stats["play_count"]))}
	result.Music = t.musicMetadata(obj(data["music"]), base, id, expiresAt)
	images := obj(first(data["imagePost"], data["image_post_info"]))
	if len(images) > 0 {
		result.ContentType = "slideshow"
		for i, raw := range arr(images["images"]) {
			item := obj(raw)
			address := first(item["imageURL"], item["display_image"], item["owner_watermark_image"], item["user_watermark_image"])
			if d := register(urls(address), "image", fmt.Sprintf("%s_%d.jpg", id, i+1), "", i); d != nil {
				result.Media = append(result.Media, *d)
			}
		}
		if len(result.Media) == 0 {
			return core.TikTokExtraction{}, core.Upstream("TikTok slideshow has no image assets")
		}
		return result, nil
	}
	video := obj(data["video"])
	duration := integer(video["duration"])
	if app {
		duration /= 1000
	}
	if t.cfg.MaxVideoDuration > 0 && duration > t.cfg.MaxVideoDuration {
		return core.TikTokExtraction{}, core.NewError("content_too_long", "TikTok video exceeds MAX_VIDEO_DURATION", 413)
	}
	selected := selectVideo(video, app)
	if selected == nil {
		return core.TikTokExtraction{}, core.Upstream("TikTok response has no video asset")
	}

	result.ContentType = "video"
	result.Width = positive(selected.Width)
	result.Height = positive(selected.Height)
	result.DurationSeconds = positive(duration)
	asset := base
	asset.Kind = "video"
	asset.Filename = id + ".mp4"
	asset.UpstreamURL = selected.URLs[0]
	asset.AlternateURLs = selected.URLs[1:]
	asset.ContentType = "video/mp4"
	asset.DurationSeconds = duration
	if len(selected.AudioURLs) > 0 {
		asset.Audio = &core.AuxiliaryAsset{UpstreamURL: selected.AudioURLs[0], AlternateURLs: selected.AudioURLs[1:], ContentType: "audio/mp4", Cookies: base.Cookies}
	}
	result.Media = append(result.Media, t.assets.RegisterUntil(asset, 0, expiresAt))
	result.Cover = register(firstURLs(video["cover"], video["originCover"], video["origin_cover"]), "cover", id+"_cover.jpg", "", 0)
	return result, nil
}

func (t *TikTok) musicMetadata(music object, base core.AssetContext, id string, expires time.Time) *core.MusicMetadata {
	if len(music) == 0 {
		return nil
	}
	register := func(source []string, kind, name, mime string) *core.AssetDescriptor {
		if len(source) == 0 {
			return nil
		}
		asset := base
		asset.Kind = kind
		asset.Filename = name
		asset.UpstreamURL = source[0]
		asset.AlternateURLs = source[1:]
		asset.ContentType = mime
		d := t.assets.RegisterUntil(asset, 0, expires)
		return &d
	}
	return &core.MusicMetadata{Title: str(music["title"]), Author: str(first(music["authorName"], music["author"])), DurationSeconds: integer(music["duration"]), Audio: register(firstURLs(music["playUrl"], music["play_url"]), "audio", id+".mp3", "audio/mpeg"), Cover: register(firstURLs(music["coverLarge"], music["cover_large"], music["coverMedium"], music["cover_medium"], music["coverThumb"], music["cover_thumb"]), "cover", id+"_music_cover.jpg", "")}
}
