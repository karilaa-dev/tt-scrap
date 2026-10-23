package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestDownloadHashAcrossStorageAndResponseModes(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			for _, test := range []struct {
				name    string
				chunked bool
				ranged  bool
				disk    bool
				size    int
			}{
				{"small_below_prefix", false, false, false, 511},
				{"small_prefix", false, false, false, 512},
				{"small_below_cutoff", false, false, false, 1<<10 - 1},
				{"small_cutoff", false, false, false, 1 << 10},
				{"small_above_cutoff", false, false, false, 1<<10 + 1},
				{"small_whole_first_part", false, true, false, 511},
				{"small_cutoff_whole_first_part", false, true, false, 1 << 10},
				{"known_ram", false, false, false, 2<<20 + 123},
				{"known_disk", false, false, true, 2<<20 + 123},
				{"chunked_ram", true, false, false, 2<<20 + 123},
				{"chunked_spill", true, false, true, 2<<20 + 123},
				{"ranged_ram", false, true, false, 2<<20 + 123},
				{"ranged_disk", false, true, true, 2<<20 + 123},
				{"whole_first_part", false, true, false, firstPart - 123},
			} {
				t.Run(test.name, func(t *testing.T) {
					data := largeVideo(test.size)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if test.ranged {
							w.Header().Set("ETag", `"fixture"`)
							http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
							return
						}
						if test.chunked {
							w.(http.Flusher).Flush()
						} else {
							w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						}
						_, _ = w.Write(data)
					}))
					defer server.Close()
					cfg := testConfig()
					cfg.TempDir = t.TempDir()
					if test.disk {
						cfg.MediaMemoryFileBytes = 1 << 20
					}
					d := NewDownloader(cfg, nil)
					defer d.Close()
					var created, written atomic.Int64
					d.newDigest = func() hash.Hash {
						created.Add(1)
						return &observedDigest{Hash: sha256.New(), written: &written}
					}
					got, err := contract.download(d, context.Background(), core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: server.URL}, 0, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer got.Cleanup()
					wantCreated, wantWritten := int64(1), int64(len(data))
					if contract == deliveryDownload {
						wantCreated, wantWritten = 0, 0
					}
					if created.Load() != wantCreated || written.Load() != wantWritten {
						t.Fatalf("digest work: instances=%d bytes=%d, want %d/%d", created.Load(), written.Load(), wantCreated, wantWritten)
					}
					actual, err := io.ReadAll(got.Data.Reader(context.Background(), 0, -1))
					if err != nil || !bytes.Equal(actual, data) || got.SHA256 != contract.sum(data) || got.Filename != "video.mp4" || got.ContentType != "video/mp4" || got.Size != int64(len(data)) || got.Width != 1080 || got.Height != 1920 || got.Data.InMemory() == test.disk {
						t.Fatal("media or metadata changed", got, err)
					}
					got.Cleanup()
					if d.MemoryInUse() != 0 {
						t.Fatal("completed download retained memory", d.MemoryInUse())
					}
				})
			}
		})
	}
}

func TestDownloadCancellationReleasesHasherAndMemory(t *testing.T) {
	for _, contract := range []downloadContract{identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(3 << 20)
			for _, ranged := range []bool{false, true} {
				for _, disk := range []bool{false, true} {
					name := "single"
					if ranged {
						name = "ranged"
					}
					if disk {
						name += "_disk"
					}
					t.Run(name, func(t *testing.T) {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/success" {
								w.Header().Set("Content-Length", strconv.Itoa(len(data)))
								_, _ = w.Write(data)
								return
							}
							if ranged {
								w.Header().Set("ETag", `"fixture"`)
								w.Header().Set("Content-Range", "bytes 0-"+strconv.Itoa(firstPart-1)+"/"+strconv.Itoa(len(data)))
								w.Header().Set("Content-Length", strconv.Itoa(firstPart))
								w.WriteHeader(http.StatusPartialContent)
							} else {
								w.Header().Set("Content-Length", strconv.Itoa(len(data)))
							}
							_, _ = w.Write(data[:512])
							w.(http.Flusher).Flush()
							<-r.Context().Done()
						}))
						defer server.Close()
						cfg := testConfig()
						cfg.TempDir = t.TempDir()
						if disk {
							cfg.MediaMemoryFileBytes = 0
						}
						d := NewDownloader(cfg, nil)
						defer d.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						announced := false
						err := contract.withIdentity(d)(ctx, "", func(download core.DownloadFunc) error {
							got, err := download(ctx, core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: server.URL}, 0, func(*core.Downloaded) {
								announced = true
								cancel()
							})
							got.Cleanup()
							return err
						})
						if !announced || !errors.Is(err, context.Canceled) || d.MemoryInUse() != 0 {
							t.Fatal("cancelled download retained work", announced, err, d.MemoryInUse())
						}
						got, err := d.Download(context.Background(), core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: server.URL + "/success"}, 0)
						if err != nil {
							t.Fatal("download after cancellation failed", err)
						}
						got.Cleanup()
					})
				}
			}
		})
	}
}

