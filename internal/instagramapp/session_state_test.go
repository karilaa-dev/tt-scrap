package instagramapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func sessionTestURL(value string) *url.URL {
	u, err := url.Parse(value)
	if err != nil {
		panic(err)
	}
	return u
}

func restoreSessionTest(t *testing.T, s *sessionContext) *sessionContext {
	t.Helper()
	raw, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var saved SessionState
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	restored, err := newSessionContext(&saved)
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestSessionStateRestoresCookieScopesAndServerHeaders(t *testing.T) {
	s, _ := newSessionContext(nil)
	origin := sessionTestURL("https://i.instagram.com/api/v1/accounts/login")
	s.SetCookies(origin, []*http.Cookie{
		{Name: "host", Value: "only-i", Path: "/", Secure: true, HttpOnly: true},
		{Name: "domain", Value: "both-api-hosts", Domain: ".instagram.com", Path: "/", Secure: true},
		{Name: "path", Value: "accounts-only", Secure: true},
		{Name: "lifetime", Value: "absolute", Path: "/", MaxAge: 3600, Expires: time.Now().Add(-time.Hour)},
	})
	header := http.Header{}
	header.Set("ig-set-x-mid", "server-mid")
	header.Set("x-ig-set-www-claim", "server-claim")
	header.Set("ig-set-ig-u-rur", "server-rur")
	header.Set("ig-set-authorization", "ignored-token")
	header.Set("X-FB-Friendly-Name", "login-only")
	s.Observe(origin, header)
	restored := restoreSessionTest(t, s)
	for _, endpoint := range []string{
		"https://i.instagram.com/api/v1/accounts/current_user/",
		"https://i.instagram.com/api/v1/media/123/info/",
		"https://b.i.instagram.com/api/v1/accounts/current_user/",
	} {
		u := sessionTestURL(endpoint)
		if !reflect.DeepEqual(s.Cookies(u), restored.Cookies(u)) {
			t.Errorf("cookie routing changed after restore at %s", endpoint)
		}
	}
	if got := len(restored.Cookies(sessionTestURL("https://b.i.instagram.com/"))); got != 1 {
		t.Fatalf("host-only cookies crossed API hosts: count %d", got)
	}
	if got := len(restored.Cookies(sessionTestURL("https://i.instagram.com/api/v1/accounts/current_user/"))); got != 4 {
		t.Fatalf("account path lost a cookie: count %d", got)
	}
	if got := len(restored.Cookies(sessionTestURL("https://i.instagram.com/api/v1/media/123/info/"))); got != 3 {
		t.Fatalf("default cookie path widened to unrelated endpoint: count %d", got)
	}
	req := &http.Request{URL: origin, Header: http.Header{}}
	restored.Apply(req)
	if req.Header.Get("X-MID") != "server-mid" || req.Header.Get("X-IG-WWW-Claim") != "server-claim" || req.Header.Get("IG-U-RUR") != "server-rur" || len(req.Header) != 3 {
		t.Fatal("restored headers missing or included unsupported upstream headers")
	}
	for i, before := range s.Snapshot().Cookies {
		after := restored.Snapshot().Cookies[i]
		if before.Cookie.MaxAge != 0 || !before.Cookie.Expires.Equal(after.Cookie.Expires) {
			t.Fatal("restore extended a cookie lifetime")
		}
	}
}

func TestSessionStateDoesNotSendOrAcceptStateOutsideAPIOrigins(t *testing.T) {
	s, _ := newSessionContext(&SessionState{MID: "private"})
	origin := sessionTestURL("https://i.instagram.com/")
	s.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "private", Domain: ".instagram.com", Path: "/"}})
	for _, endpoint := range []string{
		"http://i.instagram.com/",
		"https://i.instagram.com:444/",
		"https://www.instagram.com/",
		"https://scontent.cdninstagram.com/",
		"https://instagram.com.attacker.invalid/",
		"https://attacker.invalid/",
		"https://user@i.instagram.com/",
	} {
		u := sessionTestURL(endpoint)
		if len(s.Cookies(u)) != 0 {
			t.Errorf("private cookie sent outside API origin: %s", endpoint)
		}
		req := &http.Request{URL: u, Header: http.Header{}}
		s.Apply(req)
		if len(req.Header) != 0 {
			t.Errorf("private header sent outside API origin: %s", endpoint)
		}
		s.SetCookies(u, []*http.Cookie{{Name: "poison", Value: "bad", Domain: ".instagram.com"}})
		h := http.Header{}
		h.Set("ig-set-x-mid", "poison")
		s.Observe(u, h)
	}
	if s.Snapshot().MID != "private" || len(s.Snapshot().Cookies) != 1 {
		t.Fatal("foreign origin contaminated session state")
	}
	if got := s.Cookies(sessionTestURL("https://I.INSTAGRAM.COM:443/")); len(got) != 1 {
		t.Fatal("canonical HTTPS origin rejected")
	}
}

