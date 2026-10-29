package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func boxBytes(kind string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	b := make([]byte, 8+len(p))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:8], kind)
	copy(b[8:], p)
	return b
}
func testMovie(w, h int) []byte {
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:], uint16(w))
	binary.BigEndian.PutUint16(entry[26:], uint16(h))
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	stsd := boxBytes("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, boxBytes("avc1", entry))
	return boxBytes("moov", boxBytes("trak", boxBytes("mdia", boxBytes("hdlr", handler), boxBytes("minf", boxBytes("stbl", stsd)))))
}
func testVideo(w, h int) []byte {
	return bytes.Join([][]byte{boxBytes("ftyp", []byte("isom0000")), testMovie(w, h), boxBytes("mdat", make([]byte, 100000))}, nil)
}
func testConfig() core.Config {
	cfg := core.DefaultConfig()
	cfg.RetryDelay = 0
	cfg.VideoInfoMaxRetries = 1
	cfg.DownloadRetries = 1
	cfg.TempDir = ""
	return cfg
}
func TestDownloadStreamsHashAndHonorsDeclaredAndChunkedLimits(t *testing.T) {
	data := testVideo(1080, 1920)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/known" {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		} else {
			w.(http.Flusher).Flush()
		}
		w.Write(data)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	d := NewDownloader(cfg, nil)
	defer d.Close()
	asset := core.AssetContext{Kind: "video", Filename: "123.mp4", UpstreamURL: server.URL + "/known"}
	got, e := d.Download(context.Background(), asset, 0)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(data)
	if got.Size != int64(len(data)) || got.SHA256 != hex.EncodeToString(sum[:]) || got.Width != 1080 || got.Height != 1920 {
		t.Fatal(got)
	}
	if !got.Data.InMemory() || d.MemoryInUse() != int64(len(data)) {
		t.Fatal("small download did not stay in RAM", d.MemoryInUse())
	}
	got.Cleanup()
	if got.Data.Err() == nil || d.MemoryInUse() != 0 {
		t.Fatal("download buffer not released")
	}
	for _, path := range []string{"/known", "/chunked"} {
		asset.UpstreamURL = server.URL + path
		_, e = d.Download(context.Background(), asset, 1024)
		var api *core.Error
		if !errors.As(e, &api) || api.Code != "asset_too_large" {
			t.Fatalf("%s: %v", path, e)
		}
	}
	files, _ := os.ReadDir(cfg.TempDir)
	if len(files) != 0 {
		t.Fatal("temp files leaked")
	}
}
func TestDownloadRejectsHTMLAndUsesFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/bad" {
			io.WriteString(w, "<html>Access denied</html>")
			return
		}
		w.Write([]byte{255, 216, 255, 0, 1, 2})
	}))
	defer server.Close()
	d := NewDownloader(testConfig(), nil)
	defer d.Close()
	got, e := d.Download(context.Background(), core.AssetContext{Kind: "image", Filename: "photo.png", UpstreamURL: server.URL + "/bad", AlternateURLs: []string{server.URL + "/good"}}, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer got.Cleanup()
	if calls.Load() != 2 || got.ContentType != "image/jpeg" || got.Filename != "photo.jpg" {
		t.Fatal(got)
	}
}
func TestMP4RangeProbeSkipsLargeMediaBox(t *testing.T) {
	movie := testMovie(1920, 1080)
	var requests []int64
	data := append(boxBytes("ftyp", []byte("isom0000")), boxBytes("mdat", make([]byte, 200000))...)
	data = append(data, movie...)
	d, e := readMovieDimensions(func(start, length int64) ([]byte, error) {
		requests = append(requests, start)
		end := min(int64(len(data)), start+length)
		return data[start:end], nil
	})
	if e != nil || d.width != 1920 || d.height != 1080 || len(requests) != 2 || requests[1] != 200024 {
		t.Fatalf("%v %v %v", d, requests, e)
	}
}

type fakeApp struct {
	calls      atomic.Int32
	mediaCalls atomic.Int32
	delay      time.Duration
	slideshow  bool
	server     string
}

