package httpapi_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

// BenchmarkImageDelivery checks complete preparation and multipart delivery.
// Local fixture work is included in timing and allocation measurements.
func BenchmarkImageDelivery(b *testing.B) {
	var goldens map[string]struct {
		SHA256       string `json:"sha256"`
		SourceSHA256 string `json:"source_sha256"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		Bytes        int    `json:"bytes"`
	}
	goldenData, err := os.ReadFile("testdata/image-golden.json")
	if err != nil {
		b.Fatal(err)
	}
	if err := json.Unmarshal(goldenData, &goldens); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"HEICPhoto", "WebPDocumentAlbum11"} {
		b.Run(name, func(b *testing.B) {
			filename, count := "sample.heic", 1
			if name == "WebPDocumentAlbum11" {
				filename, count = "static.webp", 11
			}
			data, err := os.ReadFile("../media/testdata/" + filename)
			if err != nil {
				b.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(data)) != goldens[filename].SourceSHA256 {
				b.Fatal("golden source mismatch")
			}
			var sourceBytes, verified atomic.Int64
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				n, _ := w.Write(data)
				sourceBytes.Add(int64(n))
			}))
			defer cdn.Close()
			telegram := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					http.Error(w, "multipart", 400)
					return
				}
				files, chat, valid := 0, "", true
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						valid = false
						break
					}
					body, err := io.ReadAll(part)
					if err != nil {
						valid = false
						break
					}
					if part.FormName() == "chat_id" {
						chat = string(body)
					}
					if part.FileName() == "" {
						continue
					}
					files++
					img, format, err := image.Decode(bytes.NewReader(body))
					want := goldens[filename]
					valid = valid && err == nil && format == "jpeg" && img.Bounds().Dx() == want.Width && img.Bounds().Dy() == want.Height && len(body) == want.Bytes && fmt.Sprintf("%x", sha256.Sum256(body)) == want.SHA256
				}
				if !valid || chat != "1" || files < 1 {
					http.Error(w, "invalid media", 400)
					return
				}
				verified.Add(int64(files))
				w.Header().Set("Content-Type", "application/json")
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
			cfg.APIKey, cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "local-benchmark-key-long-enough", "fixture", telegram.URL
			cfg.DownloadRetries = 1
			cfg.TempDir = b.TempDir()
			assets := core.NewAssets(time.Hour, 100)
			post := core.TikTokExtraction{ContentType: "slideshow"}
			for i := 0; i < count; i++ {
				post.Media = append(post.Media, assets.Register(core.AssetContext{Platform: "tiktok", Kind: "image", Filename: filename, UpstreamURL: cdn.URL + "/media"}, i))
			}
			downloader := platform.NewDownloader(cfg, nil)
			defer downloader.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			handler := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: savedPost{post: post}, Downloader: downloader, HTTPClient: &http.Client{Transport: transport}})
			mode := ""
			if name == "WebPDocumentAlbum11" {
				mode = `,"delivery":"document"`
			}
			payload := fmt.Sprintf(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}%s}`, mode)
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(payload))
				r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				r.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				if response.Code != 200 {
					b.Fatal(response.Code, response.Body.String())
				}
				if downloader.MemoryInUse() != 0 {
					b.Fatal("download memory retained")
				}
			}
			if verified.Load() != int64(b.N*count) || sourceBytes.Load() != int64(b.N*count*len(data)) {
				b.Fatal("work count mismatch", verified.Load(), sourceBytes.Load(), b.N)
			}
		})
	}
}
