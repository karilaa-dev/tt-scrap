package core

import (
	"context"
	"errors"
	"testing"
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
