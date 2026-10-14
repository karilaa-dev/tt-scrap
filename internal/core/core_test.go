package core

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestAssetExpiryAndBoundedEviction(t *testing.T) {
	assets := NewAssets(time.Hour, 2)
	expires := time.Now().Add(time.Hour).UTC()
	first := assets.RegisterUntil(AssetContext{Kind: "video", Filename: "one.mp4"}, 0, expires)
	if first.ExpiresAt != expires.Format(time.RFC3339Nano) {
		t.Fatal("public asset expiry drifted")
	}
	second := assets.Register(AssetContext{Kind: "video", Filename: "two.mp4"}, 1)
	if _, err := assets.GetAsset(first.AssetID); err != nil {
		t.Fatal(err)
	}
	third := assets.Register(AssetContext{Kind: "image", Filename: "three.jpg"}, 2)
	if _, err := assets.GetAsset(second.AssetID); err == nil {
		t.Fatal("least recently used asset was not evicted")
	}
	if asset, err := assets.GetAsset(third.AssetID); err != nil || asset.Filename != "three.jpg" {
		t.Fatal("new asset was lost")
	}
	expired := assets.RegisterUntil(AssetContext{Kind: "video"}, 0, time.Now().Add(-time.Second))
	var missing *Error
	if _, err := assets.GetAsset(expired.AssetID); !errors.As(err, &missing) || missing.Status != 404 || missing.Code != "asset_not_found_or_expired" {
		t.Fatal("expired asset contract changed")
	}
}
func TestLimiterCancelsWaitingWorkAndReleasesOnce(t *testing.T) {
	limiter := NewLimiter(1, 1)
	release, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = limiter.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled work acquired capacity")
	}
	release()
	release()
	next, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next()
}
func TestConfigRejectsInvalidLimitsAndDoesNotRequireDBForWeb(t *testing.T) {
	base := map[string]string{"TT_SCRAP_API_KEY": "test-key-sixteen-characters", "TIKTOK_METHOD": "web"}
	config, err := ParseConfig(base)
	if err != nil || config.Port != 8000 {
		t.Fatal("web configuration requires an app database", err)
	}
	for _, entry := range [][2]string{{"TIKTOK_APP_TIMEOUT_SECONDS", "NaN"}, {"TIKTOK_APP_TIMEOUT_SECONDS", "+Inf"}, {"EXTRACTION_CONCURRENCY", "0"}, {"MAX_ASSET_BYTES", "-1"}, {"PROXY_INCLUDE_HOST", "maybe"}, {"TIKTOK_METHOD", "invalid"}} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[entry[0]] = entry[1]
		if _, err := ParseConfig(env); err == nil {
			t.Errorf("accepted invalid %s", entry[0])
		}
	}
	base["TIKTOK_METHOD"] = "app"
	if _, err := ParseConfig(base); err == nil {
		t.Fatal("app accepted absent database")
	}
}

func TestCacheExpiresRecentEntryBeforeEvictingLiveLRU(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewCache[string](time.Hour, 2)
		c.SetUntil("short", "expires first", time.Now().Add(time.Second))
		c.Set("live", "keep this")
		if _, ok := c.Get("short"); !ok {
			t.Fatal("new entry missing")
		}
		time.Sleep(2 * time.Second)
		c.Set("new", "new value")
		if value, ok := c.Get("live"); !ok || value != "keep this" {
			t.Fatal("expired recent entry displaced a live token")
		}
		if _, ok := c.Get("short"); ok {
			t.Fatal("expired entry survived")
		}
	})
}

func TestCacheExpirationTracksReplacementDeleteAndClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewCache[int](time.Hour, 2)
		c.SetUntil("updated", 1, time.Now().Add(time.Second))
		c.Set("updated", 2)
		c.Set("live", 3)
		time.Sleep(2 * time.Second)
		if value, ok := c.Get("updated"); !ok || value != 2 {
			t.Fatal("replacement kept the old expiry")
		}
		c.SetUntil("expired", 4, time.Now().Add(-time.Second))
		if value, ok := c.Get("live"); !ok || value != 3 {
			t.Fatal("past-due insertion evicted a live entry")
		}
		c.Delete("updated")
		c.Set("new", 5)
		c.SetUntil("live", 6, time.Now().Add(time.Second))
		time.Sleep(2 * time.Second)
		c.Set("replacement", 7)
		if value, ok := c.Get("new"); !ok || value != 5 {
			t.Fatal("updated expiry was ignored")
		}
		c.Clear()
		c.Set("after-clear", 8)
		if _, ok := c.Get("replacement"); ok {
			t.Fatal("clear retained a value")
		}
		if value, ok := c.Get("after-clear"); !ok || value != 8 {
			t.Fatal("cache unusable after clear")
		}
	})
}