func TestDownloadTruncatedResponseRestartsDigest(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(128 << 10)
			broken := otherVersion(data)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				if r.URL.Path == "/broken" {
					_, _ = w.Write(broken[:len(broken)/2])
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			d := NewDownloader(testConfig(), nil)
			defer d.Close()
			got, err := contract.download(d, context.Background(), core.AssetContext{
				Kind: "video", Filename: "video.mp4", UpstreamURL: server.URL + "/broken", AlternateURLs: []string{server.URL + "/good"},
			}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer got.Cleanup()
			if !bytes.Equal(got.Data.Bytes(), data) || got.SHA256 != contract.sum(data) {
				t.Fatal("fallback reused bytes or digest state from a truncated response")
			}
			got.Cleanup()
			if d.MemoryInUse() != 0 {
				t.Fatal("fallback retained RAM", d.MemoryInUse())
			}
		})
	}
}

func TestDownloadCancellationOnFinalBytes(t *testing.T) {
	for _, contract := range []downloadContract{identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(512)
			for _, mode := range []string{"ram", "disk", "whole_first_part"} {
				t.Run(mode, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if mode == "whole_first_part" {
							w.Header().Set("ETag", `"fixture"`)
							http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
							return
						}
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						_, _ = w.Write(data)
					}))
					defer server.Close()
					cfg := testConfig()
					cfg.TempDir = t.TempDir()
					if mode == "disk" {
						cfg.MediaMemoryFileBytes = 0
					}
					d := NewDownloader(cfg, nil)
					defer d.Close()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					err := contract.withIdentity(d)(ctx, "", func(download core.DownloadFunc) error {
						got, err := download(ctx, core.AssetContext{Kind: "video", UpstreamURL: server.URL}, 0, func(*core.Downloaded) { cancel() })
						got.Cleanup()
						return err
					})
					if !errors.Is(err, context.Canceled) || d.MemoryInUse() != 0 {
						t.Fatal("cancelled final bytes completed the download", err, d.MemoryInUse())
					}
				})
			}
		})
	}
}

func TestDownloadFirstResponseFailureStopsActiveRangeWorkers(t *testing.T) {
	for _, contract := range []downloadContract{rawDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(3 << 20)
			started, stopped := make(chan struct{}), make(chan struct{}, 2)
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				span := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
				from, to, _ := strings.Cut(span, "-")
				start, _ := strconv.Atoi(from)
				end, _ := strconv.Atoi(to)
				end = min(end, len(data)-1)
				w.Header().Set("ETag", `"fixture"`)
				w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(len(data)))
				w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				if start == 0 {
					_, _ = w.Write(data[:512])
					w.(http.Flusher).Flush()
					select {
					case <-started:
						panic(http.ErrAbortHandler)
					case <-r.Context().Done():
						return
					}
				}
				w.(http.Flusher).Flush()
				once.Do(func() { close(started) })
				<-r.Context().Done()
				stopped <- struct{}{}
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.TempDir = t.TempDir()
			d := NewDownloader(cfg, nil)
			defer d.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got, err := contract.download(d, ctx, core.AssetContext{Kind: "video", UpstreamURL: server.URL}, 0, nil)
			got.Cleanup()
			if err == nil || ctx.Err() != nil || d.MemoryInUse() != 0 {
				t.Fatal("failed first response retained workers or RAM", err, ctx.Err(), d.MemoryInUse())
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("active range request was not canceled")
			}
		})
	}
}
