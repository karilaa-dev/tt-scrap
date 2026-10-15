package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

type benchmarkPosts map[string]core.TikTokExtraction

func (p benchmarkPosts) GetExtraction(_ context.Context, id string) (core.TikTokExtraction, error) {
	return p[id], nil
}
func (p benchmarkPosts) ResolveURL(context.Context, string) (core.TikTokResolution, error) {
	panic("unexpected resolution")
}
func (p benchmarkPosts) ExtractURL(context.Context, string, bool) (core.TikTokExtraction, error) {
	panic("unexpected extraction")
}
func (p benchmarkPosts) ExtractMusic(context.Context, string, bool) (core.TikTokMusic, error) {
	panic("unexpected music")
}

func BenchmarkAppDelivery(b *testing.B) {
	for _, kind := range []string{"video", "slideshow"} {
		for _, concurrency := range []int{1, 16, 64} {
			b.Run(fmt.Sprintf("%s/c%d", kind, concurrency), func(b *testing.B) {
				cfg, _ := testDatabase(b)
				cfg.APIKey, cfg.TelegramBotToken = "fixture-key-sixteen-characters", "fixture"
				cfg.MaxQueuedRequests, cfg.TelegramPipelineConcurrency = 4096, 128
				cfg.TempDir = b.TempDir()
				cfg.ThumbnailWait = 30 * time.Second
				cfg.RetryDelay, cfg.DownloadRetries = 0, 1
				addresses := make([]string, 10)
				for i := range addresses {
					addresses[i] = fmt.Sprintf("http://proxy-%d.example:8080", i)
				}
				p := openTestPool(b, cfg, addresses...)
				var requests, uploaded, badBytes atomic.Int64
				img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
				var state uint32 = 123
				for i := range img.Pix {
					state ^= state << 13
					state ^= state >> 17
					state ^= state << 5
					img.Pix[i] = byte(state)
					if i%4 == 3 {
						img.Pix[i] = 255
					}
				}
				var buffer bytes.Buffer
				if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 90}); err != nil {
					b.Fatal(err)
				}
				photo, video := buffer.Bytes(), deliveryVideo()
				photoHash, videoHash := sha256.Sum256(photo), sha256.Sum256(video)
				p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
					return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
						requests.Add(1)
						select {
						case <-time.After(20 * time.Millisecond):
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						if r.URL.Path == "/video" {
							return fixtureResponse(200, video), nil
						}
						return fixtureResponse(200, photo), nil
					}))
				}
				assets := core.NewAssets(time.Hour, 10000)
				posts := benchmarkPosts{}
				for i := range 100 {
					l, err := p.acquire(context.Background(), "")
					if err != nil {
						b.Fatal(err)
					}
					id := l.entry.id
					l.finish(nil)
					register := func(kind string) core.AssetDescriptor {
						return assets.Register(core.AssetContext{IdentityID: id, Platform: "tiktok", Kind: kind, Filename: kind, UpstreamURL: "https://cdn.example/" + kind}, 0)
					}
					post := core.TikTokExtraction{ContentType: kind}
					if kind == "video" {
						post.Media = []core.AssetDescriptor{register("video")}
						post.Cover = core.Ptr(register("cover"))
					} else {
						for range 8 {
							post.Media = append(post.Media, register("image"))
						}
					}
					posts[strconv.Itoa(i)] = post
				}
				telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reader, err := r.MultipartReader()
					if err != nil {
						badBytes.Add(1)
						http.Error(w, "multipart", 400)
						return
					}
					for {
						part, err := reader.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							badBytes.Add(1)
							break
						}
						hash := sha256.New()
						if _, err = io.Copy(hash, part); err != nil {
							badBytes.Add(1)
						}
						if part.FileName() != "" {
							got := hash.Sum(nil)
							if !bytes.Equal(got, photoHash[:]) && !bytes.Equal(got, videoHash[:]) {
								badBytes.Add(1)
							}
							uploaded.Add(1)
						}
					}
					io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
				}))
				defer telegram.Close()
				cfg.TelegramAPIBaseURL = telegram.URL
				d := platform.NewDownloader(cfg, p)
				defer d.Close()
				h := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: posts, Downloader: d})
				var next, failures atomic.Int64
				var group sync.WaitGroup
				before := p.Stats()
				b.ReportAllocs()
				b.ResetTimer()
				for range concurrency {
					group.Go(func() {
						for {
							n := next.Add(1) - 1
							if n >= int64(b.N) {
								return
							}
							body := fmt.Sprintf(`{"source":{"extraction_id":"%d"},"telegram":{"chat_id":1}}`, n%100)
							r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(body))
							r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
							r.Header.Set("Content-Type", "application/json")
							w := httptest.NewRecorder()
							h.ServeHTTP(w, r)
							if w.Code != 200 {
								failures.Add(1)
							}
						}
					})
				}
				group.Wait()
				b.StopTimer()
				parts := int64(2)
				if kind == "slideshow" {
					parts = 8
				}
				if failures.Load() != 0 || badBytes.Load() != 0 || requests.Load() != parts*int64(b.N) || uploaded.Load() != parts*int64(b.N) {
					b.Fatalf("failures=%d corrupt=%d requests=%d uploads=%d", failures.Load(), badBytes.Load(), requests.Load(), uploaded.Load())
				}
				if err := p.flush(context.Background()); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(p.Stats().Writes-before.Writes)/float64(b.N), "writes/op")
				b.ReportMetric(float64(requests.Load())/float64(b.N), "media/op")
			})
		}
	}
}
