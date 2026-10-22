package httpapi

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

func gifFixture(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := gif.Encode(&data, image.NewRGBA(image.Rect(0, 0, 16, 12)), nil); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestPreparationReleasesOnlyReplacedSources(t *testing.T) {
	for _, tc := range []struct {
		name, mime string
		data       []byte
		document   bool
		converted  bool
	}{
		{"native photo", "image/jpeg", jpegFixture(t), false, false},
		{"converted photo", "image/gif", gifFixture(t), false, true},
		{"converted document", "image/webp", readFixture(t, "../media/testdata/static.webp"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, _, downloader, _ := fixture(t)
			downloader.data, downloader.mime = tc.data, tc.mime
			service := newTelegramService(dependencies.Config, dependencies)
			var prepared preparedItem
			err := downloader.WithIdentity(context.Background(), "", func(download core.DownloadFunc) error {
				var err error
				prepared, err = service.prepare(context.Background(), deliveryItem{Kind: "image", Descriptor: core.AssetDescriptor{Filename: "photo"}}, tc.document, download, nil)
				return err
			})
			defer prepared.cleanup()
			if err != nil {
				t.Fatal(err)
			}
			wantLive := 1
			if tc.converted {
				wantLive = 0
			}
			if live := downloader.leaked(); live != wantLive {
				t.Fatalf("preparation retained %d downloads, want %d", live, wantLive)
			}
			data, err := io.ReadAll(prepared.Media.Data.Reader(context.Background(), 0, -1))
			if err != nil {
				t.Fatal(err)
			}
			if tc.converted {
				if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
					t.Fatal("converted upload is unreadable", err)
				}
			} else if !bytes.Equal(data, tc.data) {
				t.Fatal("native photo changed")
			}
			output := prepared.Media.Data
			prepared.cleanup()
			prepared.cleanup()
			if !errors.Is(output.Err(), spool.ErrClosed) || downloader.leaked() != 0 {
				t.Fatal("preparation cleanup retained buffers")
			}
		})
	}
}

