package platform

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestMusicCacheNeverOutlivesReusedAssets(t *testing.T) {
	for _, metadataTTL := range []time.Duration{time.Minute, 2 * time.Minute} {
		t.Run(metadataTTL.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := testConfig()
				cfg.CacheTTL, cfg.MetadataTTL = time.Minute, metadataTTL
				backend := &fakeApp{slideshow: true, server: "https://cdn.example"}
				assets := core.NewAssets(cfg.CacheTTL, cfg.CacheMaxEntries)
				tiktok := NewTikTok(cfg, assets, backend, nil)
				defer tiktok.Close()
				post, err := tiktok.ExtractURL(context.Background(), "https://www.tiktok.com/@creator/photo/123", false)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Second)
				music, err := tiktok.ExtractMusic(context.Background(), "123", false)
				if err != nil || music.Audio.AssetID != post.Music.Audio.AssetID || backend.calls.Load() != 1 {
					t.Fatal("live extraction was not reused", music, err)
				}
				time.Sleep(11 * time.Second)
				fresh, err := tiktok.ExtractMusic(context.Background(), "123", false)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := assets.GetAsset(fresh.Audio.AssetID); err != nil || backend.calls.Load() != 2 {
					t.Fatal("music returned an expired download URL", fresh.Audio, err, backend.calls.Load())
				}
				cached, err := tiktok.ExtractMusic(context.Background(), "123", false)
				if err != nil || cached.Audio.AssetID != fresh.Audio.AssetID || backend.calls.Load() != 2 {
					t.Fatal("fresh music was not cached", cached, err)
				}
			})
		})
	}
}

type observedContext struct {
	context.Context
	waiting chan struct{}
}

func (c observedContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestMetadataFollowerRetriesAfterLeaderCancellation(t *testing.T) {
	m := newMemo[int](time.Minute, 10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, leaderDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = m.do(ctx, "same", false, func() (int, error) {
			close(started)
			<-ctx.Done()
			return 0, core.Upstream("wrapped upstream cancellation")
		})
	}()
	<-started
	followerCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	waiting := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := m.do(observedContext{followerCtx, waiting}, "same", false, func() (int, error) { return 42, nil })
		if err != nil || v != 42 {
			t.Errorf("live follower inherited canceled leader: %d %v", v, err)
		}
	}()
	<-waiting
	cancel()
	<-leaderDone
	<-done
}

func TestMixedPlatformCachesRespectTotalEntryBudget(t *testing.T) {
	cfg := testConfig()
	cfg.CacheMaxEntries = 101
	assets := core.NewAssets(time.Hour, cfg.CacheMaxEntries)
	tiktok := NewTikTok(cfg, assets, nil, nil)
	defer tiktok.Close()
	instagram := NewInstagram(cfg, assets)
	defer instagram.Close()
	var ids []string
	for i := 0; i < cfg.CacheMaxEntries+10; i++ {
		key := strconv.Itoa(i)
		ids = append(ids, assets.Register(core.AssetContext{Kind: "image"}, i).AssetID)
		tiktok.resolveCache.put(key, core.TikTokResolution{SourceID: key})
		tiktok.posts.put(key, core.TikTokExtraction{SourceID: key})
		tiktok.extractions.put(key, core.TikTokExtraction{SourceID: key})
		tiktok.music.put(key, core.TikTokMusic{SourceID: key, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)})
		instagram.posts.put(key, core.InstagramExtraction{SourceID: key})
		instagram.extractions.put(key, core.InstagramExtraction{SourceID: key})
	}
	live := 0
	for i, id := range ids {
		key := strconv.Itoa(i)
		if _, err := assets.GetAsset(id); err == nil {
			live++
		}
		if _, ok := tiktok.resolveCache.get(key); ok {
			live++
		}
		if _, ok := tiktok.posts.get(key); ok {
			live++
		}
		if _, ok := tiktok.extractions.get(key); ok {
			live++
		}
		if _, ok := tiktok.music.get(key); ok {
			live++
		}
		if _, ok := instagram.posts.get(key); ok {
			live++
		}
		if _, ok := instagram.extractions.get(key); ok {
			live++
		}
	}
	if live != cfg.CacheMaxEntries {
		t.Fatalf("mixed traffic retained %d entries; total budget is %d", live, cfg.CacheMaxEntries)
	}
}

func TestResponseCacheCapacityDoesNotLimitConcurrentFlights(t *testing.T) {
	m := newMemo[int](time.Minute, 2, 8)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	done := make(chan error, 8)
	for i := range 8 {
		go func() {
			value, err := m.do(ctx, strconv.Itoa(i), false, func() (int, error) {
				started <- struct{}{}
				select {
				case <-release:
					return i, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			})
			if err == nil && value != i {
				t.Errorf("wrong response: %d want %d", value, i)
			}
			done <- err
		}()
	}
	for range 8 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Error("small retained-response budget throttled active flights")
		}
	}
	close(release)
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	retained := 0
	for i := range 8 {
		if _, ok := m.get(strconv.Itoa(i)); ok {
			retained++
		}
	}
	if retained != 2 {
		t.Fatal("cache did not evict excess responses", retained)
	}
}
