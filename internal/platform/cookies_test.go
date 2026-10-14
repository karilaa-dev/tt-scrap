package platform

import (
	"context"
	"fmt"
	"net/http"
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
			if tc.name == "public" && asset.Cookies["sessionid"] != "" {
				t.Error("public media inherited account credentials")
			}
		})
	}
}
