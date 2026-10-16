package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

type fakeTikTok struct {
	extraction core.TikTokExtraction
	resolveErr error
	resolve    func(context.Context) error
	musicID    string
	refresh    bool
}

func (f *fakeTikTok) ResolveURL(ctx context.Context, url string) (core.TikTokResolution, error) {
	err := f.resolveErr
	if f.resolve != nil {
		err = f.resolve(ctx)
	}
	return core.TikTokResolution{Platform: "tiktok", SourceID: "7695962883523464478", SourceURL: url, ResolvedURL: url}, err
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

// fakeFile overrides the downloader's data and mime for one upstream URL.
type fakeFile struct {
	data []byte
	mime string
}

type fakeDownloader struct {
	data    []byte
	mime    string
	files   map[string]fakeFile
	fail    string
	mu      sync.Mutex
	buffers []*spool.Buffer
	calls   int
}

// WithIdentity announces each download as started, so single deliveries take
// the streamed upload path.
func (d *fakeDownloader) WithIdentity(_ context.Context, _ string, run func(core.DownloadFunc) error) error {
	return run(func(ctx context.Context, asset core.AssetContext, maxBytes int64, started func(*core.Downloaded)) (*core.Downloaded, error) {
		downloaded, err := d.Download(ctx, asset, maxBytes)
		if err == nil && started != nil {
			started(&core.Downloaded{Data: downloaded.Data, Filename: downloaded.Filename, ContentType: downloaded.ContentType, Size: downloaded.Size})
		}
		return downloaded, err
	})
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
	file := fakeFile{d.data, d.mime}
	if override, ok := d.files[asset.UpstreamURL]; ok {
		file = override
	}
	data := spool.Bytes(bytes.Clone(file.data))
	d.mu.Lock()
	d.buffers = append(d.buffers, data)
	d.mu.Unlock()
	hash := sha256.Sum256(file.data)
	return &core.Downloaded{Data: data, Filename: asset.Filename, ContentType: file.mime, Size: int64(len(file.data)), SHA256: hex.EncodeToString(hash[:])}, nil
}

// leaked counts downloads whose buffers were never released.
func (d *fakeDownloader) leaked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, data := range d.buffers {
		if data.Err() == nil {
			count++
		}
	}
	return count
}
func fixture(t *testing.T) (Dependencies, *fakeTikTok, *fakeDownloader, *core.Assets) {
	t.Helper()
	c := core.DefaultConfig()
	c.APIKey = "test-secret-key-with-length"
	c.RapidAPIKey = "configured"
	c.ThumbnailWait = 0
	assets := core.NewAssets(time.Minute, 100)
	tt := &fakeTikTok{extraction: core.TikTokExtraction{Platform: "tiktok", SourceID: "7695962883523464478", ContentType: "video", Media: []core.AssetDescriptor{}}}
	downloader := &fakeDownloader{data: []byte("media"), mime: "video/mp4"}
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
	if downloader.leaked() != 0 {
		t.Fatal("download not released")
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

func TestAPIServesOpenAPIWithIdentityPreparation(t *testing.T) {
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
	if len(schema.Paths) != 10 {
		t.Fatalf("want 10 paths, got %d", len(schema.Paths))
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

func TestCallerCancellationIsNotServerError(t *testing.T) {
	for _, tc := range []struct {
		name           string
		err            error
		callerCanceled bool
		status         int
		code           string
	}{
		{"caller canceled", fmt.Errorf("resolve: %w", context.Canceled), true, 499, "request_canceled"},
		{"internal cancel", context.Canceled, false, 500, "internal_error"},
		{"upstream deadline", context.DeadlineExceeded, true, 504, "upstream_timeout"},
		{"internal failure", errors.New("boom"), false, 500, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, tt, _, _ := fixture(t)
			tt.resolveErr = tc.err
			var event RequestEvent
			dependencies.OnRequest = func(e RequestEvent) { event = e }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.callerCanceled {
				cancel()
			}
			r := httptest.NewRequestWithContext(ctx, "POST", "/v1/tiktok/resolutions", strings.NewReader(`{"url":"https://www.tiktok.com/@x/video/123"}`))
			r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
			w := httptest.NewRecorder()
			NewHandler(dependencies).ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			if event.Status != tc.status || event.ErrorCode != tc.code {
				t.Fatalf("completion event %d %q", event.Status, event.ErrorCode)
			}
		})
	}
}

// Shutdown cancels the server's base context, which every request inherits.
// Whichever of shutdown and the caller cancels first decides the outcome.
func TestShutdownCancellationIsNotCallerCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		status int
		code   string
	}{
		{"shutdown", "shutdown", 503, "service_shutting_down"},
		{"caller before shutdown", "caller", 499, "request_canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, tt, _, _ := fixture(t)
			entered, canceled, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			tt.resolve = func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-proceed
				return fmt.Errorf("resolve: %w", ctx.Err())
			}
			events := make(chan RequestEvent, 1)
			dependencies.OnRequest = func(e RequestEvent) { events <- e }
			lifetime, stop := context.WithCancelCause(context.Background())
			defer stop(nil)
			server := httptest.NewUnstartedServer(NewHandler(dependencies))
			server.Config.BaseContext = func(net.Listener) context.Context { return lifetime }
			server.Start()
			defer server.Close()
			caller, leave := context.WithCancel(context.Background())
			defer leave()
			r, _ := http.NewRequestWithContext(caller, "POST", server.URL+"/v1/tiktok/resolutions", strings.NewReader(`{"url":"https://www.tiktok.com/@x/video/123"}`))
			r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
			responses := make(chan *http.Response, 1)
			go func() {
				response, _ := server.Client().Do(r)
				responses <- response
			}()
			<-entered
			if tc.first == "caller" {
				leave()
				<-canceled
				// Shutdown arrives before the handler reports the outcome.
				stop(ErrServiceShutdown)
			} else {
				stop(ErrServiceShutdown)
				<-canceled
			}
			close(proceed)
			event := <-events
			if event.Status != tc.status || event.ErrorCode != tc.code {
				t.Fatalf("completion event %d %q", event.Status, event.ErrorCode)
			}
			if response := <-responses; tc.first == "shutdown" {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 503 || !strings.Contains(string(body), `"code":"service_shutting_down"`) {
					t.Fatalf("response %d %s", response.StatusCode, body)
				}
			}
		})
	}
}

