package instagramapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func newTestPool(t *testing.T, n int, perAccount int) *Pool {
	t.Helper()
	accounts := make([]map[string]any, n)
	for index := range accounts {
		id := string(rune('a' + index))
		accounts[index] = map[string]any{"id": id, "authorization": "IGT:2:" + id}
	}
	data, _ := json.Marshal(map[string]any{"accounts": accounts})
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(Options{AccountsFile: path, PerAccountConcurrency: perAccount, MaxQueuedRequests: 1, MaxAttempts: 3, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func TestAccountCapacityFairnessAndQueue(t *testing.T) {
	p := newTestPool(t, 3, 2)
	var releases []func()
	for _, want := range []string{"instagram:a", "instagram:b", "instagram:c", "instagram:a", "instagram:b", "instagram:c"} {
		a, release, err := p.acquire(context.Background(), "", nil)
		if err != nil || a.id != want {
			t.Fatalf("acquire = %v %v, want %s", a, err, want)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, release, err := p.acquire(ctx, "", nil)
		if release != nil {
			release()
		}
		waiting <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		queued := p.queued
		p.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	_, _, err := p.acquire(context.Background(), "", nil)
	assertCode(t, err, "service_busy")
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
	releases[1]()
	a, release, err := p.acquire(context.Background(), "", nil)
	if err != nil || a.id != "instagram:b" {
		t.Fatalf("least busy = %v %v", a, err)
	}
	release()
	for _, release := range releases {
		release()
		release()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.busy != 0 {
			t.Fatal("leaked slot")
		}
	}
}

func TestDetailProtocolNumbersNDJSONAndNoRedirect(t *testing.T) {
	p := newTestPool(t, 1, 1)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer IGT:2:a" || r.Header.Get("X-IG-App-ID") != AppID || r.Header.Get("User-Agent") != UserAgent {
			t.Error("missing protocol headers")
		}
		if r.URL.Path != "/api/v1/media/3902057773219159336/info/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, "{\"status\":\"ok\",\"items\":[{\"pk\":3902057773219159336}]}\n{\"items\":[{\"id\":\"second\"}]}")
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	result, err := p.Detail(context.Background(), "3902057773219159336")
	if err != nil {
		t.Fatal(err)
	}
	items := result.Response["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["pk"] != json.Number("3902057773219159336") || result.IdentityID != "instagram:a" {
		t.Fatalf("bad metadata: %#v", result)
	}
	redirected := atomic.Bool{}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, http.StatusFound) }))
	defer redirect.Close()
	p.apiOrigin = redirect.URL
	_, err = p.Detail(context.Background(), "123")
	if err == nil || redirected.Load() {
		t.Fatal("metadata redirect followed")
	}
}

func TestFailureClassificationAndRetryOtherAccount(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		disabled   bool
	}{
		{"login", "{\"message\":\"login_required\",\"status\":\"fail\"}", 200, true},
		{"challenge", "{\"challenge_required\":true}", 200, true},
		{"forbidden_challenge", `{"message":"challenge_required"}`, 403, true},
		{"rate", "", 429, false},
		{"rate_with_challenge_body", `{"challenge_required":true}`, 429, false},
		{"forbidden_feedback", `{"message":"feedback_required"}`, 403, false},
		{"feedback", "{\"message\":\"feedback_required\",\"status\":\"fail\"}", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, 2, 1)
			var first, second atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer IGT:2:a" {
					first.Add(1)
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
					return
				}
				second.Add(1)
				io.WriteString(w, `{"items":[{"id":"123"}]}`)
			}))
			defer server.Close()
			p.apiOrigin = server.URL
			result, err := p.Detail(context.Background(), "123")
			if err != nil || result.IdentityID != "instagram:b" || first.Load() != 1 || second.Load() != 1 {
				t.Fatalf("retry = %#v %v, counts %d/%d", result, err, first.Load(), second.Load())
			}
			p.mu.Lock()
			disabled := p.accounts[0].disabled
			cooldown := p.accounts[0].cooldown
			p.mu.Unlock()
			if disabled != tc.disabled {
				t.Fatal("wrong disabled state")
			}
			if !disabled && !cooldown.After(time.Now()) {
				t.Fatal("missing cooldown")
			}
			_, err = p.Detail(context.Background(), "123")
			if err != nil {
				t.Fatal(err)
			}
			if first.Load() != 1 {
				t.Fatal("retried unavailable account")
			}
		})
	}
}

