package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

func mp4Box(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	box := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], body)
	return box
}

func TestMultipartWithholdsFinal64KiBUntilReleased(t *testing.T) {
	for _, size := range []int{65535, 65536, 65537} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/canceled=%t", size, canceled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				data := spool.Bytes(bytes.Repeat([]byte{'v'}, size))
				defer data.Close()
				gate, release := make(chan struct{}), make(chan struct{})
				body, _, _, err := multipartBody(ctx, nil, []upload{{
					Prepared: media.Prepared{Data: data, Filename: "video.mp4", ContentType: "video/mp4", Size: int64(size)},
					Field:    "video_file",
					hold: func(ctx context.Context) error {
						close(gate)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				}})
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				done := make(chan error, 1)
				go func() {
					_, err := io.Copy(&output, body)
					done <- err
				}()
				select {
				case <-gate:
				case <-time.After(time.Second):
					t.Fatal("upload did not reach its final-byte gate")
				}
				prefix := bytes.Clone(output.Bytes())
				headerEnd := bytes.Index(prefix, []byte("\r\n\r\n")) + 4
				if headerEnd < 4 || !bytes.Equal(prefix[headerEnd:], bytes.Repeat([]byte{'v'}, max(0, size-65536))) {
					t.Fatal("upload released media tail or multipart ending before validation")
				}
				if canceled {
					cancel()
				} else {
					close(release)
				}
				select {
				case err := <-done:
					if canceled {
						if !errors.Is(err, context.Canceled) || !bytes.Equal(output.Bytes(), prefix) {
							t.Fatal("canceled upload sent its final bytes", err)
						}
					} else if err != nil || !bytes.HasPrefix(output.Bytes()[headerEnd:], bytes.Repeat([]byte{'v'}, size)) || !bytes.HasSuffix(output.Bytes(), []byte("--\r\n")) {
						t.Fatal("released upload did not finish exactly", err)
					}
				case <-time.After(time.Second):
					t.Fatal("upload did not leave its final-byte gate")
				}
			})
		}
	}
}

func TestStreamedVideoRecoveryKeepsTimelyThumbnail(t *testing.T) {
	valid, invalid := streamVideo(128<<10, true), streamVideo(128<<10, false)
	cover := jpegFixture(t)
	uploading := make(chan struct{})
	var first sync.Once
	var downloads, complete, aborted atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cover" {
			_, _ = w.Write(cover)
			return
		}
		downloads.Add(1)
		w.Header().Set("Content-Length", "131072")
		if r.URL.Path == "/bad" {
			_, _ = w.Write(invalid[:32<<10])
			w.(http.Flusher).Flush()
			select {
			case <-uploading:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(invalid[32<<10:])
		} else {
			_, _ = w.Write(valid)
		}
	}))
	defer cdn.Close()
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		var gotVideo, gotCover []byte
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				aborted.Add(1)
				return
			}
			var prefix []byte
			if part.FormName() == "video_file" {
				prefix = make([]byte, 512)
				if _, err := io.ReadFull(part, prefix); err != nil {
					aborted.Add(1)
					return
				}
				first.Do(func() { close(uploading) })
			}
			data, err := io.ReadAll(part)
			if err != nil {
				aborted.Add(1)
				return
			}
			switch part.FormName() {
			case "video_file":
				gotVideo = append(prefix, data...)
			case "thumbnail_file":
				gotCover = data
			}
		}
		if !bytes.Equal(gotVideo, valid) || !bytes.Equal(gotCover, cover) {
			t.Error("recovery sent wrong media or lost its thumbnail")
		}
		complete.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer telegram.Close()
	dependencies, tt, _, assets := fixture(t)
	cfg := dependencies.Config
	cfg.TelegramBotToken, cfg.TelegramAPIBaseURL = "fixture", telegram.URL
	cfg.ThumbnailWait = core.DefaultConfig().ThumbnailWait
	cfg.DownloadRetries, cfg.RetryDelay, cfg.TelegramUploadConcurrency = 1, 0, 1
	cfg.TempDir = t.TempDir()
	downloader := platform.NewDownloader(cfg, nil)
	defer downloader.Close()
	dependencies.Config, dependencies.Downloader = cfg, downloader
	tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/bad", AlternateURLs: []string{cdn.URL + "/good"}}, 0)}
	thumbnail := assets.Register(core.AssetContext{Kind: "image", UpstreamURL: cdn.URL + "/cover"}, 0)
	tt.extraction.Cover = &thumbnail
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/tiktok/telegram-deliveries", strings.NewReader(`{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	response := httptest.NewRecorder()
	NewHandler(dependencies).ServeHTTP(response, r)
	telegram.Close()
	if response.Code != 200 || downloads.Load() != 2 || complete.Load() != 1 || aborted.Load() != 1 || downloader.MemoryInUse() != 0 {
		t.Fatalf("status=%d downloads=%d complete=%d aborted=%d retained=%d: %s", response.Code, downloads.Load(), complete.Load(), aborted.Load(), downloader.MemoryInUse(), response.Body.String())
	}
}

// streamVideo builds an MP4 whose movie metadata declares a 32x24 track.
func streamVideo(size int, valid bool) []byte {
	return sizedVideo(size, valid, 32, 24)
}

