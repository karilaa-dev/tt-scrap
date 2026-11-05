package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

func operationCall(h http.Handler, path, body string, budget, attempt int, stream bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-secret-key-with-length")
	r.Header.Set("X-TT-Operation-Budget-Ms", strconv.Itoa(budget))
	r.Header.Set("X-TT-Operation-Attempt", strconv.Itoa(attempt))
	if stream {
		r.Header.Set("Accept", "application/x-ndjson")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func readOperationEvents(t *testing.T, w *httptest.ResponseRecorder) ([]core.AttemptProgress, terminalEvent) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("not NDJSON: %d %s", w.Code, w.Body.String())
	}
	decoder := json.NewDecoder(w.Body)
	var progress []core.AttemptProgress
	var terminal terminalEvent
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if terminal.Type != "" {
			t.Fatal("event after terminal")
		}
		var event core.AttemptProgress
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "terminal" {
			if err := json.Unmarshal(raw, &terminal); err != nil {
				t.Fatal(err)
			}
		} else {
			progress = append(progress, event)
		}
	}
	if terminal.Type != "terminal" {
		t.Fatal("missing terminal")
	}
	return progress, terminal
}

func TestOperationStreamRealRetriesAndDeliveryOutcomes(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown", "rejected", "not_started", "malformed", "server_error"} {
		t.Run(outcome, func(t *testing.T) {
			dependencies, tt, _, assets := fixture(t)
			cfg := dependencies.Config
			cfg.TelegramBotToken = "fixture"
			cfg.DownloadParts = 1
			cfg.DownloadRetries = 10
			cfg.RetryDelay = 0
			data := jpegFixture(t)
			var downloads, sends atomic.Int32
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := downloads.Add(1)
				if n < 3 || outcome == "not_started" {
					w.WriteHeader(503)
					return
				}
				w.Write(data)
			}))
			defer cdn.Close()
			d := platform.NewDownloader(cfg, nil)
			defer d.Close()
			dependencies.Config, dependencies.Downloader = cfg, d
			tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "image", Filename: "image.jpg", UpstreamURL: cdn.URL}, 0)}
			var commitSeen atomic.Bool
			dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return nil, err
				}
				commitSeen.Store(true)
				if outcome == "unknown" {
					return nil, io.ErrUnexpectedEOF
				}
				body := `{"ok":true,"result":{"message_id":1}}`
				if outcome == "rejected" {
					body = `{"ok":false,"error_code":400}`
				}
				status := 200
				if outcome == "malformed" {
					body = `not-json`
				}
				if outcome == "server_error" {
					status = 500
					body = `{"ok":false,"error_code":500}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			w := operationCall(NewHandler(dependencies), "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"delivery":"document","telegram":{"chat_id":1}}`, 15000, 1, true)
			progress, terminal := readOperationEvents(t, w)
			wantState := outcome
			if outcome == "malformed" || outcome == "server_error" {
				wantState = "unknown"
			}
			if terminal.DeliveryState != wantState || terminal.Attempt != 3 || downloads.Load() != 3 {
				t.Fatalf("terminal=%+v downloads=%d body=%s", terminal, downloads.Load(), terminal.Result)
			}
			want := 3
			if outcome != "not_started" {
				want++
			}
			if len(progress) != want {
				t.Fatal("unexpected progress", progress)
			}
			for i := range 3 {
				if progress[i].Type != "attempt" || progress[i].Attempt != i+1 {
					t.Fatal("invented or unordered retry", progress)
				}
			}
			if outcome != "not_started" && (progress[3].Type != "delivery_commit" || sends.Load() != 1 || !commitSeen.Load()) {
				t.Fatal("missing commit", progress)
			}
			if terminal.SafeToRetry != (outcome == "not_started" || outcome == "rejected") {
				t.Fatal("unsafe retry classification", terminal)
			}
			if d.MemoryInUse() != 0 {
				t.Fatal("retained download memory")
			}
		})
	}
}

func TestOperationDeadlineIncludesPipelineQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dependencies, _, _, _ := fixture(t)
		dependencies.Config.TelegramBotToken = "fixture"
		dependencies.Config.TelegramPipelineConcurrency = 1
		h := NewHandler(dependencies).(*handler)
		release, err := h.telegram.pipelines.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		began := time.Now()
		w := operationCall(h, "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`, 15000, 1, true)
		progress, terminal := readOperationEvents(t, w)
		if time.Since(began) != 15*time.Second || len(progress) != 1 || terminal.Status != 504 || terminal.DeliveryState != "not_started" || !terminal.SafeToRetry {
			t.Fatal("queue escaped operation deadline", terminal, time.Since(began))
		}
	})
}

