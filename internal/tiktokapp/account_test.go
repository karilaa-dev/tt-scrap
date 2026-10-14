package tiktokapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAccountCookiesAreRequestScoped(t *testing.T) {
	ctx := context.Background()
	session := newTestSession(t)
	seed := seedResponse(t)
	details := 0
	client := NewClient(session, doerFunc(func(r *http.Request) (*http.Response, error) {
		wantAccount := r.URL.Path == "/aweme/v1/aweme/detail/" && details == 1
		for _, name := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
			cookie, err := r.Cookie(name)
			if wantAccount && (err != nil || cookie.Value != "fixture-login") {
				t.Error("restricted detail is missing account cookie", name)
			}
			if !wantAccount && err == nil {
				t.Error("account cookie leaked to anonymous request", r.URL.Path, name)
			}
		}
		if _, err := r.Cookie("authenticated_only"); err == nil {
			t.Error("authenticated response contaminated device cookies")
		}
		switch r.URL.Path {
		case "/service/2/device_register/":
			return testResponse(200, []byte(`{"device_id_str":"7500000000000000001","install_id_str":"7500000000000000002"}`), nil), nil
		case "/ms/get_seed":
			return testResponse(200, seed, nil), nil
		case "/aweme/v1/aweme/detail/":
			details++
			if r.Header.Get("X-Argus") == "" {
				t.Error("detail request lost its signature")
			}
			header := make(http.Header)
			if wantAccount {
				header.Add("Set-Cookie", "sessionid=rotated; Domain=.tiktokv.com; Path=/; Secure")
				header.Add("Set-Cookie", "authenticated_only=private; Domain=.tiktokv.com; Path=/; Secure")
			}
			return testResponse(200, []byte(`{"aweme_detail":{"aweme_id":"1234567890123456789"}}`), header), nil
		case "/aweme/v1/play/":
			return testResponse(200, []byte("media bytes"), nil), nil
		default:
			return testResponse(200, []byte(`{"status_code":0}`), nil), nil
		}
	}))
	if _, err := client.Detail(ctx, "1234567890123456789"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DetailWithAccount(ctx, "1234567890123456789", Account{SessionID: "fixture-login"}); err != nil {
		t.Fatal(err)
	}
	client.SyncCookies()
	if strings.Contains(session.Cookies, "rotated") || strings.Contains(session.Cookies, "authenticated_only") || strings.Contains(session.Cookies, "fixture-login") {
		t.Fatal("account credentials entered persisted device session")
	}
	if _, err := client.Detail(ctx, "1234567890123456789"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Feed(ctx); err != nil {
		t.Fatal(err)
	}
	response, err := client.MediaRequest(ctx, "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestAccountCookieScopeAndEmptySession(t *testing.T) {
	for _, target := range []string{"https://cdn.example.org/video", "https://api19-va.tiktokv.com/unrelated", "https://tiktokv.com.example.org/aweme/detail"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if _, err := r.Cookie("sid_tt"); err != nil {
						t.Error("missing account on initial request")
					}
					return testResponse(302, nil, http.Header{"Location": {target}, "Set-Cookie": {"sessionid=rotated; Path=/; Secure"}}), nil
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("account leaked through redirect")
				}
				return testResponse(200, []byte(`{"aweme_detail":{"aweme_id":"1234567890123456789"}}`), nil), nil
			}))
			if _, err := client.DetailWithAccount(context.Background(), "1234567890123456789", Account{SessionID: "login"}); err != nil || calls != 2 {
				t.Fatal("redirect failed", err)
			}
		})
	}
	client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" {
			t.Error("empty account sent cookies")
		}
		return testResponse(200, []byte(`{"aweme_detail":{"aweme_id":"1234567890123456789"}}`), nil), nil
	}))
	_, err := client.DetailWithAccount(context.Background(), "1234567890123456789", Account{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContentRestrictionClassification(t *testing.T) {
	for _, message := range []string{"This video is age-restricted", "AGE_RESTRICTED", "age verification required", "Login required", "Please log in to watch this video", "Authentication is required", "This content is restricted", "Not available in your region"} {
		t.Run(message, func(t *testing.T) {
			failure := ClassifyFailure(200, 1, message, false, 0)
			if !IsContentRestricted(failure) || failure.Recovery != RecoveryNone {
				t.Fatal("restriction not recognized", failure.Code)
			}
		})
	}
	for _, tc := range []struct {
		status  int
		message string
		blocked bool
	}{
		{403, "challenge required", false}, {429, "age restricted", false}, {503, "login required", false},
		{404, "age restricted", false}, {403, "age restricted", true}, {403, "invalid signature", false},
		{200, "invalid device_id", false}, {200, "request rejected", false},
	} {
		t.Run(fmt.Sprintf("%d/%s/%t", tc.status, tc.message, tc.blocked), func(t *testing.T) {
			if IsContentRestricted(ClassifyFailure(tc.status, 1, tc.message, tc.blocked, 0)) {
				t.Fatal("unrelated failure triggered account fallback")
			}
		})
	}
}

func TestAuthenticatedRedirectUsesRotatedCookies(t *testing.T) {
	for _, name := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
		t.Run(name, func(t *testing.T) {
			session := registeredTestSession(t)
			calls := 0
			client := NewClient(session, doerFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch calls {
				case 1:
					cookie, err := r.Cookie(name)
					if err != nil || cookie.Value != "initial-login" {
						t.Error("initial request missing configured account")
					}
					return testResponse(302, nil, http.Header{"Location": {"/aweme/v1/aweme/detail/?redirect=1"}, "Set-Cookie": {name + "=rotated-login; Path=/; Secure; HttpOnly"}}), nil
				case 2:
					cookie, err := r.Cookie(name)
					if err != nil || cookie.Value != "rotated-login" {
						t.Error("redirect reused original token instead of rotated cookie")
					}
				case 3:
					if r.Header.Get("Cookie") != "" {
						t.Error("account cookies escaped the authenticated request")
					}
				}
				return testResponse(200, []byte(`{"aweme_detail":{"aweme_id":"1234567890123456789"}}`), nil), nil
			}))
			if _, err := client.DetailWithAccount(context.Background(), "1234567890123456789", Account{SessionID: "initial-login"}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Detail(context.Background(), "1234567890123456789"); err != nil {
				t.Fatal(err)
			}
			client.SyncCookies()
			if calls != 3 || strings.Contains(session.Cookies, "login") {
				t.Fatal("rotation changed anonymous session state")
			}
		})
	}
}

