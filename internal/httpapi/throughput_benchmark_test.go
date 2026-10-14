package httpapi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

type savedPost struct {
	core.TikTokService
	post core.TikTokExtraction
}

func (s savedPost) GetExtraction(context.Context, string) (core.TikTokExtraction, error) {
	return s.post, nil
}

func benchmarkBox(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	box := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], body)
	return box
}

func benchmarkMP4(size int) []byte {
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:], 1080)
	binary.BigEndian.PutUint16(entry[26:], 1920)
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	stsd := benchmarkBox("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, benchmarkBox("avc1", entry))
	head := append(benchmarkBox("ftyp", []byte("isom0000")), benchmarkBox("moov", benchmarkBox("trak", benchmarkBox("mdia", benchmarkBox("hdlr", handler), benchmarkBox("minf", benchmarkBox("stbl", stsd)))))...)
	return append(head, benchmarkBox("mdat", make([]byte, size-len(head)-8))...)
}

// throttle paces 64 KiB chunks at bytesPerSecond.
func throttle(bytesPerSecond int, chunk int) {
	time.Sleep(time.Duration(chunk) * time.Second / time.Duration(bytesPerSecond))
}

// BenchmarkThrottledVideoDelivery models a CDN that limits each connection
// and a Telegram endpoint with finite intake, both slower than local disk.
func BenchmarkThrottledVideoDelivery(b *testing.B) {
	const size, cdnRate, telegramRate = 8 << 20, 10 << 20, 20 << 20
	data := benchmarkMP4(size)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, status := 0, len(data), http.StatusOK
		if value := r.Header.Get("Range"); value != "" {
			if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			end = min(end+1, len(data))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
			status = http.StatusPartialContent
		}
		w.Header().Set("ETag", `"benchmark"`)
		w.Header().Set("Content-Length", strconv.Itoa(end-start))
		w.WriteHeader(status)
		for offset := start; offset < end; offset += 64 << 10 {
			chunk := data[offset:min(offset+64<<10, end)]
			throttle(cdnRate, len(chunk))
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer cdn.Close()
	telegram := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffer := make([]byte, 64<<10)
		var received int64
		for {
			n, err := io.ReadFull(r.Body, buffer)
			received += int64(n)
			throttle(telegramRate, n)
			if err != nil {
				break
			}
		}
		if received < size {
			http.Error(w, "short upload", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	// As in the multipart test, avoid this host's loopback zero-window stalls.
	telegram.Config.ConnState = func(connection net.Conn, state http.ConnState) {
		if tcp, ok := connection.(*net.TCPConn); ok && state == http.StateNew {
			_ = tcp.SetReadBuffer(1 << 20)
		}
	}
	telegram.Start()
	defer telegram.Close()
	cfg := core.DefaultConfig()
	cfg.APIKey, cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "benchmark-key-sixteen-chars", "fixture", telegram.URL
	cfg.TempDir = b.TempDir()
	cfg.ThumbnailWait = 0
	assets := core.NewAssets(time.Hour, 100)
	post := core.TikTokExtraction{ContentType: "video", Media: []core.AssetDescriptor{assets.Register(core.AssetContext{Platform: "tiktok", Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}}
	downloader := platform.NewDownloader(cfg, nil)
	defer downloader.Close()
	handler := httpapi.NewHandler(httpapi.Dependencies{Config: cfg, Assets: assets, TikTok: savedPost{post: post}, Downloader: downloader})
	deliver := func() {
		r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", bytes.NewBufferString(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`))
		r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			b.Fatal(w.Code, w.Body.String())
		}
	}
	// New loopback connections start with small receive windows; warm them first.
	deliver()
	b.SetBytes(size)
	for b.Loop() {
		deliver()
	}
}
