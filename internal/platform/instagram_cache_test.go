package platform

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestInstagramPostAliasesShareCacheAndRefresh(t *testing.T) {
	var calls atomic.Int32
	assets := core.NewAssets(time.Minute, 100)
	service := NewInstagram(testConfig(), assets, instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
		calls.Add(1)
		return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:one"}, nil
	}})
	defer service.Close()
	var first string
	for _, route := range []string{"reel", "reels", "p", "tv"} {
		source := "https://www.instagram.com/" + route + "/DYm5N7px50o/"
		post, err := service.ExtractURL(context.Background(), source+"?igsh=tracking", false)
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = post.ExtractionID
		}
		if post.ExtractionID != first || post.SourceURL != source+"?igsh=tracking" || post.ResolvedURL != source {
			t.Fatal("alias lost cache identity or caller URL")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("aliases repeated upstream lookup", calls.Load())
	}
	refreshed, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", true)
	if err != nil || refreshed.ExtractionID == first || calls.Load() != 2 {
		t.Fatal("alias refresh did not fetch fresh metadata", err)
	}
	cached, err := service.ExtractURL(context.Background(), "https://www.instagram.com/reel/DYm5N7px50o/", false)
	if err != nil || cached.ExtractionID != refreshed.ExtractionID || calls.Load() != 2 {
		t.Fatal("refresh did not replace shared entry", err)
	}
	if _, err := assets.GetAsset(cached.Media[0].Asset.AssetID); err != nil {
		t.Fatal(err)
	}
}

func TestInstagramConcurrentAliasesShareUpstreamRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		service := NewInstagram(testConfig(), core.NewAssets(time.Minute, 100), instagramFixtureBackend{detail: func(ctx context.Context, _ string) (core.AppDetail, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				return core.AppDetail{}, ctx.Err()
			}
			return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:one"}, nil
		}})
		defer service.Close()
		var wg sync.WaitGroup
		results := make(chan string, 4)
		for _, route := range []string{"reel", "reels", "p", "tv"} {
			wg.Go(func() {
				post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/"+route+"/DYm5N7px50o/", false)
				if err != nil {
					t.Error(err)
					return
				}
				results <- post.ExtractionID
			})
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("parallel aliases started %d lookups", calls.Load())
		}
		close(release)
		wg.Wait()
		close(results)
		first := ""
		for id := range results {
			if first == "" {
				first = id
			}
			if first != id {
				t.Error("parallel aliases did not share extraction")
			}
		}
	})
}
