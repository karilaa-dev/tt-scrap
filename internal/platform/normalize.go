package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func (t *TikTok) normalize(ctx context.Context, data object, base core.AssetContext, resolved core.TikTokResolution) (core.TikTokExtraction, error) {
	if err := ctx.Err(); err != nil {
		return core.TikTokExtraction{}, err
	}
	id := resolved.SourceID
	extraction := core.NewID()
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
	stats := obj(data["statistics"])
	result := core.TikTokExtraction{ExtractionID: extraction, Platform: "tiktok", SourceID: id, SourceURL: resolved.SourceURL, ResolvedURL: resolved.ResolvedURL, CreatorUsername: nonempty(strings.TrimSpace(str(author["unique_id"]))), Media: []core.AssetDescriptor{}, ExpiresAt: expires, Likes: numeric(stats["digg_count"]), Views: numeric(stats["play_count"])}
	result.Music = t.musicMetadata(obj(data["music"]), base, id, expiresAt)
	images := obj(data["image_post_info"])
	if len(images) > 0 {
		result.ContentType = "slideshow"
		for i, raw := range arr(images["images"]) {
			item := obj(raw)
			address := first(item["display_image"], item["owner_watermark_image"], item["user_watermark_image"])
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
	duration := integer(video["duration"]) / 1000
	if t.cfg.MaxVideoDuration > 0 && duration > t.cfg.MaxVideoDuration {
		return core.TikTokExtraction{}, core.NewError("content_too_long", "TikTok video exceeds MAX_VIDEO_DURATION", 413)
	}
	selected := selectVideo(video)
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
	asset.ExpectedWidth = selected.Width
	asset.ExpectedHeight = selected.Height
	result.Media = append(result.Media, t.assets.RegisterUntil(asset, 0, expiresAt))
	result.Cover = register(firstURLs(video["cover"], video["origin_cover"]), "cover", id+"_cover.jpg", "", 0)
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
	return &core.MusicMetadata{Title: str(music["title"]), Author: str(music["author"]), DurationSeconds: integer(music["duration"]), Audio: register(urls(music["play_url"]), "audio", id+".mp3", "audio/mpeg"), Cover: register(firstURLs(music["cover_large"], music["cover_medium"], music["cover_thumb"]), "cover", id+"_music_cover.jpg", "")}
}
