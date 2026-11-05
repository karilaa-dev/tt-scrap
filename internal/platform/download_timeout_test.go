package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestDownloadStallFallsBackWithinBudget(t *testing.T) {
	for _, phase := range []string{"headers", "body", "partial"} {
		t.Run(phase, func(t *testing.T) {
			data := append([]byte{255, 216, 255}, make([]byte, 2045)...)
			var good atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/good" {
					good.Add(1)
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.Write(data)
					return
				}
				if phase != "headers" {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.WriteHeader(200)
					if phase == "partial" {
						w.Write(data[:1024])
					}
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.DownloadParts = 1
			cfg.DownloadTimeout = time.Second
			cfg.DownloadHeaderTimeout, cfg.DownloadIdleTimeout = 200*time.Millisecond, 200*time.Millisecond
			d := NewDownloader(cfg, nil)
			defer d.Close()
			got, err := d.Download(context.Background(), core.AssetContext{Kind: "image", UpstreamURL: server.URL + "/stall", AlternateURLs: []string{server.URL + "/good"}}, 0)
			if err != nil {
				t.Fatalf("working alternate never completed: %v (requests=%d)", err, good.Load())
			}
			if !bytes.Equal(got.Data.Bytes(), data) || good.Load() != 1 {
				t.Fatal("fallback changed bytes or retried success")
			}
			got.Cleanup()
			if d.MemoryInUse() != 0 {
				t.Fatal("failed transfer retained spool memory")
			}
		})
	}
}

// A context-aware local transport avoids depending on TCP chunk coalescing for
// timing assertions. Each read delivers progress well inside the idle window.
type pacedBody struct {
	ctx   context.Context
	data  []byte
	delay time.Duration
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(b.delay):
	}
	n := copy(p, b.data[:min(256, len(b.data))])
	b.data = b.data[n:]
	return n, nil
}
func (b *pacedBody) Close() error { return nil }

func TestDownloadProgressAndTotalBudget(t *testing.T) {
	for _, total := range []time.Duration{2 * time.Second, 150 * time.Millisecond} {
		t.Run(total.String(), func(t *testing.T) {
			cfg := testConfig()
			cfg.DownloadParts = 1
			cfg.DownloadTimeout = total
			cfg.DownloadIdleTimeout = 100 * time.Millisecond
			d := NewDownloader(cfg, nil)
			defer d.Close()
			data := append([]byte{255, 216, 255}, make([]byte, 2045)...)
			var calls atomic.Int32
			d.client.Transport = smallResponseTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: make(http.Header), Body: &pacedBody{ctx: r.Context(), data: data, delay: 30 * time.Millisecond}}, nil
			})
			got, err := d.Download(context.Background(), core.AssetContext{Kind: "image", UpstreamURL: "https://fixture.invalid/media"}, 0)
			if total > time.Second {
				if err != nil || !bytes.Equal(got.Data.Bytes(), data) {
					t.Fatal("progressing download failed", err)
				}
			} else {
				var api *core.Error
				if !errors.As(err, &api) || api.Code != "upstream_timeout" {
					t.Fatal("total budget not enforced", err)
				}
			}
			got.Cleanup()
			if calls.Load() != 1 || d.MemoryInUse() != 0 {
				t.Fatal("unexpected retry or memory leak")
			}
		})
	}
}

func TestDownloadCallerCancellationDoesNotTryAlternate(t *testing.T) {
	cfg := testConfig()
	cfg.DownloadParts = 1
	d := NewDownloader(cfg, nil)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	d.client.Transport = smallResponseTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err := d.Download(ctx, core.AssetContext{UpstreamURL: "https://fixture.invalid/one", AlternateURLs: []string{"https://fixture.invalid/two"}}, 0)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancellation retried or lost", err, calls)
	}
}

func TestRangeStallResumesUnderPinnedValidator(t *testing.T) {
	cfg := testConfig()
	cfg.DownloadConcurrency = 1
	cfg.DownloadRetries = 2
	cfg.DownloadIdleTimeout = 50 * time.Millisecond
	d := NewDownloader(cfg, nil)
	defer d.Close()
	data := largeVideo(2 << 20)
	var ranges []string
	d.client.Transport = smallResponseTransport(func(r *http.Request) (*http.Response, error) {
		span := r.Header.Get("Range")
		ranges = append(ranges, span)
		var start, end int
		fmt.Sscanf(span, "bytes=%d-%d", &start, &end)
		if start > 0 && r.Header.Get("If-Range") != `"v1"` {
			t.Error("lost validator")
		}
		header := http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))}, "Etag": {`"v1"`}}
		var body io.ReadCloser = io.NopCloser(bytes.NewReader(data[start : end+1]))
		if len(ranges) == 2 {
			body = &stallAfterPrefix{ctx: r.Context(), prefix: bytes.NewReader(data[start : start+1024])}
		}
		return &http.Response{StatusCode: 206, Header: header, ContentLength: int64(end - start + 1), Body: body}, nil
	})
	got, err := d.Download(context.Background(), core.AssetContext{Kind: "video", UpstreamURL: "https://fixture.invalid/video"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Cleanup()
	if !bytes.Equal(got.Data.Bytes(), data) || len(ranges) != 3 || ranges[2] != fmt.Sprintf("bytes=%d-%d", firstPart+1024, len(data)-1) {
		t.Fatal("range failed to resume safely", ranges)
	}
}

type stallAfterPrefix struct {
	ctx    context.Context
	prefix *bytes.Reader
}

func (b *stallAfterPrefix) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *stallAfterPrefix) Close() error { return nil }

func TestDownloadDiagnosticsExcludeRequestSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := core.WithDiagnosticLogger(context.Background(), logger)
	cfg := testConfig()
	cfg.DownloadHeaderTimeout = 10 * time.Millisecond
	d := NewDownloader(cfg, nil)
	defer d.Close()
	d.client.Transport = smallResponseTransport(func(r *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(r.Context())
		trace.ConnectStart("tcp", "private-proxy:123")
		<-r.Context().Done()
		return nil, fmt.Errorf("secret error: %s: %w", r.URL, r.Context().Err())
	})
	_, err := d.Download(ctx, core.AssetContext{UpstreamURL: "https://user:password@secret-host/private-content?token=signed-secret", Headers: http.Header{"Cookie": {"cookie-secret"}, "Authorization": {"Bearer bearer-secret"}}}, 0)
	if err == nil {
		t.Fatal("expected stall")
	}
	output := logs.String()
	for _, secret := range []string{"secret-host", "private-content", "signed-secret", "cookie-secret", "bearer-secret", "password", "private-proxy", "secret error"} {
		if strings.Contains(output, secret) {
			t.Fatalf("diagnostics leaked %q", secret)
		}
	}
	for _, field := range []string{`"phase":"connect"`, `"attempt":1`, `"source_index":0`, `"transfer_id":`, `"error_code":"upstream_timeout"`, `"phase":"download_queue"`} {
		if !strings.Contains(output, field) {
			t.Errorf("missing diagnostic %s: %s", field, output)
		}
	}
}