func TestAuthenticatedRedirectHonorsAccountCookieDeletion(t *testing.T) {
	for _, name := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
		for _, tc := range []struct {
			name       string
			attributes string
			deleted    bool
		}{
			{"max-age", "Max-Age=0; Path=/", true},
			{"expires", "Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/", true},
			{"parent-domain", "Max-Age=0; Domain=.tiktokv.com; Path=/", true},
			{"default-path", "Max-Age=0", true},
			{"different-path", "Max-Age=0; Path=/aweme/v1/play/", false},
			{"unrelated-domain", "Max-Age=0; Domain=example.org; Path=/", false},
			{"public-suffix", "Max-Age=0; Domain=com; Path=/", false},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				session := registeredTestSession(t)
				calls := 0
				client := NewClient(session, doerFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					for _, cookieName := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
						cookie, err := r.Cookie(cookieName)
						wantAbsent := calls == 4 || (tc.deleted && cookieName == name && (calls == 2 || calls == 3))
						if wantAbsent {
							if err != http.ErrNoCookie {
								t.Errorf("request %d sent cookie %s after deletion or during anonymous access", calls, cookieName)
							}
						} else if err != nil || cookie.Value != "initial-login" {
							t.Errorf("request %d missing configured cookie %s", calls, cookieName)
						}
					}
					switch calls {
					case 1:
						return testResponse(302, nil, http.Header{
							"Location":   {"/aweme/v1/aweme/detail/?redirect=1"},
							"Set-Cookie": {name + "=; Secure; " + tc.attributes},
						}), nil
					case 2:
						return testResponse(302, nil, http.Header{"Location": {"/aweme/v1/aweme/detail/?redirect=2"}}), nil
					}
					return testResponse(200, []byte(`{"aweme_detail":{"aweme_id":"1234567890123456789"}}`), nil), nil
				}))
				ctx := context.Background()
				account := Account{SessionID: "initial-login"}
				if _, err := client.DetailWithAccount(ctx, "1234567890123456789", account); err != nil {
					t.Fatal(err)
				}
				if _, err := client.Detail(ctx, "1234567890123456789"); err != nil {
					t.Fatal(err)
				}
				if _, err := client.DetailWithAccount(ctx, "1234567890123456789", account); err != nil {
					t.Fatal(err)
				}
				client.SyncCookies()
				if calls != 5 || strings.Contains(session.Cookies, "login") {
					t.Fatal("account cookie deletion changed anonymous session state")
				}
			})
		}
	}
}