func TestThumbnailDeliveryReadiness(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		for _, cover := range []string{"native", "converted", "stalled", "failed", "disabled"} {
			t.Run(cover+"/buffered="+strconv.FormatBool(buffered), func(t *testing.T) {
				data := streamVideo(64<<10, true)
				coverData := jpegFixture(t)
				if cover == "converted" {
					coverData = gifFixture(t)
				}
				coverStarted := make(chan struct{})
				coverStopped := make(chan struct{})
				var once sync.Once
				cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/cover" {
						defer close(coverStopped)
						once.Do(func() { close(coverStarted) })
						switch cover {
						case "stalled":
							<-r.Context().Done()
						case "failed":
							http.Error(w, "missing cover", http.StatusNotFound)
						default:
							_, _ = w.Write(coverData)
						}
						return
					}
					if cover != "disabled" {
						select {
						case <-coverStarted:
						case <-r.Context().Done():
							return
						}
					}
					w.Header().Set("Content-Type", "video/mp4")
					if !buffered {
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					} else {
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write(data)
				}))
				defer cdn.Close()
				telegram, received := documentTelegram(t, `{"ok":true,"result":{"message_id":1}}`)
				dependencies, tt, _, assets := fixture(t)
				cfg := dependencies.Config
				cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "fixture", telegram.URL
				cfg.ThumbnailWait = core.DefaultConfig().ThumbnailWait
				if cover == "disabled" {
					cfg.ThumbnailWait = 0
				}
				cfg.DownloadRetries = 1
				cfg.TempDir = t.TempDir()
				downloader := platform.NewDownloader(cfg, nil)
				defer downloader.Close()
				dependencies.Config, dependencies.Downloader = cfg, downloader
				tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}
				thumbnail := assets.Register(core.AssetContext{Kind: "image", Filename: "cover", UpstreamURL: cdn.URL + "/cover"}, 0)
				tt.extraction.Cover = &thumbnail
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1,"caption":"kept"}}`)).WithContext(ctx)
				r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				response := httptest.NewRecorder()
				NewHandler(dependencies).ServeHTTP(response, r)
				if response.Code != http.StatusOK {
					t.Fatalf("delivery waited for optional cover: %d %s", response.Code, response.Body.String())
				}
				method, fields, parts := received()
				wantThumbnail := cover == "native" || cover == "converted"
				wantParts := 1
				if wantThumbnail {
					wantParts++
				}
				if method != "sendVideo" || fields["caption"] != "kept" || len(parts) != wantParts || !bytes.Equal(parts[0].data, data) {
					t.Fatalf("upload method=%s fields=%v parts=%d", method, fields, len(parts))
				}
				if wantThumbnail {
					if fields["thumbnail"] != "attach://thumbnail_file" || fields["cover"] != "attach://thumbnail_file" || parts[1].field != "thumbnail_file" {
						t.Fatal("missing thumbnail references", fields, parts[1].field)
					}
					config, err := jpeg.DecodeConfig(bytes.NewReader(parts[1].data))
					if err != nil || config.Width != 16 || config.Height != 12 {
						t.Fatal("invalid thumbnail", config, err)
					}
					if cover == "native" && !bytes.Equal(parts[1].data, coverData) {
						t.Fatal("native cover changed")
					}
				} else if fields["thumbnail"] != "" || fields["cover"] != "" {
					t.Fatal("dangling thumbnail references", fields)
				}
				if cover != "disabled" {
					select {
					case <-coverStopped:
					case <-ctx.Done():
						t.Fatal("cover work did not stop")
					}
				} else {
					select {
					case <-coverStarted:
						t.Fatal("disabled thumbnail was downloaded")
					default:
					}
				}
				if used := downloader.MemoryInUse(); used != 0 {
					t.Fatal("delivery retained download memory", used)
				}
			})
		}
	}
}

func TestThumbnailPreparationRetainsOnlyUploadBytes(t *testing.T) {
	for _, converted := range []bool{false, true} {
		t.Run(strconv.FormatBool(converted), func(t *testing.T) {
			dependencies, _, downloader, assets := fixture(t)
			dependencies.Config.ThumbnailWait = time.Second
			cover := jpegFixture(t)
			mime := "image/jpeg"
			if converted {
				cover, mime = gifFixture(t), "image/gif"
			}
			downloader.files = map[string]fakeFile{"cover": {data: cover, mime: mime}}
			descriptor := assets.Register(core.AssetContext{Kind: "image", UpstreamURL: "cover"}, 0)
			service := newTelegramService(dependencies.Config, dependencies)
			var prepared preparedItem
			err := downloader.WithIdentity(context.Background(), "", func(download core.DownloadFunc) error {
				var err error
				prepared, err = service.prepare(context.Background(), deliveryItem{Kind: "video", Cover: &descriptor}, false, download, nil)
				return err
			})
			defer prepared.cleanup()
			if err != nil || prepared.Thumbnail == nil {
				t.Fatal("missing timely thumbnail", err)
			}
			if _, err := jpeg.Decode(prepared.Thumbnail.Data.Reader(context.Background(), 0, -1)); err != nil {
				t.Fatal("unreadable thumbnail", err)
			}
			wantLive := 2 // video and a native thumbnail source
			if converted {
				wantLive = 1
			}
			if live := downloader.leaked(); live != wantLive {
				t.Fatalf("retained %d downloads, want %d", live, wantLive)
			}
			prepared.cleanup()
			if downloader.leaked() != 0 {
				t.Fatal("thumbnail source leaked")
			}
		})
	}
}

func TestCancelDeliveryStopsMediaAndThumbnail(t *testing.T) {
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	}))
	defer cdn.Close()
	dependencies, tt, _, assets := fixture(t)
	cfg := dependencies.Config
	cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "fixture", cdn.URL
	cfg.ThumbnailWait = core.DefaultConfig().ThumbnailWait
	cfg.TempDir = t.TempDir()
	downloader := platform.NewDownloader(cfg, nil)
	defer downloader.Close()
	dependencies.Config, dependencies.Downloader = cfg, downloader
	tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}
	cover := assets.Register(core.AssetContext{Kind: "image", UpstreamURL: cdn.URL + "/cover"}, 0)
	tt.extraction.Cover = &cover
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		NewHandler(dependencies).ServeHTTP(response, r)
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("media and cover did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled delivery did not stop")
	}
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("canceled download did not stop")
		}
	}
	if response.Code != 499 || downloader.MemoryInUse() != 0 {
		t.Fatalf("status=%d retained=%d body=%s", response.Code, downloader.MemoryInUse(), response.Body.String())
	}
}
