package platform

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWebSessionIsOnlyUsedAfterContentRestriction(t *testing.T) {
	for _, tc := range []struct {
		name, detail  string
		status, calls int
		wantError     bool
	}{
		{"public", `"itemInfo":{"itemStruct":{"id":"1234567890123456789"}}`, 200, 1, false},
		{"age restricted", `"statusCode":1,"statusMsg":"Age restricted"`, 200, 2, false},
		{"private", `"statusCode":10216`, 200, 2, false},
		{"HTTP login required", `"statusCode":1,"statusMsg":"Login required"`, 401, 2, false},
		{"region", `"statusCode":10204`, 200, 2, false},
		{"rate limit", `"statusCode":1`, 429, 1, true},
		{"challenge", `"statusCode":1`, 403, 1, true},
		{"deleted", `"statusCode":10202`, 200, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			service := resolutionFixture(t, "web", func(w http.ResponseWriter, r *http.Request) {
				calls++
				cookie, err := r.Cookie("sessionid")
				if calls == 1 {
					if err == nil {
						t.Error("web request used account before restriction")
					}
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{%s}}}</script>`, tc.detail)
					return
				}
				if err != nil || cookie.Value != "web-login" {
					t.Error("web fallback missing account cookie")
				}
				fmt.Fprint(w, `<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{"statusCode":0,"itemInfo":{"itemStruct":{"id":"1234567890123456789"}}}}}</script>`)
			})
			service.cfg.TikTokSessionID = "web-login"
			_, asset, err := service.fetchWeb(context.Background(), "https://www.tiktok.com/@creator/video/1234567890123456789", "1234567890123456789")
			if (err != nil) != tc.wantError || calls != tc.calls {
				t.Fatalf("calls=%d want=%d error=%v", calls, tc.calls, err)
			}
			if tc.name == "public" {
				for _, cookie := range asset.CookieJar.Cookies(&url.URL{Scheme: "https", Host: "www.tiktok.com", Path: "/"}) {
					if cookie.Name == "sessionid" {
						t.Error("public media inherited account credentials")
					}
				}
			}
		})
	}
}

func TestWebHTTP401DoesNotAuthenticateChallenges(t *testing.T) {
	for _, tc := range []struct{ name, body, blocked string }{
		{"block header", "Unauthorized", "1"},
		{"captcha", "Complete the CAPTCHA", ""},
		{"challenge text", "Challenge required", ""},
		{"challenge markup", `<div id="cs" class="encoded"></div>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			service := resolutionFixture(t, "web", func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Cookie") != "" {
					t.Error("bot challenge received account credentials")
				}
				w.Header().Set("tt_block", tc.blocked)
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, tc.body)
			})
			service.cfg.TikTokSessionID = "web-login"
			_, _, err := service.fetchWeb(context.Background(), "https://www.tiktok.com/@creator/video/1234567890123456789", "1234567890123456789")
			if err == nil || calls != 1 {
				t.Fatal("bot challenge triggered account fallback", err, calls)
			}
		})
	}
}

func TestRestrictedWebMediaKeepsCookieDomainAndSecureScope(t *testing.T) {
	const postURL = "https://www.tiktok.com/@creator/video/1234567890123456789"
	seen := map[string]bool{}
	service := resolutionFixture(t, "web", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sessionid")
		if r.URL.Path == "/@creator/video/1234567890123456789" {
			if err != nil {
				fmt.Fprint(w, `<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{"statusCode":10216}}}</script>`)
				return
			}
			fmt.Fprint(w, `<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__">{"__DEFAULT_SCOPE__":{"webapp.video-detail":{"itemInfo":{"itemStruct":{"id":"1234567890123456789"}}}}}</script>`)
			return
		}
		seen[r.Host+r.URL.Path] = true
		if r.Host == "www.tiktok.com" {
			if err != nil || cookie.Value != "web-login" {
				t.Error("trusted HTTPS media lost session")
			}
		} else if err == nil {
			t.Error("account cookie leaked outside TikTok", r.Host)
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://cdn.example.org/redirected", http.StatusFound)
			return
		}
		fmt.Fprint(w, "media bytes")
	})
	service.cfg.TikTokSessionID = "web-login"
	_, asset, err := service.fetchWeb(context.Background(), postURL, "1234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	downloader := NewDownloader(service.cfg, nil, nil)
	downloader.transports.items[""] = service.transports.items[""].Clone()
	t.Cleanup(downloader.Close)
	for _, address := range []string{"https://www.tiktok.com/media", "https://cdn.example.org/direct", "https://tiktok.com.example.org/spoofed", "https://www.tiktok.com/redirect"} {
		response, err := downloader.open(context.Background(), asset, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "media bytes" {
			t.Fatal("media request failed", err)
		}
	}
	if len(seen) != 5 {
		t.Fatal("did not exercise each media destination", seen)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("sessionid"); err == nil {
			t.Error("account cookie leaked over HTTP")
		}
		fmt.Fprint(w, "media bytes")
	}))
	defer plain.Close()
	plainTransport := plain.Client().Transport.(*http.Transport).Clone()
	plainTransport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, plain.Listener.Addr().String())
	}
	downloader.transports.items[""].CloseIdleConnections()
	downloader.transports.items[""] = plainTransport
	response, err := downloader.open(context.Background(), asset, "http://www.tiktok.com/media", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
