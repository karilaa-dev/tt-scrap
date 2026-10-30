package platform

import (
	"fmt"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func instagramBenchmarkMedia(kind string) object {
	images := make([]any, 0, 12)
	for index, width := range []int{1080, 750, 640, 480, 320, 240, 150, 1080, 640, 480, 320, 240} {
		images = append(images, object{"width": width, "height": width * 16 / 9, "url": fmt.Sprintf("https://cdn.example/image-%d.jpg?expires=1793100000&signature=fixture", index)})
	}
	media := object{"id": "3902057773219159336_123", "media_type": 1, "image_versions2": object{"candidates": images}, "user": object{"username": "creator"}, "like_count": 92819, "play_count": 2753477, "comment_count": 361, "caption": object{"text": "Benchmark fixture"}}
	if kind == "video" {
		media["media_type"] = 2
		media["video_duration"] = 50.366
		videos := make([]any, 0, 6)
		for index, width := range []int{720, 720, 640, 640, 480, 480} {
			videos = append(videos, object{"width": width, "height": width * 16 / 9, "bandwidth": 2000000 - index*100000, "type": 101 + index%3, "url": fmt.Sprintf("https://cdn.example/video-%d.mp4?expires=1793100000&signature=fixture", index), "fallback": fmt.Sprintf("https://fallback.example/video-%d.mp4?expires=1793100000&signature=fixture", index)})
		}
		media["video_versions"] = videos
	}
	if kind == "carousel" {
		media["media_type"] = 8
		media["carousel_media_count"] = 5
		media["carousel_media"] = []any{instagramBenchmarkMedia("photo"), instagramBenchmarkMedia("photo"), instagramBenchmarkMedia("video"), instagramBenchmarkMedia("photo"), instagramBenchmarkMedia("photo")}
	}
	return media
}

func BenchmarkInstagramPrepare(b *testing.B) {
	for _, kind := range []string{"photo", "video"} {
		b.Run(kind, func(b *testing.B) {
			media := instagramBenchmarkMedia(kind)
			service := &Instagram{cfg: core.DefaultConfig()}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				prepared, err := service.prepareInstagramItem(media)
				if err != nil || prepared.selected.url == "" {
					b.Fatal("invalid selection", err)
				}
			}
		})
	}
}

func BenchmarkInstagramNormalize(b *testing.B) {
	for _, kind := range []string{"photo", "video", "carousel"} {
		b.Run(kind, func(b *testing.B) {
			media := instagramBenchmarkMedia(kind)
			cfg := core.DefaultConfig()
			service := &Instagram{cfg: cfg, assets: core.NewAssets(cfg.CacheTTL, cfg.CacheMaxEntries)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				post, err := service.normalizeApp(media, "instagram:benchmark", "https://www.instagram.com/p/DYm5N7px50o/", "DYm5N7px50o", "https://www.instagram.com/p/DYm5N7px50o/")
				if err != nil || len(post.Media) == 0 {
					b.Fatal("invalid normalization", err)
				}
			}
		})
	}
}

func TestInstagramBestRenditionSelection(t *testing.T) {
	rendition := func(address string, width, height, bandwidth, format int) object {
		return object{"url": address, "width": width, "height": height, "bandwidth": bandwidth, "type": format}
	}
	for _, tc := range []struct {
		name    string
		video   bool
		values  []any
		wantURL string
	}{
		{"photo ignores bitrate and type ties", false, []any{rendition("https://cdn.example/first", 1080, 1920, 0, 0), rendition("https://cdn.example/second", 1080, 1920, 9999999, 999)}, "https://cdn.example/first"},
		{"photo prefers area", false, []any{rendition("https://cdn.example/wide", 1080, 720, 0, 0), rendition("https://cdn.example/portrait", 1000, 2000, 0, 0)}, "https://cdn.example/portrait"},
		{"video prefers area before bitrate", true, []any{rendition("https://cdn.example/small", 640, 360, 9999999, 999), rendition("https://cdn.example/large", 1280, 720, 1, 1)}, "https://cdn.example/large"},
		{"video prefers bandwidth before type", true, []any{rendition("https://cdn.example/type", 1280, 720, 1000000, 999), rendition("https://cdn.example/bandwidth", 1280, 720, 2000000, 1)}, "https://cdn.example/bandwidth"},
		{"video uses type for final ranking", true, []any{rendition("https://cdn.example/type1", 1280, 720, 2000000, 101), rendition("https://cdn.example/type2", 1280, 720, 2000000, 102)}, "https://cdn.example/type2"},
		{"video keeps first complete tie", true, []any{rendition("https://cdn.example/first", 1280, 720, 2000000, 101), rendition("https://cdn.example/second", 1280, 720, 2000000, 101)}, "https://cdn.example/first"},
		{"invalid best and tied urls cannot hide valid media", true, []any{rendition("https://:bad", 1920, 1080, 9999999, 999), rendition("file:///not-media", 1280, 720, 2000000, 101), rendition("https://user:secret@cdn.example/private", 1280, 720, 2000000, 101), rendition("https://cdn.example/valid", 1280, 720, 2000000, 101)}, "https://cdn.example/valid"},
		{"later invalid improvement leaves earlier valid selection", true, []any{rendition("https://cdn.example/valid", 1280, 720, 2000000, 101), rendition("http://cdn.example/insecure", 1920, 1080, 9999999, 999)}, "https://cdn.example/valid"},
		{"all invalid", false, []any{nil, rendition("http://cdn.example/a", 1920, 1080, 0, 0), rendition("https://", 640, 360, 0, 0)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, ok := selectInstagramRendition(tc.values, tc.video)
			if ok != (tc.wantURL != "") || selected.url != tc.wantURL {
				t.Fatalf("selected %q, usable=%t; want %q", selected.url, ok, tc.wantURL)
			}
		})
	}
	first := rendition("https://cdn.example/a", 640, 360, 1, 1)
	first["fallback"] = "https://cdn.example/a-fallback"
	winner := rendition("https://cdn.example/b", 1280, 720, 1, 1)
	winner["fallback"] = "https://cdn.example/b-fallback"
	selected, ok := selectInstagramRendition([]any{first, winner}, true)
	if !ok || selected.fallback != "https://cdn.example/b-fallback" {
		t.Fatal("fallback does not belong to selected rendition")
	}
	winner["fallback"] = "http://cdn.example/invalid-fallback"
	selected, ok = selectInstagramRendition([]any{first, winner}, true)
	if !ok || selected.fallback != "" {
		t.Fatal("insecure selected fallback accepted")
	}
}
