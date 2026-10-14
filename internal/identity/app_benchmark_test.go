package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func benchmarkMovie(width, height uint16) []byte {
	box := func(kind string, body []byte) []byte {
		out := make([]byte, 8+len(body))
		binary.BigEndian.PutUint32(out, uint32(len(out)))
		copy(out[4:], kind)
		copy(out[8:], body)
		return out
	}
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:], width)
	binary.BigEndian.PutUint16(entry[26:], height)
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, box("avc1", entry)...)
	return box("moov", box("trak", box("mdia", append(box("hdlr", handler), box("minf", box("stbl", box("stsd", stsd)))...))))
}

func BenchmarkAppExtraction(b *testing.B) {
	for _, delay := range []time.Duration{0, 20 * time.Millisecond} {
		for _, concurrency := range []int{1, 4, 16} {
			b.Run(fmt.Sprintf("delay%dms/c%d", delay.Milliseconds(), concurrency), func(b *testing.B) {
				cfg, _ := testDatabase(b)
				cfg.TikTokMethod = "app"
				cfg.MaxQueuedRequests = 128
				cfg.CacheMaxEntries = 64
				p := openTestPool(b, cfg, "")
				var details, probes atomic.Int64
				movies := [][]byte{benchmarkMovie(540, 960), benchmarkMovie(720, 1280), benchmarkMovie(1080, 1920)}
				p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
					return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
						if delay > 0 {
							select {
							case <-time.After(delay):
							case <-r.Context().Done():
								return nil, r.Context().Err()
							}
						}
						if r.URL.Path == "/aweme/v1/aweme/detail/" {
							details.Add(1)
							id := r.URL.Query().Get("aweme_id")
							video := map[string]any{"duration": 1000}
							for n, key := range []string{"play_addr", "play_addr_h264", "play_addr_bytevc1"} {
								video[key] = map[string]any{"width": 2160 - n*540, "height": 3840 - n*960, "url_list": []string{fmt.Sprintf("https://cdn.example/%s/%d", id, n)}}
							}
							body, _ := json.Marshal(map[string]any{"status_code": 0, "aweme_detail": map[string]any{"aweme_id": id, "video": video}})
							res := fixtureResponse(200, body)
							res.Header.Add("Set-Cookie", "request=received; Secure; Path=/")
							return res, nil
						}
						probes.Add(1)
						n, _ := strconv.Atoi(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:])
						body := movies[n]
						return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body))}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
					}))
				}
				for range 10 {
					l, err := p.acquire(context.Background(), "")
					if err != nil {
						b.Fatal(err)
					}
					now := time.Now().UTC()
					s := l.entry.session
					s.Identity.Kind = "registered"
					s.Identity.DeviceID = "7500000000000000001"
					s.Identity.InstallID = "7500000000000000002"
					s.Identity.RegisteredAt = &now
					s.SDK = &tiktokapp.SDKState{Seed: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 98)), Algorithm: 3, FetchedAt: now}
					if err = l.finish(nil); err != nil {
						b.Fatal(err)
					}
				}
				service := platform.NewTikTok(cfg, core.NewAssets(time.Minute, 64), p, nil)
				defer service.Close()
				before := p.Stats()
				var next atomic.Int64
				var failures atomic.Int64
				var wg sync.WaitGroup
				b.ReportAllocs()
				b.ResetTimer()
				for range concurrency {
					wg.Go(func() {
						for {
							i := next.Add(1)
							if i > int64(b.N) {
								return
							}
							id := strconv.FormatInt(7600000000000000000+i, 10)
							post, err := service.ExtractURL(context.Background(), "https://www.tiktok.com/@fixture/video/"+id, true)
							if err != nil || post.Width == nil || *post.Width != 2160 || post.Height == nil || *post.Height != 3840 {
								failures.Add(1)
								b.Errorf("extraction failed: %v", err)
							}
						}
					})
				}
				wg.Wait()
				b.StopTimer()
				if failures.Load() != 0 || details.Load() != int64(b.N) || probes.Load() != 0 {
					b.Fatalf("work mismatch details=%d probes=%d errors=%d", details.Load(), probes.Load(), failures.Load())
				}
				after := p.Stats()
				if after.Busy != 0 || after.Queued != 0 || after.Reads != before.Reads {
					b.Fatal("pool leaked work or queried DB", after)
				}
				b.ReportMetric(float64(after.Writes-before.Writes)/float64(b.N), "DB-writes/op")
			})
		}
	}
}
