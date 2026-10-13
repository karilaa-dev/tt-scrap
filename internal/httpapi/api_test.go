package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type fakeTikTok struct {
	extraction core.TikTokExtraction
	resolveErr error
	musicID    string
	refresh    bool
}

func (f *fakeTikTok) ResolveURL(_ context.Context, url string) (core.TikTokResolution, error) {
	return core.TikTokResolution{Platform: "tiktok", SourceID: "7695962883523464478", SourceURL: url, ResolvedURL: url}, f.resolveErr
}
func (f *fakeTikTok) ExtractURL(_ context.Context, _ string, refresh bool) (core.TikTokExtraction, error) {
	f.refresh = refresh
	return f.extraction, nil
}
func (f *fakeTikTok) GetExtraction(_ context.Context, id string) (core.TikTokExtraction, error) {
	if id == "missing" || id == "" {
		return core.TikTokExtraction{}, core.MissingExtraction()
	}
	return f.extraction, nil
}
func (f *fakeTikTok) ExtractMusic(_ context.Context, id string, _ bool) (core.TikTokMusic, error) {
	f.musicID = id
	return core.TikTokMusic{Platform: "tiktok", SourceID: id, Audio: core.AssetDescriptor{}}, nil
}

type fakeInstagram struct{ extraction core.InstagramExtraction }

func (f *fakeInstagram) ExtractURL(context.Context, string, bool) (core.InstagramExtraction, error) {
	return f.extraction, nil
}
func (f *fakeInstagram) GetExtraction(context.Context, string) (core.InstagramExtraction, error) {
	return f.extraction, nil
}

type fakeDownloader struct {
	directory string
	data      []byte
	mime      string
	fail      string
	mu        sync.Mutex
	files     []string
	calls     int
}

func (d *fakeDownloader) Download(ctx context.Context, asset core.AssetContext, _ int64) (*core.Downloaded, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if asset.UpstreamURL == d.fail && d.fail != "" {
		return nil, core.NewError("upstream_network_error", "Fixture failed", 502)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(d.directory, "download-")
	if err != nil {
		return nil, err
	}
	_, err = f.Write(d.data)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.files = append(d.files, f.Name())
	d.mu.Unlock()
	hash := sha256.Sum256(d.data)
	return &core.Downloaded{Path: f.Name(), Filename: asset.Filename, ContentType: d.mime, Size: int64(len(d.data)), SHA256: hex.EncodeToString(hash[:]), Remove: func() { _ = os.Remove(f.Name()) }}, nil
}
func fixture(t *testing.T) (Dependencies, *fakeTikTok, *fakeDownloader, *core.Assets) {
	t.Helper()
	c := core.DefaultConfig()
	c.APIKey = "test-secret-key-with-length"
	c.RapidAPIKey = "configured"
	c.ThumbnailWait = 0
	assets := core.NewAssets(time.Minute, 100)
	tt := &fakeTikTok{extraction: core.TikTokExtraction{Platform: "tiktok", SourceID: "7695962883523464478", ContentType: "video", Media: []core.AssetDescriptor{}}}
	downloader := &fakeDownloader{directory: t.TempDir(), data: []byte("media"), mime: "video/mp4"}
	return Dependencies{Config: c, TikTok: tt, Instagram: &fakeInstagram{}, Downloader: downloader, Assets: assets, OpenAPI: []byte(`{"openapi":"3.1.0","paths":{}}`)}, tt, downloader, assets
}
func request(handler http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.Header.Set("Authorization", "Bearer test-secret-key-with-length")
	}
	r.Header.Set("X-Request-ID", " fixture-request ")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestHTTPContract(t *testing.T) {
	dependencies, tt, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	cases := []struct {
		method, path, body string
		auth               bool
		status             int
		contains           string
	}{
		{"GET", "/health/live", "", false, 200, `"status":"ok"`}, {"GET", "/health/ready", "", false, 200, `"status":"ready"`},
		{"POST", "/v1/tiktok/resolutions", `{"url":"https://www.tiktok.com/@x/video/123"}`, false, 401, `"code":"authentication_required"`},
		{"POST", "/v1/tiktok/resolutions", `{`, false, 422, `"code":"validation_error"`},
		{"POST", "/v1/tiktok/extractions", `{"url":"mailto:x@example.com"}`, true, 422, `"code":"validation_error"`},
		{"POST", "/v1/tiktok/extractions", `{"url":"https://www.tiktok.com/@x/video/123","refresh":"yes"}`, true, 200, `"cover":null`},
		{"POST", "/v1/tiktok/music", `{"video_id":7695962883523464478}`, true, 200, `"source_id":"7695962883523464478"`},
		{"POST", "/v1/tiktok/music", `{"video_id":"1/1"}`, true, 422, `"code":"validation_error"`},
		{"POST", "/v1/tiktok/music", `{"video_id":1e999999999}`, true, 422, `"code":"validation_error"`},
		{"POST", "/v1/tiktok/music", `{"video_id":0}`, true, 422, `"code":"validation_error"`},
		{"GET", "/v1/assets/unknown", "", true, 404, `"code":"asset_not_found_or_expired"`},
		{"GET", "/v1/tiktok/extractions", "", true, 405, `"detail":"Method Not Allowed"`}, {"GET", "/missing", "", false, 404, `"detail":"Not Found"`},
		{"GET", "/docs", "", false, 200, "SwaggerUIBundle"}, {"GET", "/redoc", "", false, 200, "redoc"}, {"GET", "/openapi.json", "", false, 200, `"openapi":"3.1.0"`},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path+tc.contains, func(t *testing.T) {
			response := request(handler, tc.method, tc.path, tc.body, tc.auth)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.contains) {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Request-ID") != "fixture-request" || !strings.HasPrefix(response.Header().Get("Server-Timing"), "app;dur=") {
				t.Fatal("missing correlation/timing headers")
			}
		})
	}
	if tt.musicID != "7695962883523464478" || !tt.refresh {
		t.Fatal("lossless ID or refresh coercion failed")
	}
	response := request(handler, "POST", "/v1/tiktok/music/", `{}`, true)
	if response.Code != 307 || response.Header().Get("Location") != "/v1/tiktok/music" {
		t.Fatalf("redirect: %d %v", response.Code, response.Header())
	}
	response = request(handler, "HEAD", "/openapi.json", "", false)
	if response.Code != 200 || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "30" {
		t.Fatal("OpenAPI HEAD returned a body")
	}
}

