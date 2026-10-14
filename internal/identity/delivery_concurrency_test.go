package identity

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

type deliveryPost struct {
	core.TikTokService
	post core.TikTokExtraction
}

func (p deliveryPost) GetExtraction(context.Context, string) (core.TikTokExtraction, error) {
	return p.post, nil
}

func deliveryJPEG(t testing.TB) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func deliveryVideo() []byte {
	data := make([]byte, 16)
	binary.BigEndian.PutUint32(data, 16)
	copy(data[4:], "ftypisom0000")
	return append(data, benchmarkMovie(32, 24)...)
}

func TestIdentityWaitDoesNotReserveDownloadCapacity(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.DownloadConcurrency = 1
	cfg.TempDir = t.TempDir()
	p := openTestPool(t, cfg, "")
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(*http.Request) (*http.Response, error) {
			return fixtureResponse(200, []byte{255, 216, 255, 0}), nil
		}))
	}
	busy := mustAcquire(t, p, "")
	defer busy.finish(nil)
	free := mustAcquire(t, p, "")
	freeID := free.entry.id
	finishLease(t, free, nil)
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		file, err := d.Download(ctx, core.AssetContext{IdentityID: busy.entry.id, Kind: "image", Filename: "busy.jpg", UpstreamURL: "https://cdn.example/busy"}, 0)
		file.Cleanup()
		done <- err
	}()
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	unrelated, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	file, err := d.Download(unrelated, core.AssetContext{IdentityID: freeID, Kind: "image", Filename: "free.jpg", UpstreamURL: "https://cdn.example/free"}, 0)
	file.Cleanup()
	cancel()
	<-done
	if err != nil {
		t.Fatalf("idle identity could not use available transfer capacity: %v", err)
	}
}

