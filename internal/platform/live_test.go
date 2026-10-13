package platform

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestLiveWebExtractionDownload(t *testing.T) {
	if os.Getenv("TT_SCRAP_LIVE_WEB") != "1" {
		t.Skip("set TT_SCRAP_LIVE_WEB=1 for a real public TikTok download")
	}
	raw, e := os.ReadFile("../../proxies.txt")
	if e != nil {
		t.Fatal("proxy configuration unavailable")
	}
	var proxy string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			proxy = line
			break
		}
	}
	cfg := testConfig()
	cfg.TikTokMethod = "web"
	cfg.TempDir = t.TempDir()
	assets := core.NewAssets(time.Minute, 100)
	service := NewTikTok(cfg, assets, nil, []string{proxy})
	defer service.Close()
	download := NewDownloader(cfg, nil, []string{proxy})
	defer download.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	file, e := download.Download(ctx, asset, 30<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Cleanup()
	report := map[string]any{"method": "go_web", "metadata_ms": metadataElapsed.Milliseconds(), "download_ms": time.Since(started).Milliseconds(), "bytes": file.Size, "width": file.Width, "height": file.Height, "sha256": file.SHA256, "source_id": post.SourceID}
	data, _ := json.Marshal(report)
	t.Log(string(data))
	if e = os.WriteFile("/tmp/tt-scrap-go-web-smoke.json", data, 0600); e != nil {
		t.Fatal(e)
	}
}