// failingWriter accepts the first write, then fails as a lost connection does.
type failingWriter struct {
	*httptest.ResponseRecorder
	writes, headers int
	fail            func()
}

func (w *failingWriter) WriteHeader(status int) {
	w.headers++
	w.ResponseRecorder.WriteHeader(status)
}
func (w *failingWriter) Write(data []byte) (int, error) {
	if w.writes++; w.writes > 1 {
		w.fail()
		return 0, errors.New("connection lost")
	}
	return w.ResponseRecorder.Write(data)
}

func TestInterruptedResponseBodyIsNotReportedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		status int
		code   string
	}{
		{"caller canceled", true, 499, "request_canceled"},
		{"write failure", false, 500, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, _, downloader, assets := fixture(t)
			downloader.data = bytes.Repeat([]byte("abcd"), 64*1024)
			descriptor := assets.Register(core.AssetContext{Filename: "video.mp4"}, 0)
			var event RequestEvent
			dependencies.OnRequest = func(value RequestEvent) { event = value }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, "GET", descriptor.DownloadURL, nil)
			r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
			writer := &failingWriter{ResponseRecorder: httptest.NewRecorder(), fail: func() {
				if tc.cancel {
					cancel()
				}
			}}
			NewHandler(dependencies).ServeHTTP(writer, r)
			if writer.headers != 1 || writer.Code != 200 {
				t.Fatalf("wire status %d written %d times", writer.Code, writer.headers)
			}
			if event.Status != tc.status || event.WireStatus != 200 || event.ErrorCode != tc.code {
				t.Fatalf("completion event %d wire %d %q", event.Status, event.WireStatus, event.ErrorCode)
			}
			if event.Bytes <= 0 || event.Bytes >= int64(len(downloader.data)) || event.Bytes != int64(writer.Body.Len()) {
				t.Fatalf("sent %d of %d bytes, event %d", writer.Body.Len(), len(downloader.data), event.Bytes)
			}
			if downloader.leaked() != 0 {
				t.Fatal("download not released")
			}
		})
	}
}

// disconnectingBody delivers part of the JSON, then loses the connection.
type disconnectingBody struct {
	data   *strings.Reader
	cancel context.CancelFunc
}

func (b *disconnectingBody) Read(p []byte) (int, error) {
	if b.data.Len() > 0 {
		return b.data.Read(p)
	}
	if b.cancel != nil {
		b.cancel()
	}
	return 0, io.ErrUnexpectedEOF
}

func TestRequestBodyCancellationIsNotValidationFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   io.Reader
		status int
		code   string
	}{
		// The body ends mid-object because the caller disconnected.
		{"disconnect during body", &disconnectingBody{data: strings.NewReader(`{"url":"https://www.tik`)}, 499, "request_canceled"},
		// A complete but invalid body stays invalid even after cancellation.
		{"invalid body", strings.NewReader(`{"url":`), 422, "validation_error"},
		// A body that fails without cancellation is still rejected as invalid.
		{"read failure", &disconnectingBody{data: strings.NewReader(`{"url":`)}, 422, "validation_error"},
		// The 1 MiB limit is a payload failure even when the caller also left.
		{"oversized body", strings.NewReader(`{"url":"` + strings.Repeat("x", 1<<20) + `"}`), 422, "validation_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, _, _, _ := fixture(t)
			var event RequestEvent
			dependencies.OnRequest = func(e RequestEvent) { event = e }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if body, ok := tc.body.(*disconnectingBody); ok && tc.status == 499 {
				body.cancel = cancel
			} else if !ok {
				cancel()
			}
			r := httptest.NewRequestWithContext(ctx, "POST", "/v1/tiktok/resolutions", tc.body)
			r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
			w := httptest.NewRecorder()
			NewHandler(dependencies).ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) || event.ErrorCode != tc.code {
				t.Fatalf("response %d %s event %q", w.Code, w.Body.String(), event.ErrorCode)
			}
		})
	}
}
