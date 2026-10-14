package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func resolutionFixture(t *testing.T, handler http.HandlerFunc) *TikTok {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	cfg := testConfig()
	cfg.ResolveRetries = 1
	service := NewTikTok(cfg, core.NewAssets(time.Minute, 100), nil, nil)
	service.transports.items[""] = transport
	t.Cleanup(service.Close)
	return service
}

func TestAppShortLinkValidatesRedirectBeforeAcceptingID(t *testing.T) {
	for _, target := range []string{
		"https://example.com/@creator/video/1234567890123456789",
		"https://www.tiktok.com.example.com/@creator/video/1234567890123456789",
		"http://www.tiktok.com/@creator/video/1234567890123456789",
		"https://user@www.tiktok.com/@creator/video/1234567890123456789",
	} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, target, http.StatusFound)
			})
			if _, err := service.ResolveURL(context.Background(), "https://vm.tiktok.com/short"); err == nil || calls.Load() != 1 {
				t.Fatalf("unsafe redirect: error = %v, requests = %d", err, calls.Load())
			}
		})
	}
}

func TestAppShortLinkFollowsQueryOnlyIDAndRelativeRedirect(t *testing.T) {
	var calls atomic.Int32
	service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/short":
			http.Redirect(w, r, "https://www.tiktok.com/hop?next=/@creator/video/999", http.StatusFound)
		case "/hop":
			http.Redirect(w, r, "/@creator/video/1234567890123456789", http.StatusTemporaryRedirect)
		default:
			t.Error("fetched the post page")
		}
	})
	result, err := service.ResolveURL(context.Background(), "https://vm.tiktok.com/short")
	if err != nil || result.SourceID != "1234567890123456789" || result.ResolvedURL != "https://www.tiktok.com/@creator/video/1234567890123456789" || calls.Load() != 2 {
		t.Fatalf("resolution = %+v, error = %v, requests = %d", result, err, calls.Load())
	}
}

func TestShortLinkDoesNotTreatQueryOrFragmentAsPostID(t *testing.T) {
	for _, suffix := range []string{"?next=/@creator/video/999", "#/@creator/video/999"} {
		t.Run(suffix, func(t *testing.T) {
			var calls atomic.Int32
			service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, "https://www.tiktok.com/@creator/video/1234567890123456789", http.StatusFound)
			})
			result, err := service.ResolveURL(context.Background(), "https://vm.tiktok.com/short"+suffix)
			if err != nil || result.SourceID != "1234567890123456789" || calls.Load() != 1 {
				t.Fatalf("resolution = %+v, error = %v, requests = %d", result, err, calls.Load())
			}
		})
	}
}

func TestAppShortLinkKeepsRedirectLimitAndErrorStatuses(t *testing.T) {
	for _, status := range []int{302, 404, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/loop")
				w.WriteHeader(status)
			})
			_, err := service.ResolveURL(context.Background(), "https://vm.tiktok.com/short")
			var failure *core.Error
			if !errors.As(err, &failure) {
				t.Fatalf("missing error for HTTP %d: %v", status, err)
			}
			wantCalls := int32(1)
			if status == 302 {
				wantCalls = 10
			}
			if calls.Load() != wantCalls {
				t.Fatalf("requests = %d, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestAppShortLinkCoalescesConcurrentResolutions(t *testing.T) {
	var calls atomic.Int32
	service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://www.tiktok.com/@creator/video/1234567890123456789", http.StatusFound)
	})
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			result, err := service.ResolveURL(context.Background(), "https://vm.tiktok.com/short")
			if err != nil || result.SourceID != "1234567890123456789" {
				t.Errorf("resolution = %+v, error = %v", result, err)
			}
		})
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent resolutions made %d requests, want 1", calls.Load())
	}
}

func TestAppShortLinkStopsBeforePostPage(t *testing.T) {
	for _, kind := range []string{"video", "photo"} {
		t.Run(kind, func(t *testing.T) {
			var redirects, pages atomic.Int32
			canonical := "https://www.tiktok.com/@creator/" + kind + "/1234567890123456789?share=1"
			service := resolutionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/short":
					redirects.Add(1)
					http.Redirect(w, r, "https://www.tiktok.com/hop", http.StatusFound)
				case "/hop":
					redirects.Add(1)
					http.Redirect(w, r, canonical, http.StatusFound)
				default:
					pages.Add(1)
					w.Write([]byte("unnecessary post page"))
				}
			})
			raw := "https://vm.tiktok.com/short"
			for range 2 {
				result, err := service.ResolveURL(context.Background(), raw)
				if err != nil || result.SourceID != "1234567890123456789" || result.SourceURL != raw || result.ResolvedURL != canonical {
					t.Fatalf("resolution = %+v, error = %v", result, err)
				}
			}
			wantPages := int32(0)
			if redirects.Load() != 2 || pages.Load() != wantPages {
				t.Fatalf("redirect requests = %d; post-page requests = %d, want %d", redirects.Load(), pages.Load(), wantPages)
			}
		})
	}
}
