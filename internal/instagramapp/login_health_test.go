package instagramapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestLoginHealthClassificationAndRetryDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		truncated        bool
	}{
		{"challenge_429", `{"message":"challenge_required"}`, "challenge_required", 429, false},
		{"feedback_429", `{"message":"feedback_required"}`, "feedback_required", 429, false},
		{"restricted_429", `{"error_type":"account_disabled"}`, "account_restricted", 429, false},
		{"expired_429", `{"message":"login_required"}`, "upstream_rate_limited", 429, false},
		{"feedback_with_wait_text", `{"message":"Please wait a few minutes","error_type":"feedback_required"}`, "feedback_required", 400, false},
		{"challenge_with_rate_text", `{"message":"rate_limit","error_type":"challenge_required"}`, "challenge_required", 400, false},
		{"api_throttle_400", `{"message":"Please wait a few minutes"}`, "upstream_rate_limited", 400, false},
		{"api_throttle_200", `{"error_type":"rate_limit"}`, "upstream_rate_limited", 200, false},
		{"empty_429", ``, "upstream_rate_limited", 429, false},
		{"malformed_429", `{"message":"challenge_required"`, "upstream_rate_limited", 429, false},
		{"trailing_invalid_429", `{"message":"challenge_required"}invalid`, "upstream_rate_limited", 429, false},
		{"truncated_challenge_429", `{"message":"challenge_required"}`, "upstream_rate_limited", 429, true},
		{"oversized_challenge_429", `{"message":"challenge_required"}` + strings.Repeat(" ", metadataLimit), "upstream_rate_limited", 429, false},
		{"outage_with_challenge", `{"message":"challenge_required"}`, "", 503, false},
	} {
		for _, path := range []string{"post", "verify"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				calls := 0
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					var body io.ReadCloser = io.NopCloser(strings.NewReader(tc.body))
					if tc.truncated {
						body = &incompleteBody{Reader: strings.NewReader(tc.body)}
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"172800"}, "X-Private-Fixture": {"private-response-header"}}, Body: body}, nil
				})}
				var err error
				if path == "post" {
					s := &loginSession{client: client}
					_, err = s.post(context.Background(), "https://fixture.invalid", "/login", url.Values{}, nil)
				} else {
					_, err = verifyLogin(context.Background(), client, "https://fixture.invalid", "Bearer IGT:2:private-fixture", nil)
				}
				want := tc.code
				if tc.status == 503 {
					want = "upstream_server_error"
					if path == "verify" {
						want = "session_check_failed"
					}
				}
				if errorCode(err) != want || calls != 1 {
					t.Fatalf("code=%q calls=%d; want %q and one request", errorCode(err), calls, want)
				}
				remaining := failureCooldown(err)
				if tc.status != 503 {
					if remaining < 47*time.Hour+59*time.Minute || remaining > 48*time.Hour {
						t.Fatalf("server deadline lost: %v", remaining)
					}
				} else if remaining != 0 {
					t.Fatalf("outage unexpectedly acquired a server throttle: %v", remaining)
				}
				for _, secret := range []string{"private-response-header", "private-fixture", tc.body} {
					if secret != "" && strings.Contains(fmt.Sprint(err), secret) {
						t.Fatal("login failure exposed response or credential")
					}
				}
			})
		}
	}
}

func TestLoginThrottleDeadlineAndSafeWrapping(t *testing.T) {
	header := http.Header{"Retry-After": {time.Now().Add(72 * time.Hour).UTC().Format(http.TimeFormat)}}
	err := fmt.Errorf("wrapped: %w", loginThrottle(header))
	var public *core.Error
	if !errors.As(err, &public) || public.Code != "upstream_rate_limited" || public.Details != nil {
		t.Fatal("safe public error was not preserved")
	}
	if remaining := failureCooldown(err); remaining < 71*time.Hour+59*time.Minute || remaining > 72*time.Hour {
		t.Fatalf("HTTP-date deadline lost: %v", remaining)
	}
	if failureCooldown(loginFailure("challenge_required")) != 0 || failureCooldown(nil) != 0 {
		t.Fatal("invented cooldown for a non-throttle error")
	}
	if failureCooldown(&loginCooldownError{cause: loginFailure("upstream_rate_limited"), until: time.Now().Add(-time.Second)}) != 0 {
		t.Fatal("expired deadline became negative or restarted")
	}
	generic := rateLimitFailure(2 * time.Hour)
	if !errors.As(generic, &public) || public.Status != http.StatusTooManyRequests || failureCooldown(generic) < 119*time.Minute {
		t.Fatal("generic account throttle lost status or deadline")
	}
}

func TestLoginNestedThrottleRestriction(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader(`{"payload":{"login_response":{"message":"Please wait","error_type":"feedback_required"}}}`))}, nil
	})}
	s := &loginSession{client: client}
	_, err := s.post(context.Background(), "https://fixture.invalid", "/login", url.Values{}, nil)
	if errorCode(err) != "feedback_required" || failureCooldown(err) < 59*time.Minute {
		t.Fatal("nested explicit restriction was masked by rate limiting", err)
	}
}
