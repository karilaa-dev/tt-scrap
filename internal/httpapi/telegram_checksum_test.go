package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

type checksumDeliveryDownloader struct {
	*platform.Downloader
	leases, downloads atomic.Int32
}

func (d *checksumDeliveryDownloader) WithIdentity(context.Context, string, func(core.DownloadFunc) error) error {
	return errors.New("Telegram selected the checksum contract")
}

func (d *checksumDeliveryDownloader) WithDeliveryIdentity(ctx context.Context, id string, run func(core.DownloadFunc) error) error {
	d.leases.Add(1)
	return d.Downloader.WithDeliveryIdentity(ctx, id, func(download core.DownloadFunc) error {
		return run(func(ctx context.Context, asset core.AssetContext, maximum int64, started func(*core.Downloaded)) (*core.Downloaded, error) {
			got, err := download(ctx, asset, maximum, started)
			if err == nil {
				d.downloads.Add(1)
				if got.SHA256 != "" {
					got.Cleanup()
					return nil, errors.New("Telegram prepared a checksum")
				}
			}
			return got, err
		})
	})
}

func TestTelegramSelectsDeliveryChecksumContract(t *testing.T) {
	for _, mode := range []string{"streamed", "photo", "album"} {
		t.Run(mode, func(t *testing.T) {
			dependencies, tt, _, assets := fixture(t)
			photo, video := jpegFixture(t), streamVideo(128<<10, true)
			data, kind, filename, method, count := photo, "image", "photo.jpg", "sendPhoto", 1
			if mode == "streamed" {
				data, kind, filename, method = video, "video", "video.mp4", "sendVideo"
			} else if mode == "album" {
				count, method = 2, "sendMediaGroup"
			}
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := data
				if r.URL.Path == "/cover" {
					body = photo
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body)
			}))
			defer cdn.Close()
			for range count {
				tt.extraction.Media = append(tt.extraction.Media, assets.Register(core.AssetContext{Kind: kind, Filename: filename, UpstreamURL: cdn.URL + "/media"}, 0))
			}
			if kind == "image" {
				tt.extraction.ContentType = "slideshow"
			} else {
				tt.extraction.Cover = core.Ptr(assets.Register(core.AssetContext{Kind: "image", Filename: "cover.jpg", UpstreamURL: cdn.URL + "/cover"}, 0))
			}
			cfg := dependencies.Config
			cfg.TempDir = t.TempDir()
			cfg.DownloadRetries, cfg.RetryDelay = 1, 0
			cfg.TelegramBotToken = "fixture"
			cfg.TelegramAPIBaseURL = "http://telegram.invalid"
			cfg.ThumbnailWait = time.Second
			d := &checksumDeliveryDownloader{Downloader: platform.NewDownloader(cfg, nil)}
			defer d.Close()
			dependencies.Config, dependencies.Downloader = cfg, d
			var uploads atomic.Int32
			dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
				uploads.Add(1)
				if r.URL.Path != "/botfixture/"+method {
					t.Errorf("unexpected upload method %s", r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					return nil, err
				}
				defer r.MultipartForm.RemoveAll()
				var sources int
				for _, files := range r.MultipartForm.File {
					for _, file := range files {
						part, err := file.Open()
						if err != nil {
							return nil, err
						}
						body, err := io.ReadAll(part)
						part.Close()
						if err != nil {
							return nil, err
						}
						if bytes.Equal(body, data) {
							sources++
						}
					}
				}
				if sources != count {
					t.Errorf("uploaded %d unchanged sources, want %d", sources, count)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"ok":true,"result":{"message_id":1}}`))}, nil
			})
			handler := NewHandler(dependencies)
			response := request(handler, "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`, true)
			wantDownloads := count
			if mode == "streamed" {
				wantDownloads++
			}
			if response.Code != http.StatusOK || uploads.Load() != 1 || d.leases.Load() != 1 || d.downloads.Load() != int32(wantDownloads) {
				t.Fatalf("status=%d leases=%d downloads=%d uploads=%d: %s", response.Code, d.leases.Load(), d.downloads.Load(), uploads.Load(), response.Body.String())
			}
			raw := request(handler, "GET", tt.extraction.Media[0].DownloadURL, "", true)
			sum := sha256.Sum256(data)
			want := hex.EncodeToString(sum[:])
			if raw.Code != http.StatusOK || !bytes.Equal(raw.Body.Bytes(), data) || raw.Header().Get("X-Content-Sha256") != want || raw.Header().Get("ETag") != `"`+want+`"` {
				t.Fatal("raw asset checksum contract changed", raw.Code, raw.Header())
			}
			if d.MemoryInUse() != 0 {
				t.Fatal("delivery or raw API retained source RAM")
			}
		})
	}
}

type checksumRecoveryBackend struct {
	core.AppBackend
	withMedia func(context.Context, string, func(core.OpenAppMedia) error) error
}

func (b *checksumRecoveryBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	return b.withMedia(ctx, id, run)
}

func TestTelegramDeliveryWaitsForIdentityRecovery(t *testing.T) {
	for _, failLease := range []bool{false, true} {
		t.Run("lease_failure="+strconv.FormatBool(failLease), func(t *testing.T) {
			dependencies, tt, _, assets := fixture(t)
			original := streamVideo(128<<10, true)
			recovered := bytes.Clone(original)
			recovered[64<<10] ^= 1
			uploading := make(chan struct{})
			leaseErr := core.NewError("service_not_configured", "fixture identity save failed", 503)
			backend := &checksumRecoveryBackend{withMedia: func(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
				if id != "fixture" {
					return errors.New("wrong fixture identity")
				}
				prepare := func(data []byte) error {
					return run(func(context.Context, string, http.Header) (*http.Response, error) {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
					})
				}
				if err := prepare(original); err != nil {
					return err
				}
				select {
				case <-uploading:
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return errors.New("fixture upload did not start while identity lease was held")
				}
				if failLease {
					return leaseErr
				}
				return prepare(recovered)
			}}
			cfg := dependencies.Config
			cfg.TempDir = t.TempDir()
			cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "fixture", "http://telegram.invalid"
			cfg.DownloadRetries, cfg.RetryDelay = 1, 0
			d := platform.NewDownloader(cfg, backend)
			defer d.Close()
			dependencies.Config, dependencies.Downloader = cfg, d
			tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "video.mp4", IdentityID: "fixture", UpstreamURL: "http://cdn.invalid/video"}, 0)}
			var calls, complete, aborted atomic.Int32
			dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				prefix := make([]byte, 8<<10)
				if _, err := io.ReadFull(r.Body, prefix); err != nil {
					return nil, err
				}
				if call == 1 {
					close(uploading)
				}
				tail, err := io.ReadAll(r.Body)
				if err != nil {
					aborted.Add(1)
					return nil, err
				}
				if !bytes.Contains(append(prefix, tail...), recovered) {
					t.Error("completed upload did not contain exactly the recovered media")
				}
				complete.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"ok":true,"result":{"message_id":1}}`))}, nil
			})
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`, true)
			wantStatus, wantComplete, wantCalls := http.StatusOK, int32(1), int32(2)
			if failLease {
				wantStatus, wantComplete, wantCalls = http.StatusServiceUnavailable, 0, 1
			}
			if response.Code != wantStatus || complete.Load() != wantComplete || calls.Load() != wantCalls || aborted.Load() != 1 || d.MemoryInUse() != 0 {
				t.Fatalf("status=%d complete=%d calls=%d aborted=%d RAM=%d: %s", response.Code, complete.Load(), calls.Load(), aborted.Load(), d.MemoryInUse(), response.Body.String())
			}
		})
	}
}
