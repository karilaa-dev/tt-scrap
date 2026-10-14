package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
func TestWebJSONPreservesIDAndSIGIFallback(t *testing.T) {
	for _, html := range []string{`<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{"statusCode":0,"itemInfo":{"itemStruct":{"id":7695962883523464478,"video":{"width":1080}}}}}}</script>`, `<script type="application/json" id="SIGI_STATE">{"ItemModule":{"7695962883523464478":{"id":"7695962883523464478","video":{"width":1080}}}}</script>`} {
		detail, status, e := ParseWebDetail([]byte(html), "7695962883523464478")
		if e != nil || status != 0 || str(detail["id"]) != "7695962883523464478" {
			t.Fatalf("detail=%v status=%d err=%v", detail, status, e)
		}
	}
}
func TestWebChallengeSolvesNativeSHA256(t *testing.T) {
	prefix := []byte("fixture-seed")
	sum := sha256.Sum256(append(prefix, []byte("42")...))
	challenge, _ := json.Marshal(object{"v": object{"a": base64.StdEncoding.EncodeToString(prefix), "c": base64.StdEncoding.EncodeToString(sum[:])}})
	body := []byte(fmt.Sprintf(`<div id="cs" class="%s"></div><div id="wci" class="_wafchallengeid"></div>`, base64.StdEncoding.EncodeToString(challenge)))
	jar, _ := cookiejar.New(nil)
	ok, e := solveWebChallenge(context.Background(), body, jar)
	if e != nil || !ok {
		t.Fatal(e)
	}
	u, _ := url.Parse("https://www.tiktok.com/")
	cookies := jar.Cookies(u)
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	raw, _ := base64.StdEncoding.DecodeString(cookies[0].Value)
	decoded, _ := decode(raw)
	if decoded["d"] != "NDI=" {
		t.Fatal(decoded)
	}
}
func TestWebQualityPrefersMeasuredMVMAFAtMaximumResolution(t *testing.T) {
	source := selectVideo(object{"width": 720, "height": 1280, "playAddr": "https://x/regular", "bitrateInfo": []any{object{"GearName": "normal_1080", "Bitrate": 9000000, "MVMAF": `{"v2.0":{"ori":{"v1080":91}}}`, "PlayAddr": object{"Width": 1080, "Height": 1920, "UrlList": []any{"https://x/highrate"}}}, object{"GearName": "normal_1080", "Bitrate": 5000000, "MVMAF": `{"v2.0":{"ori":{"v1080":95}}}`, "PlayAddr": object{"Width": 1080, "Height": 1920, "UrlList": []any{"https://x/best"}}}}}, false)
	if source.URLs[0] != "https://x/best" {
		t.Fatal(source)
	}
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
	d := NewDownloader(cfg, nil, nil)
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
	got.Cleanup()
	if _, e = os.Stat(got.Path); !os.IsNotExist(e) {
		t.Fatal("temp file not removed")
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
func TestDownloadRejectsHTMLAndFallbackPreservesCookies(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/bad" {
			io.WriteString(w, "<html>Access denied</html>")
			return
		}
		if r.Header.Get("Cookie") != "session=sticky" {
			t.Error("missing cookie")
		}
		w.Write([]byte{255, 216, 255, 0, 1, 2})
	}))
	defer server.Close()
	d := NewDownloader(testConfig(), nil, nil)
	defer d.Close()
	got, e := d.Download(context.Background(), core.AssetContext{Kind: "image", Filename: "photo.png", UpstreamURL: server.URL + "/bad", AlternateURLs: []string{server.URL + "/good"}, Cookies: map[string]string{"session": "sticky"}}, 0)
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

func TestWebNormalizationPreservesCookies(t *testing.T) {
	cfg := testConfig()
	cfg.TikTokMethod = "web"
	assets := core.NewAssets(time.Minute, 100)
	service := NewTikTok(cfg, assets, nil, nil)
	defer service.Close()

	data, e := parseFixtureWeb("123")
	if e != nil {
		t.Fatal(e)
	}
	post, e := service.normalize(context.Background(), data, core.AssetContext{Platform: "tiktok", Cookies: map[string]string{"web_session": "sticky"}}, core.TikTokResolution{SourceID: "123", SourceURL: "https://www.tiktok.com/@webuser/video/123", ResolvedURL: "https://www.tiktok.com/@webuser/video/123"}, false)
	if e != nil {
		t.Fatal(e)
	}
	asset, e := assets.GetAsset(post.Media[0].AssetID)
	if e != nil || asset.Cookies["web_session"] != "sticky" || asset.UpstreamURL != "https://cdn.example/video.mp4" || *post.DurationSeconds != 15 {
		t.Fatal(asset, e)
	}
}

