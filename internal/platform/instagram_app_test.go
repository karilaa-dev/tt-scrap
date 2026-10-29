package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type instagramFixtureBackend struct {
	detail func(context.Context, string) (core.AppDetail, error)
}

func (b instagramFixtureBackend) Detail(ctx context.Context, id string) (core.AppDetail, error) {
	return b.detail(ctx, id)
}
func (b instagramFixtureBackend) WithMedia(context.Context, string, func(core.OpenAppMedia) error) error {
	return errors.New("unexpected media request during metadata extraction")
}

func instagramFixture(t *testing.T, raw string) object {
	t.Helper()
	data, err := decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const instagramVideoFixture = `{
  "status":"ok","items":[{
    "id":"3902057773219159336_123","code":"DYm5N7px50o","media_type":2,
    "user":{"username":"creator"},"caption":{"text":"An example Reel"},
    "taken_at":1750000000,"like_count":0,"play_count":12345678901,"comment_count":12,
    "video_duration":12.3,"is_third_party_downloads_eligible":false,
    "image_versions2":{"candidates":[{"width":320,"height":180,"url":"https://cdn.example/small.jpg"},{"width":1280,"height":720,"url":"https://cdn.example/cover.jpg"}]},
    "video_versions":[
      {"width":640,"height":360,"bandwidth":9000000,"type":102,"url":"https://cdn.example/small.mp4"},
      {"width":1280,"height":720,"bandwidth":1000000,"type":101,"url":"https://cdn.example/medium.mp4"},
      {"width":1280,"height":720,"bandwidth":2000000,"type":101,"url":"https://cdn.example/high.mp4","fallback":"https://cdn.example/fallback.mp4"}
    ],
    "clips_metadata":{"music_info":{"music_asset_info":{"title":"Track","display_artist":"Artist","duration_in_ms":180000,"progressive_download_url":"https://cdn.example/audio.m4a","cover_artwork_uri":"https://cdn.example/music.jpg"}}}
  }]
}`

func TestInstagramAppMetadataAndPinnedAssets(t *testing.T) {
	cfg := testConfig()
	assets := core.NewAssets(time.Minute, 100)
	var calls atomic.Int32
	backend := instagramFixtureBackend{detail: func(_ context.Context, id string) (core.AppDetail, error) {
		calls.Add(1)
		if id != "3902057773219159336" {
			t.Errorf("media ID lost precision: %s", id)
		}
		return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:account-1"}, nil
	}}
	service := NewInstagram(cfg, assets, backend)
	defer service.Close()
	post, err := service.ExtractURL(context.Background(), "https://instagram.com/reel/DYm5N7px50o/?igsh=tracking", false)
	if err != nil {
		t.Fatal(err)
	}
	if post.Method != "app" || post.SourceID != "DYm5N7px50o" || post.ResolvedURL != "https://www.instagram.com/reel/DYm5N7px50o/" || post.ContentType != "video" || len(post.Media) != 1 {
		t.Fatalf("unexpected post: %+v", post)
	}
	if post.CreatorUsername == nil || *post.CreatorUsername != "creator" || post.Caption == nil || *post.Caption != "An example Reel" || post.CreatedAt == nil || *post.CreatedAt != "2025-06-15T15:06:40Z" {
		t.Fatalf("missing creator/caption/time: %+v", post)
	}
	if post.Width == nil || *post.Width != 1280 || post.Height == nil || *post.Height != 720 || post.DurationSeconds == nil || *post.DurationSeconds != 13 {
		t.Fatalf("missing dimensions/duration: %+v", post)
	}
	if post.Likes == nil || *post.Likes != 0 || post.Views == nil || *post.Views != 12345678901 || post.Comments == nil || *post.Comments != 12 || post.ThirdPartyDownloadEligible == nil || *post.ThirdPartyDownloadEligible {
		t.Fatalf("missing counters or eligibility: %+v", post)
	}
	if post.Music == nil || post.Music.Title != "Track" || post.Music.Author != "Artist" || post.Music.DurationSeconds != 180 || post.Music.Audio == nil || post.Music.Cover == nil || post.Cover == nil || post.Media[0].Thumbnail == nil {
		t.Fatalf("missing music or covers: %+v", post)
	}
	for _, descriptor := range []core.AssetDescriptor{post.Media[0].Asset, *post.Cover, *post.Media[0].Thumbnail, *post.Music.Audio, *post.Music.Cover} {
		asset, err := assets.GetAsset(descriptor.AssetID)
		if err != nil || asset.IdentityID != "instagram:account-1" || asset.Platform != "instagram" || len(asset.Headers) != 0 || asset.ExtractionID != post.ExtractionID {
			t.Fatalf("asset did not retain credential-free account route: %+v, %v", asset, err)
		}
	}
	video, _ := assets.GetAsset(post.Media[0].Asset.AssetID)
	if video.UpstreamURL != "https://cdn.example/high.mp4" || len(video.AlternateURLs) != 1 || video.AlternateURLs[0] != "https://cdn.example/fallback.mp4" || video.DurationSeconds != 13 {
		t.Fatalf("wrong rendition: %+v", video)
	}
	again, err := service.ExtractURL(context.Background(), "https://www.instagram.com/reel/DYm5N7px50o/", false)
	if err != nil || again.ExtractionID != post.ExtractionID || calls.Load() != 1 {
		t.Fatalf("metadata cache failed: %v, calls %d", err, calls.Load())
	}
	cached, err := service.GetExtraction(context.Background(), post.ExtractionID)
	if err != nil || cached.ExtractionID != post.ExtractionID {
		t.Fatal("extraction unavailable", err)
	}
}

func TestInstagramAppOrderedMixedCarousel(t *testing.T) {
	fixture := instagramFixture(t, `{"items":[{"id":"3902057773219159336_123","media_type":8,"carousel_media_count":3,"carousel_media":[
      {"id":"11_123","media_type":1,"image_versions2":{"candidates":[{"width":1080,"height":1350,"url":"https://cdn.example/first.jpg"}]}},
      {"id":"12_123","media_type":2,"video_duration":2,"video_versions":[{"width":720,"height":900,"url":"https://cdn.example/second.mp4"}]} ,
      {"id":"13_123","media_type":1,"image_versions2":{"candidates":[{"width":1080,"height":1350,"url":"https://cdn.example/third.jpg"}]}}
    ]}]}`)
	assets := core.NewAssets(time.Minute, 100)
	service := NewInstagram(testConfig(), assets, instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
		return core.AppDetail{Response: fixture, IdentityID: "instagram:one"}, nil
	}})
	defer service.Close()
	post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
	if err != nil || post.ContentType != "carousel" || len(post.Media) != 3 || post.DurationSeconds != nil {
		t.Fatalf("unexpected carousel: %+v, %v", post, err)
	}
	for index, name := range []string{"first.jpg", "second.mp4", "third.jpg"} {
		item := post.Media[index]
		asset, err := assets.GetAsset(item.Asset.AssetID)
		if err != nil || item.Position != index || asset.UpstreamURL != "https://cdn.example/"+name || item.Width == nil || item.Height == nil {
			t.Fatalf("wrong carousel order or child metadata: %+v %+v %v", item, asset, err)
		}
	}
	if post.Media[1].DurationSeconds == nil || *post.Media[1].DurationSeconds != 2 {
		t.Fatal("missing per-video duration")
	}
}

