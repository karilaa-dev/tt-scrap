package httpapi_test

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

func benchmarkImage(b *testing.B, converted bool) []byte {
	b.Helper()
	var output bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	var err error
	if converted {
		err = gif.Encode(&output, img, nil)
	} else {
		err = jpeg.Encode(&output, img, nil)
	}
	if err != nil {
		b.Fatal(err)
	}
	return output.Bytes()
}

func deliverBenchmark(b *testing.B, handler http.Handler, cfg core.Config) {
	b.Helper()
	r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`))
	r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		b.Fatal(w.Code, w.Body.String())
	}
}

// Small media makes optional-cover delay visible without a bandwidth bottleneck.
// first-media-ns/op measures the first video byte received, not request headers.
func BenchmarkVideoCoverPreparation(b *testing.B) {
	for _, buffered := range []bool{false, true} {
		for _, cover := range []string{"none", "ready", "stalled"} {
			b.Run("buffered="+strconv.FormatBool(buffered)+"/"+cover, func(b *testing.B) {
				const size = 64 << 10
				data, thumbnail := benchmarkMP4(size), benchmarkImage(b, false)
				cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/cover" {
						if cover == "stalled" {
							<-r.Context().Done()
							return
						}
						_, _ = w.Write(thumbnail)
						return
					}
					if buffered {
						w.(http.Flusher).Flush()
					} else {
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					}
					for offset := 0; offset < len(data); offset += 16 << 10 {
						if _, err := w.Write(data[offset:min(offset+16<<10, len(data))]); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}))
				defer cdn.Close()
				var started, firstByte atomic.Int64
				telegram := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reader, err := r.MultipartReader()
					if err != nil {
						b.Error(err)
						return
					}
					videoSeen, thumbnailSeen := false, false
					fields := make(map[string]string)
					for {
						part, err := reader.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							b.Error(err)
							return
						}
						if part.FormName() != "video_file" {
							body, err := io.ReadAll(part)
							if err != nil {
								b.Error(err)
								return
							}
							if part.FileName() != "" {
								if part.FormName() != "thumbnail_file" || thumbnailSeen || !bytes.Equal(body, thumbnail) {
									b.Error("unexpected or altered thumbnail")
									return
								}
								thumbnailSeen = true
							} else {
								fields[part.FormName()] = string(body)
							}
							continue
						}
						var prefix [1]byte
						if _, err := io.ReadFull(part, prefix[:]); err != nil {
							b.Error(err)
							return
						}
						firstByte.Store(time.Now().UnixNano() - started.Load())
						rest, err := io.ReadAll(part)
						if err != nil || prefix[0] != data[0] || !bytes.Equal(rest, data[1:]) {
							b.Error("incomplete or altered video", err)
							return
						}
						videoSeen = true
					}
					if !videoSeen || thumbnailSeen != (cover == "ready") || fields["chat_id"] != "1" || fields["video"] != "attach://video_file" {
						b.Error("missing video, expected thumbnail or attachment fields")
						return
					}
					if thumbnailSeen {
						if fields["thumbnail"] != "attach://thumbnail_file" || fields["cover"] != "attach://thumbnail_file" {
							b.Error("missing thumbnail attachment references")
							return
						}
					} else if fields["thumbnail"] != "" || fields["cover"] != "" {
						b.Error("dangling thumbnail attachment references")
						return
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
				}))
				telegram.Config.ConnState = func(c net.Conn, state http.ConnState) {
					if tcp, ok := c.(*net.TCPConn); ok && state == http.StateNew {
						_ = tcp.SetReadBuffer(1 << 20)
					}
				}
				telegram.Start()
				defer telegram.Close()
				cfg := core.DefaultConfig()
				cfg.APIKey, cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "benchmark-key-sixteen-chars", "fixture", telegram.URL
				cfg.TempDir = b.TempDir()
				assets := core.NewAssets(time.Hour, 100)
				post := core.TikTokExtraction{ContentType: "video", Media: []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}}
				if cover != "none" {
					value := assets.Register(core.AssetContext{Kind: "image", Filename: "cover.jpg", UpstreamURL: cdn.URL + "/cover"}, 0)
					post.Cover = &value
				}
				downloader := platform.NewDownloader(cfg, nil)
				defer downloader.Close()
				handler := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: savedPost{post: post}, Downloader: downloader})
				var latency int64
				b.SetBytes(size)
				b.ReportAllocs()
				for b.Loop() {
					started.Store(time.Now().UnixNano())
					deliverBenchmark(b, handler, cfg)
					latency += firstByte.Load()
					if downloader.MemoryInUse() != 0 {
						b.Fatal("delivery retained download memory")
					}
				}
				b.ReportMetric(float64(latency)/float64(b.N), "first-media-ns/op")
			})
		}
	}
}

// The download metrics count reserved source RAM at upload boundaries, excluding
// converted output buffers and decoder memory. Preflight still prepares all 21.
func BenchmarkAlbumPreparationRetention(b *testing.B) {
	for _, converted := range []bool{false, true} {
		b.Run("converted="+strconv.FormatBool(converted), func(b *testing.B) {
			data := benchmarkImage(b, converted)
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(data)
			}))
			defer cdn.Close()
			cfg := core.DefaultConfig()
			cfg.APIKey, cfg.TelegramBotToken, cfg.TempDir = "benchmark-key-sixteen-chars", "fixture", b.TempDir()
			downloader := platform.NewDownloader(cfg, nil)
			defer downloader.Close()
			var batch atomic.Int32
			var first, last atomic.Int64
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := batch.Add(1)
				if current == 1 {
					first.Add(downloader.MemoryInUse())
				} else if current == 3 {
					last.Add(downloader.MemoryInUse())
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					b.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				want := []int{10, 9, 2}
				if current > 3 || len(r.MultipartForm.File) != want[current-1] {
					b.Error("incorrect album batch size", current, len(r.MultipartForm.File))
					return
				}
				for _, files := range r.MultipartForm.File {
					file, err := files[0].Open()
					if err != nil {
						b.Error(err)
						return
					}
					image, err := jpeg.Decode(file)
					file.Close()
					if err != nil || image.Bounds().Dx() != 32 || image.Bounds().Dy() != 24 {
						b.Error("incorrect album image", err)
						return
					}
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			}))
			defer telegram.Close()
			cfg.TelegramAPIBaseURL = telegram.URL
			assets := core.NewAssets(time.Hour, 100)
			post := core.TikTokExtraction{ContentType: "slideshow"}
			for i := range 21 {
				post.Media = append(post.Media, assets.Register(core.AssetContext{Kind: "image", Filename: fmt.Sprintf("%d.jpg", i), UpstreamURL: cdn.URL}, i))
			}
			handler := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: savedPost{post: post}, Downloader: downloader})
			b.ReportAllocs()
			for b.Loop() {
				batch.Store(0)
				deliverBenchmark(b, handler, cfg)
				if batch.Load() != 3 || downloader.MemoryInUse() != 0 {
					b.Fatal("incomplete album or retained download memory")
				}
			}
			b.ReportMetric(float64(first.Load())/float64(b.N), "download-B/first-upload")
			b.ReportMetric(float64(last.Load())/float64(b.N), "download-B/last-upload")
		})
	}
}