// sizedVideo builds an MP4 whose movie metadata declares a width x height track.
func sizedVideo(size int, valid bool, width, height uint16) []byte {
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:], width)
	binary.BigEndian.PutUint16(entry[26:], height)
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	stsd := mp4Box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, mp4Box("avc1", entry))
	movie := mp4Box("moov", mp4Box("trak", mp4Box("mdia", mp4Box("hdlr", handler), mp4Box("minf", mp4Box("stbl", stsd)))))
	if !valid {
		movie = mp4Box("free", make([]byte, len(movie)-8))
	}
	head := append(mp4Box("ftyp", []byte("isom0000")), movie...)
	payload := make([]byte, size-len(head)-8)
	for i := range payload {
		payload[i] = byte(i * 13)
	}
	return append(head, mp4Box("mdat", payload)...)
}

// The CDN serves only the first part until Telegram starts receiving the
// upload, so success proves the upload overlaps the download.
func TestSingleVideoUploadStreamsWhileDownloading(t *testing.T) {
	for _, outcome := range []string{"delivered", "download-fails", "invalid-mp4"} {
		t.Run(outcome, func(t *testing.T) {
			data := streamVideo(3<<20, outcome != "invalid-mp4")
			uploading := make(chan struct{})
			var signal sync.Once
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"video"`)
				if !strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
					select {
					case <-uploading:
					case <-time.After(5 * time.Second):
						t.Error("Telegram upload did not start before the download finished")
					}
					if outcome == "download-fails" {
						http.Error(w, "gone", http.StatusNotFound)
						return
					}
				}
				http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
			}))
			defer cdn.Close()
			var complete, aborted atomic.Int32
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						aborted.Add(1)
						return
					}
					var prefix []byte
					if part.FormName() == "video_file" {
						prefix = make([]byte, 512)
						if _, err := io.ReadFull(part, prefix); err != nil {
							aborted.Add(1)
							return
						}
						signal.Do(func() { close(uploading) })
					}
					body, err := io.ReadAll(part)
					if err != nil {
						aborted.Add(1)
						return
					}
					if part.FormName() == "video_file" && !bytes.Equal(append(prefix, body...), data) {
						t.Error("streamed upload altered the video")
					}
				}
				complete.Add(1)
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			}))
			defer telegram.Close()
			dependencies, tt, _, assets := fixture(t)
			cfg := dependencies.Config
			cfg.TelegramBotToken = "fixture"
			cfg.TelegramAPIBaseURL = telegram.URL
			cfg.DownloadRetries = 1
			cfg.RetryDelay = 0
			dependencies.Config = cfg
			downloader := platform.NewDownloader(cfg, nil)
			defer downloader.Close()
			dependencies.Downloader = downloader
			tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Platform: "tiktok", Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1}}`, true)
			if outcome == "delivered" {
				if response.Code != 200 || complete.Load() != 1 || aborted.Load() != 0 {
					t.Fatalf("status=%d complete=%d aborted=%d: %s", response.Code, complete.Load(), aborted.Load(), response.Body.String())
				}
			} else {
				// An aborted upload never reaches its closing boundary.
				telegram.Close()
				if response.Code != 502 || complete.Load() != 0 || aborted.Load() != 1 {
					t.Fatalf("status=%d complete=%d aborted=%d: %s", response.Code, complete.Load(), aborted.Load(), response.Body.String())
				}
			}
			if downloader.MemoryInUse() != 0 {
				t.Fatal("delivery kept download RAM")
			}
		})
	}
}

// Extraction metadata advertises 1080x1920, but the served rendition is
// 540x960. Only measured dimensions of complete media reach Telegram; a
// streamed upload starts before they are known and omits them.
func TestVideoDimensionsComeFromDownloadedMedia(t *testing.T) {
	data := sizedVideo(256<<10, true, 540, 960)
	for _, tc := range []struct {
		name, caller string
		streamed     bool
		want         map[string]string
	}{
		{"buffered", "", false, map[string]string{"width": "540", "height": "960"}},
		{"streamed", "", true, map[string]string{}},
		{"caller dimensions", `,"width":720,"height":1280`, false, map[string]string{"width": "720", "height": "1280"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.streamed {
					http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(data))
					return
				}
				// Without a length the download completes before any upload.
				w.Header().Set("Content-Type", "video/mp4")
				for chunk := range slices.Chunk(data, 64<<10) {
					_, _ = w.Write(chunk)
					w.(http.Flusher).Flush()
				}
			}))
			defer cdn.Close()
			got := map[string]string{}
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				for {
					part, err := reader.NextPart()
					if err != nil {
						break
					}
					body, _ := io.ReadAll(part)
					if name := part.FormName(); name == "width" || name == "height" {
						got[name] = string(body)
					} else if name == "video_file" && !bytes.Equal(body, data) {
						t.Error("upload altered the video")
					}
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			}))
			defer telegram.Close()
			dependencies, tt, _, assets := fixture(t)
			dependencies.Config.TelegramBotToken = "fixture"
			dependencies.Config.TelegramAPIBaseURL = telegram.URL
			downloader := platform.NewDownloader(dependencies.Config, nil)
			defer downloader.Close()
			dependencies.Downloader = downloader
			tt.extraction.Width, tt.extraction.Height = core.Ptr(1080), core.Ptr(1920)
			tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Platform: "tiktok", Kind: "video", Filename: "video.mp4", UpstreamURL: cdn.URL + "/video"}, 0)}
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"telegram":{"chat_id":1`+tc.caller+`}}`, true)
			if response.Code != 200 || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("status=%d dimensions=%v: %s", response.Code, got, response.Body.String())
			}
		})
	}
}