func TestInstagramAppPhotoAndOriginalMusic(t *testing.T) {
	fixture := instagramFixture(t, `{"clips_items":[{"media":{"strong_id__":"3902057773219159336_123","media_type":1,
      "image_versions2":{"candidates":[{"width":100,"height":100,"url":"https://cdn.example/first.png"},{"width":100,"height":100,"url":"https://cdn.example/tied.png"}]},
      "clips_metadata":{"original_sound_info":{"original_audio_title":"Original audio","ig_artist":{"username":"artist"},"progressive_download_url":"https://cdn.example/original.m4a"}}
    }}]}`)
	assets := core.NewAssets(time.Minute, 100)
	service := NewInstagram(testConfig(), assets, instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
		return core.AppDetail{Response: fixture, IdentityID: "instagram:one"}, nil
	}})
	defer service.Close()
	post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
	if err != nil || post.ContentType != "image" || post.Likes != nil || post.Views != nil || post.Comments != nil || post.DurationSeconds != nil || post.ThirdPartyDownloadEligible != nil || post.Music == nil || post.Music.Title != "Original audio" || post.Music.Author != "artist" {
		t.Fatalf("photo nullable metadata or music was lost: %+v, %v", post, err)
	}
	asset, err := assets.GetAsset(post.Media[0].Asset.AssetID)
	if err != nil || asset.UpstreamURL != "https://cdn.example/first.png" {
		t.Fatal("image ties must preserve service order", asset, err)
	}
}

func TestInstagramAppRequiresPinnedAccount(t *testing.T) {
	for _, identity := range []string{"", "instagram:", "tiktok-account"} {
		service := NewInstagram(testConfig(), core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
			return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: identity}, nil
		}})
		post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/reel/DYm5N7px50o/", false)
		service.Close()
		if err == nil || len(post.Media) != 0 {
			t.Fatal("unpinned app extraction accepted", identity, err)
		}
	}
}