func (f *fakeApp) Detail(ctx context.Context, id string) (core.AppDetail, error) {
	f.calls.Add(1)
	if e := sleepContext(ctx, f.delay); e != nil {
		return core.AppDetail{}, e
	}
	detail := object{"aweme_id": id, "author": object{"unique_id": "creator"}, "statistics": object{"digg_count": 42, "play_count": 99}, "music": object{"title": "Song", "author": "Singer", "duration": 30, "play_url": object{"url_list": []any{f.server + "/music"}}}, "video": object{"duration": 12000, "play_addr": object{"url_list": []any{f.server + "/low"}, "width": 2160, "height": 3840}, "play_addr_h264": object{"url_list": []any{f.server + "/high"}, "width": 720, "height": 1280}}}
	if f.slideshow {
		detail["image_post_info"] = object{"images": []any{object{"display_image": object{"url_list": []any{f.server + "/image1"}}}, object{"display_image": object{"url_list": []any{f.server + "/image2"}}}}}
	}
	return core.AppDetail{IdentityID: "sticky", Response: object{"aweme_detail": detail}, UserAgent: "test-app"}, nil
}
func (f *fakeApp) Feed(context.Context) (map[string]any, error) { return nil, nil }
func (f *fakeApp) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	return run(func(ctx context.Context, source string, headers http.Header) (*http.Response, error) {
		return f.OpenMedia(ctx, id, source, headers)
	})
}
func (f *fakeApp) OpenMedia(ctx context.Context, id, source string, headers http.Header) (*http.Response, error) {
	f.mediaCalls.Add(1)
	if id != "sticky" {
		return nil, errors.New("wrong identity")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", source, nil)
	req.Header = headers
	return http.DefaultClient.Do(req)
}
func TestAppMetadataQualityWithoutMediaRequestsAndStickyAssets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		width, height := 540, 960
		if r.URL.Path == "/high" {
			width, height = 1080, 1920
		}
		data := testVideo(width, height)
		http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	backend := &fakeApp{server: server.URL, delay: 50 * time.Millisecond}
	cfg := testConfig()
	assets := core.NewAssets(time.Minute, 100)
	service := NewTikTok(cfg, assets, backend, nil)
	defer service.Close()
	run := func(refresh bool) {
		t.Helper()
		var wg sync.WaitGroup
		results := make(chan core.TikTokExtraction, 20)
		for n := 0; n < 20; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				post, e := service.ExtractURL(context.Background(), "https://www.tiktok.com/@creator/video/123", refresh)
				if e != nil {
					t.Error(e)
					return
				}
				results <- post
			}()
		}
		wg.Wait()
		close(results)
		var id string
		for post := range results {
			if id != "" && post.ExtractionID != id {
				t.Error("duplicate extraction")
			}
			id = post.ExtractionID
			if post.CreatorUsername == nil || *post.CreatorUsername != "creator" || post.DurationSeconds == nil || *post.DurationSeconds != 12 || post.Likes == nil || *post.Likes != 42 || post.Views == nil || *post.Views != 99 {
				t.Errorf("app metadata was not normalized: %+v", post)
			}
			if post.Width == nil || *post.Width != 2160 || post.Height == nil || *post.Height != 3840 {
				t.Error("API resolution was not selected", post)
			}
			asset, e := assets.GetAsset(post.Media[0].AssetID)
			if e != nil || asset.IdentityID != "sticky" || !strings.HasSuffix(asset.UpstreamURL, "/low") {
				t.Error(asset, e)
			}
		}
	}
	run(false)
	if backend.calls.Load() != 1 {
		t.Fatal(backend.calls.Load())
	}
	run(true)
	if backend.calls.Load() != 2 {
		t.Fatal(backend.calls.Load())
	}
	if backend.mediaCalls.Load() != 0 {
		t.Fatal("extraction contacted CDN instead of using API metadata", backend.mediaCalls.Load())
	}
}
func TestAppSlidesAndMusicReuseExtraction(t *testing.T) {
	backend := &fakeApp{slideshow: true, server: "https://cdn.example"}
	cfg := testConfig()
	service := NewTikTok(cfg, core.NewAssets(time.Minute, 100), backend, nil)
	defer service.Close()
	post, e := service.ExtractURL(context.Background(), "https://www.tiktok.com/@creator/photo/123", false)
	if e != nil {
		t.Fatal(e)
	}
	if post.ContentType != "slideshow" || len(post.Media) != 2 {
		t.Fatal(post)
	}
	music, e := service.ExtractMusic(context.Background(), "123", false)
	if e != nil || music.Title != "Song" || backend.calls.Load() != 1 {
		t.Fatal(music, e)
	}
	cached, e := service.GetExtraction(context.Background(), post.ExtractionID)
	if e != nil || cached.ExtractionID != post.ExtractionID {
		t.Fatal(e)
	}
}
func TestInstagramProviderCarouselAndCoalescing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/convert" || r.Header.Get("X-Rapidapi-Key") != "fixture-key" || r.URL.Query().Get("url") != "https://www.instagram.com/p/abc/" {
			t.Error("wrong provider request")
		}
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, `{"owner":{"username":"insta"},"media":[{"type":"video","url":"https://cdn.example/video.mp4","thumbnail":"https://cdn.example/thumb.jpg","quality":"HD"},{"type":"image","url":"https://cdn.example/image.jpg"}]}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.RapidAPIKey = "fixture-key"
	cfg.InstagramMethod = "legacy"
	cfg.RapidAPIBaseURL = server.URL
	service := NewInstagram(cfg, core.NewAssets(time.Minute, 100))
	defer service.Close()
	var wg sync.WaitGroup
	for n := 0; n < 15; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post, e := service.ExtractURL(context.Background(), "https://instagram.com/p/abc/?utm_source=test", true)
			if e != nil || post.ContentType != "carousel" || len(post.Media) != 2 || post.Media[0].Thumbnail == nil {
				t.Error(post, e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestCanceledMetadataWaiterDoesNotCancelLeader(t *testing.T) {
	m := newMemo[int](time.Minute, 10, 10)
	entered := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan int)
	go func() {
		v, _ := m.do(context.Background(), "x", false, func() (int, error) { close(entered); <-finish; return 42, nil })
		done <- v
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := m.do(ctx, "x", false, func() (int, error) { t.Error("duplicate call"); return 0, nil })
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	close(finish)
	if <-done != 42 {
		t.Fatal("leader canceled")
	}
	v, e := m.do(context.Background(), "x", false, func() (int, error) { t.Error("cache miss"); return 0, nil })
	if e != nil || v != 42 {
		t.Fatal(v, e)
	}
}

func TestMusicDoesNotDownloadOrRejectLongVideoAndSharesExpiry(t *testing.T) {
	backend := &fakeApp{server: "https://unused.example"}
	cfg := testConfig()
	cfg.MaxVideoDuration = 1
	cfg.MetadataTTL = time.Second
	cfg.CacheTTL = 10 * time.Minute
	service := NewTikTok(cfg, core.NewAssets(cfg.CacheTTL, 100), backend, nil)
	defer service.Close()
	before := time.Now()
	music, e := service.ExtractMusic(context.Background(), "123", false)
	if e != nil || music.Title != "Song" {
		t.Fatal(music, e)
	}
	expires, e := time.Parse(time.RFC3339Nano, music.ExpiresAt)
	if e != nil || expires.Before(before.Add(9*time.Minute)) || music.Audio.ExpiresAt != music.ExpiresAt {
		t.Fatal("incorrect expiry", music)
	}
	if backend.mediaCalls.Load() != 0 {
		t.Fatal("music performed video probes")
	}
}
func TestPostAndMediaExposeSameAssetExpiry(t *testing.T) {
	backend := &fakeApp{slideshow: true, server: "https://cdn.example"}
	cfg := testConfig()
	cfg.MetadataTTL = time.Second
	cfg.CacheTTL = 10 * time.Minute
	service := NewTikTok(cfg, core.NewAssets(cfg.CacheTTL, 100), backend, nil)
	defer service.Close()
	post, e := service.ExtractURL(context.Background(), "https://www.tiktok.com/@creator/photo/123", false)
	if e != nil {
		t.Fatal(e)
	}
	for _, asset := range post.Media {
		if asset.ExpiresAt != post.ExpiresAt {
			t.Fatal("post asset expiry mismatch")
		}
	}
	if post.Music.Audio.ExpiresAt != post.ExpiresAt {
		t.Fatal("music expiry mismatch")
	}
	expires, _ := time.Parse(time.RFC3339Nano, post.ExpiresAt)
	if time.Until(expires) < 9*time.Minute {
		t.Fatal("public expiry used metadata TTL")
	}
}

type closeFailureBody struct{ io.Reader }

func (closeFailureBody) Close() error {
	return core.NewError("service_not_configured", "Identity state could not be saved", 503)
}

type closeFailureBackend struct{ fakeApp }

func (f *closeFailureBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	return run(func(ctx context.Context, source string, headers http.Header) (*http.Response, error) {
		return f.OpenMedia(ctx, id, source, headers)
	})
}

func (f *closeFailureBackend) OpenMedia(context.Context, string, string, http.Header) (*http.Response, error) {
	data := testVideo(1080, 1920)
	return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: make(http.Header), Body: closeFailureBody{bytes.NewReader(data)}}, nil
}
func TestIdentitySaveFailureRejectsDownloadedMedia(t *testing.T) {
	backend := &closeFailureBackend{fakeApp: fakeApp{server: "https://unused.example"}}
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	downloader := NewDownloader(cfg, backend)
	defer downloader.Close()
	_, e := downloader.Download(context.Background(), core.AssetContext{Kind: "video", IdentityID: "sticky", Filename: "123.mp4", UpstreamURL: "https://unused.example/video"}, 0)
	var api *core.Error
	if !errors.As(e, &api) || api.Code != "service_not_configured" {
		t.Fatal("download swallowed save failure", e)
	}
	files, _ := os.ReadDir(cfg.TempDir)
	if len(files) != 0 {
		t.Fatal("failed download leaked file")
	}
}

type openFailureBackend struct{ fakeApp }

func (f *openFailureBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	return run(func(ctx context.Context, source string, headers http.Header) (*http.Response, error) {
		return f.OpenMedia(ctx, id, source, headers)
	})
}
func (f *openFailureBackend) OpenMedia(context.Context, string, string, http.Header) (*http.Response, error) {
	f.mediaCalls.Add(1)
	return nil, core.NewError("service_not_configured", "Identity owner lost", 503)
}
func TestOpenMediaOwnershipFailureIsPreservedWithoutRetries(t *testing.T) {
	backend := &openFailureBackend{}
	cfg := testConfig()
	cfg.DownloadRetries = 5
	downloader := NewDownloader(cfg, backend)
	defer downloader.Close()
	_, err := downloader.Download(context.Background(), core.AssetContext{Kind: "video", IdentityID: "sticky", UpstreamURL: "https://unused.example/video"}, 0)
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "service_not_configured" || backend.mediaCalls.Load() != 1 {
		t.Fatal(err, backend.mediaCalls.Load())
	}
}
func TestOverlongVideoRejectedWithoutMediaRequests(t *testing.T) {
	backend := &fakeApp{server: "https://unused.example"}
	cfg := testConfig()
	cfg.MaxVideoDuration = 1
	service := NewTikTok(cfg, core.NewAssets(time.Minute, 100), backend, nil)
	defer service.Close()
	_, err := service.ExtractURL(context.Background(), "https://www.tiktok.com/@creator/video/123", false)
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "content_too_long" || backend.mediaCalls.Load() != 0 {
		t.Fatal(err, backend.mediaCalls.Load())
	}
}
func TestCreatorAndCoverFallbacks(t *testing.T) {
	username := instagramCreator(object{"username": 42, "owner": object{"username": "  "}, "author": object{"username": "  author  "}})
	if username == nil || *username != "author" {
		t.Fatal(username)
	}
	cfg := testConfig()
	assets := core.NewAssets(time.Minute, 100)
	service := NewTikTok(cfg, assets, nil, nil)
	defer service.Close()
	response, _ := (&fakeApp{server: "https://cdn.example"}).Detail(context.Background(), "123")
	data := appDetail(response.Response, "123")
	obj(data["video"])["cover"] = object{"url_list": []any{}}
	obj(data["video"])["origin_cover"] = []any{"https://cdn.example/cover.jpg"}
	data["music"] = object{"play_url": "https://cdn.example/music.mp3", "cover_large": []any{}, "cover_medium": "https://cdn.example/music-cover.jpg"}
	post, err := service.normalize(context.Background(), data, core.AssetContext{}, core.TikTokResolution{SourceID: "123"})
	if err != nil || post.Cover == nil || post.Music.Cover == nil {
		t.Fatal(post, err)
	}
	cover, _ := assets.GetAsset(post.Cover.AssetID)
	if cover.UpstreamURL != "https://cdn.example/cover.jpg" {
		t.Fatal(cover)
	}
}

func TestDownloadRecognizesAllSupportedImageFormats(t *testing.T) {
	cases := []struct {
		prefix     []byte
		mime, name string
	}{{[]byte("BM1234"), "image/bmp", "x.bmp"}, {[]byte("II*\x00anything"), "image/tiff", "x.tiff"}, {boxBytes("ftyp", []byte("mif10000")), "image/heif", "x.heif"}, {boxBytes("ftyp", []byte("hevx0000")), "image/heic", "x.heic"}}
	for _, c := range cases {
		mime := detectMedia(c.prefix, "image")
		if mime != c.mime || detectedFilename("x.jpg", mime) != c.name {
			t.Fatal(mime, c.mime)
		}
	}
}
func TestFractionalJSONNumbersPreserveFPSRanking(t *testing.T) {
	data, err := decode([]byte(`{"width":1080.0,"duration":12345.0,"play_addr":{"width":1080,"height":1920,"fps":30,"url_list":["https://cdn.example/30"]},"play_addr_60":{"width":1080,"height":1920,"fps":59.94,"url_list":["https://cdn.example/60"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	selected := selectVideo(data)
	if integer(data["width"]) != 1080 || integer(data["duration"]) != 12345 || selected.URLs[0] != "https://cdn.example/60" {
		t.Fatal(selected, data)
	}
}

func TestInstagramProviderAuthenticationFailureIsNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	cfg := testConfig()
	cfg.RapidAPIKey = "fixture-key"
	cfg.InstagramMethod = "legacy"
	cfg.RapidAPIBaseURL = server.URL
	cfg.InstagramMaxAttempts = 1
	service := NewInstagram(cfg, core.NewAssets(time.Minute, 100))
	defer service.Close()
	_, err := service.ExtractURL(context.Background(), "https://www.instagram.com/p/abc/", false)
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "upstream_network_error" || api.Status != 502 {
		t.Fatal(err)
	}
}

func TestFailedDownloadReturnsByteBudgetBeforeFallback(t *testing.T) {
	data := testVideo(32, 24)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			_, _ = w.Write(bytes.Repeat([]byte("x"), len(data)/2))
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	cfg.RetryDelay = time.Millisecond
	d := NewDownloader(cfg, nil)
	defer d.Close()
	file, err := d.Download(context.Background(), core.AssetContext{Kind: "video", Filename: "x.mp4", UpstreamURL: server.URL + "/bad", AlternateURLs: []string{server.URL + "/good"}}, int64(len(data)))
	if err != nil {
		t.Fatal("failed attempt consumed fallback byte allowance", err)
	}
	if file.Size != int64(len(data)) {
		t.Fatal(file.Size)
	}
	file.Cleanup()
	files, err := os.ReadDir(cfg.TempDir)
	if err != nil || len(files) != 0 {
		t.Fatal("fallback leaked files", files, err)
	}
}

func TestDownloadAcceptsVideoWhoseDimensionsDifferFromMetadata(t *testing.T) {
	var first, fallback atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			first.Add(1)
		} else {
			fallback.Add(1)
		}
		w.Write(testVideo(540, 960))
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	assets := core.NewAssets(time.Minute, 100)
	backend := &fakeApp{server: server.URL}
	service := NewTikTok(cfg, assets, backend, nil)
	defer service.Close()
	downloader := NewDownloader(cfg, backend)
	defer downloader.Close()
	data := object{"video": object{"width": 1080, "height": 1920}}
	obj(data["video"])["play_addr"] = object{"width": 1080, "height": 1920, "url_list": []any{server.URL + "/first", server.URL + "/fallback"}}
	post, err := service.normalize(context.Background(), data, core.AssetContext{Platform: "tiktok", IdentityID: "sticky"}, core.TikTokResolution{SourceID: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if post.Width == nil || post.Height == nil || *post.Width != 1080 || *post.Height != 1920 {
		t.Fatalf("metadata dimensions changed: %v x %v", post.Width, post.Height)
	}
	asset, err := assets.GetAsset(post.Media[0].AssetID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := downloader.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal("rejected a valid video with inaccurate metadata dimensions", err)
	}
	if file.Width != 540 || file.Height != 960 {
		t.Errorf("did not report downloaded dimensions: %dx%d", file.Width, file.Height)
	}
	file.Cleanup()
	if first.Load() != 1 || fallback.Load() != 0 {
		t.Errorf("valid first source was not used once: first=%d fallback=%d", first.Load(), fallback.Load())
	}
	files, err := os.ReadDir(cfg.TempDir)
	if err != nil || len(files) != 0 {
		t.Fatal("download leaked files", files, err)
	}
}
