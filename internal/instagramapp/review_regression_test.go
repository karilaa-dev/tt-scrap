package instagramapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExpiredRenewalDeadlineBlocksReplacementAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	proxy := ""
	seed := storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:original", ProxyURL: &proxy, Login: combinedFixture().backup("123")}, Username: "alice", UserID: "123"}
	if ok, err := store.Insert(ctx, seed); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"login_required"}`))}, nil
	})
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{}, loginCheck(loginResponse{status: 401, header: http.Header{"Retry-After": {"7200"}}, value: map[string]any{"message": "login_required"}})
	}
	_, err = p.Detail(ctx, "123")
	assertCode(t, err, "session_expired")
	reopened, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := Open(Options{Store: reopened})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	a := restored.accounts[0]
	if !a.disabled || a.deadReason != "session_expired" || time.Until(a.cooldown) < 119*time.Minute {
		t.Fatal("session-expiry deadline was lost across restart")
	}
	a.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("replacement verification ignored server deadline")
		return nil, errors.New("unexpected request")
	})
	result, err := NewAccountManager(restored, reopened, nil).Import(ctx, []Credentials{{Cookies: cookieFixture("123")}})
	if err != nil || len(result) != 1 || result[0].ErrorCode != "upstream_rate_limited" {
		t.Fatal("replacement admitted during server cooldown", result, err)
	}
}

func TestCanceledRenewalResponseDoesNotInventHealth(t *testing.T) {
	for _, tc := range []struct {
		post   bool
		status int
	}{{false, 0}, {false, 200}, {false, 429}, {true, 0}, {true, 200}, {true, 429}} {
		status := tc.status
		store, _ := testStore(t)
		p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 1, 1).options.AccountsFile, MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		a := p.accounts[0]
		a.login = combinedFixture().backup("123")
		a.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"login_required"}`))}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		verifyClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cancel()
			if status == 0 {
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"3600"}}, Body: &incompleteBody{Reader: strings.NewReader("")}}, nil
		})}
		calls := 0
		p.login = func(ctx context.Context, _ *http.Client, origin string, _ Credentials) (loginResult, error) {
			calls++
			if calls > 1 {
				return loginResult{}, loginFailure("login_session_not_returned")
			}
			if tc.post {
				s := &loginSession{client: verifyClient}
				_, err := s.post(ctx, origin, "/api/v1/accounts/login/", nil, nil)
				return loginResult{}, err
			}
			return verifyLogin(ctx, verifyClient, origin, "Bearer IGT:2:candidate", nil)
		}
		_, err = p.Detail(ctx, "123")
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		rows, err := store.Load(context.Background())
		if err != nil || len(rows) != 1 || rows[0].Dead {
			t.Fatal("canceled verification disabled account", err)
		}
		if status == 429 {
			if time.Until(rows[0].CooldownUntil) < 59*time.Minute {
				t.Fatal("cancellation discarded observed throttle")
			}
		} else {
			if !rows[0].CooldownUntil.IsZero() || !a.cooldown.IsZero() || !a.refreshAfter.IsZero() {
				t.Errorf("POST=%t status %d: cancellation invented cooldown or cached renewal failure", tc.post, status)
			}
			_ = p.renew(context.Background(), a, a.authorization)
			if calls != 2 {
				t.Errorf("POST=%t status %d: next caller could not retry canceled response", tc.post, status)
			}
		}
	}
}
