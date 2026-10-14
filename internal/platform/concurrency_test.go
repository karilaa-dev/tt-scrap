package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestDownloadsRespectGlobalConcurrency(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			var active, peak atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); current > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, current) {
						break
					}
				}
				select {
				case <-time.After(20 * time.Millisecond):
				case <-r.Context().Done():
				}
				w.WriteHeader(503)
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.DownloadConcurrency = limit
			cfg.TempDir = t.TempDir()
			d := NewDownloader(cfg, nil)
			defer d.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 6)
			for range 6 {
				go func() {
					file, err := d.Download(ctx, core.AssetContext{Kind: "video", UpstreamURL: server.URL + "/video"}, 0)
					file.Cleanup()
					done <- err
				}()
			}
			for range 6 {
				if err := <-done; err == nil {
					t.Error("accepted failed download")
				}
			}
			if ctx.Err() != nil || peak.Load() > int32(limit) {
				t.Fatalf("downloads exceeded concurrency %d or stalled: peak=%d, %v", limit, peak.Load(), ctx.Err())
			}
		})
	}
}
