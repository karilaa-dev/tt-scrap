package instagramapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func waitForAccountQueue(t *testing.T, p *Pool, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		queued := p.queued
		p.mu.Unlock()
		if queued == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue count=%d, want %d", queued, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMetadataContinuesDuringPinnedDownload(t *testing.T) {
	p := newTestPool(t, 1, 1)
	p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"123"}]}`))}, nil
	})
	entered := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- p.WithMedia(context.Background(), "instagram:a", func(core.OpenAppMedia) error {
			close(entered)
			<-finish
			return nil
		})
	}()
	<-entered
	defer func() {
		close(finish)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	detail, err := p.Detail(ctx, "123")
	if err != nil || detail.IdentityID != "instagram:a" {
		t.Fatalf("metadata stalled behind CDN transfer: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accounts[0].busy != 0 || p.accounts[0].mediaBusy != 1 {
		t.Fatal("metadata and media reservations are not independent")
	}
}

func TestSeparateMetadataAndDownloadCapacitySharesQueueBound(t *testing.T) {
	p := newTestPool(t, 1, 1)
	p.options.PerAccountDownloadConcurrency = 2
	var releases []func()
	for _, id := range []string{"", "instagram:a", "instagram:a"} {
		_, release, err := p.acquire(context.Background(), id, nil)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
			release()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, release, err := p.acquire(ctx, "", nil)
		if release != nil {
			release()
		}
		waiting <- err
	}()
	waitForAccountQueue(t, p, 1)
	_, _, err := p.acquire(context.Background(), "instagram:a", nil)
	assertCode(t, err, "service_busy")
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal("metadata waiter ignored cancellation", err)
	}
	waitForAccountQueue(t, p, 0)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, release, err := p.acquire(ctx, "instagram:a", nil)
		if release != nil {
			release()
		}
		waiting <- err
	}()
	waitForAccountQueue(t, p, 1)
	releases[1]()
	if err := <-waiting; err != nil {
		t.Fatal("download waiter not released", err)
	}
	waitForAccountQueue(t, p, 0)
}

func TestMetadataBalancesBusyDownloadsWithoutRotatingPinnedRoute(t *testing.T) {
	p := newTestPool(t, 2, 1)
	_, releaseMedia, err := p.acquire(context.Background(), "instagram:a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMedia()
	a, releaseMetadata, err := p.acquire(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMetadata()
	if a.id != "instagram:b" {
		t.Fatal("metadata ignored less-loaded account")
	}
	a, releaseSecond, err := p.acquire(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	if a.id != "instagram:a" {
		t.Fatal("download blocked remaining metadata capacity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, release, err := p.acquire(ctx, "instagram:a", nil)
		if release != nil {
			release()
		}
		waiting <- err
	}()
	waitForAccountQueue(t, p, 1)
	p.Close()
	err = <-waiting
	assertCode(t, err, "service_not_configured")
}

func TestDownloadCapacityOptions(t *testing.T) {
	for _, value := range []int{-1, 65} {
		p, err := Open(Options{PerAccountDownloadConcurrency: value})
		if p != nil {
			p.Close()
		}
		if err == nil {
			t.Fatalf("accepted download concurrency %d", value)
		}
	}
	p, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.options.PerAccountDownloadConcurrency != 1 {
		t.Fatal("download concurrency default changed")
	}
}
