package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type documentPart struct {
	field, filename, mime string
	data                  []byte
}

// documentTelegram records each request's form fields and file parts.
func documentTelegram(t *testing.T, response string) (*httptest.Server, func() (string, map[string]string, []documentPart)) {
	t.Helper()
	var mu sync.Mutex
	var method string
	values := map[string]string{}
	var parts []documentPart
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		method = strings.TrimPrefix(r.URL.Path, "/botfixture/")
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
			if part.FileName() == "" {
				values[part.FormName()] = string(data)
				continue
			}
			parts = append(parts, documentPart{part.FormName(), part.FileName(), part.Header.Get("Content-Type"), data})
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	return server, func() (string, map[string]string, []documentPart) {
		mu.Lock()
		defer mu.Unlock()
		return method, values, parts
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertJPEGDocument checks that a WebP document was replaced by a full-size JPEG.
func assertJPEGDocument(t *testing.T, part documentPart, filename string, width, height int) {
	t.Helper()
	if part.filename != filename || part.mime != "image/jpeg" || !bytes.HasPrefix(part.data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("WebP document was not converted: %s %s %.12q", part.filename, part.mime, part.data)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(part.data))
	if err != nil || config.Width != width || config.Height != height {
		t.Fatalf("converted document %v %v", config, err)
	}
}

// Telegram rejects WebP documents in media groups, so albums send them as JPEG
// and keep other originals unchanged and in order.
func TestDocumentAlbumConvertsWebPToJPEG(t *testing.T) {
	webp := readFixture(t, "../media/testdata/static.webp")
	heic := readFixture(t, "../media/testdata/sample.heic")
	original := jpegFixture(t)
	files := []struct {
		name string
		file fakeFile
	}{
		{"first.webp", fakeFile{webp, "image/webp"}},
		{"second.jpg", fakeFile{original, "image/jpeg"}},
		{"third.heic", fakeFile{heic, "image/heic"}},
		{"fourth.webp", fakeFile{webp, "image/webp"}},
	}
	for _, platform := range []string{"tiktok", "instagram"} {
		t.Run(platform, func(t *testing.T) {
			dependencies, tt, downloader, assets := fixture(t)
			downloader.files = map[string]fakeFile{}
			instagram := &fakeInstagram{extraction: core.InstagramExtraction{Platform: "instagram", ContentType: "carousel"}}
			tt.extraction.ContentType = "slideshow"
			for i, file := range files {
				url := fmt.Sprint("https://cdn.example/", i)
				downloader.files[url] = file.file
				descriptor := assets.Register(core.AssetContext{Platform: platform, Filename: file.name, Kind: "image", UpstreamURL: url}, i)
				tt.extraction.Media = append(tt.extraction.Media, descriptor)
				instagram.extraction.Media = append(instagram.extraction.Media, core.InstagramMedia{MediaType: "image", Asset: descriptor})
			}
			dependencies.Instagram = instagram
			telegram, received := documentTelegram(t, `{"ok":true,"result":[]}`)
			dependencies.Config.TelegramBotToken = "fixture"
			dependencies.Config.TelegramAPIBaseURL = telegram.URL
			body := fmt.Sprintf(`{"source":{"url":"https://www.%s.com/p/x/"},"delivery":"document","telegram":{"chat_id":1}}`, platform)
			response := request(NewHandler(dependencies), "POST", "/v1/"+platform+"/telegram-deliveries", body, true)
			if response.Code != 200 {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
			method, values, parts := received()
			var group []map[string]any
			if err := json.Unmarshal([]byte(values["media"]), &group); err != nil || method != "sendMediaGroup" || len(group) != 4 || len(parts) != 4 {
				t.Fatalf("%s %v %v parts %d", method, group, err, len(parts))
			}
			for i, item := range group {
				if item["type"] != "document" || item["media"] != "attach://"+parts[i].field || item["disable_content_type_detection"] != true {
					t.Fatalf("item %d %#v", i, item)
				}
			}
			assertJPEGDocument(t, parts[0], "first.jpg", 400, 301)
			if parts[1].filename != "second.jpg" || parts[1].mime != "image/jpeg" || !bytes.Equal(parts[1].data, original) {
				t.Fatal("JPEG document altered", parts[1].filename, parts[1].mime)
			}
			if parts[2].filename != "third.heic" || parts[2].mime != "image/heic" || !bytes.Equal(parts[2].data, heic) {
				t.Fatal("HEIC document altered", parts[2].filename, parts[2].mime)
			}
			assertJPEGDocument(t, parts[3], "fourth.jpg", 400, 301)
			if downloader.leaked() != 0 {
				t.Fatal("download buffers leaked")
			}
		})
	}
}

// A single document streams while it downloads; WebP must not start streaming
// its original bytes before conversion.
func TestSingleDocumentConvertsWebPAndStreamsOthers(t *testing.T) {
	webp := readFixture(t, "../media/testdata/static.webp")
	original := jpegFixture(t)
	for _, test := range []struct {
		name     string
		file     fakeFile
		filename string
	}{
		{"photo.webp", fakeFile{webp, "image/webp"}, "photo.jpg"},
		{"photo.jpg", fakeFile{original, "image/jpeg"}, "photo.jpg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies, tt, downloader, assets := fixture(t)
			downloader.files = map[string]fakeFile{"https://cdn.example/photo": test.file}
			tt.extraction.ContentType = "slideshow"
			tt.extraction.Media = []core.AssetDescriptor{assets.Register(core.AssetContext{Platform: "tiktok", Filename: test.name, Kind: "image", UpstreamURL: "https://cdn.example/photo"}, 0)}
			telegram, received := documentTelegram(t, `{"ok":true,"result":{"message_id":1}}`)
			dependencies.Config.TelegramBotToken = "fixture"
			dependencies.Config.TelegramAPIBaseURL = telegram.URL
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", `{"source":{"extraction_id":"saved"},"delivery":"document","telegram":{"chat_id":1}}`, true)
			if response.Code != 200 || response.Header().Get("X-Telegram-Method") != "sendDocument" {
				t.Fatalf("%d %v %s", response.Code, response.Header(), response.Body.String())
			}
			method, values, parts := received()
			if method != "sendDocument" || len(parts) != 1 || parts[0].field != "document_file" || values["document"] != "attach://document_file" || values["disable_content_type_detection"] != "true" {
				t.Fatalf("%s %v %d", method, values, len(parts))
			}
			if test.file.mime == "image/webp" {
				assertJPEGDocument(t, parts[0], test.filename, 400, 301)
			} else if parts[0].filename != test.filename || parts[0].mime != test.file.mime || !bytes.Equal(parts[0].data, test.file.data) {
				t.Fatal("original document altered", parts[0].filename, parts[0].mime)
			}
			if downloader.leaked() != 0 {
				t.Fatal("download buffers leaked")
			}
		})
	}
}
