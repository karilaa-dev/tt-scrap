package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type rangeCDN struct {
	data     []byte
	mu       sync.Mutex
	ranges   []string
	active   atomic.Int32
	peak     atomic.Int32
	dropPart int64
	dropped  atomic.Bool
}

func (c *rangeCDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	current := c.active.Add(1)
	defer c.active.Add(-1)
	for previous := c.peak.Load(); current > previous && !c.peak.CompareAndSwap(previous, current); previous = c.peak.Load() {
	}
	byteRange := r.Header.Get("Range")
	c.mu.Lock()
	c.ranges = append(c.ranges, byteRange)
	c.mu.Unlock()
	// Let parallel parts overlap so the fixture observes concurrent connections.
	time.Sleep(5 * time.Millisecond)
	if c.dropPart > 0 && strings.HasPrefix(byteRange, "bytes="+strconv.FormatInt(c.dropPart, 10)+"-") && c.dropped.CompareAndSwap(false, true) {
		end := min(c.dropPart+minPart, int64(len(c.data)))
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(c.dropPart, 10)+"-"+strconv.FormatInt(end-1, 10)+"/"+strconv.Itoa(len(c.data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-c.dropPart, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(c.data[c.dropPart : c.dropPart+1000])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(c.data))
}

func (c *rangeCDN) requested() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ranges...)
}

func largeVideo(size int) []byte {
	head := append(boxBytes("ftyp", []byte("isom0000")), testMovie(1080, 1920)...)
	payload := make([]byte, size-len(head)-8)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	return append(head, boxBytes("mdat", payload)...)
}

func TestRangedDownloadUsesParallelPartsAndAnnouncesEarly(t *testing.T) {
	cdn := &rangeCDN{data: largeVideo(5<<20 + 12345), dropPart: 2 << 20}
	server := httptest.NewServer(cdn)
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	cfg.DownloadRetries = 2
	d := NewDownloader(cfg, nil)
	defer d.Close()
	var announced []*core.Downloaded
	var download core.DownloadFunc
	if err := d.WithIdentity(context.Background(), "", func(f core.DownloadFunc) error { download = f; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := download(context.Background(), core.AssetContext{Kind: "video", Filename: "123.mp4", UpstreamURL: server.URL + "/video", ExpectedWidth: 1080, ExpectedHeight: 1920}, 0, func(started *core.Downloaded) {
		if started.Data.Bytes() != nil || started.Size != int64(len(cdn.data)) || started.ContentType != "video/mp4" || started.Filename != "123.mp4" {
			t.Errorf("announced after completion or without metadata: %+v", started)
		}
		announced = append(announced, started)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Cleanup()
	sum := sha256.Sum256(cdn.data)
	if !bytes.Equal(got.Data.Bytes(), cdn.data) || got.SHA256 != hex.EncodeToString(sum[:]) || got.Width != 1080 || got.Height != 1920 {
		t.Fatal("ranged download altered media")
	}
	if len(announced) != 1 || announced[0].Data != got.Data {
		t.Fatal("transfer not announced exactly once", len(announced))
	}
	ranges := cdn.requested()
	if ranges[0] != "bytes=0-"+strconv.Itoa(firstPart-1) || cdn.peak.Load() < 2 {
		t.Fatalf("parts were not fetched in parallel: peak=%d ranges=%v", cdn.peak.Load(), ranges)
	}
	resumed := "bytes=" + strconv.Itoa(2<<20+1000) + "-" + strconv.Itoa(3<<20-1)
	found := false
	for _, value := range ranges {
		found = found || value == resumed
	}
	if !found {
		t.Fatalf("dropped part did not resume from its last byte: %v", ranges)
	}
	if files, _ := os.ReadDir(cfg.TempDir); len(files) != 0 || !got.Data.InMemory() {
		t.Fatal("media below the RAM limit used disk", files)
	}
}

func TestLargeDownloadUsesDiskAndSinglePartWhenParallelDisabled(t *testing.T) {
	cdn := &rangeCDN{data: largeVideo(3 << 20)}
	server := httptest.NewServer(cdn)
	defer server.Close()
	cfg := testConfig()
	cfg.TempDir = t.TempDir()
	cfg.MediaMemoryFileBytes = 1 << 20
	d := NewDownloader(cfg, nil)
	defer d.Close()
	got, err := d.Download(context.Background(), core.AssetContext{Kind: "video", Filename: "big.mp4", UpstreamURL: server.URL + "/video"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Data.InMemory() || d.MemoryInUse() != 0 || got.Size != int64(len(cdn.data)) {
		t.Fatal("media above the RAM limit stayed in RAM")
	}
	got.Cleanup()
	cfg.DownloadParts = 1
	cfg.MediaMemoryFileBytes = 250 << 20
	single := NewDownloader(cfg, nil)
	defer single.Close()
	before := len(cdn.requested())
	got, err = single.Download(context.Background(), core.AssetContext{Kind: "video", Filename: "big.mp4", UpstreamURL: server.URL + "/video"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Cleanup()
	if ranges := cdn.requested()[before:]; len(ranges) != 1 || ranges[0] != "" || !bytes.Equal(got.Data.Bytes(), cdn.data) {
		t.Fatalf("single-part download sent ranges: %v", ranges)
	}
}

func TestRangedPartsReuseConnections(t *testing.T) {
	cdn := &rangeCDN{data: largeVideo(9 << 20)}
	server := httptest.NewUnstartedServer(cdn)
	var connections atomic.Int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	cfg := testConfig()
	d := NewDownloader(cfg, nil)
	defer d.Close()
	got, err := d.Download(context.Background(), core.AssetContext{Kind: "video", Filename: "big.mp4", UpstreamURL: server.URL + "/video"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Cleanup()
	if parts := len(cdn.requested()); parts < 6 || connections.Load() > int32(cfg.DownloadParts) {
		t.Fatalf("%d parts used %d connections, want at most %d", parts, connections.Load(), cfg.DownloadParts)
	}
}
