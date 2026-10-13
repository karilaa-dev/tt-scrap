package platform_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/identity"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

func TestLiveAppFeed(t *testing.T) {
	if os.Getenv("TT_SCRAP_LIVE_FEED") != "1" {
		t.Skip("set TT_SCRAP_LIVE_FEED=1 for real TikTok feed and DB integration")
	}
	env, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("test environment unavailable")
	}
	cfg, err := core.ParseConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyFile != "" && !filepath.IsAbs(cfg.ProxyFile) {
		cfg.ProxyFile = filepath.Join("../..", cfg.ProxyFile)
	}
	proxies, err := identity.ReadProxies(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := identity.Open(ctx, cfg, proxies)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	before := pool.Stats()
	response, err := pool.Feed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, ok := response["aweme_list"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("feed returned no posts")
	}
	idPattern := regexp.MustCompile(`^[0-9]{15,25}$`)
	publicVideos := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("feed item is not an object")
		}
		id, ok := item["aweme_id"].(string)
		if !ok || !idPattern.MatchString(id) {
			t.Fatal("feed item ID is not a decimal string")
		}
		if video, ok := item["video"].(map[string]any); ok {
			if address, ok := video["play_addr"].(map[string]any); ok {
				if urls, ok := address["url_list"].([]any); ok && len(urls) > 0 {
					publicVideos++
				}
			}
		}
	}
	if publicVideos == 0 {
		t.Fatal("feed returned no videos with playable URLs")
	}
	report := map[string]any{"method": "go_app_feed", "posts": len(items), "playable_videos": publicVideos, "pool_before": before, "pool_after": pool.Stats()}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
	if err = os.WriteFile("/tmp/tt-scrap-go-feed-smoke.json", data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveAppExtractionDownload(t *testing.T) {
	if os.Getenv("TT_SCRAP_LIVE_APP") != "1" {
		t.Skip("set TT_SCRAP_LIVE_APP=1 for real TikTok app and DB integration")
	}
	env, e := godotenv.Read("../../.env")
	if e != nil {
		t.Fatal("test environment unavailable")
	}
	cfg, e := core.ParseConfig(env)
	if e != nil {
		t.Fatal(e)
	}
	cfg.TikTokMethod = "app"
	cfg.TempDir = t.TempDir()
	if cfg.ProxyFile != "" && !filepath.IsAbs(cfg.ProxyFile) {
		cfg.ProxyFile = filepath.Join("../..", cfg.ProxyFile)
	}
	proxies, e := identity.ReadProxies(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, e := identity.Open(ctx, cfg, proxies)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	initial := pool.Stats()
	assets := core.NewAssets(cfg.CacheTTL, cfg.CacheMaxEntries)
	service := platform.NewTikTok(cfg, assets, pool, proxies)
	defer service.Close()
	downloader := platform.NewDownloader(cfg, pool)
	defer downloader.Close()
	started := time.Now()
	post, e := service.ExtractURL(ctx, "https://www.tiktok.com/@wwe/video/7695962883523464478", true)
	if e != nil {
		t.Fatal(e)
	}
	metadataElapsed := time.Since(started)
	asset, e := assets.GetAsset(post.Media[0].AssetID)
	if e != nil {
		t.Fatal(e)
	}
	started = time.Now()
	file, e := downloader.Download(ctx, asset, 30<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Cleanup()
	report := map[string]any{"method": "go_app", "metadata_ms": metadataElapsed.Milliseconds(), "download_ms": time.Since(started).Milliseconds(), "bytes": file.Size, "width": file.Width, "height": file.Height, "sha256": file.SHA256, "source_id": post.SourceID, "pool_before": initial, "pool_after": pool.Stats()}
	data, _ := json.Marshal(report)
	t.Log(string(data))
	if e = os.WriteFile("/tmp/tt-scrap-go-app-smoke.json", data, 0600); e != nil {
		t.Fatal(e)
	}
}