func TestSessionStateDeletionExpiryAndOrderingSurviveRestart(t *testing.T) {
	s, _ := newSessionContext(nil)
	u := sessionTestURL("https://i.instagram.com/")
	s.SetCookies(u, []*http.Cookie{
		{Name: "same", Value: "domain-first", Domain: ".instagram.com", Path: "/"},
		{Name: "same", Value: "host-second", Path: "/"},
		{Name: "delete", Value: "gone", Path: "/"},
		{Name: "expired", Value: "gone", Path: "/"},
	})
	s.SetCookies(u, []*http.Cookie{
		{Name: "same", Value: "domain-updated", Domain: "instagram.com", Path: "/"},
		{Name: "delete", MaxAge: -1, Path: "/"},
		{Name: "expired", Expires: time.Now().Add(-time.Hour), Path: "/"},
	})
	restored := restoreSessionTest(t, s)
	cookies := restored.Cookies(u)
	if len(cookies) != 2 || cookies[0].Value != "domain-updated" || cookies[1].Value != "host-second" {
		t.Fatal("cookie deletion or same-path creation order changed on restart")
	}
	saved := restored.Snapshot()
	saved.Cookies[0].Cookie.Expires = time.Now().Add(-time.Hour)
	expired, err := newSessionContext(saved)
	if err != nil || len(expired.Cookies(u)) != 1 || len(expired.Snapshot().Cookies) != 1 {
		t.Fatal("cookie expired while stopped was restored")
	}
}

