package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func TestAccountIsNotStoredOrRestoredFromDatabase(t *testing.T) {
	cfg, db := testDatabase(t)
	cfg.TikTokSessionID = "environment-only-login"
	p := openTestPool(t, cfg, "")
	fixture := newProtocolFixture(t, "age restricted", 200)
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/aweme/v1/aweme/detail/" && fixture.details == 1 {
				cookie, err := r.Cookie("sid_tt")
				if err != nil || cookie.Value != "environment-only-login" {
					t.Error("fallback did not use configured session")
				}
			}
			return fixture.do(r)
		}))
	}
	if _, err := p.Detail(context.Background(), "7695962883523464478"); err != nil || fixture.details != 2 {
		t.Fatal("configured account fallback failed", err)
	}
	var exists bool
	if err := db.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='scrap_accounts')", cfg.DBSchema).Scan(&exists); err != nil || exists {
		t.Fatal("account storage table must not be created", err)
	}
	if err := db.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM "+p.identitiesTable+" WHERE session::text LIKE '%environment-only-login%')").Scan(&exists); err != nil || exists {
		t.Fatal("environment session was persisted", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.TikTokSessionID = ""
	reopened := openTestPool(t, cfg, "")
	fixture = newProtocolFixture(t, "age restricted", 200)
	fixture.install(reopened)
	_, err := reopened.Detail(context.Background(), "7695962883523464478")
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "content_restricted" || fixture.details != 1 {
		t.Fatal("removing the environment session must disable fallback after restart", err)
	}
}

func TestDetailUsesAccountOnlyForContentRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		status, calls int
		without       bool
		persistent    bool
		blocked       bool
		wantError     bool
	}{
		{name: "public", body: `{"aweme_detail":{"aweme_id":"7695962883523464478"}}`, status: 200, calls: 1},
		{name: "age", body: `{"status_code":1,"status_msg":"Age restricted"}`, status: 200, calls: 2},
		{name: "login", body: `{"status_code":1,"status_msg":"Login required"}`, status: 401, calls: 2},
		{name: "private post", body: `{"status_code":10216}`, status: 200, calls: 2},
		{name: "private account", body: `{"status_code":10222}`, status: 200, calls: 2},
		{name: "region", body: `{"status_code":10204}`, status: 200, calls: 2},
		{name: "empty restricted detail", body: `{"status_code":0,"status_msg":"Login required","aweme_detail":null}`, status: 200, calls: 2},
		{name: "no account", body: `{"status_code":10216}`, status: 200, calls: 1, without: true, wantError: true},
		{name: "account denied", body: `{"status_code":10216}`, status: 200, calls: 2, persistent: true, wantError: true},
		{name: "rate limited", body: `{"status_code":1}`, status: 429, calls: 1, wantError: true},
		{name: "server failure", body: `{"status_code":1}`, status: 503, calls: 1, wantError: true},
		{name: "challenge", body: `{"status_code":1,"status_msg":"challenge required"}`, status: 403, calls: 1, wantError: true},
		{name: "blocked private post", body: `{"status_code":10216}`, status: 200, calls: 1, blocked: true, wantError: true},
		{name: "blocked private account", body: `{"status_code":10222}`, status: 200, calls: 1, blocked: true, wantError: true},
		{name: "blocked region", body: `{"status_code":10204}`, status: 200, calls: 1, blocked: true, wantError: true},
		{name: "deleted", body: `{}`, status: 404, calls: 1, wantError: true},
		{name: "empty unknown detail", body: `{"status_code":0}`, status: 200, calls: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, db := testDatabase(t)
			if !tc.without {
				cfg.TikTokSessionID = "fallback-login"
			}
			p := openTestPool(t, cfg, "")
			fixture := newProtocolFixture(t, "", 200)
			calls := 0
			p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
				return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/aweme/v1/aweme/detail/" {
						if _, err := r.Cookie("sessionid"); err == nil {
							t.Error("bootstrap used account credentials")
						}
						return fixture.do(r)
					}
					calls++
					cookie, err := r.Cookie("sid_tt")
					if calls == 1 {
						if err == nil {
							t.Error("first request was authenticated")
						}
						response := fixtureResponse(tc.status, []byte(tc.body))
						if tc.blocked {
							response.Header.Set("tt_block", "1")
						}
						return response, nil
					}
					if err != nil || cookie.Value != "fallback-login" {
						t.Error("fallback missing account")
					}
					if tc.persistent {
						return fixtureResponse(tc.status, []byte(tc.body)), nil
					}
					response := fixtureResponse(200, []byte(`{"aweme_detail":{"aweme_id":"7695962883523464478"}}`))
					response.Header.Set("Set-Cookie", "account-response=private; Path=/; Secure")
					return response, nil
				}))
			}
			_, err := p.Detail(context.Background(), "7695962883523464478")
			if (err != nil) != tc.wantError || calls != tc.calls {
				t.Fatalf("calls=%d want=%d error=%v", calls, tc.calls, err)
			}
			if tc.blocked {
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "upstream_extraction_error" {
					t.Fatal("bot block was treated as a content restriction", err)
				}
				for _, entry := range p.entries {
					if entry.health.Failures != 1 || entry.health.CooldownUntil == 0 {
						t.Error("blocked identity did not enter cooldown")
					}
				}
			}
			var leaked bool
			if err := db.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM "+p.identitiesTable+" WHERE session::text LIKE '%fallback-login%' OR session::text LIKE '%account-response%')").Scan(&leaked); err != nil || leaked {
				t.Fatal("account contaminated saved identity", err)
			}
			if tc.persistent || tc.without {
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "content_private" {
					t.Fatal("restriction error was lost", err)
				}
				for _, entry := range p.entries {
					if entry.health.Failures != 0 {
						t.Error("content restriction penalized anonymous identity")
					}
				}
			}
		})
	}
}

