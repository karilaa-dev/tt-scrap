package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

type stalledMediaBody struct{ ctx context.Context }

func (b stalledMediaBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b stalledMediaBody) Close() error             { return nil }

func TestMediaStallHealthAccounting(t *testing.T) {
	for _, scenario := range []string{"headers", "body", "fallback", "caller", "total", "direct"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			cfg.TempDir = t.TempDir()
			cfg.DownloadParts = 1
			cfg.DownloadRetries = 1
			cfg.RetryDelay = 0
			cfg.DownloadTimeout = time.Second
			cfg.DownloadHeaderTimeout = 50 * time.Millisecond
			cfg.DownloadIdleTimeout = 50 * time.Millisecond
			if scenario == "total" {
				cfg.DownloadTimeout = 10 * time.Millisecond
			}
			p := openTestPool(t, cfg, "")
			active := mustAcquire(t, p, "")
			id := active.entry.id
			active.finish(nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stalled := fixtureDoer(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/good") {
					return fixtureResponse(200, []byte{255, 216, 255, 0}), nil
				}
				if scenario == "caller" {
					cancel()
				}
				if scenario == "body" {
					return &http.Response{StatusCode: 200, ContentLength: 1024, Header: make(http.Header), Body: stalledMediaBody{r.Context()}}, nil
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				client := tiktokapp.NewClient(s, stalled)
				if scenario == "direct" {
					client.SetMediaTransport(stalled)
				}
				return client
			}
			d := platform.NewDownloader(cfg, p)
			defer d.Close()
			asset := core.AssetContext{IdentityID: id, Kind: "image", UpstreamURL: "https://cdn.example/stall"}
			if scenario == "fallback" {
				asset.AlternateURLs = []string{"https://cdn.example/good"}
			}
			got, err := d.Download(ctx, asset, 0)
			got.Cleanup()
			if scenario == "fallback" {
				if err != nil {
					t.Fatal(err)
				}
			} else if scenario == "caller" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				var api *core.Error
				if !errors.As(err, &api) || api.Code != "upstream_timeout" {
					t.Fatal("timeout contract lost", err)
				}
			}
			p.mu.Lock()
			identityHealth, routeHealth := p.entries[id].health, p.entries[id].proxy.health
			p.mu.Unlock()
			want := 0
			if scenario == "headers" || scenario == "body" {
				want = 1
			}
			if identityHealth.Failures != 0 || int(routeHealth.Failures) != want {
				t.Fatalf("incorrect health: identity=%+v route=%+v want=%d", identityHealth, routeHealth, want)
			}
			if want > 0 && (routeHealth.CooldownUntil <= time.Now().UnixMilli() || *routeHealth.LastFailureCode != "upstream_timeout") {
				t.Fatal("stall did not cool route")
			}
			if d.MemoryInUse() != 0 {
				t.Fatal("retained spool")
			}
		})
	}
}

func TestMediaBatchCancellationDoesNotMaskStall(t *testing.T) {
	b := &mediaBatch{}
	b.recordLocked(context.Canceled)
	b.recordLocked(&core.DownloadAttemptTimeout{Phase: "body"})
	var stall *core.DownloadAttemptTimeout
	if !errors.As(b.err, &stall) {
		t.Fatal("optional canceled request masked a stall")
	}
}

func TestOperationDeadlineIncludesIdentityWait(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	active := mustAcquire(t, p, "")
	defer active.finish(nil)
	op := core.NewOperation(1)
	ctx, cancel := context.WithTimeout(core.WithOperation(context.Background(), op), 30*time.Millisecond)
	defer cancel()
	called := false
	err := p.WithMedia(ctx, active.entry.id, func(core.OpenAppMedia) error { called = true; return nil })
	if err == nil || called || op.Attempt() != 1 {
		t.Fatal("identity wait escaped caller budget", err, called)
	}
	active.finish(nil)
	if active.entry.health.Failures != 0 || active.entry.proxy.health.Failures != 0 {
		t.Fatal("identity wait penalized route")
	}
}