func TestOperationBudgetAndAttemptContinueAcrossCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dependencies, tt, _, _ := fixture(t)
		tt.resolve = func(ctx context.Context) error {
			time.Sleep(5 * time.Second)
			if !core.AllowRetry(ctx, "resolution") {
				return errors.New("denied")
			}
			return nil
		}
		h := NewHandler(dependencies)
		began := time.Now()
		first := operationCall(h, "/v1/tiktok/resolutions", `{"url":"https://www.tiktok.com/@x/video/123"}`, 15000, 1, true)
		progress, result := readOperationEvents(t, first)
		if len(progress) != 2 || result.Attempt != 2 || result.Status != 200 {
			t.Fatal(progress, result)
		}
		tt.resolve = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		second := operationCall(h, "/v1/tiktok/resolutions", `{"url":"https://www.tiktok.com/@x/video/123"}`, 10000, result.Attempt, true)
		progress, result = readOperationEvents(t, second)
		if time.Since(began) != 15*time.Second || len(progress) != 1 || progress[0].Attempt != 2 || result.Status != 504 {
			t.Fatal("budget reset between calls", progress, result, time.Since(began))
		}
	})
}

func TestOperationJSONCompatibilityAndHeaderValidation(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	h := NewHandler(dependencies)
	body := `{"url":"https://www.tiktok.com/@x/video/123"}`
	legacy := request(h, "POST", "/v1/tiktok/resolutions", body, true)
	opted := operationCall(h, "/v1/tiktok/resolutions", body, 15000, 2, false)
	if legacy.Body.String() != opted.Body.String() || opted.Header().Get("X-TT-Operation-Attempt") != "2" {
		t.Fatal("changed JSON contract")
	}
	for _, budget := range []int{-1, 0, 15001} {
		if w := operationCall(h, "/v1/tiktok/resolutions", body, budget, 1, true); w.Code != 422 {
			t.Fatal("invalid budget accepted", budget, w.Code)
		}
	}
	for _, attempt := range []int{0, 4} {
		if w := operationCall(h, "/v1/tiktok/resolutions", body, 15000, attempt, true); w.Code != 422 {
			t.Fatal("invalid attempt accepted", attempt, w.Code)
		}
	}
}

func TestOperationStreamPrecommitTimeoutIsSafe(t *testing.T) {
	dependencies, tt, _, assets := fixture(t)
	cfg := dependencies.Config
	cfg.TelegramBotToken = "fixture"
	cfg.DownloadParts = 1
	cfg.DownloadRetries = 1
	cfg.DownloadIdleTimeout = 200 * time.Millisecond
	data := streamVideo(128<<10, true)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data[:1024])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer cdn.Close()
	d := platform.NewDownloader(cfg, nil)
	defer d.Close()
	dependencies.Config, dependencies.Downloader = cfg, d
	tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "v.mp4", UpstreamURL: cdn.URL}, 0)}
	var sends atomic.Int32
	dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		return nil, err
	})
	w := operationCall(NewHandler(dependencies), "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`, 15000, 1, true)
	progress, terminal := readOperationEvents(t, w)
	if len(progress) != 1 || terminal.Status != 504 || terminal.DeliveryState != "uncommitted" || !terminal.SafeToRetry || sends.Load() != 1 {
		t.Fatal(progress, terminal, sends.Load())
	}
	if d.MemoryInUse() != 0 {
		t.Fatal("retained failed stream")
	}
}

// A failed progress write cancels work, including a commit notification. The
// multipart gate must remain shut even when the consumer disappears at commit.
type failingEventWriter struct {
	header http.Header
	failed bool
}

func (w *failingEventWriter) Header() http.Header { return w.header }
func (w *failingEventWriter) WriteHeader(int)     {}
func (w *failingEventWriter) Flush()              {}
func (w *failingEventWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"type":"delivery_commit"`) {
		w.failed = true
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
func TestOperationDisconnectBeforeCommitCancelsUpload(t *testing.T) {
	dependencies, tt, d, assets := fixture(t)
	dependencies.Config.TelegramBotToken = "fixture"
	d.data = streamVideo(128<<10, true)
	tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "v.mp4", UpstreamURL: "fixture"}, 0)}
	var complete atomic.Int32
	dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, r.Body)
		if err == nil {
			complete.Add(1)
		}
		return nil, io.ErrUnexpectedEOF
	})
	r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", strings.NewReader(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`))
	r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
	r.Header.Set("Accept", "application/x-ndjson")
	r.Header.Set("X-TT-Operation-Budget-Ms", "15000")
	w := &failingEventWriter{header: make(http.Header)}
	NewHandler(dependencies).ServeHTTP(w, r)
	if !w.failed || complete.Load() != 0 || d.leaked() != 0 {
		t.Fatal("failed event committed an upload", w.failed, complete.Load(), d.leaked())
	}
}
