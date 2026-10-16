package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

func jpegFixture(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 15), G: uint8(y * 20), B: 100, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestTelegramSingleVideoMultipartAndRawResponse(t *testing.T) {
	dependencies, tt, downloader, assets := fixture(t)
	downloader.data = bytes.Repeat([]byte("video"), 600000)
	descriptor := assets.Register(core.AssetContext{Platform: "tiktok", Filename: "video.mp4", Kind: "video"}, 0)
	tt.extraction.Media = []core.AssetDescriptor{descriptor}
	tt.extraction.Width = core.Ptr(1080)
	tt.extraction.Height = core.Ptr(1920)
	tt.extraction.DurationSeconds = core.Ptr(18)
	var received atomic.Bool
	telegram := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botfixture/sendVideo" || r.ContentLength <= int64(len(downloader.data)) || len(r.TransferEncoding) != 0 {
			t.Errorf("request metadata %s %d %v", r.URL.Path, r.ContentLength, r.TransferEncoding)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		parameters := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			if part.FileName() != "" {
				if part.FormName() != "video_file" || !bytes.Equal(data, downloader.data) || part.Header.Get("Content-Type") != "video/mp4" {
					t.Error("upload altered")
				}
			} else {
				parameters[part.FormName()] = string(data)
			}
		}
		expected := map[string]string{"chat_id": "-1001234567890123456", "video": "attach://video_file", "duration": "18", "supports_streaming": "true", "caption": "hello", "future_flag": "yes"}
		if !reflect.DeepEqual(parameters, expected) {
			t.Errorf("fields: %#v", parameters)
		}
		received.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}`)
	}))
	// This host can shrink a loopback receiver window below its 32 KiB MSS.
	// Give the fixture enough receive space to avoid zero-window probe delays.
	telegram.Config.ConnState = func(connection net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if tcp, ok := connection.(*net.TCPConn); ok {
				_ = tcp.SetReadBuffer(1 << 20)
			}
		}
	}
	telegram.Start()
	defer telegram.Close()
	dependencies.Config.TelegramBotToken = "fixture"
	dependencies.Config.TelegramAPIBaseURL = telegram.URL
	response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"url":"https://www.tiktok.com/@x/video/123"},"telegram":{"chat_id":-1001234567890123456,"caption":"hello","future_flag":"yes","video_file":"must-be-replaced"}}`, true)
	if response.Code != 429 || response.Body.String() != `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}` || !received.Load() {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Telegram-Method") != "sendVideo" {
		t.Fatal("missing actual Telegram method", response.Header())
	}
	if downloader.leaked() != 0 {
		t.Fatal("download buffers leaked")
	}
}