func TestRestrictedAppMediaFallbackIsIsolatedAcrossBatch(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.TikTokSessionID = "media-login"
	p := openTestPool(t, cfg, "")
	fixture := newProtocolFixture(t, "", 200)
	var mu sync.Mutex
	attempts := map[string]int{}
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if !strings.HasPrefix(r.URL.Path, "/aweme/v1/play/") {
				return fixture.do(r)
			}
			id := r.URL.Query().Get("video_id")
			attempts[id]++
			cookie, err := r.Cookie("sid_tt")
			if attempts[id] == 1 && err == nil {
				t.Error("media did not start anonymously")
			}
			if id == "restricted" && err != nil {
				return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"Age restricted"}`)), nil
			}
			if (id == "restricted-json" || id == "restricted-sniffed") && err != nil {
				response := fixtureResponse(200, []byte(" \n"+`{"status_code":10216}`))
				if id == "restricted-json" {
					response.Header.Set("Content-Type", "application/json; charset=utf-8")
				}
				return response, nil
			}
			if strings.HasPrefix(id, "restricted") && (err != nil || cookie.Value != "media-login") {
				t.Error("media fallback missing account")
			}
			return fixtureResponse(200, []byte("media bytes")), nil
		}))
	}
	result, err := p.Detail(context.Background(), "7695962883523464478")
	if err != nil {
		t.Fatal(err)
	}
	err = p.WithMedia(context.Background(), result.IdentityID, func(open core.OpenAppMedia) error {
		var wg sync.WaitGroup
		for _, id := range []string{"restricted", "restricted-json", "restricted-sniffed", "public"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				response, err := open(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/?video_id="+id, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "media bytes" {
					t.Error("media bytes lost", err)
				}
			}()
		}
		wg.Wait()
		return nil
	})
	if err != nil || attempts["restricted"] != 2 || attempts["restricted-json"] != 2 || attempts["restricted-sniffed"] != 2 || attempts["public"] != 1 {
		t.Fatal("incorrect media fallback attempts", err, attempts)
	}
}

func TestAccountFallbackDistinguishesRejectionFromUpstreamFailure(t *testing.T) {
	for _, operation := range []string{"detail", "media", "batch"} {
		for _, tc := range []struct {
			status, publicStatus, failures int
			code                           string
		}{
			{401, 403, 0, "content_private"},
			{403, 403, 0, "content_private"},
			{429, 429, 1, "upstream_rate_limited"},
			{503, 502, 1, "upstream_extraction_error"},
		} {
			t.Run(fmt.Sprintf("%s/%d", operation, tc.status), func(t *testing.T) {
				cfg, db := testDatabase(t)
				cfg.TikTokSessionID = "stale-login"
				p := openTestPool(t, cfg, "")
				fixture := newProtocolFixture(t, "", 200)
				anonymous, authenticated := 0, 0
				p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
					return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == "/aweme/v1/play/" || (operation == "detail" && r.URL.Path == "/aweme/v1/aweme/detail/") {
							if _, err := r.Cookie("sid_tt"); err != nil {
								anonymous++
								return fixtureResponse(403, []byte(`{"status_code":10216}`)), nil
							}
							authenticated++
							return fixtureResponse(tc.status, []byte("Unavailable")), nil
						}
						return fixture.do(r)
					}))
				}
				var err error
				if operation == "detail" {
					_, err = p.Detail(context.Background(), "7695962883523464478")
				} else {
					result, e := p.Detail(context.Background(), "7695962883523464478")
					if e != nil {
						t.Fatal(e)
					}
					address := "https://api19-va.tiktokv.com/aweme/v1/play/"
					if operation == "media" {
						_, err = p.OpenMedia(context.Background(), result.IdentityID, address, nil)
					} else {
						err = p.WithMedia(context.Background(), result.IdentityID, func(open core.OpenAppMedia) error {
							_, e := open(context.Background(), address, nil)
							return e
						})
					}
				}
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != tc.code || failure.Status != tc.publicStatus {
					t.Error("incorrect account fallback error", err)
				}
				if anonymous != 1 || authenticated != 1 {
					t.Fatal("account rejection retried unexpectedly", anonymous, authenticated)
				}
				for _, entry := range p.entries {
					if entry.health.Failures != tc.failures || (entry.health.CooldownUntil == 0) != (tc.failures == 0) || entry.proxy.health.Failures != 0 {
						t.Error("account fallback updated identity health incorrectly")
					}
					var failures int
					if err := db.QueryRow(context.Background(), "SELECT (health->>'failures')::int FROM "+p.identitiesTable+" WHERE id=$1", entry.id).Scan(&failures); err != nil || failures != tc.failures {
						t.Error("account fallback persisted incorrect health", err)
					}
				}
			})
		}
	}
}
