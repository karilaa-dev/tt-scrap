package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type instagramMediaBackend struct {
	calls atomic.Int32
	data  []byte
}

func (b *instagramMediaBackend) Detail(context.Context, string) (core.AppDetail, error) {
	return core.AppDetail{}, errors.New("unused")
}
func (b *instagramMediaBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	if id != "instagram:one" {
		return errors.New("wrong account")
	}
	b.calls.Add(1)
	return run(func(ctx context.Context, source string, h http.Header) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/mp4"}}, ContentLength: int64(len(b.data)), Body: io.NopCloser(bytes.NewReader(b.data))}, nil
	})
}

func TestInstagramDownloadsUsePinnedBackend(t *testing.T) {
	cfg := testConfig()
	cfg.DownloadParts = 1
	backend := &instagramMediaBackend{data: largeVideo(4096)}
	d := NewDownloader(cfg, nil, backend)
	defer d.Close()
	asset := core.AssetContext{Platform: "instagram", IdentityID: "instagram:one", Kind: "video", Filename: "video.mp4", UpstreamURL: "https://cdn.example/video.mp4"}
	file, err := d.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Cleanup()
	if backend.calls.Load() != 1 || !bytes.Equal(file.Data.Bytes(), backend.data) {
		t.Fatal("Instagram download did not use pinned account")
	}
	err = d.WithIdentity(context.Background(), asset.IdentityID, func(download core.DownloadFunc) error {
		file, err := download(context.Background(), asset, 0, nil)
		if err != nil {
			return err
		}
		file.Cleanup()
		other := asset
		other.IdentityID = "instagram:two"
		if _, err = download(context.Background(), other, 0, nil); err == nil {
			t.Fatal("batch accepted different account")
		}
		return nil
	})
	if err != nil || backend.calls.Load() != 2 {
		t.Fatal("batch did not reuse pinned account", err)
	}
	missing := NewDownloader(cfg, nil)
	defer missing.Close()
	if _, err := missing.Download(context.Background(), asset, 0); err == nil {
		t.Fatal("missing Instagram backend fell through to direct download")
	}
}

type instagramCarouselBackend struct {
	response map[string]any
	client   *http.Client
}

func (b instagramCarouselBackend) Detail(context.Context, string) (core.AppDetail, error) {
	return core.AppDetail{Response: b.response, IdentityID: "instagram:carousel"}, nil
}
func (b instagramCarouselBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	if id != "instagram:carousel" {
		return errors.New("wrong carousel route")
	}
	return run(func(ctx context.Context, source string, headers http.Header) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		req.Header = headers
		return b.client.Do(req)
	})
}

func TestInstagramMixedCarouselExtractionThenDownload(t *testing.T) {
	var photo bytes.Buffer
	if err := png.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 16, 24))); err != nil {
		t.Fatal(err)
	}
	video := largeVideo(4096)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization on CDN")
		}
		if r.URL.Path == "/video.mp4" {
			w.Write(video)
		} else {
			w.Write(photo.Bytes())
		}
	}))
	defer server.Close()
	response := instagramFixture(t, fmt.Sprintf(`{"items":[{"id":"3902057773219159336_123","media_type":8,"carousel_media_count":3,"carousel_media":[
 {"media_type":1,"image_versions2":{"candidates":[{"url":"%s/first.png","width":16,"height":24}]}},
 {"media_type":2,"video_versions":[{"url":"%s/video.mp4","width":1080,"height":1920}]},
 {"media_type":1,"image_versions2":{"candidates":[{"url":"%s/last.png","width":16,"height":24}]}}
 ]}]}`, server.URL, server.URL, server.URL))
	cfg := testConfig()
	cfg.DownloadParts = 1
	assets := core.NewAssets(cfg.CacheTTL, cfg.CacheMaxEntries)
	backend := instagramCarouselBackend{response: response, client: server.Client()}
	service := NewInstagram(cfg, assets, backend)
	defer service.Close()
	downloader := NewDownloader(cfg, nil, backend)
	defer downloader.Close()
	post, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/DYm5N7px50o/", false)
	if err != nil {
		t.Fatal(err)
	}
	if post.ContentType != "carousel" || len(post.Media) != 3 {
		t.Fatal("incomplete carousel")
	}
	err = downloader.WithIdentity(context.Background(), "instagram:carousel", func(download core.DownloadFunc) error {
		for n, item := range post.Media {
			asset, err := assets.GetAsset(item.Asset.AssetID)
			if err != nil {
				return err
			}
			got, err := download(context.Background(), asset, 0, nil)
			if err != nil {
				return err
			}
			want := photo.Bytes()
			if n == 1 {
				want = video
			}
			equal := bytes.Equal(got.Data.Bytes(), want)
			got.Cleanup()
			if !equal || item.Position != n {
				return errors.New("carousel bytes or order changed")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