func TestPinnedCDNRouteAndCredentialStripping(t *testing.T) {
	p := newTestPool(t, 2, 1)
	p.accounts[1].device, _ = newDeviceIdentity("galaxy-s23-android-14")
	var seen atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		for _, key := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-IG-App-ID", "X-IG-Device-ID", "X-IG-Family-Device-ID", "X-IG-Android-ID", "IG-U-RUR"} {
			if r.Header.Get(key) != "" {
				t.Errorf("credential header %s leaked", key)
			}
		}
		if r.Header.Get("User-Agent") != p.accounts[1].device.userAgent() {
			t.Error("CDN request changed device profile")
		}
		io.WriteString(w, "media")
	}))
	defer server.Close()
	p.accounts[0].mediaClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("wrong account route")
		return nil, errors.New("wrong route")
	})
	p.accounts[1].mediaClient.Transport = server.Client().Transport
	// Even a challenged metadata session can download its already signed CDN URLs.
	p.accounts[1].disabled = true
	err := p.WithMedia(context.Background(), "instagram:b", func(open core.OpenAppMedia) error {
		headers := http.Header{"Authorization": {"secret"}, "Cookie": {"secret"}, "X-IG-App-ID": {"secret"}, "Proxy-Authorization": {"secret"}, "IG-U-RUR": {"secret"}}
		p.accounts[1].device.headers(headers)
		headers.Set("User-Agent", "Mozilla/5.0") // The downloader supplies a browser UA before calling this backend.
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := open(context.Background(), server.URL, headers)
				if err != nil {
					t.Error(err)
					return
				}
				res.Body.Close()
			}()
		}
		wg.Wait()
		if headers.Get("Authorization") != "secret" {
			t.Error("mutated caller headers")
		}
		return nil
	})
	if err != nil || seen.Load() != 3 {
		t.Fatalf("media: %v count=%d", err, seen.Load())
	}
	assertCode(t, p.WithMedia(context.Background(), "instagram:unknown", func(core.OpenAppMedia) error { return nil }), "asset_not_found_or_expired")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfiguredProxyUsedForMetadataAndCDN(t *testing.T) {
	// An HTTP metadata origin exercises the explicit proxy; HTTPS CDN uses CONNECT
	// through exactly the same transport. This avoids depending on external egress.
	var metadata, connect atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connect.Add(1)
			w.WriteHeader(502)
			return
		}
		metadata.Add(1)
		io.WriteString(w, `{"items":[{"id":"123"}]}`)
	}))
	defer proxy.Close()
	path := filepath.Join(t.TempDir(), "accounts.json")
	data, _ := json.Marshal(map[string]any{"accounts": []map[string]string{{"id": "proxy", "authorization": "IGT:2:proxy", "proxy_url": proxy.URL}}})
	os.WriteFile(path, data, 0600)
	p, err := Open(Options{AccountsFile: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.apiOrigin = "http://instagram.invalid"
	result, err := p.Detail(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	err = p.WithMedia(context.Background(), result.IdentityID, func(open core.OpenAppMedia) error {
		_, err := open(context.Background(), "https://cdn.invalid/video.mp4", nil)
		return err
	})
	if err == nil || metadata.Load() != 1 || connect.Load() != 1 {
		t.Fatalf("proxy counts %d %d err=%v", metadata.Load(), connect.Load(), err)
	}
	if strings.Contains(err.Error(), proxy.URL) {
		t.Fatal("proxy leaked in error")
	}
}

func TestUnavailableReadinessAndCancellation(t *testing.T) {
	p, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Count() != 0 || p.Ready() {
		t.Fatal("empty pool ready")
	}
	_, err = p.Detail(context.Background(), "123")
	assertCode(t, err, "service_not_configured")
	p = newTestPool(t, 1, 1)
	if !p.Ready() {
		t.Fatal("loaded pool unready")
	}
	p.accounts[0].cooldown = time.Now().Add(time.Minute)
	if !p.Ready() {
		t.Fatal("cooldown affects readiness")
	}
	p.accounts[0].disabled = true
	if p.Ready() {
		t.Fatal("disabled pool ready")
	}
	p.Close()
	if p.Ready() {
		t.Fatal("closed pool ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Detail(ctx, "123")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnavailableMediaIsNotRetriedOrLeaked(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 410} {
		for _, body := range []string{
			`{"status":"fail","message":"Media not found or unavailable","debug":"private-upstream-detail"}`,
			`{"status":"fail","error_type":"media_not_found"}`,
			`{"status":"fail","message":"This media has been deleted"}`,
		} {
			p := newTestPool(t, 2, 1)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			p.apiOrigin = server.URL
			_, err := p.Detail(context.Background(), "123")
			server.Close()
			assertCode(t, err, "content_deleted")
			if calls.Load() != 1 {
				t.Fatal("retried missing media across accounts")
			}
			if strings.Contains(err.Error(), "private-upstream-detail") {
				t.Fatal("upstream text leaked")
			}
			if !p.Ready() {
				t.Fatal("missing media disabled account")
			}
		}
	}
}