func TestTelegramRelatedMediaShareIdentityAndRunConcurrently(t *testing.T) {
	for _, kind := range []string{"video", "slideshow"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			cfg.APIKey = "fixture-key-sixteen-characters"
			cfg.TelegramBotToken = "fixture"
			cfg.TempDir = t.TempDir()
			cfg.ThumbnailWait = 3 * time.Second
			cfg.DownloadRetries = 1
			cfg.MaxQueuedRequests = 32
			p := openTestPool(t, cfg, "")
			l := mustAcquire(t, p, "")
			id := l.entry.id
			finishLease(t, l, nil)
			jpegData := deliveryJPEG(t)
			started := make(chan struct{}, 3)
			release := make(chan struct{})
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					started <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					if r.URL.Path == "/video" {
						return fixtureResponse(200, deliveryVideo()), nil
					}
					return fixtureResponse(200, jpegData), nil
				}))
			}
			assets := core.NewAssets(time.Minute, 100)
			register := func(kind string) core.AssetDescriptor {
				return assets.Register(core.AssetContext{IdentityID: id, Platform: "tiktok", Kind: kind, Filename: "media", UpstreamURL: "https://cdn.example/" + kind}, 0)
			}
			post := core.TikTokExtraction{ContentType: kind, Media: []core.AssetDescriptor{register("video")}, Cover: core.Ptr(register("cover"))}
			want := 2
			if kind == "slideshow" {
				post.Media = []core.AssetDescriptor{register("image"), register("image"), register("image")}
				post.Cover = nil
				want = 3
			}
			var uploads atomic.Int32
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				// A video upload may start during the lease, but its body completes
				// only after the identity checkpoint.
				if p.Stats().Busy != 0 {
					t.Error("Telegram upload completed while the identity was held")
				}
				if len(r.MultipartForm.File) != want {
					t.Errorf("uploaded %d parts, want %d", len(r.MultipartForm.File), want)
				}
				uploads.Add(1)
				io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			}))
			defer telegram.Close()
			cfg.TelegramAPIBaseURL = telegram.URL
			d := platform.NewDownloader(cfg, p)
			defer d.Close()
			h := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: deliveryPost{post: post}, Downloader: d})
			before := p.Stats()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`))
				r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				done <- w
			}()
			parallel := true
			for range want {
				select {
				case <-started:
				case <-time.After(time.Second):
					parallel = false
				}
				if !parallel {
					break
				}
			}
			close(release)
			response := <-done
			if !parallel {
				t.Fatal("related media requests were serialized by the identity lease")
			}
			if response.Code != 200 || uploads.Load() != 1 || p.Stats().Writes != before.Writes+1 || p.Stats().Busy != 0 {
				t.Fatalf("status=%d uploads=%d writes=%d: %s", response.Code, uploads.Load(), p.Stats().Writes-before.Writes, response.Body.String())
			}
		})
	}
}

func serveDelivery(ctx context.Context, cfg core.Config, handler http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertDeliveryIdle(t *testing.T, p *Pool, directory string) {
	t.Helper()
	if s := p.Stats(); s.Busy != 0 || s.Queued != 0 || !s.Ready {
		t.Fatal("delivery leaked identity work", s)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatal("delivery leaked temporary files", files, err)
	}
}

func TestTelegramPreparationBoundsWorkAndCancelsSiblings(t *testing.T) {
	for _, failure := range []string{"cancel", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			cfg.APIKey, cfg.TelegramBotToken = "fixture-key-sixteen-characters", "fixture"
			cfg.TempDir = t.TempDir()
			cfg.DownloadRetries, cfg.TelegramPreparationConcurrency = 1, 2
			cfg.MaxAssetBytes = 10
			p := openTestPool(t, cfg, "")
			l := mustAcquire(t, p, "")
			id := l.entry.id
			finishLease(t, l, nil)
			started := make(chan struct{}, 12)
			release := make(chan struct{})
			var requests, stopped atomic.Int32
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					requests.Add(1)
					started <- struct{}{}
					if r.URL.Path == "/0" && failure == "oversized" {
						select {
						case <-release:
							return fixtureResponse(200, bytes.Repeat([]byte("x"), 11)), nil
						case <-r.Context().Done():
						}
					}
					<-r.Context().Done()
					stopped.Add(1)
					return nil, r.Context().Err()
				}))
			}
			assets := core.NewAssets(time.Minute, 100)
			post := core.TikTokExtraction{ContentType: "slideshow"}
			for i := range 12 {
				post.Media = append(post.Media, assets.Register(core.AssetContext{IdentityID: id, Kind: "image", Filename: "photo.jpg", UpstreamURL: fmt.Sprintf("https://cdn.example/%d", i)}, 0))
			}
			var uploads atomic.Int32
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads.Add(1)
				io.WriteString(w, `{"ok":true,"result":{}}`)
			}))
			defer telegram.Close()
			cfg.TelegramAPIBaseURL = telegram.URL
			d := platform.NewDownloader(cfg, p)
			defer d.Close()
			h := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: deliveryPost{post: post}, Downloader: d})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- serveDelivery(ctx, cfg, h) }()
			for range 2 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("preparation workers did not start")
				}
			}
			select {
			case <-started:
				t.Error("one album exceeded its preparation worker limit")
			case <-time.After(30 * time.Millisecond):
			}
			if failure == "cancel" {
				cancel()
			} else {
				close(release)
			}
			response := <-done
			if response.Code == 200 || uploads.Load() != 0 || requests.Load() != 2 || stopped.Load() == 0 {
				t.Fatalf("status=%d uploads=%d requests=%d stopped=%d", response.Code, uploads.Load(), requests.Load(), stopped.Load())
			}
			if failure == "oversized" && response.Code != 413 {
				t.Fatalf("lost size error: %d %s", response.Code, response.Body.String())
			}
			assertDeliveryIdle(t, p, cfg.TempDir)
		})
	}
}

func TestTelegramRecoveryRepreparesBeforeOneUpload(t *testing.T) {
	for _, failure := range []string{"invalid signature", "invalid device_id"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := testDatabase(t)
			cfg.APIKey, cfg.TelegramBotToken = "fixture-key-sixteen-characters", "fixture"
			cfg.TempDir = t.TempDir()
			cfg.ThumbnailWait, cfg.DownloadRetries = 5*time.Second, 1
			p := openTestPool(t, cfg, "http://proxy.example:8080")
			protocol := newProtocolFixture(t, "", 0)
			photo := deliveryJPEG(t)
			coverDone := make(chan struct{})
			var plays, covers atomic.Int32
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/aweme/v1/play/":
						if plays.Add(1) == 1 {
							select {
							case <-coverDone:
							case <-r.Context().Done():
								return nil, r.Context().Err()
							}
							return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"`+failure+`"}`)), nil
						}
						return fixtureResponse(200, deliveryVideo()), nil
					case "/cover":
						res := fixtureResponse(200, photo)
						if covers.Add(1) == 1 {
							res.Body = &deliveryBody{ReadCloser: res.Body, done: coverDone}
						}
						return res, nil
					default:
						return protocol.do(r)
					}
				}))
			}
			initial, err := p.Detail(context.Background(), "7695962883523464478")
			if err != nil {
				t.Fatal(err)
			}
			assets := core.NewAssets(time.Minute, 100)
			register := func(kind, address string) core.AssetDescriptor {
				return assets.Register(core.AssetContext{IdentityID: initial.IdentityID, Platform: "tiktok", Kind: kind, Filename: "media", UpstreamURL: address}, 0)
			}
			post := core.TikTokExtraction{ContentType: "video", Media: []core.AssetDescriptor{register("video", "https://api19-va.tiktokv.com/aweme/v1/play/")}, Cover: core.Ptr(register("image", "https://cdn.example/cover"))}
			var uploads atomic.Int32
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				files, err := os.ReadDir(cfg.TempDir)
				if err != nil || len(files) != 0 {
					t.Error("small media was written to disk", files, err)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
				} else {
					defer r.MultipartForm.RemoveAll()
					if len(r.MultipartForm.File) != 2 {
						t.Error("recovered delivery lost thumbnail")
					}
				}
				if p.Stats().Busy != 0 {
					t.Error("upload completed while the identity was held")
				}
				uploads.Add(1)
				io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			}))
			defer telegram.Close()
			cfg.TelegramAPIBaseURL = telegram.URL
			d := platform.NewDownloader(cfg, p)
			defer d.Close()
			h := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: deliveryPost{post: post}, Downloader: d})
			before := p.Stats()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			response := serveDelivery(ctx, cfg, h)
			registrations := 1
			if failure == "invalid device_id" {
				registrations = 2
			}
			if response.Code != 200 || uploads.Load() != 1 || covers.Load() != 2 || plays.Load() != 2 || protocol.registrations != registrations || protocol.seeds != 2 || p.Stats().Writes != before.Writes+1 {
				t.Fatalf("status=%d uploads=%d plays=%d covers=%d protocol=%+v stats=%+v: %s", response.Code, uploads.Load(), plays.Load(), covers.Load(), protocol, p.Stats(), response.Body.String())
			}
			assertDeliveryIdle(t, p, cfg.TempDir)
		})
	}
}

