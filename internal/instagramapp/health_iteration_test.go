package instagramapp

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

type observedHealthBody struct {
	io.ReadCloser
	closed chan struct{}
}

func (b *observedHealthBody) Close() error { close(b.closed); return b.ReadCloser.Close() }

func TestObservedHealthSurvivesCallerCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		dead       bool
	}{
		{"challenge", `{"message":"challenge_required"}`, 403, true},
		{"rate", `{}`, 429, false},
		{"restricted_rate", `{"message":"challenge_required"}`, 429, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cfg := testStore(t)
			p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 1, 1).options.AccountsFile})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			observed := make(chan struct{})
			p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"7200"}}, Body: &observedHealthBody{io.NopCloser(strings.NewReader(tc.body)), observed}}, nil
			})
			// A renewal can hold this gate when another request observes a restriction.
			p.accounts[0].refreshGate <- struct{}{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := p.Detail(ctx, "123"); done <- err }()
			<-observed
			cancel()
			select {
			case err := <-done:
				<-p.accounts[0].refreshGate
				t.Fatalf("discarded health before gate released: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			<-p.accounts[0].refreshGate
			if err = <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			restored, err := OpenStore(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			replacement, err := Open(Options{Store: restored})
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			a := replacement.accounts[0]
			if a.disabled != tc.dead {
				t.Fatal("lost durable restriction")
			}
			if tc.dead && a.deadReason != "challenge_required" {
				t.Fatal("lost reason")
			}
			if time.Until(a.cooldown) < 119*time.Minute {
				t.Fatal("lost durable throttle")
			}
		})
	}
}

func TestCanceledRenewalPreservesObservedRestriction(t *testing.T) {
	for _, code := range []string{"challenge_required", "feedback_required"} {
		t.Run(code, func(t *testing.T) {
			p := newTestPool(t, 2, 1)
			p.accounts[0].login = combinedFixture().backup("123")
			p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"message":"login_required"}`)), Header: make(http.Header)}, nil
			})
			p.accounts[1].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("restriction retried another account")
				return nil, errors.New("unexpected")
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logins := 0
			p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
				logins++
				cancel()
				return loginResult{}, loginFailure(code)
			}
			_, err := p.Detail(ctx, "123")
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if !p.accounts[0].disabled || p.accounts[0].deadReason != code {
				t.Fatal("lost restriction after canceled login")
			}
			assertCode(t, p.renew(context.Background(), p.accounts[0], p.accounts[0].authorization), code)
			if logins != 1 {
				t.Fatal("repeated restricted login")
			}
		})
	}
}

func TestLateRenewalCannotUseUnavailableReplacement(t *testing.T) {
	for _, blocked := range []string{"challenge_required", "feedback_required", "rate"} {
		t.Run(blocked, func(t *testing.T) {
			p := newTestPool(t, 1, 2)
			a := p.accounts[0]
			a.authorization = "Bearer IGT:2:replacement"
			a.login = combinedFixture().backup("123")
			if blocked == "rate" {
				a.cooldown = time.Now().Add(2 * time.Hour)
			} else {
				a.disabled = true
				a.deadReason = blocked
			}
			p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
				t.Error("login on unavailable replacement")
				return loginResult{}, errors.New("unexpected")
			}
			a.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("metadata on unavailable replacement")
				return nil, errors.New("unexpected")
			})
			want := blocked
			if blocked == "rate" {
				want = "upstream_rate_limited"
			}
			assertCode(t, p.renew(context.Background(), a, "Bearer IGT:2:old"), want)
			_, state, err := p.lookupRecovering(context.Background(), a, "123")
			assertCode(t, err, want)
			if blocked != "rate" && state.retry {
				t.Fatal("lost terminal disposition")
			}
		})
	}
}

func TestRenewalRetainsServerThrottleDeadline(t *testing.T) {
	p := newTestPool(t, 1, 1)
	p.accounts[0].login = combinedFixture().backup("123")
	p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"login_required"}`))}, nil
	})
	logins := 0
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		logins++
		return loginResult{}, rateLimitFailure(48 * time.Hour)
	}
	_, err := p.Detail(context.Background(), "123")
	assertCode(t, err, "upstream_rate_limited")
	a := p.accounts[0]
	if time.Until(a.cooldown) < 47*time.Hour || time.Until(a.refreshAfter) < 47*time.Hour {
		t.Fatal("login throttle shortened")
	}
	assertCode(t, p.renew(context.Background(), a, a.authorization), "upstream_rate_limited")
	if logins != 1 {
		t.Fatal("login retried before deadline")
	}
}