func TestInstagramAppRejectsWrongOrIncompleteMedia(t *testing.T) {
	for name, fixture := range map[string]string{
		"wrong_id":            `{"items":[{"id":"3902057773219159337_123","code":"DYm5N7px50o","media_type":2}]}`,
		"wrong_code":          `{"items":[{"id":"3902057773219159336_123","code":"other","media_type":2}]}`,
		"numeric_pk_only":     `{"items":[{"pk":3902057773219159336,"media_type":2}]}`,
		"empty_carousel":      `{"items":[{"id":"3902057773219159336_123","media_type":8,"carousel_media":[]}]}`,
		"incomplete_carousel": `{"items":[{"id":"3902057773219159336_123","media_type":8,"carousel_media_count":2,"carousel_media":[{"media_type":1,"image_versions2":{"candidates":[{"url":"https://cdn.example/a.jpg"}]}}]}]}`,
		"unusable_child":      `{"items":[{"id":"3902057773219159336_123","media_type":8,"carousel_media_count":2,"carousel_media":[{"media_type":1,"image_versions2":{"candidates":[{"url":"https://cdn.example/a.jpg"}]}},{"media_type":2,"video_versions":[]}]}]}`,
		"invalid_media_url":   `{"items":[{"id":"3902057773219159336_123","media_type":1,"image_versions2":{"candidates":[{"url":"file:///etc/passwd"}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			service := NewInstagram(testConfig(), core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
				return core.AppDetail{Response: instagramFixture(t, fixture), IdentityID: "instagram:one"}, nil
			}})
			defer service.Close()
			post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
			if err == nil || len(post.Media) != 0 {
				t.Fatal("invalid post was accepted", post, err)
			}
		})
	}
}

func TestInstagramLegacyFallbackToggle(t *testing.T) {
	var calls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"media":[{"type":"video","url":"https://cdn.example/legacy.mp4"}]}`)
	}))
	defer legacy.Close()
	for _, enabled := range []bool{false, true} {
		cfg := testConfig()
		cfg.InstagramLegacyFallback, cfg.RapidAPIBaseURL, cfg.RapidAPIKey = enabled, legacy.URL, "fixture-key"
		service := NewInstagram(cfg, core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
			return core.AppDetail{}, core.Upstream("app request failed")
		}})
		post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
		service.Close()
		if enabled {
			if err != nil || post.Method != "legacy" || len(post.Media) != 1 || calls.Load() != 1 {
				t.Fatalf("fallback failed: %+v, %v, calls %d", post, err, calls.Load())
			}
		} else if err == nil || calls.Load() != 0 {
			t.Fatal("disabled fallback was called", err, calls.Load())
		}
	}
	cfg := testConfig()
	cfg.InstagramLegacyFallback, cfg.RapidAPIBaseURL, cfg.RapidAPIKey = true, legacy.URL, "fixture-key"
	service := NewInstagram(cfg, core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
		return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:one"}, nil
	}})
	defer service.Close()
	post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
	if err != nil || post.Method != "app" || calls.Load() != 1 {
		t.Fatal("successful app request invoked fallback", post.Method, err, calls.Load())
	}
}

func TestInstagramFallbackDoesNotRunOnCancellationOrDurationPolicy(t *testing.T) {
	var calls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer legacy.Close()
	for _, mode := range []string{"canceled", "deadline", "too_long"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig()
			cfg.InstagramLegacyFallback, cfg.RapidAPIBaseURL, cfg.RapidAPIKey = true, legacy.URL, "fixture-key"
			cfg.MaxVideoDuration = 10
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := NewInstagram(cfg, core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
				switch mode {
				case "canceled":
					cancel()
					return core.AppDetail{}, context.Canceled
				case "deadline":
					return core.AppDetail{}, context.DeadlineExceeded
				default:
					return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:one"}, nil
				}
			}})
			defer service.Close()
			_, err := service.ExtractURL(ctx, "https://www.instagram.com/reel/DYm5N7px50o/", false)
			if err == nil || calls.Load() != 0 {
				t.Fatal("fallback bypassed cancellation or policy", err, calls.Load())
			}
		})
	}
}

func TestInstagramShortcodeAndURLValidation(t *testing.T) {
	id, err := instagramMediaID("DYm5N7px50o")
	if err != nil || id != "3902057773219159336" {
		t.Fatal(id, err)
	}
	for _, code := range []string{"", "A", "bad/code", strings.Repeat("a", 21)} {
		if _, err := instagramMediaID(code); err == nil {
			t.Fatal("invalid shortcode accepted", code)
		}
	}
	for _, raw := range []string{"https://instagram.com/p/abc/unexpected/", "https://instagram.com/p/a%2Fb/", "https://user:password@instagram.com/p/abc/", "https://instagram.com/share/reel/abc/"} {
		if _, _, err := normalizeInstagram(raw); err == nil {
			t.Fatal("invalid URL accepted")
		}
	}
}
