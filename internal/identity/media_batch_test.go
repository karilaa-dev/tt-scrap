package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func TestMediaBatchRunsConcurrentlyAndPersistsOnce(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	started, release := make(chan struct{}, 3), make(chan struct{})
	var clients atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		clients.Add(1)
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			response := fixtureResponse(200, []byte("media"))
			response.Header.Set("Set-Cookie", "probe"+r.URL.Path[1:]+"=saved; Path=/; Secure")
			return response, nil
		}))
	}
	before := p.Stats()
	done := make(chan error, 1)
	go func() {
		done <- p.WithMedia(context.Background(), id, func(open core.OpenAppMedia) error {
			var wg sync.WaitGroup
			for n := range 3 {
				wg.Go(func() {
					res, err := open(context.Background(), "https://cdn.example/"+strconv.Itoa(n), nil)
					if err != nil {
						t.Error(err)
						return
					}
					if _, err = io.ReadAll(res.Body); err != nil {
						t.Error(err)
					}
					if err = res.Body.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			return nil
		})
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			<-done
			t.Fatal("media probes were serialized")
		}
	}
	if stats := p.Stats(); stats.Busy != 1 || stats.Checkpoints != before.Checkpoints {
		t.Fatal("batch released or saved before completion", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if other, err := p.acquire(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		if other != nil {
			other.finish(nil)
		}
		t.Fatal("batch lost exclusive identity ownership", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats := p.Stats(); stats.Busy != 0 || stats.Queued != 0 || stats.Checkpoints != before.Checkpoints+1 || stats.Reads != before.Reads {
		t.Fatal("batch did not save exactly once", stats)
	}
	persisted(t, p)
	var cookies string
	if err := db.QueryRow(context.Background(), "SELECT session->>'cookies' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&cookies); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe0", "probe1", "probe2"} {
		if !strings.Contains(cookies, name) {
			t.Fatal("concurrent cookie lost", name)
		}
	}
	if err := p.WithMedia(context.Background(), id, func(open core.OpenAppMedia) error {
		res, err := open(context.Background(), "https://cdn.example/next", nil)
		if err != nil {
			return err
		}
		return res.Body.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if clients.Load() != 1 {
		t.Fatal("identity rebuilt its protocol client", clients.Load())
	}
}

type trackedMediaBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b trackedMediaBody) Close() error { b.closed.Store(true); return nil }

func TestMediaBatchClosesBodiesAndRejectsLateRequests(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	var closed atomic.Bool
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: trackedMediaBody{strings.NewReader("media"), &closed}}, nil
		}))
	}
	var after core.OpenAppMedia
	err := p.WithMedia(context.Background(), id, func(open core.OpenAppMedia) error {
		after = open
		_, err := open(context.Background(), "https://cdn.example/video", nil)
		return err
	})
	if err != nil || !p.Ready() || !closed.Load() {
		t.Fatal("open body was lost", err, p.Stats(), closed.Load())
	}
	if _, err = after(context.Background(), "https://cdn.example/late", nil); !errors.Is(err, context.Canceled) {
		t.Fatal("finished batch accepted work", err)
	}
}

func TestMediaBatchRecoversSigningAfterAllProbesFinish(t *testing.T) {
	for _, failure := range []string{"invalid signature", "invalid device_id"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			p := openTestPool(t, cfg, "")
			l := mustAcquire(t, p, "")
			id := l.entry.id
			l.finish(nil)
			fixture := newProtocolFixture(t, "", 0)
			plays, attempts := 0, 0
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/aweme/v1/play/" {
						plays++
						if plays == 1 {
							return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"`+failure+`"}`)), nil
						}
						return fixtureResponse(200, []byte("video")), nil
					}
					return fixture.do(r)
				}))
			}
			before := p.Stats().Checkpoints
			err := p.WithMedia(context.Background(), id, func(open core.OpenAppMedia) error {
				attempts++
				res, err := open(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
				if err != nil {
					return nil
				}
				return res.Body.Close()
			})
			registered := 1
			if failure == "invalid device_id" {
				registered = 2
			}
			if err != nil || plays != 2 || attempts != 2 || fixture.registrations != registered || fixture.seeds != 2 || p.Stats().Checkpoints != before+1 {
				t.Fatal("incorrect grouped recovery", err, plays, attempts, fixture, p.Stats())
			}
		})
	}
}

func TestMediaBatchCancellationStopsAllRequestsAndReleasesIdentity(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	started := make(chan struct{}, 3)
	var stopped atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-r.Context().Done()
			stopped.Add(1)
			return nil, r.Context().Err()
		}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := p.Stats().Checkpoints
	done := make(chan error, 1)
	go func() {
		done <- p.WithMedia(ctx, id, func(open core.OpenAppMedia) error {
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() { _, _ = open(context.Background(), "https://cdn.example/video", nil) })
			}
			wg.Wait()
			return ctx.Err()
		})
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("batch requests did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation was lost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled batch did not finish")
	}
	if stats := p.Stats(); stopped.Load() != 3 || stats.Busy != 0 || stats.Queued != 0 || stats.Checkpoints != before+1 || !stats.Ready {
		t.Fatal("cancelled work leaked or invalidated the pool", stopped.Load(), stats)
	}
}

func TestDownloadFallbackRetainsPinnedIdentity(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.RetryDelay = time.Millisecond
	cfg.TempDir = t.TempDir()
	p := openTestPool(t, cfg, "http://proxy.example:8080")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	var bad, good atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/bad" {
				bad.Add(1)
				res := fixtureResponse(403, []byte("blocked"))
				res.Header.Set("Set-Cookie", "fallback=saved; Path=/; Secure")
				return res, nil
			}
			good.Add(1)
			cookie, err := r.Cookie("fallback")
			if err != nil || cookie.Value != "saved" {
				t.Error("fallback lost identity cookies", err)
			}
			return fixtureResponse(200, []byte{255, 216, 255, 0, 1, 2}), nil
		}))
	}
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	before := p.Stats().Checkpoints
	asset := core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "x.jpg", IdentityID: id, UpstreamURL: "https://cdn.example/bad", AlternateURLs: []string{"https://cdn.example/good"}}
	file, err := d.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal("backup URL was not usable", err)
	}
	file.Cleanup()
	if bad.Load() != 1 || good.Load() != 1 || p.Stats().Checkpoints != before+1 {
		t.Fatal("fallback was not one operation", bad.Load(), good.Load(), p.Stats())
	}
	asset.UpstreamURL = "https://cdn.example/good"
	asset.AlternateURLs = nil
	file, err = d.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal("successful fallback left the identity cooling down", err)
	}
	file.Cleanup()
}

func TestAppDataOnlyUsesDirectCDNWithPinnedPlayRequest(t *testing.T) {
	var cdnCalls atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnCalls.Add(1)
		if r.Header.Get("X-Argus") != "" || r.Header.Get("User-Agent") == "" {
			t.Error("incorrect CDN headers")
		}
		_, _ = w.Write([]byte{255, 216, 255, 0, 1, 2})
	}))
	defer cdn.Close()
	cfg, _ := testDatabase(t)
	cfg.ProxyDataOnly = true
	cfg.TempDir = t.TempDir()
	p := openTestPool(t, cfg, "http://proxy.example:8080")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	fixture := newProtocolFixture(t, "", 0)
	var plays atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == strings.TrimPrefix(cdn.URL, "http://") {
				return nil, errors.New("CDN request used metadata proxy")
			}
			if r.URL.Path == "/aweme/v1/play/" {
				plays.Add(1)
				if r.Header.Get("X-Argus") == "" {
					t.Error("play request lost signing")
				}
				return fixtureResponse(200, []byte{255, 216, 255, 0, 1, 2}), nil
			}
			return fixture.do(r)
		}))
	}
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	file, err := d.Download(context.Background(), core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "x.jpg", IdentityID: id, UpstreamURL: "https://api19-va.tiktokv.com/aweme/v1/play/"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.Cleanup()
	file, err = d.Download(context.Background(), core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "x.jpg", IdentityID: id, UpstreamURL: cdn.URL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.Cleanup()
	if cdnCalls.Load() != 1 || plays.Load() != 1 || fixture.registrations != 1 || p.entries[id].proxy.key != "proxy.example:8080" {
		t.Fatal("identity route changed", cdnCalls.Load(), plays.Load(), fixture)
	}
}

func TestPinnedDownloadWaitDoesNotConsumeTransferTimeout(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.TempDir = t.TempDir()
	cfg.DownloadTimeout = 200 * time.Millisecond
	p := openTestPool(t, cfg, "")
	var calls atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return fixtureResponse(200, []byte{255, 216, 255, 0, 1, 2}), nil
		}))
	}
	active := mustAcquire(t, p, "")
	id := active.entry.id
	defer active.finish(nil)
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		file, err := d.Download(ctx, core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "slide.jpg", IdentityID: id, UpstreamURL: "https://cdn.example/slide"}, 0)
		file.Cleanup()
		done <- err
	}()
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	time.Sleep(2 * cfg.DownloadTimeout)
	active.finish(nil)
	if err := <-done; err != nil {
		t.Fatal("queued slide used up its transfer timeout", err)
	}
	if calls.Load() != 1 {
		t.Fatal("queued slide was not downloaded exactly once", calls.Load())
	}

	active = mustAcquire(t, p, id)
	defer active.finish(nil)
	queued, stop := context.WithCancel(context.Background())
	go func() {
		file, err := d.Download(queued, core.AssetContext{Platform: "tiktok", Kind: "image", IdentityID: id, UpstreamURL: "https://cdn.example/slide"}, 0)
		file.Cleanup()
		done <- err
	}()
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation was lost while queued", err)
	}
	active.finish(nil)
	if calls.Load() != 1 {
		t.Fatal("canceled queued slide contacted upstream")
	}
}

func TestPinnedDownloadTransferTimeoutRemainsEnforced(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.DownloadTimeout = 20 * time.Millisecond
	cfg.TempDir = t.TempDir()
	p := openTestPool(t, cfg, "")
	active := mustAcquire(t, p, "")
	id := active.entry.id
	active.finish(nil)
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}))
	}
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	file, err := d.Download(ctx, core.AssetContext{Platform: "tiktok", Kind: "image", IdentityID: id, UpstreamURL: "https://cdn.example/slow"}, 0)
	file.Cleanup()
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "upstream_timeout" || ctx.Err() != nil {
		t.Fatal("per-transfer timeout lost", err, ctx.Err())
	}
	if p.entries[id].health.CooldownUntil != 0 || p.entries[id].proxy.health.CooldownUntil != 0 {
		t.Fatal("local transfer deadline penalized the identity or its proxy")
	}
}
