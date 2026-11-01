package instagramapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func sessionMetadataResponse(header http.Header) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"status":"ok","items":[]}`))}
}

func TestMetadataPersistsSessionContextAcrossRestartWithoutCDNLeak(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 1, 1).options.AccountsFile, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.accounts[0].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-MID") != "" {
			t.Error("legacy session invented context")
		}
		header := http.Header{}
		header.Set("Set-Cookie", "mobile_session=account-a; Path=/api/; Secure; Max-Age=3600")
		header.Set("ig-set-x-mid", "account-a-mid")
		header.Set("x-ig-set-www-claim", "account-a-claim")
		header.Set("ig-set-ig-u-rur", "account-a-rur")
		return sessionMetadataResponse(header), nil
	})
	if _, err = p.Detail(ctx, "123"); err != nil {
		t.Fatal(err)
	}
	p.Close()
	store.Close()
	reopened, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := Open(Options{Store: reopened, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	a := restored.accounts[0]
	a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cookie, err := r.Cookie("mobile_session")
		if err != nil || cookie.Value != "account-a" || r.Header.Get("X-MID") != "account-a-mid" || r.Header.Get("X-IG-WWW-Claim") != "account-a-claim" || r.Header.Get("IG-U-RUR") != "account-a-rur" {
			t.Error("metadata lost persisted session context")
		}
		return sessionMetadataResponse(nil), nil
	})
	if _, err = restored.Detail(ctx, "456"); err != nil {
		t.Fatal(err)
	}
	a.mediaClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, name := range []string{"Cookie", "Authorization", "X-MID", "X-IG-WWW-Claim", "IG-U-RUR"} {
			if r.Header.Get(name) != "" {
				t.Error("CDN request exposed session state", name)
			}
		}
		return sessionMetadataResponse(nil), nil
	})
	err = restored.WithMedia(ctx, a.id, func(open core.OpenAppMedia) error {
		header := http.Header{}
		for _, name := range []string{"Cookie", "Authorization", "X-MID", "X-IG-WWW-Claim", "IG-U-RUR"} {
			header.Set(name, "must-be-stripped")
		}
		res, err := open(ctx, "https://cdn.example.invalid/media", header)
		if err == nil {
			res.Body.Close()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLateMetadataContextCannotReplaceRenewedSession(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	proxy := ""
	if ok, err := store.Insert(ctx, storedAccount{accountConfig: accountConfig{
		ID: "a", Authorization: "Bearer IGT:2:old", ProxyURL: &proxy,
		Login: &passwordCredentials{Username: "alice", Password: "fixture", UserID: "123"},
	}, Username: "alice", UserID: "123"}); err != nil || !ok {
		t.Fatal("could not seed account", err)
	}
	p, err := Open(Options{Store: store, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	a := p.accounts[0]
	started, release := make(chan struct{}), make(chan struct{})
	a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		header := http.Header{}
		header.Set("ig-set-x-mid", "old-response-mid")
		header.Set("Set-Cookie", "mobile_session=old-response; Path=/; Secure")
		return sessionMetadataResponse(header), nil
	})
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{Authorization: "Bearer IGT:2:new", UserID: "123", Session: &SessionState{MID: "new-login-mid"}}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := p.Detail(ctx, "123"); done <- err }()
	<-started
	err = p.renew(ctx, a, "Bearer IGT:2:old")
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	rows, err := store.Load(ctx)
	if err != nil || len(rows) != 1 || rows[0].Session == nil || rows[0].Session.MID != "new-login-mid" || len(rows[0].Session.Cookies) != 0 {
		t.Fatal("late response overwrote new context", err)
	}
	if state := a.sessionState.Snapshot(); state.MID != "new-login-mid" || len(state.Cookies) != 0 {
		t.Fatal("late response contaminated new in-memory context")
	}
}
