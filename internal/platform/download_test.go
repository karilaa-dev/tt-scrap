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
	w.Header().Set("ETag", `"v1"`)
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
	got, err := download(context.Background(), core.AssetContext{Kind: "video", Filename: "123.mp4", UpstreamURL: server.URL + "/video"}, 0, func(started *core.Downloaded) {
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

// versionCDN serves one of several same-sized representations and switches to
// the next after a chosen number of requests, as a CDN replacing a file would.
type versionCDN struct {
	versions      [][]byte
	etags         []string
	switchAfter   int
	ignoreIfRange bool
	dropPart      int64
	mu            sync.Mutex
	current       int
	served        int
	requests      []string
}

func (c *versionCDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
	version := c.current
	c.served++
	if c.served == c.switchAfter && c.current+1 < len(c.versions) {
		c.current++
	}
	drop := c.dropPart > 0 && strings.HasPrefix(r.Header.Get("Range"), "bytes="+strconv.FormatInt(c.dropPart, 10)+"-")
	if drop {
		// The representation changes while this part is in flight.
		c.dropPart = 0
		c.current++
	}
	c.mu.Unlock()
	data := c.versions[version]
	if c.etags[version] != "" {
		w.Header().Set("ETag", c.etags[version])
	}
	if drop {
		end := min(firstPart+minPart, len(data))
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(firstPart)+"-"+strconv.Itoa(end-1)+"/"+strconv.Itoa(len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(end-firstPart))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[firstPart : firstPart+1000])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	if c.ignoreIfRange {
		r.Header.Del("If-Range")
	}
	http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
}

func (c *versionCDN) requested() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

// otherVersion returns same-sized media with the same MP4 header and different
// payload bytes in every part.
func otherVersion(data []byte) []byte {
	other := bytes.Clone(data)
	for i := 4096; i < len(other); i++ {
		other[i] ^= 0x5a
	}
	return other
}

func downloadVersion(t *testing.T, contract downloadContract, url string, retries int) ([]byte, error) {
	t.Helper()
	cfg := testConfig()
	cfg.DownloadRetries = retries
	d := NewDownloader(cfg, nil)
	defer d.Close()
	got, err := contract.download(d, context.Background(), core.AssetContext{Kind: "video", Filename: "v.mp4", UpstreamURL: url}, 0, nil)
	if err != nil {
		return nil, err
	}
	defer got.Cleanup()
	if got.SHA256 != contract.sum(got.Data.Bytes()) {
		t.Fatal("download hash differs after representation or range retries")
	}
	return bytes.Clone(got.Data.Bytes()), nil
}

func TestRangedDownloadNeverSplicesChangedRepresentations(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			a := largeVideo(3 << 20)
			b := otherVersion(a)
			for _, test := range []struct {
				name string
				cdn  *versionCDN
			}{
				{"strong validator", &versionCDN{etags: []string{`"a"`, `"b"`}, switchAfter: 1}},
				{"If-Range ignored", &versionCDN{etags: []string{`"a"`, `"b"`}, switchAfter: 1, ignoreIfRange: true}},
				{"no validator", &versionCDN{etags: []string{"", ""}, switchAfter: 1}},
				{"weak validator", &versionCDN{etags: []string{`W/"a"`, `W/"b"`}, switchAfter: 1}},
				{"resumed part", &versionCDN{etags: []string{`"a"`, `"b"`}, dropPart: firstPart}},
			} {
				t.Run(test.name, func(t *testing.T) {
					test.cdn.versions = [][]byte{a, b}
					server := httptest.NewServer(test.cdn)
					defer server.Close()
					got, err := downloadVersion(t, contract, server.URL+"/video", 3)
					if err != nil {
						t.Fatal(err, test.cdn.requested())
					}
					if !bytes.Equal(got, b) {
						t.Fatalf("download mixed representations: equal to first=%v requests=%v", bytes.Equal(got, a), test.cdn.requested())
					}
				})
			}
		})
	}
}

func TestRangedDownloadPinsStrongValidatorAndFallsBackWithoutOne(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(5 << 20)
			strong := &versionCDN{versions: [][]byte{data}, etags: []string{`"a"`}}
			server := httptest.NewServer(strong)
			defer server.Close()
			if got, err := downloadVersion(t, contract, server.URL+"/video", 1); err != nil || !bytes.Equal(got, data) {
				t.Fatal("pinned ranged download failed", err)
			}
			requests := strong.requested()
			if len(requests) < 3 {
				t.Fatalf("strong validator did not allow parallel parts: %v", requests)
			}
			for _, request := range requests[1:] {
				if !strings.HasPrefix(request, "bytes=") || !strings.HasSuffix(request, `|"a"`) {
					t.Fatalf("part was not bound to the first representation: %v", requests)
				}
			}
			unpinned := &versionCDN{versions: [][]byte{data}, etags: []string{""}}
			fallback := httptest.NewServer(unpinned)
			defer fallback.Close()
			if got, err := downloadVersion(t, contract, fallback.URL+"/video", 1); err != nil || !bytes.Equal(got, data) {
				t.Fatal("unpinned download failed", err)
			}
			if requests := unpinned.requested(); len(requests) != 2 || requests[1] != "|" {
				t.Fatalf("unpinned media did not restart as one request: %v", requests)
			}
		})
	}
}

func TestRangedPartRetriesRejectOtherRedirectTargets(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			a := largeVideo(2 << 20)
			b := otherVersion(a)
			var redirects, finals atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Both targets share a validator; only the address identifies the file.
				w.Header().Set("ETag", `"same"`)
				switch r.URL.Path {
				case "/video":
					if redirects.Add(1) == 1 {
						http.Redirect(w, r, "/final", http.StatusFound)
					} else {
						http.Redirect(w, r, "/other", http.StatusFound)
					}
				case "/final":
					if finals.Add(1) == 2 {
						http.Error(w, "busy", http.StatusServiceUnavailable)
						return
					}
					http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(a))
				case "/other":
					http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(b))
				}
			}))
			defer server.Close()
			got, err := downloadVersion(t, contract, server.URL+"/video", 3)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, a) {
				t.Fatalf("part retry accepted another redirect target: equal to other=%v", bytes.Equal(got[firstPart:], b[firstPart:]))
			}
		})
	}
}
