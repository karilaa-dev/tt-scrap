package instagramapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestUnconfirmedFailuresRetryWithoutChangingAccountHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		status             int
		truncated, network bool
	}{
		{name: "bare_unauthorized", status: 401},
		{name: "bare_forbidden", status: 403},
		{name: "proxy_denial", status: 403, body: "<html>Access denied</html>"},
		{name: "unknown_auth_error", status: 401, body: `{"status":"fail","message":"unknown"}`},
		{name: "incidental_challenge_text", status: 403, body: `{"status":"fail","message":"challenge service unavailable"}`},
		{name: "false_challenge", status: 403, body: `{"challenge_required":false,"checkpoint_required":"false","challenge":{}}`},
		{name: "proxy_sentry_block", status: 403, body: `{"message":"sentry_block"}`},
		{name: "bad_request", status: 400},
		{name: "unknown_api_failure", status: 200, body: `{"status":"fail"}`},
		{name: "malformed_metadata", status: 200, body: "{"},
		{name: "empty_metadata", status: 200},
		{name: "null_metadata", status: 200, body: "null"},
		{name: "partial_metadata", status: 200, body: `{"items":[]} {`},
		{name: "truncated_restriction", status: 403, body: `{"message":"challenge_required"}`, truncated: true},
		{name: "oversized_restriction", status: 403, body: `{"message":"challenge_required"}` + strings.Repeat(" ", metadataLimit)},
		{name: "server_error", status: 503, body: `{"message":"challenge_required"}`},
		{name: "network_error", network: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, 2, 1)
			// Ambiguous denials must not trigger password recovery either.
			p.accounts[0].login = combinedFixture().backup("123")
			p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
				t.Fatal("unconfirmed failure triggered password login")
				return loginResult{}, nil
			}
			var requests []string
			failed := false
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				auth := r.Header.Get("Authorization")
				requests = append(requests, auth)
				if auth == "Bearer IGT:2:a" && !failed {
					failed = true
					if tc.network {
						return nil, errors.New("fixture network error")
					}
					var body io.ReadCloser = io.NopCloser(strings.NewReader(tc.body))
					if tc.truncated {
						body = &incompleteBody{Reader: strings.NewReader(tc.body)}
					}
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"123"}]}`))}, nil
			})
			for _, a := range p.accounts {
				a.client.Transport = transport
			}
			result, err := p.Detail(context.Background(), "123")
			if err != nil || result.IdentityID != "instagram:b" {
				t.Fatalf("retry: %#v %v", result, err)
			}
			if p.accounts[0].disabled || !p.accounts[0].cooldown.IsZero() {
				t.Fatal("unconfirmed failure changed account health")
			}
			result, err = p.Detail(context.Background(), "456")
			if err != nil || result.IdentityID != "instagram:a" {
				t.Fatalf("account was not reusable: %#v %v", result, err)
			}
			if !reflect.DeepEqual(requests, []string{"Bearer IGT:2:a", "Bearer IGT:2:b", "Bearer IGT:2:a"}) {
				t.Fatal("unexpected attempts", requests)
			}
		})
	}
}

type incompleteBody struct{ *strings.Reader }

func (b *incompleteBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (*incompleteBody) Close() error { return nil }

func TestRetryUsesEachAccountAtMostOnce(t *testing.T) {
	for _, tc := range []struct{ accounts, active, attempts, want int }{
		{1, 0, 3, 1}, {2, 0, 3, 2}, {4, 0, 3, 3}, {4, 2, 3, 2},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			p := newTestPool(t, tc.accounts, 1)
			p.options.ActiveAccounts, p.options.MaxAttempts = tc.active, tc.attempts
			seen := map[string]bool{}
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				auth := r.Header.Get("Authorization")
				if seen[auth] {
					t.Error("retried the same failing account")
				}
				seen[auth] = true
				return nil, errors.New("fixture failure")
			})
			for _, a := range p.accounts {
				a.client.Transport = transport
			}
			_, err := p.Detail(context.Background(), "123")
			assertCode(t, err, "upstream_network_error")
			if len(seen) != tc.want {
				t.Fatalf("attempts = %d, want %d", len(seen), tc.want)
			}
			for _, a := range p.accounts {
				if a.disabled {
					t.Fatal("exhaustion disabled account")
				}
			}
		})
	}
}

func TestConfirmedFailuresPersistSpecificReason(t *testing.T) {
	for _, tc := range []struct{ body, reason string }{
		{`{"message":"challenge_required"}`, "challenge_required"},
		{`{"checkpoint_required":true}`, "challenge_required"},
		{`{"message":"login_required"}`, "session_expired"},
		{`{"error_type":"account_disabled"}`, "account_restricted"},
		{`{"error_type":"account_suspended"}`, "account_restricted"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			store, _ := testStore(t)
			p := newTestPool(t, 2, 1)
			ctx := context.Background()
			if err := store.Seed(ctx, []accountConfig{{ID: "a", Authorization: "Bearer IGT:2:a"}, {ID: "b", Authorization: "Bearer IGT:2:b"}}); err != nil {
				t.Fatal(err)
			}
			p.options.Store = store
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer IGT:2:a" {
					w.WriteHeader(403)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, `{"items":[]}`)
			}))
			defer server.Close()
			p.apiOrigin = server.URL
			if result, err := p.Detail(ctx, "123"); err != nil || result.IdentityID != "instagram:b" {
				t.Fatal("replacement failed", err)
			}
			var dead bool
			var reason string
			var deadAt time.Time
			if err := store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
				return db.QueryRow(ctx, `SELECT dead,dead_reason,dead_at FROM `+store.accounts+` WHERE id='a'`).Scan(&dead, &reason, &deadAt)
			}); err != nil {
				t.Fatal(err)
			}
			if !dead || reason != tc.reason || deadAt.IsZero() {
				t.Fatal("missing confirmed death", dead, reason, deadAt)
			}
		})
	}
}
