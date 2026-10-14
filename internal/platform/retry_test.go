package platform

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type retryBackend struct {
	core.AppBackend
	starts []time.Time
}

func (b *retryBackend) WithMedia(ctx context.Context, _ string, run func(core.OpenAppMedia) error) error {
	return run(func(context.Context, string, http.Header) (*http.Response, error) {
		b.starts = append(b.starts, time.Now())
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})
}

func TestMediaRetriesUseExponentialJitterAndHonorDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig()
		cfg.RetryDelay, cfg.DownloadRetries = 10*time.Millisecond, 4
		cfg.DownloadTimeout = time.Minute
		asset := core.AssetContext{IdentityID: "pinned", UpstreamURL: "https://cdn.example/video", Kind: "video"}
		firstDelays := map[time.Duration]bool{}
		for range 8 {
			backend := &retryBackend{}
			d := NewDownloader(cfg, backend)
			file, err := d.Download(context.Background(), asset, 0)
			file.Cleanup()
			d.Close()
			if err == nil || len(backend.starts) != 4 {
				t.Fatal("retry count", err, len(backend.starts))
			}
			for i, minimum := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond} {
				gap := backend.starts[i+1].Sub(backend.starts[i])
				if gap < minimum || gap > minimum+minimum/10 {
					t.Errorf("retry %d waited %s; want %s to %s", i, gap, minimum, minimum+minimum/10)
				}
			}
			firstDelays[backend.starts[1].Sub(backend.starts[0])] = true
		}
		if len(firstDelays) < 2 {
			t.Error("retries have no jitter")
		}
		cfg.DownloadTimeout = 25 * time.Millisecond
		backend := &retryBackend{}
		d := NewDownloader(cfg, backend)
		defer d.Close()
		start := time.Now()
		file, err := d.Download(context.Background(), asset, 0)
		file.Cleanup()
		if err == nil || len(backend.starts) != 2 || time.Since(start) != 25*time.Millisecond {
			t.Fatal("backoff bypassed download deadline", err, len(backend.starts), time.Since(start))
		}
	})
}
