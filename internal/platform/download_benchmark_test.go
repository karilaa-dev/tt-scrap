package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

// Local fixtures only. The same benchmark can run against an unchanged source
// checkout to compare network, spool and hash cost with identical limits.
func BenchmarkDownloadLocal(b *testing.B) {
	data := largeVideo(4 << 20)
	want := sha256.Sum256(data)
	wantHash := hex.EncodeToString(want[:])
	for _, mode := range []string{"known_ram", "known_disk", "chunked_ram", "chunked_spill", "ranged_ram", "ranged_disk"} {
		b.Run(mode, func(b *testing.B) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "ranged_ram", "ranged_disk":
					w.Header().Set("ETag", `"fixture"`)
					http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
					return
				case "chunked_ram", "chunked_spill":
					w.(http.Flusher).Flush()
				default:
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				}
				// Flush bounded chunks to avoid loopback zero-window stalls when
				// a single response spans the host's initial receive window.
				for offset := 0; offset < len(data); offset += 64 << 10 {
					if _, err := w.Write(data[offset:min(offset+64<<10, len(data))]); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.TempDir = b.TempDir()
			if mode == "known_disk" || mode == "ranged_disk" || mode == "chunked_spill" {
				cfg.MediaMemoryFileBytes = 1 << 20
			}
			d := NewDownloader(cfg, nil)
			defer d.Close()
			asset := core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: server.URL}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for range b.N {
				got, err := d.Download(context.Background(), asset, 0)
				if err != nil {
					b.Fatal(err)
				}
				if got.SHA256 != wantHash || got.Size != int64(len(data)) {
					got.Cleanup()
					b.Fatal("download differs")
				}
				got.Cleanup()
			}
			b.StopTimer()
			if d.MemoryInUse() != 0 {
				b.Fatal("download retained memory", d.MemoryInUse())
			}
		})
	}
}

func BenchmarkDownloadSmallKnown(b *testing.B) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil); err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{encoded.Len(), 1 << 10, 1<<10 + 1, 16 << 10, 16<<10 + 1} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			data := make([]byte, size)
			copy(data, encoded.Bytes())
			sum := sha256.Sum256(data)
			want := hex.EncodeToString(sum[:])
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data)
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.TempDir = b.TempDir()
			d := NewDownloader(cfg, nil)
			defer d.Close()
			b.ReportAllocs()
			for b.Loop() {
				got, err := d.Download(context.Background(), core.AssetContext{Kind: "image", UpstreamURL: server.URL}, 0)
				if err != nil {
					b.Fatal(err)
				}
				if got.Size != int64(len(data)) || got.SHA256 != want || got.ContentType != "image/jpeg" {
					b.Fatal("download changed", got)
				}
				got.Cleanup()
			}
			if d.MemoryInUse() != 0 {
				b.Fatal("download memory retained", d.MemoryInUse())
			}
		})
	}
}
