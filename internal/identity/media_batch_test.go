package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func TestMediaBatchRunsConcurrentlyAndPersistsOnce(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	finishLease(t, l, nil)
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
	if stats := p.Stats(); stats.Busy != 1 || stats.Writes != before.Writes {
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
	if stats := p.Stats(); stats.Busy != 0 || stats.Queued != 0 || stats.Writes != before.Writes+1 || stats.Reads != before.Reads {
		t.Fatal("batch did not save exactly once", stats)
	}
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

func TestMediaBatchClosesBodiesAndRejectsCheckpointFailure(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	finishLease(t, l, nil)
	var closed atomic.Bool
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: trackedMediaBody{strings.NewReader("media"), &closed}}, nil
		}))
	}
	var after core.OpenAppMedia
	err := p.WithMedia(context.Background(), id, func(open core.OpenAppMedia) error {
		after = open
		if _, err := open(context.Background(), "https://cdn.example/video", nil); err != nil {
			return err
		}
		_, err := db.Exec(context.Background(), "UPDATE "+p.identitiesTable+" SET revision=revision+1 WHERE id=$1", id)
		return err
	})
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "service_not_configured" || p.Ready() || !closed.Load() {
		t.Fatal("checkpoint failure or open body was lost", err, p.Stats(), closed.Load())
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
			finishLease(t, l, nil)
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
			before := p.Stats().Writes
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
			if err != nil || plays != 2 || attempts != 2 || fixture.registrations != registered || fixture.seeds != 2 || p.Stats().Writes != before+1 {
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
	finishLease(t, l, nil)
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
	before := p.Stats().Writes
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
	if stats := p.Stats(); stopped.Load() != 3 || stats.Busy != 0 || stats.Queued != 0 || stats.Writes != before+1 || !stats.Ready {
		t.Fatal("cancelled work leaked or invalidated the pool", stopped.Load(), stats)
	}
}
