package platform_test

import (
	"context"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/instagramapp"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

// Opt-in only: consumes saved Android sessions, never attempts account login.
func TestLiveInstagramExtractionDownload(t *testing.T) {
	if os.Getenv("TT_SCRAP_LIVE_INSTAGRAM") != "1" {
		t.Skip("set TT_SCRAP_LIVE_INSTAGRAM=1 and INSTAGRAM_ACCOUNTS_FILE or INSTAGRAM_ACCOUNTS_DIR")
	}
	carouselCode := os.Getenv("TT_SCRAP_INSTAGRAM_CAROUSEL_CODE")
	if carouselCode == "" {
		carouselCode = "DezfpMuEUZO"
	}
	cfg := core.DefaultConfig()
	cfg.InstagramAccountsFile = os.Getenv("INSTAGRAM_ACCOUNTS_FILE")
	cfg.InstagramAccountsDir = os.Getenv("INSTAGRAM_ACCOUNTS_DIR")
	cfg.TempDir = t.TempDir()
	cfg.DownloadTimeout = 90 * time.Second
	pool, err := instagramapp.Open(instagramapp.Options{AccountsFile: cfg.InstagramAccountsFile, AccountsDir: cfg.InstagramAccountsDir, PerAccountConcurrency: 1, MaxQueuedRequests: 16, Timeout: 20 * time.Second, MaxAttempts: 2, RetryDelay: time.Second, HTTPMaxConnections: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if pool.Count() == 0 {
		t.Fatal("live test requires saved sessions")
	}
	assets := core.NewAssets(cfg.CacheTTL, cfg.CacheMaxEntries)
	service := platform.NewInstagram(cfg, assets, pool)
	defer service.Close()
	downloader := platform.NewDownloader(cfg, nil, pool)
	defer downloader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, test := range []struct {
		code, kind string
		count      int
	}{
		{"DYm5N7px50o", "video", 1},
		{"De50be8t8Ln", "image", 1},
		{carouselCode, "carousel", 0},
	} {
		t.Run(test.kind, func(t *testing.T) {
			post, err := service.ExtractURL(ctx, "https://www.instagram.com/p/"+test.code+"/", true)
			if err != nil {
				t.Fatal(err)
			}
			if post.ContentType != test.kind || (test.count > 0 && len(post.Media) != test.count) || len(post.Media) == 0 {
				t.Fatal("unexpected live media shape")
			}
			cached, err := service.GetExtraction(ctx, post.ExtractionID)
			if err != nil || cached.ExtractionID != post.ExtractionID {
				t.Fatal("extraction lookup failed", err)
			}
			var total int64
			for _, item := range post.Media {
				asset, err := assets.GetAsset(item.Asset.AssetID)
				if err != nil {
					t.Fatal(err)
				}
				if asset.IdentityID == "" || asset.Headers.Get("Authorization") != "" {
					t.Fatal("missing account affinity or leaked authorization")
				}
				file, err := downloader.Download(ctx, asset, 50<<20)
				if err != nil {
					t.Fatal(err)
				}
				if file.Size == 0 || file.SHA256 == "" || (item.MediaType == "video" && (file.Width == 0 || file.Height == 0)) {
					file.Cleanup()
					t.Fatal("download validation missing")
				}
				if item.MediaType == "image" {
					decoded, _, err := image.DecodeConfig(file.Data.Reader(ctx, 0, file.Size))
					if err != nil || decoded.Width == 0 || decoded.Height == 0 {
						file.Cleanup()
						t.Fatal("image validation failed")
					}
				}
				total += file.Size
				file.Cleanup()
			}
			t.Logf("accounts=%d kind=%s assets=%d bytes=%d metadata_dimensions=%t likes_present=%t", pool.Count(), test.kind, len(post.Media), total, post.Width != nil && post.Height != nil, post.Likes != nil)
		})
	}
}