func TestReadinessAndSanitizedFailure(t *testing.T) {
	dependencies, tt, _, _ := fixture(t)
	dependencies.IsReady = func() bool { return false }
	response := request(NewHandler(dependencies), "GET", "/health/ready", "", false)
	if response.Code != 503 {
		t.Fatal(response.Code)
	}
	tt.resolveErr = errors.New("secret upstream URL password")
	response = request(NewHandler(dependencies), "POST", "/v1/tiktok/resolutions", `{"url":"https://www.tiktok.com/x"}`, true)
	if response.Code != 500 || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

func TestAssetHTTPStreamsExactBytesAndCleansTemporaryFile(t *testing.T) {
	dependencies, _, downloader, assets := fixture(t)
	downloader.data = bytes.Repeat([]byte("abcd"), 512*1024+77)
	descriptor := assets.Register(core.AssetContext{Platform: "tiktok", Filename: "movie.webm", ContentType: "video/mp4"}, 0)
	server := httptest.NewServer(NewHandler(dependencies))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+descriptor.DownloadURL, nil)
	req.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, downloader.data) || response.ContentLength != int64(len(body)) || len(response.TransferEncoding) > 0 {
		t.Fatalf("stream mismatch: length %d bytes %d encoding %v", response.ContentLength, len(body), response.TransferEncoding)
	}
	hash := sha256.Sum256(body)
	sha := hex.EncodeToString(hash[:])
	if response.Header.Get("X-Content-Sha256") != sha || response.Header.Get("Etag") != `"`+sha+`"` || response.Header.Get("Content-Disposition") != `attachment; filename="movie.mp4"` {
		t.Fatalf("headers %v", response.Header)
	}
	server.Close()
	entries, err := os.ReadDir(downloader.directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("download not removed: %v %v", entries, err)
	}
}

func TestInvalidTelegramBodiesFailBeforeExtraction(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	bodies := []string{`{"source":{"url":"https://x.test","extraction_id":"x"},"telegram":{"chat_id":1}}`, `{"source":{"video_id":123},"telegram":{"chat_id":1}}`, `{"source":{"extraction_id":"x"},"refresh":true,"telegram":{"chat_id":1}}`, `{"source":{"url":"https://x.test"},"telegram":{"chat_id":1,"media":null}}`, `{"source":{"url":"https://x.test"},"telegram":{"chat_id":1,"width":"x"}}`, `{"source":{"url":"https://x.test"},"telegram":{"chat_id":1,"caption_entities":[1]}}`}
	for _, body := range bodies {
		response := request(handler, "POST", "/v1/tiktok/telegram-deliveries", body, true)
		if response.Code != 422 {
			t.Fatalf("got %d for %s", response.Code, body)
		}
	}
}

func TestRequestBodyLimit(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	body := `{"url":"https://example.com/` + strings.Repeat("x", 1<<20) + `"}`
	response := request(NewHandler(dependencies), "POST", "/v1/tiktok/extractions", body, true)
	if response.Code != 422 {
		t.Fatal(response.Code)
	}
}

func TestAPIUsesOriginalOpenAPIContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	dependencies, _, _, _ := fixture(t)
	dependencies.OpenAPI = data
	response := request(NewHandler(dependencies), "GET", "/openapi.json", "", false)
	if !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatal("schema changed")
	}
	var schema struct {
		Paths map[string]any `json:"paths"`
	}
	if err = json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Paths) != 9 {
		t.Fatalf("want 9 paths, got %d", len(schema.Paths))
	}
}

