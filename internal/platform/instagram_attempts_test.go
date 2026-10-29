package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type attemptFixture struct {
	methods  []bool
	outcomes []bool
}

func (f *attemptFixture) Attempt(_ context.Context, _ string, legacy bool, run func() (core.InstagramExtraction, error)) (core.InstagramExtraction, error) {
	f.methods = append(f.methods, legacy)
	result, err := run()
	f.outcomes = append(f.outcomes, err == nil)
	return result, err
}
func TestInstagramAttemptTracking(t *testing.T) {
	for _, mode := range []string{"app", "fallback", "legacy", "legacy_failure"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig()
			cfg.InstagramMethod = "app"
			cfg.InstagramLegacyFallback = mode == "fallback"
			cfg.RapidAPIKey = "fixture"
			cfg.InstagramMaxAttempts = 1
			if mode == "legacy" || mode == "legacy_failure" {
				cfg.InstagramMethod = "legacy"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "legacy_failure" {
					w.WriteHeader(500)
					return
				}
				fmt.Fprint(w, `{"media":[{"type":"video","url":"https://cdn.example/video.mp4"}]}`)
			}))
			defer server.Close()
			cfg.RapidAPIBaseURL = server.URL
			backend := instagramFixtureBackend{detail: func(context.Context, string) (core.AppDetail, error) {
				if mode == "fallback" {
					return core.AppDetail{}, core.Upstream("fixture failure")
				}
				return core.AppDetail{Response: instagramFixture(t, instagramVideoFixture), IdentityID: "instagram:fixture"}, nil
			}}
			service := NewInstagram(cfg, core.NewAssets(cfg.CacheTTL, 100), backend)
			defer service.Close()
			recorder := &attemptFixture{}
			service.Attempts = recorder
			_, err := service.ExtractURL(context.Background(), "https://www.instagram.com/reel/DYm5N7px50o/", false)
			if (err != nil) != (mode == "legacy_failure") {
				t.Fatal(err)
			}
			switch mode {
			case "app":
				if len(recorder.methods) != 1 || recorder.methods[0] {
					t.Fatal(recorder)
				}
			case "fallback":
				if len(recorder.methods) != 2 || recorder.methods[0] || !recorder.methods[1] || recorder.outcomes[0] || !recorder.outcomes[1] {
					t.Fatal(recorder)
				}
			default:
				if len(recorder.methods) != 1 || !recorder.methods[0] {
					t.Fatal(recorder)
				}
			}
			if err == nil {
				before := len(recorder.methods)
				service.ExtractURL(context.Background(), "https://www.instagram.com/reel/DYm5N7px50o/", false)
				if len(recorder.methods) != before {
					t.Fatal("cache hit counted as upstream attempt")
				}
			}
		})
	}
}