func TestMetadataThrottlePayloadHonorsRetryAfter(t *testing.T) {
	for _, delay := range []string{"10", "172800"} {
		for _, status := range []int{200, 400, 401, 403, 429} {
			p := newTestPool(t, 1, 1)
			p.accounts[0].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {delay}}, Body: io.NopCloser(strings.NewReader(`{"message":"Please wait before trying again","status":"fail"}`))}, nil
			})
			_, err := p.Detail(context.Background(), "123")
			assertCode(t, err, "upstream_rate_limited")
			remaining := time.Until(p.accounts[0].cooldown)
			want := retryAfter(delay)
			if remaining < want-time.Second || remaining > want {
				t.Errorf("status %d changed Retry-After %s: remaining %v", status, delay, remaining)
			}
		}
	}
}

func TestRetryAfterLongDelaysAndOverflow(t *testing.T) {
	for _, v := range []string{"172800", " 172800 ", time.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat)} {
		if d := retryAfter(v); d < 47*time.Hour || d > 49*time.Hour {
			t.Fatalf("long delay shortened: %s %v", v, d)
		}
	}
	for _, v := range []string{"18446744073709551615", "9999999999999999999999999999"} {
		if d := retryAfter(v); d != time.Duration(math.MaxInt64) {
			t.Fatal("delay overflow", d)
		}
	}
	for _, v := range []string{"", "-1", "invalid", "0"} {
		if d := retryAfter(v); d != time.Minute {
			t.Fatal("invalid fallback", v, d)
		}
	}
}

func TestRestrictionAndCooldownAreSavedTogether(t *testing.T) {
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 1, 1).options.AccountsFile})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	a := p.accounts[0]
	a.login = combinedFixture().backup("123")
	a.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"login_required"}`))}, nil
	})
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{}, loginCheck(loginResponse{status: 429, header: http.Header{"Retry-After": {"172800"}}, value: map[string]any{"error_type": "feedback_required", "message": "Please wait"}})
	}
	_, err = p.Detail(context.Background(), "123")
	assertCode(t, err, "feedback_required")
	restored, err := OpenStore(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	replacement, err := Open(Options{Store: restored})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	a = replacement.accounts[0]
	if !a.disabled || a.deadReason != "feedback_required" || time.Until(a.cooldown) < 47*time.Hour {
		t.Fatal("lost one of the two server health signals")
	}
}

func TestLateThrottleStillAppliesAfterSessionRenewal(t *testing.T) {
	p := newTestPool(t, 1, 2)
	a := p.accounts[0]
	a.login = combinedFixture().backup("123")
	started, finish := make(chan struct{}), make(chan struct{})
	a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"items":[]}`
		header := make(http.Header)
		if strings.Contains(r.URL.Path, "/111/") {
			close(started)
			<-finish
			status = 429
			body = `{}`
			header.Set("Retry-After", "3600")
		} else if r.Header.Get("Authorization") != "Bearer IGT:2:replacement" {
			status = 401
			body = `{"message":"login_required"}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{Authorization: "Bearer IGT:2:replacement", UserID: "123"}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := p.Detail(context.Background(), "111"); done <- err }()
	<-started
	_, err := p.Detail(context.Background(), "222")
	close(finish)
	if err != nil {
		t.Fatal(err)
	}
	assertCode(t, <-done, "upstream_rate_limited")
	if a.disabled || a.authorization != "Bearer IGT:2:replacement" || time.Until(a.cooldown) < 59*time.Minute {
		t.Fatal("renewal discarded the old request's account throttle")
	}
}
