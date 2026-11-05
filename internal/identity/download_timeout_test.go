package identity

import (
	"context"
	"errors"
	"io"
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

type progressingMediaPrefix struct {
	ctx       context.Context
	remaining int
}

func (b *progressingMediaPrefix) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	select {
	case <-time.After(20 * time.Millisecond):
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	n := min(len(p), 128, b.remaining)
	for i := range n {
		p[i] = 0
	}
	if b.remaining == 1024 {
		copy(p, []byte{255, 216, 255})
	}
	b.remaining -= n
	return n, nil
}
func (b *progressingMediaPrefix) Close() error { return nil }

func TestMediaPrefixInspectionUsesIdleNotHeaderTimeout(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.DownloadParts = 1
	cfg.DownloadRetries = 1
	cfg.DownloadHeaderTimeout = 10 * time.Millisecond
	cfg.DownloadIdleTimeout = 100 * time.Millisecond
	p := openTestPool(t, cfg, "")
	active := mustAcquire(t, p, "")
	id := active.entry.id
	active.finish(nil)
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: 1024, Body: &progressingMediaPrefix{ctx: r.Context(), remaining: 1024}}, nil
		}))
	}
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	file, err := d.Download(context.Background(), core.AssetContext{IdentityID: id, Kind: "image", UpstreamURL: "https://cdn.example/slow-prefix"}, 0)
	file.Cleanup()
	if err != nil {
		t.Fatal("progressing prefix was treated as stalled headers", err)
	}
}

func TestOperationMediaRepairsIdentityBeforeSpendingSourceRetries(t *testing.T) {
	for _, failure := range []string{"invalid signature", "invalid device_id"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			cfg.DownloadParts = 1
			cfg.DownloadRetries = 3
			cfg.RetryDelay = 0
			p := openTestPool(t, cfg, "")
			active := mustAcquire(t, p, "")
			id := active.entry.id
			active.finish(nil)
			fixture := newProtocolFixture(t, "", 0)
			plays := 0
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/aweme/v1/play/" {
						plays++
						if fixture.seeds < 2 {
							return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"`+failure+`"}`)), nil
						}
						return fixtureResponse(200, []byte{255, 216, 255, 0}), nil
					}
					return fixture.do(r)
				}))
			}
			op := core.NewOperation(1)
			ctx := core.WithOperation(context.Background(), op)
			var progress []core.AttemptProgress
			op.Observe(ctx, func(event core.AttemptProgress) { progress = append(progress, event) })
			d := platform.NewDownloader(cfg, p)
			defer d.Close()
			file, err := d.Download(ctx, core.AssetContext{IdentityID: id, Kind: "image", UpstreamURL: "https://api19-va.tiktokv.com/aweme/v1/play/"}, 0)
			file.Cleanup()
			if err != nil || plays != 2 || op.Attempt() != 2 || len(progress) != 2 || progress[1].Stage != "identity_recovery" {
				t.Fatal("source retries starved identity repair", err, plays, op.Attempt(), progress)
			}
		})
	}
}
