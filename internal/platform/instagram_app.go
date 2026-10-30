package platform

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

const instagramAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func instagramMediaID(shortcode string) (string, error) {
	if len(shortcode) == 0 || len(shortcode) > 20 {
		return "", core.InvalidLink("Invalid Instagram shortcode")
	}
	value := new(big.Int)
	for _, character := range shortcode {
		digit := strings.IndexRune(instagramAlphabet, character)
		if digit < 0 {
			return "", core.InvalidLink("Invalid Instagram shortcode")
		}
		value.Lsh(value, 6)
		value.Add(value, big.NewInt(int64(digit)))
	}
	if value.Sign() <= 0 {
		return "", core.InvalidLink("Invalid Instagram shortcode")
	}
	return value.String(), nil
}

func (i *Instagram) extractApp(ctx context.Context, normalized, shortcode, raw string) (core.InstagramExtraction, error) {
	if strings.Contains(strings.ToLower(normalized), "/stories/") {
		return core.InstagramExtraction{}, core.InvalidLink("Instagram app extraction requires a canonical post or Reel URL")
	}
	mediaID, err := instagramMediaID(shortcode)
	if err != nil {
		return core.InstagramExtraction{}, err
	}
	if i.app == nil {
		return core.InstagramExtraction{}, core.NewError("service_not_configured", "Instagram accounts are not configured", 503)
	}
	detail, err := i.app.Detail(ctx, mediaID)
	if err != nil {
		return core.InstagramExtraction{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.InstagramExtraction{}, err
	}
	if !strings.HasPrefix(detail.IdentityID, "instagram:") || len(detail.IdentityID) == len("instagram:") {
		return core.InstagramExtraction{}, core.Upstream("Instagram app did not provide an account route")
	}
	media := instagramAppMedia(detail.Response, mediaID, shortcode)
	if media == nil {
		return core.InstagramExtraction{}, core.Upstream("Instagram app did not return the requested post")
	}
	return i.normalizeApp(media, detail.IdentityID, normalized, shortcode, raw)
}

// Never use the floating-point pk field: media IDs exceed JavaScript's integer
// precision, and an unrelated item must not be returned as the requested post.
func instagramAppMedia(payload object, mediaID, shortcode string) object {
	candidates := append([]any(nil), arr(payload["items"])...)
	for _, entry := range arr(payload["clips_items"]) {
		clip := obj(entry)
		if media := obj(clip["media"]); len(media) > 0 {
			candidates = append(candidates, media)
		} else {
			candidates = append(candidates, clip)
		}
	}
	var match object
	for _, entry := range candidates {
		media := obj(entry)
		identity, _ := first(media["id"], media["strong_id__"]).(string)
		prefix, _, _ := strings.Cut(identity, "_")
		if prefix != mediaID || str(media["code"]) != "" && str(media["code"]) != shortcode {
			continue
		}
		if match == nil || len(arr(media["video_versions"])) > 0 || len(arr(match["video_versions"])) == 0 {
			match = media
		}
	}
	return match
}

type instagramRendition struct {
	url, fallback     string
	width, height     int
	bandwidth, format int64
}

// selectInstagramRendition preserves service order for equally ranked resources.
// Only the best rendition is used, so sorting and parsing every discarded URL
// needlessly adds work for photos and carousels with many candidate sizes.
func selectInstagramRendition(values []any, video bool) (instagramRendition, bool) {
	var best instagramRendition
	var fallback any
	found := false
	for _, value := range values {
		item := obj(value)
		candidate := instagramRendition{width: max(0, integer(item["width"])), height: max(0, integer(item["height"])), bandwidth: number(item["bandwidth"]), format: number(item["type"])}
		if found {
			pixels, bestPixels := int64(candidate.width)*int64(candidate.height), int64(best.width)*int64(best.height)
			better := pixels > bestPixels
			if video && pixels == bestPixels {
				better = candidate.bandwidth > best.bandwidth || candidate.bandwidth == best.bandwidth && candidate.format > best.format
			}
			if !better {
				continue
			}
		}
		candidate.url = instagramMediaURL(item["url"])
		if candidate.url == "" {
			continue
		}
		best, fallback, found = candidate, item["fallback"], true
	}
	if found {
		best.fallback = instagramMediaURL(fallback)
	}
	return best, found
}

func instagramMediaURL(value any) string {
	raw, ok := value.(string)
	if !ok {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return raw
}

type instagramPreparedItem struct {
	kind, extension, contentType string
	selected                     instagramRendition
	cover                        *instagramRendition
	duration                     int
}

func (i *Instagram) prepareInstagramItem(media object) (instagramPreparedItem, error) {
	var result instagramPreparedItem
	image, hasImage := selectInstagramRendition(arr(obj(media["image_versions2"])["candidates"]), false)
	if hasImage {
		result.cover = &image
	}
	switch integer(media["media_type"]) {
	case 1:
		if !hasImage {
			return result, core.Upstream("Instagram photo has no image candidates")
		}
		result.kind, result.extension = "image", "jpg"
		result.selected = image
	case 2:
		video, hasVideo := selectInstagramRendition(arr(media["video_versions"]), true)
		if !hasVideo {
			return result, core.Upstream("Instagram video has no progressive MP4 rendition")
		}
		seconds := decimal(media["video_duration"])
		if i.cfg.MaxVideoDuration > 0 && seconds > float64(i.cfg.MaxVideoDuration) {
			return result, core.NewError("content_too_long", "Instagram video exceeds MAX_VIDEO_DURATION", 413)
		}
		if seconds > 0 && seconds < float64(math.MaxInt32) {
			result.duration = int(math.Ceil(seconds))
		}
		result.kind, result.extension, result.contentType = "video", "mp4", "video/mp4"
		result.selected = video
	default:
		return result, core.Upstream("Instagram response contains an unsupported media type")
	}
	if result.selected.width == 0 {
		result.selected.width = max(0, integer(media["original_width"]))
	}
	if result.selected.height == 0 {
		result.selected.height = max(0, integer(media["original_height"]))
	}
	return result, nil
}

func (i *Instagram) normalizeApp(media object, identityID, normalized, shortcode, raw string) (core.InstagramExtraction, error) {
	children := []any{media}
	carousel := integer(media["media_type"]) == 8 || media["carousel_media"] != nil
	if carousel {
		children = arr(media["carousel_media"])
		if len(children) == 0 || media["carousel_media_count"] != nil && integer(media["carousel_media_count"]) != len(children) {
			return core.InstagramExtraction{}, core.Upstream("Instagram carousel response is incomplete")
		}
	}
	// Validate every child before registering assets: incomplete carousels are
	// errors, never apparently successful posts with missing slides.
	prepared := make([]instagramPreparedItem, 0, len(children))
	for _, child := range children {
		item, err := i.prepareInstagramItem(obj(child))
		if err != nil {
			return core.InstagramExtraction{}, err
		}
		prepared = append(prepared, item)
	}
	extraction := core.NewID()
	expires := time.Now().UTC().Add(i.cfg.CacheTTL)
	base := core.AssetContext{Platform: "instagram", IdentityID: identityID, ExtractionID: extraction}
	register := func(rendition instagramRendition, kind, filename, contentType string, duration, position int) core.AssetDescriptor {
		asset := base
		asset.Kind, asset.Filename, asset.ContentType = kind, filename, contentType
		asset.UpstreamURL = rendition.url
		if rendition.fallback != "" && rendition.fallback != rendition.url {
			asset.AlternateURLs = []string{rendition.fallback}
		}
		asset.DurationSeconds = duration
		return i.assets.RegisterUntil(asset, position, expires)
	}
	result := core.InstagramExtraction{
		ExtractionID: extraction, Platform: "instagram", Method: "app", SourceID: shortcode, SourceURL: raw, ResolvedURL: normalized,
		CreatorUsername: instagramCreator(media), Likes: numeric(media["like_count"]), Views: numeric(first(media["play_count"], media["ig_play_count"], media["view_count"])), Comments: numeric(media["comment_count"]),
		Caption: nonempty(str(first(obj(media["caption"])["text"], media["caption"]))), ExpiresAt: expires.Format(time.RFC3339Nano), Media: make([]core.InstagramMedia, 0, len(prepared)),
	}
	if timestamp := number(media["taken_at"]); timestamp > 0 && timestamp <= 253402300799 {
		result.CreatedAt = core.Ptr(time.Unix(timestamp, 0).UTC().Format(time.RFC3339))
	}
	if eligible, ok := media["is_third_party_downloads_eligible"].(bool); ok {
		result.ThirdPartyDownloadEligible = &eligible
	}
	for position, prepared := range prepared {
		stem := shortcode
		if carousel {
			stem = fmt.Sprintf("%s_%d", shortcode, position+1)
		}
		item := core.InstagramMedia{Position: position, MediaType: prepared.kind, Width: positive(prepared.selected.width), Height: positive(prepared.selected.height), DurationSeconds: positive(prepared.duration)}
		if item.Width != nil && item.Height != nil {
			item.Quality = core.Ptr(fmt.Sprintf("%dx%d", *item.Width, *item.Height))
		}
		item.Asset = register(prepared.selected, prepared.kind, stem+"."+prepared.extension, prepared.contentType, prepared.duration, position)
		if prepared.cover != nil {
			thumbnail := register(*prepared.cover, "thumbnail", stem+"_thumbnail.jpg", "", 0, position)
			item.Thumbnail = &thumbnail
			if position == 0 {
				cover := register(*prepared.cover, "cover", shortcode+"_cover.jpg", "", 0, 0)
				result.Cover = &cover
			}
		}
		result.Media = append(result.Media, item)
	}
	result.ContentType = prepared[0].kind
	if carousel {
		result.ContentType = "carousel"
	} else {
		result.Width, result.Height, result.DurationSeconds = result.Media[0].Width, result.Media[0].Height, result.Media[0].DurationSeconds
	}
	result.Music = i.instagramMusic(media, base, shortcode, expires)
	return result, nil
}

func (i *Instagram) instagramMusic(media object, base core.AssetContext, shortcode string, expires time.Time) *core.MusicMetadata {
	clips := obj(media["clips_metadata"])
	music := obj(obj(clips["music_info"])["music_asset_info"])
	original := false
	if len(music) == 0 {
		music = obj(clips["original_sound_info"])
		original = true
	}
	if len(music) == 0 {
		return nil
	}
	result := &core.MusicMetadata{Title: str(music["title"]), Author: str(music["display_artist"]), DurationSeconds: max(0, integer(music["duration_in_ms"])/1000)}
	if original {
		result.Title = str(music["original_audio_title"])
		result.Author = str(obj(music["ig_artist"])["username"])
	}
	if url := instagramMediaURL(first(music["cover_artwork_uri"], music["cover_artwork_thumbnail_uri"])); url != "" {
		asset := base
		asset.Kind, asset.Filename, asset.UpstreamURL = "cover", shortcode+"_music_cover.jpg", url
		descriptor := i.assets.RegisterUntil(asset, 0, expires)
		result.Cover = &descriptor
	}
	return result
}