func parseFixtureWeb(id string) (object, error) {
	return decode([]byte(fmt.Sprintf(`{"id":"%s","author":{"uniqueId":"webuser"},"video":{"width":1080,"height":1920,"duration":15,"playAddr":"https://cdn.example/video.mp4"}}`, id)))
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
	downloader := NewDownloader(cfg, backend, nil)
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
	downloader := NewDownloader(cfg, backend, nil)
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
	data, _ := parseFixtureWeb("123")
	obj(data["video"])["cover"] = object{"url_list": []any{}}
	obj(data["video"])["originCover"] = []any{"https://cdn.example/cover.jpg"}
	data["music"] = object{"playUrl": "https://cdn.example/music.mp3", "coverLarge": []any{}, "coverMedium": "https://cdn.example/music-cover.jpg"}
	post, err := service.normalize(context.Background(), data, core.AssetContext{}, core.TikTokResolution{SourceID: "123"}, false)
	if err != nil || post.Cover == nil || post.Music.Cover == nil {
		t.Fatal(post, err)
	}
	cover, _ := assets.GetAsset(post.Cover.AssetID)
	if cover.UpstreamURL != "https://cdn.example/cover.jpg" {
		t.Fatal(cover)
	}
}
func TestWebProxyDataOnlyDownloadsDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte{255, 216, 255, 0, 1, 2}) }))
	defer server.Close()
	cfg := testConfig()
	cfg.ProxyDataOnly = true
	downloader := NewDownloader(cfg, nil, nil)
	defer downloader.Close()
	file, err := downloader.Download(context.Background(), core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "test.jpg", ProxyURL: "invalid-proxy", UpstreamURL: server.URL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Cleanup()
}
func TestSeparateAudioMuxPreservesBothStreamsAndRejectsOversize(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is required for mux verification")
	}
	dir := t.TempDir()
	videoPath, audioPath := filepath.Join(dir, "video.mp4"), filepath.Join(dir, "audio.m4a")
	commands := [][]string{{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=black:s=32x24:r=1:d=1", "-an", "-c:v", "libx264", "-threads", "1", "-movflags", "+faststart", videoPath}, {"-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "3", "-c:a", "aac", "-threads", "1", audioPath}}
	for _, args := range commands {
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	video, _ := os.ReadFile(videoPath)
	audio, _ := os.ReadFile(audioPath)
	var trackRequests atomic.Int32
	bothStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trackRequests.Add(1) == 2 {
			close(bothStarted)
		}
		select {
		case <-bothStarted:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/audio" {
			w.Write(audio)
		} else {
			w.Write(video)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = dir
	cfg.DownloadTimeout = 2 * time.Second
	downloader := NewDownloader(cfg, nil, nil)
	defer downloader.Close()
	asset := core.AssetContext{Platform: "tiktok", Kind: "video", Filename: "123.mp4", UpstreamURL: server.URL + "/video", Audio: &core.AuxiliaryAsset{UpstreamURL: server.URL + "/audio", ContentType: "audio/mp4"}}
	file, err := downloader.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Cleanup()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", file.Path).Output()
	if err != nil || !strings.Contains(string(out), "video") || !strings.Contains(string(out), "audio") {
		t.Fatal(string(out), err)
	}
	out, err = exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file.Path).Output()
	if err != nil {
		t.Fatal(err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration < 0.9 || duration > 1.25 {
		t.Fatalf("one-second video with three-second audio muxed to %v seconds: %v", duration, err)
	}
	if file.Width != 32 || file.Height != 24 {
		t.Fatal(file)
	}
	cfg.DownloadConcurrency = 1
	serial := NewDownloader(cfg, nil, nil)
	defer serial.Close()
	serialized, err := serial.Download(context.Background(), asset, 0)
	if err != nil {
		t.Fatal("one transfer slot must serialize both tracks and still mux", err)
	}
	serialized.Cleanup()
	_, err = downloader.Download(context.Background(), asset, int64(max(len(video), len(audio))))
	var api *core.Error
	if !errors.As(err, &api) || api.Code != "asset_too_large" {
		t.Fatal("oversized mux was returned", err)
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
	candidates := appCandidates(data)
	if integer(data["width"]) != 1080 || integer(data["duration"]) != 12345 || candidates[0].URLs[0] != "https://cdn.example/60" {
		t.Fatal(candidates, data)
	}
}

func TestInstagramProviderAuthenticationFailureIsNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	cfg := testConfig()
	cfg.RapidAPIKey = "fixture-key"
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

func TestAdaptiveTracksShareByteBudgetBeforeMux(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(strconv.FormatBool(chunked), func(t *testing.T) {
			video := testVideo(32, 24)
			audio := append([]byte("ID3"), make([]byte, len(video)-3)...)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := video
				if r.URL.Path == "/audio" {
					data = audio
				}
				if chunked {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.TempDir = t.TempDir()
			d := NewDownloader(cfg, nil, nil)
			defer d.Close()
			asset := core.AssetContext{Kind: "video", Filename: "x.mp4", UpstreamURL: server.URL + "/video", Audio: &core.AuxiliaryAsset{UpstreamURL: server.URL + "/audio"}}
			result, err := d.Download(context.Background(), asset, int64(len(video)+len(audio)-1))
			result.Cleanup()
			var api *core.Error
			if !errors.As(err, &api) || api.Code != "asset_too_large" {
				t.Fatal("track sum reached mux instead of byte limit", err)
			}
			files, err := os.ReadDir(cfg.TempDir)
			if err != nil || len(files) != 0 {
				t.Fatal("failed tracks leaked temporary files", files, err)
			}
		})
	}
}

func TestWebMediaRetryRotatesConfiguredProxies(t *testing.T) {
	var first, backup atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { first.Add(1); w.WriteHeader(503) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backup.Add(1)
		if r.URL.Host != "cdn.example" {
			t.Error("proxy lost original URL", r.URL)
		}
		_, _ = w.Write([]byte{255, 216, 255, 0, 1, 2})
	}))
	defer good.Close()
	cfg := testConfig()
	cfg.DownloadRetries = 2
	cfg.RetryDelay = time.Millisecond
	d := NewDownloader(cfg, nil, []string{good.URL, bad.URL})
	defer d.Close()
	file, err := d.Download(context.Background(), core.AssetContext{Platform: "tiktok", Kind: "image", Filename: "x.jpg", UpstreamURL: "http://cdn.example/photo", ProxyURL: bad.URL}, 0)
	if err != nil {
		t.Fatal("healthy backup proxy was not attempted", err)
	}
	file.Cleanup()
	if first.Load() != 1 || backup.Load() != 1 {
		t.Fatal("initial proxy was not retained before rotation", first.Load(), backup.Load())
	}
}

func TestAdaptiveFailureCancelsSiblingAndCleansFiles(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio" {
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(403)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	d := NewDownloader(cfg, nil, nil)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	file, err := d.Download(ctx, core.AssetContext{Kind: "video", UpstreamURL: server.URL + "/video", Audio: &core.AuxiliaryAsset{UpstreamURL: server.URL + "/audio"}}, 0)
	file.Cleanup()
	if err == nil || ctx.Err() != nil {
		t.Fatal("failure did not cancel the sibling promptly", err, ctx.Err())
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("audio request leaked")
	}
	files, err := os.ReadDir(cfg.TempDir)
	if err != nil || len(files) != 0 {
		t.Fatal("failed adaptive download leaked files", files, err)
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
	d := NewDownloader(cfg, nil, nil)
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

func TestSelectedRenditionDimensionsValidateDownloadedFallbacks(t *testing.T) {
	for _, app := range []bool{true, false} {
		t.Run(fmt.Sprintf("app=%t", app), func(t *testing.T) {
			var mismatches, matches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/match" {
					matches.Add(1)
					w.Write(testVideo(1080, 1920))
				} else {
					mismatches.Add(1)
					w.Write(testVideo(540, 960))
				}
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.TempDir = t.TempDir()
			assets := core.NewAssets(time.Minute, 100)
			backend := &fakeApp{server: server.URL}
			service := NewTikTok(cfg, assets, backend, nil)
			defer service.Close()
			downloader := NewDownloader(cfg, backend, nil)
			defer downloader.Close()
			for _, fallback := range []string{"/match", "/mismatch-again"} {
				data := object{"video": object{"width": 1080, "height": 1920}}
				base := core.AssetContext{Platform: "tiktok"}
				sources := []any{server.URL + "/mismatch", server.URL + fallback}
				if app {
					obj(data["video"])["play_addr"] = object{"width": 1080, "height": 1920, "url_list": sources}
					base.IdentityID = "sticky"
				} else {
					obj(data["video"])["playAddr"] = object{"Width": 1080, "Height": 1920, "UrlList": sources}
				}
				post, err := service.normalize(context.Background(), data, base, core.TikTokResolution{SourceID: "123"}, app)
				if err != nil {
					t.Fatal(err)
				}
				asset, err := assets.GetAsset(post.Media[0].AssetID)
				if err != nil {
					t.Fatal(err)
				}
				file, err := downloader.Download(context.Background(), asset, 0)
				if fallback == "/match" {
					if err != nil || file.Width != 1080 || file.Height != 1920 {
						t.Errorf("did not return matching fallback: file=%+v error=%v", file, err)
					}
				} else if err == nil {
					t.Error("accepted a video that does not match advertised dimensions")
				}
				file.Cleanup()
			}
			if mismatches.Load() != 3 || matches.Load() != 1 {
				t.Errorf("fallbacks not validated: mismatches=%d matches=%d", mismatches.Load(), matches.Load())
			}
			files, err := os.ReadDir(cfg.TempDir)
			if err != nil || len(files) != 0 {
				t.Fatal("rendition validation leaked files", files, err)
			}
		})
	}
}