func TestTelegramAlbumPreflightBatchesAndPartial207(t *testing.T) {
	dependencies, tt, downloader, assets := fixture(t)
	downloader.data = jpegFixture(t)
	downloader.mime = "image/jpeg"
	tt.extraction.ContentType = "slideshow"
	for i := 0; i < 11; i++ {
		tt.extraction.Media = append(tt.extraction.Media, assets.Register(core.AssetContext{Filename: fmt.Sprintf("image%d.jpg", i), Kind: "image", UpstreamURL: fmt.Sprint(i)}, i))
	}
	var sizes []int
	var mu sync.Mutex
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloader.mu.Lock()
		completed := len(downloader.buffers)
		downloader.mu.Unlock()
		if completed != 11 {
			t.Errorf("sent before preflight: %d downloaded", completed)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		var group []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("media")), &group); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sizes = append(sizes, len(group))
		batch := len(sizes)
		mu.Unlock()
		if len(r.MultipartForm.File) != len(group) {
			t.Errorf("missing uploads %d %d", len(r.MultipartForm.File), len(group))
		}
		for _, item := range group {
			if item["type"] != "photo" {
				t.Error(item)
			}
			name := strings.TrimPrefix(item["media"].(string), "attach://")
			part, _, err := r.FormFile(name)
			if err != nil {
				t.Error(err)
				continue
			}
			data, _ := io.ReadAll(part)
			part.Close()
			if !bytes.Equal(data, downloader.data) {
				t.Error("native JPEG altered")
			}
		}
		if batch == 1 && r.FormValue("reply_parameters") == "" {
			t.Error("missing first reply")
		}
		if batch > 1 && r.FormValue("reply_parameters") != "" {
			t.Error("repeated reply")
		}
		w.Header().Set("Content-Type", "application/json")
		if batch == 1 {
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"message_id":1}]}`)
		} else {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"ok":false,"description":"fixture rejection"}`)
		}
	}))
	defer telegram.Close()
	dependencies.Config.TelegramBotToken = "fixture"
	dependencies.Config.TelegramAPIBaseURL = telegram.URL
	response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1,"reply_parameters":{"message_id":2}}}`, true)
	if response.Code != 207 || !strings.Contains(response.Body.String(), `"partial":true`) || !reflect.DeepEqual(sizes, []int{9, 2}) {
		t.Fatalf("got %d %s sizes %v", response.Code, response.Body.String(), sizes)
	}
	if downloader.leaked() != 0 {
		t.Fatal("download buffers leaked")
	}
}

func TestTelegramPreflightFailureNeverUploads(t *testing.T) {
	for _, failure := range []string{"download", "caption", "too-large"} {
		t.Run(failure, func(t *testing.T) {
			dependencies, tt, downloader, assets := fixture(t)
			tt.extraction.ContentType = "slideshow"
			for i := 0; i < 3; i++ {
				tt.extraction.Media = append(tt.extraction.Media, assets.Register(core.AssetContext{Filename: "image.jpg", Kind: "image", UpstreamURL: fmt.Sprint(i)}, i))
			}
			downloader.fail = "1"
			var uploads atomic.Int32
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1); _, _ = io.WriteString(w, `{"ok":true}`) }))
			defer telegram.Close()
			dependencies.Config.TelegramBotToken = "fixture"
			dependencies.Config.TelegramAPIBaseURL = telegram.URL
			body := `{"source":{"extraction_id":"saved"},"delivery":"document","telegram":{"chat_id":1}}`
			want := 502
			if failure == "caption" {
				body = `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1,"caption":"not supported"}}`
				want = 422
			}
			if failure == "too-large" {
				downloader.fail = ""
				dependencies.Config.TelegramMaxBytes = 4
				want = 413
			}
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", body, true)
			if response.Code != want || uploads.Load() != 0 {
				t.Fatalf("%d uploads %d: %s", response.Code, uploads.Load(), response.Body.String())
			}
			if failure == "caption" && downloader.calls != 0 {
				t.Fatal("invalid fields performed download")
			}
			if downloader.leaked() != 0 {
				t.Fatal("download buffers leaked")
			}
		})
	}
}

func TestInstagramAlbumCaptionAndVideoOverrides(t *testing.T) {
	dependencies, _, downloader, assets := fixture(t)
	downloader.data = jpegFixture(t)
	downloader.mime = "image/jpeg"
	instagram := &fakeInstagram{extraction: core.InstagramExtraction{Platform: "instagram", ContentType: "carousel"}}
	for i, kind := range []string{"image", "video"} {
		instagram.extraction.Media = append(instagram.extraction.Media, core.InstagramMedia{MediaType: kind, Asset: assets.Register(core.AssetContext{Filename: "media.jpg", Kind: kind}, i)})
	}
	dependencies.Instagram = instagram
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		var items []map[string]any
		_ = json.Unmarshal([]byte(r.FormValue("media")), &items)
		if len(items) != 2 || items[0]["caption"] != "gallery" || items[1]["caption"] != nil || items[0]["has_spoiler"] != true || items[1]["supports_streaming"] != false {
			t.Errorf("items %#v", items)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer telegram.Close()
	dependencies.Config.TelegramBotToken = "fixture"
	dependencies.Config.TelegramAPIBaseURL = telegram.URL
	response := request(NewHandler(dependencies), "POST", "/v1/instagram/telegram-deliveries", `{"source":{"url":"https://www.instagram.com/p/x/"},"telegram":{"chat_id":1,"caption":"gallery","has_spoiler":true,"supports_streaming":false}}`, true)
	if response.Code != 200 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

func TestMultipartClientCancellationAndNoAutomaticRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	config := core.DefaultConfig()
	config.TelegramBotToken = "fixture"
	config.TelegramAPIBaseURL = server.URL
	config.TelegramTimeout = 30 * time.Millisecond
	client := newTelegramClient(config, nil)
	video := spool.Bytes(bytes.Repeat([]byte{42}, 1<<20))
	_, err := client.call(context.Background(), "sendVideo", fields{"chat_id": 1}, []upload{{Prepared: media.Prepared{Data: video, Filename: "video.mp4", ContentType: "video/mp4", Size: 1 << 20}, Field: "video_file"}})
	if failure, ok := err.(*core.Error); !ok || failure.Code != "telegram_timeout" || calls.Load() != 1 {
		t.Fatalf("calls %d error %v", calls.Load(), err)
	}
}

func TestAlbumBatchBoundaries(t *testing.T) {
	for _, tc := range []struct {
		count int
		want  []int
	}{{11, []int{9, 2}}, {21, []int{10, 9, 2}}, {20, []int{10, 10}}} {
		var sizes []int
		for _, batch := range albumBatches(make([]int, tc.count)) {
			sizes = append(sizes, len(batch))
		}
		if !reflect.DeepEqual(sizes, tc.want) {
			t.Fatal(sizes)
		}
	}
}

// stalledBody fails a Telegram response read with err, or once the request
// ends.
type stalledBody struct {
	ctx     context.Context
	reading chan struct{}
	err     error
}

func (b *stalledBody) Read([]byte) (int, error) {
	close(b.reading)
	if b.err != nil {
		return 0, b.err
	}
	<-b.ctx.Done()
	return 0, errors.New("read on canceled connection")
}
func (b *stalledBody) Close() error { return nil }

func TestTelegramResponseReadPreservesCallerCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		readErr    error
		cancel     bool
		timeout    time.Duration
	}{
		{"caller canceled", "", nil, true, time.Minute},
		{"upstream timeout", "telegram_timeout", nil, false, 30 * time.Millisecond},
		{"read failure", "telegram_network_error", errors.New("reset"), false, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			config := core.DefaultConfig()
			config.TelegramBotToken = "fixture"
			config.TelegramTimeout = tc.timeout
			client := newTelegramClient(config, deliveryDoer(func(r *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, r.Body)
				body := &stalledBody{ctx: r.Context(), reading: make(chan struct{}), err: tc.readErr}
				if tc.cancel {
					go func() { <-body.reading; cancel() }()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
			}))
			_, err := client.call(ctx, "sendMessage", fields{"chat_id": 1}, nil)
			var failure *core.Error
			if tc.code == "" {
				if !errors.Is(err, context.Canceled) || errors.As(err, &failure) {
					t.Fatalf("caller cancellation became %v", err)
				}
			} else if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("error %v", err)
			}
		})
	}
}
