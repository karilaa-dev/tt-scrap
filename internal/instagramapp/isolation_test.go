package instagramapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise actual account transports with two local proxy endpoints. Each route
// rejects the other account's identity, including while requests run together.
func TestConcurrentAccountsKeepAuthorizationDeviceAndProxyIsolated(t *testing.T) {
	var counts [2]atomic.Int32
	configs := make([]accountConfig, 2)
	for n := range configs {
		device, err := newDeviceIdentity("pixel-7-android-15")
		if err != nil {
			t.Fatal(err)
		}
		auth, _, err := cookieAuthorization(cookieFixture(fmt.Sprint(n + 101)))
		if err != nil {
			t.Fatal(err)
		}
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[n].Add(1)
			if r.URL.Host != "instagram.invalid" || r.Header.Get("Authorization") != auth {
				t.Error("metadata crossed account authorization/proxy boundary")
			}
			if r.Header.Get("X-IG-Device-ID") != device.DeviceID || r.Header.Get("X-IG-Family-Device-ID") != device.FamilyID || r.Header.Get("X-IG-Android-ID") != device.AndroidID {
				t.Error("metadata crossed device identity boundary")
			}
			if r.Header.Get("Cookie") != "" {
				t.Error("metadata unexpectedly inherited cookies")
			}
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: fmt.Sprintf("route-%d", n), Path: "/"})
			fmt.Fprint(w, `{"status":"ok","items":[]}`)
		}))
		t.Cleanup(proxy.Close)
		configs[n] = accountConfig{ID: fmt.Sprintf("account-%d", n), Authorization: auth, Device: device, ProxyURL: &proxy.URL}
	}
	manifest, err := json.Marshal(map[string]any{"accounts": configs})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(Options{AccountsFile: path, PerAccountConcurrency: 2, MaxQueuedRequests: 64, MaxAttempts: 1, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.apiOrigin = "http://instagram.invalid"
	if p.accounts[0].client.Transport == p.accounts[1].client.Transport {
		t.Fatal("accounts share a connection pool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			if _, err := p.Detail(ctx, "123"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if counts[0].Load() == 0 || counts[1].Load() == 0 || counts[0].Load()+counts[1].Load() != 24 {
		t.Fatalf("expected both isolated routes to serve requests, got %d/%d", counts[0].Load(), counts[1].Load())
	}
}

func TestLoginSessionsKeepCookieJarsAndHeadersIsolated(t *testing.T) {
	endpoint, _ := url.Parse("https://i.instagram.com/")
	baseJar, _ := cookiejar.New(nil)
	baseJar.SetCookies(endpoint, []*http.Cookie{{Name: "sessionid", Value: "base"}})
	base := &http.Client{Jar: baseJar}
	first, err := newLoginSession(base, endpoint.String(), "pixel-7-android-15")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newLoginSession(base, endpoint.String(), "pixel-7-android-15")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.client.Jar.Cookies(endpoint)) != 0 || len(second.client.Jar.Cookies(endpoint)) != 0 {
		t.Fatal("fresh login inherited another session's cookies")
	}
	first.client.Jar.SetCookies(endpoint, []*http.Cookie{{Name: "sessionid", Value: "first"}})
	second.client.Jar.SetCookies(endpoint, []*http.Cookie{{Name: "sessionid", Value: "second"}})
	for n, item := range []struct {
		client *http.Client
		want   string
	}{{base, "base"}, {first.client, "first"}, {second.client, "second"}} {
		cookies := item.client.Jar.Cookies(endpoint)
		if len(cookies) != 1 || cookies[0].Value != item.want {
			t.Errorf("cookie jar %d contaminated", n)
		}
	}
	if first.guid == second.guid || first.phone == second.phone || first.device == second.device {
		t.Fatal("independent logins reused device identifiers")
	}
	first.mid = "first-mid"
	first.usdidHeader = "first-proof"
	firstHeaders := first.headers()
	firstHeaders.Set("Authorization", "first-auth")
	secondHeaders := second.headers()
	if secondHeaders.Get("X-Mid") != "" || secondHeaders.Get("X-Meta-Usdid") != "" || secondHeaders.Get("Authorization") != "" || secondHeaders.Get("X-IG-Device-ID") != second.guid {
		t.Fatal("login headers crossed account boundary")
	}
}

func TestRenewingOneAccountDoesNotReplaceAnotherSession(t *testing.T) {
	p := newTestPool(t, 2, 1)
	for n, a := range p.accounts {
		var err error
		a.device, err = newDeviceIdentity("pixel-7-android-15")
		if err != nil {
			t.Fatal(err)
		}
		a.login = &passwordCredentials{Username: fmt.Sprintf("user-%d", n), Password: fmt.Sprintf("password-%d", n), UserID: fmt.Sprint(n + 101)}
	}
	otherAuth, otherDevice := p.session(p.accounts[1])
	otherIdentity := *otherDevice
	var calls atomic.Int32
	p.login = func(_ context.Context, client *http.Client, _ string, c Credentials) (loginResult, error) {
		calls.Add(1)
		if client != p.accounts[0].client || c.Username != "user-0" || c.Password != "password-0" || c.device != p.accounts[0].device {
			t.Error("renewal used another account's client, device or credentials")
		}
		return loginResult{Authorization: "Bearer IGT:2:renewed-a", UserID: "101", Username: "user-0", Device: c.device}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer IGT:2:a":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"login_required"}`)
		case "Bearer IGT:2:renewed-a":
			fmt.Fprint(w, `{"status":"ok","items":[]}`)
		case otherAuth:
			if r.Header.Get("X-IG-Device-ID") != otherIdentity.DeviceID {
				t.Error("second account's request used renewed account's device")
			}
			fmt.Fprint(w, `{"status":"ok","items":[]}`)
		default:
			t.Error("unknown account authorization")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	first, err := p.Detail(context.Background(), "123")
	if err != nil || first.IdentityID != "instagram:a" {
		t.Fatalf("first account renewal failed: %v", err)
	}
	second, err := p.Detail(context.Background(), "456")
	if err != nil || second.IdentityID != "instagram:b" {
		t.Fatalf("second account request failed: %v", err)
	}
	auth, device := p.session(p.accounts[1])
	if calls.Load() != 1 || auth != otherAuth || *device != otherIdentity {
		t.Fatal("renewing first account changed second account's session")
	}
}