type deliveryBody struct {
	io.ReadCloser
	done chan struct{}
}

func (b *deliveryBody) Close() error {
	close(b.done)
	return b.ReadCloser.Close()
}

func TestConcurrentTelegramDeliveriesRespectTransferLimit(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.APIKey, cfg.TelegramBotToken = "fixture-key-sixteen-characters", "fixture"
	cfg.TempDir = t.TempDir()
	cfg.DownloadRetries, cfg.DownloadConcurrency, cfg.MaxQueuedRequests = 1, 5, 512
	p := openTestPool(t, cfg, "")
	photo := deliveryJPEG(t)
	var active, peak atomic.Int32
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			select {
			case <-time.After(time.Millisecond):
				return fixtureResponse(200, photo), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}))
	}
	assets := core.NewAssets(time.Minute, 1000)
	posts := benchmarkPosts{}
	for i := range 10 {
		l := mustAcquire(t, p, "")
		post := core.TikTokExtraction{ContentType: "slideshow"}
		for range 8 {
			post.Media = append(post.Media, assets.Register(core.AssetContext{IdentityID: l.entry.id, Platform: "tiktok", Kind: "image", Filename: "photo.jpg", UpstreamURL: "https://cdn.example/photo"}, 0))
		}
		posts[strconv.Itoa(i)] = post
		finishLease(t, l, nil)
	}
	var uploads, parts atomic.Int32
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		for _, files := range r.MultipartForm.File {
			for _, file := range files {
				f, err := file.Open()
				if err != nil {
					t.Error(err)
					continue
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil || !bytes.Equal(data, photo) {
					t.Error("concurrent upload corrupted image bytes", err)
				}
				parts.Add(1)
			}
		}
		uploads.Add(1)
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer telegram.Close()
	cfg.TelegramAPIBaseURL = telegram.URL
	d := platform.NewDownloader(cfg, p)
	defer d.Close()
	h := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: posts, Downloader: d})
	before := p.Stats()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var group sync.WaitGroup
	const requests = 128
	for i := range requests {
		group.Go(func() {
			r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(fmt.Sprintf(`{"source":{"extraction_id":"%d"},"telegram":{"chat_id":1}}`, i%10))).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("concurrent delivery failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
	group.Wait()
	if uploads.Load() != requests || parts.Load() != requests*8 || p.Stats().Writes != before.Writes+requests || p.Stats().Reads != before.Reads || peak.Load() > int32(cfg.DownloadConcurrency) || peak.Load() < 2 || active.Load() != 0 {
		t.Fatalf("uploads=%d parts=%d peak=%d active=%d stats=%+v", uploads.Load(), parts.Load(), peak.Load(), active.Load(), p.Stats())
	}
	assertDeliveryIdle(t, p, cfg.TempDir)
}