func TestPythonURLAndJSONContentTypeCompatibility(t *testing.T) {
	dependencies, tt, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	for _, contentType := range []string{"application/json; charset=utf-8", "application/vnd.test+json", "text/plain"} {
		r := httptest.NewRequest("POST", "/v1/tiktok/resolutions", strings.NewReader(`{"url":" HTTPS://WWW.TIKTOK.COM:443/@x/video/123 "}`))
		r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if contentType == "text/plain" {
			if w.Code != 422 {
				t.Fatalf("text body accepted: %d", w.Code)
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"source_url":"https://www.tiktok.com/@x/video/123"`) {
			t.Fatalf("normalization: %d %s", w.Code, w.Body.String())
		}
	}
	tt.resolveErr = context.DeadlineExceeded
	response := request(handler, "POST", "/v1/tiktok/resolutions", `{"url":"https://www.tiktok.com/@x/video/123"}`, true)
	if response.Code != 504 || !strings.Contains(response.Body.String(), `"code":"upstream_timeout"`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

func TestPythonIntegerAndNullCompatibility(t *testing.T) {
	dependencies, tt, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	for _, input := range []string{`123.0`, `1.23e2`, `"+00123"`, `"123.0"`} {
		response := request(handler, "POST", "/v1/tiktok/music", `{"video_id":`+input+`}`, true)
		if response.Code != 200 || tt.musicID != "123" {
			t.Fatalf("%s: %d %s", input, response.Code, response.Body.String())
		}
	}
	for _, input := range []string{`null`, `1.2`, `"1e2"`, `{}`} {
		response := request(handler, "POST", "/v1/tiktok/music", `{"video_id":`+input+`}`, true)
		if response.Code != 422 {
			t.Fatalf("accepted %s: %d", input, response.Code)
		}
	}
	response := request(handler, "POST", "/v1/tiktok/extractions", `{"url":"https://www.tiktok.com/@x/video/123"}`, true)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"creator_username", "cover", "width", "height", "duration_seconds", "likes", "views", "music"} {
		value, ok := result[key]
		if !ok || value != nil {
			t.Fatalf("nullable %s: %v %v", key, value, ok)
		}
	}
}

type delayedWriter struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (w delayedWriter) Write(data []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseRecorder.Write(data)
}

func TestRequestLogIncludesResponseTransfer(t *testing.T) {
	dependencies, _, downloader, assets := fixture(t)
	descriptor := assets.Register(core.AssetContext{Filename: "video.mp4"}, 0)
	var event RequestEvent
	dependencies.OnRequest = func(value RequestEvent) { event = value }
	r := httptest.NewRequest("GET", descriptor.DownloadURL, nil)
	r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
	writer := delayedWriter{ResponseRecorder: httptest.NewRecorder(), delay: 10 * time.Millisecond}
	NewHandler(dependencies).ServeHTTP(writer, r)
	if writer.Code != 200 || event.Duration < writer.delay || event.Bytes != int64(len(downloader.data)) || event.Path != "/v1/assets/{token}" {
		t.Fatalf("log omitted transfer: %#v", event)
	}
}

func TestTrailingSlashRedirectPreservesPublicOrigin(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	r := httptest.NewRequest("POST", "http://backend.internal/v1/tiktok/music/?q=a%2Fb", strings.NewReader(`{}`))
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 307 || w.Header().Get("Location") != "/v1/tiktok/music?q=a%2Fb" {
		t.Fatalf("redirect must retain the client's origin: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestURLLengthLimitAcrossPublicRoutes(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	handler := NewHandler(dependencies)
	for _, route := range []string{"/v1/tiktok/resolutions", "/v1/tiktok/extractions", "/v1/instagram/extractions", "/v1/tiktok/telegram-deliveries", "/v1/instagram/telegram-deliveries"} {
		t.Run(route, func(t *testing.T) {
			prefix := "https://www.tiktok.com/@x/video/123?"
			if strings.Contains(route, "instagram") {
				prefix = "https://www.instagram.com/p/test/?"
			}
			url := prefix + strings.Repeat("x", 2084-len(prefix))
			body := fields{"url": url}
			if strings.HasSuffix(route, "telegram-deliveries") {
				body = fields{"source": fields{"url": url}, "telegram": fields{"chat_id": 1}}
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			response := request(handler, "POST", route, string(encoded), true)
			if response.Code != 422 || !strings.Contains(response.Body.String(), `"code":"validation_error"`) {
				t.Fatalf("overlong URL accepted: %d", response.Code)
			}
		})
	}
	prefix := "https://www.tiktok.com/@x/video/123?"
	boundary := prefix + strings.Repeat("x", 2083-len(prefix))
	for _, tc := range []struct {
		url    string
		status int
	}{
		{boundary, 200}, {boundary + "x", 422}, {" " + boundary, 422},
		{prefix + strings.Repeat("é", 2083-len(prefix)), 422},
	} {
		body, _ := json.Marshal(fields{"url": tc.url})
		response := request(handler, "POST", "/v1/tiktok/resolutions", string(body), true)
		if response.Code != tc.status {
			t.Errorf("input bytes %d: got %d, want %d", len(tc.url), response.Code, tc.status)
		}
	}
}