func TestSessionStateRejectsInvalidSavedValuesAndCookieScopes(t *testing.T) {
	validCookie := SessionCookie{Origin: "https://i.instagram.com", Cookie: http.Cookie{Name: "session", Value: "fixture", Path: "/"}}
	for name, saved := range map[string]*SessionState{
		"header injection": {MID: "value\r\nInjected: yes"},
		"large header":     {MID: strings.Repeat("x", maxSessionValue+1)},
		"many cookies":     {Cookies: make([]SessionCookie, maxSessionCookies+1)},
		"foreign origin":   {Cookies: []SessionCookie{{Origin: "https://other.invalid", Cookie: validCookie.Cookie}}},
		"public suffix":    {Cookies: []SessionCookie{{Origin: validCookie.Origin, Cookie: http.Cookie{Name: "session", Value: "fixture", Domain: "com"}}}},
		"foreign domain":   {Cookies: []SessionCookie{{Origin: validCookie.Origin, Cookie: http.Cookie{Name: "session", Value: "fixture", Domain: "example.com"}}}},
		"relative expiry":  {Cookies: []SessionCookie{{Origin: validCookie.Origin, Cookie: http.Cookie{Name: "session", Value: "fixture", MaxAge: 3600}}}},
		"partitioned":      {Cookies: []SessionCookie{{Origin: validCookie.Origin, Cookie: http.Cookie{Name: "session", Value: "fixture", Secure: true, Partitioned: true}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newSessionContext(saved); err == nil {
				t.Fatal("invalid saved state accepted")
			}
		})
	}
	s, _ := newSessionContext(nil)
	u := sessionTestURL(validCookie.Origin)
	s.SetCookies(u, []*http.Cookie{
		{Name: "foreign", Value: "fixture", Domain: "example.com"},
		{Name: "suffix", Value: "fixture", Domain: "com"},
		{Name: "malformed", Value: "fixture", Domain: "..instagram.com"},
		{Name: "newline", Value: "bad\nvalue"},
	})
	if len(s.Cookies(u)) != 0 || len(s.Snapshot().Cookies) != 0 {
		t.Fatal("invalid upstream cookies were accepted")
	}
}

func TestSessionStateSnapshotsDoNotAliasAndAccountsRemainIsolated(t *testing.T) {
	u := sessionTestURL("https://i.instagram.com/")
	initial := &SessionState{MID: "original", Cookies: []SessionCookie{{Origin: "https://i.instagram.com", Cookie: http.Cookie{Name: "session", Value: "original", Path: "/"}}}}
	first, _ := newSessionContext(initial)
	second, _ := newSessionContext(initial)
	initial.MID = "changed"
	initial.Cookies[0].Cookie.Value = "changed"
	var wg sync.WaitGroup
	for n := range 16 {
		wg.Go(func() {
			for range 20 {
				h := http.Header{}
				h.Set("ig-set-x-mid", fmt.Sprintf("first-%d", n))
				first.Observe(u, h)
				first.SetCookies(u, []*http.Cookie{{Name: "session", Value: fmt.Sprintf("first-%d", n), Path: "/"}})
				snapshot := first.Snapshot()
				snapshot.Cookies[0].Cookie.Value = "snapshot-only"
				request := &http.Request{URL: u, Header: http.Header{}}
				first.Apply(request)
				if first.Cookies(u)[0].Value == "snapshot-only" {
					t.Error("snapshot aliases live cookies")
				}
			}
		})
	}
	wg.Wait()
	if second.Cookies(u)[0].Value != "original" || second.Snapshot().MID != "original" {
		t.Fatal("account state shared with another account or caller's input")
	}
}

func TestSessionStateCookieLimitKeepsJarAndSnapshotConsistent(t *testing.T) {
	s, _ := newSessionContext(nil)
	u := sessionTestURL("https://i.instagram.com/")
	for n := range maxSessionCookies + 10 {
		s.SetCookies(u, []*http.Cookie{{Name: fmt.Sprintf("cookie%d", n), Value: "fixture", Path: "/"}})
	}
	if len(s.Cookies(u)) != maxSessionCookies || len(s.Snapshot().Cookies) != maxSessionCookies {
		t.Fatal("live and persisted cookie bounds differ")
	}
	s.SetCookies(u, []*http.Cookie{{Name: "cookie0", MaxAge: -1, Path: "/"}, {Name: "new", Value: "fixture", Path: "/"}})
	restored := restoreSessionTest(t, s)
	if !reflect.DeepEqual(s.Cookies(u), restored.Cookies(u)) {
		t.Fatal("cookie eviction and insertion changed after restart")
	}
}

func TestSessionStateExplicitTestOrigin(t *testing.T) {
	s, err := newSessionContext(nil, "http://api.fixture.invalid:34567")
	if err != nil {
		t.Fatal(err)
	}
	u := sessionTestURL("http://api.fixture.invalid:34567/api/v1/login/")
	s.SetCookies(u, []*http.Cookie{{Name: "fixture", Value: "local"}, {Name: "secure", Value: "local", Secure: true}})
	if len(s.Cookies(u)) != 1 || len(s.Snapshot().Cookies) != 2 {
		t.Fatalf("local fixture origin or secure-cookie selection failed: sent=%d saved=%d", len(s.Cookies(u)), len(s.Snapshot().Cookies))
	}
	if _, err := newSessionContext(s.Snapshot()); err == nil {
		t.Fatal("saved state granted its own non-production origin")
	}
}
