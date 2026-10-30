package instagramapp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const fallbackTOTP = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func combinedFixture() Credentials {
	return Credentials{Cookies: cookieFixture("123"), Username: "alice", Password: "fixture-password", TOTPSecret: fallbackTOTP}
}

func TestCombinedCredentialsAndCookiePriority(t *testing.T) {
	c := combinedFixture()
	if _, err := validateCredentials([]Credentials{c}); err != nil {
		t.Fatal("combined credentials rejected", err)
	}
	var csvText strings.Builder
	writer := csv.NewWriter(&csvText)
	writer.Write([]string{"User ID / Email Address", "Password", "First Secret Question", "Cookies"})
	writer.Write([]string{c.Username, c.Password, c.TOTPSecret, string(c.Cookies)})
	writer.Flush()
	parsed, err := ParseCSV(strings.NewReader(csvText.String()))
	if err != nil || len(parsed) != 1 || !reflect.DeepEqual(parsed[0], c) {
		t.Fatal("combined CSV mapping failed", err)
	}
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		expired   bool
		wantLogin bool
		wantError string
	}{
		{"valid", 200, `{"status":"ok","user":{"username":"alice","pk":123}}`, false, false, ""},
		{"rejected", 401, `{}`, false, true, ""},
		{"expired_at_import", 200, `{}`, true, true, ""},
		{"expired_message", 400, `{"status":"fail","message":"login_required"}`, false, true, ""},
		{"throttled", 429, `{}`, false, false, "upstream_rate_limited"},
		{"server_error", 503, `{}`, false, false, "session_check_failed"},
		{"challenge", 401, `{"challenge_required":true}`, false, false, "challenge_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := combinedFixture()
			if tc.expired {
				c.Cookies = json.RawMessage(strings.ReplaceAll(string(c.Cookies), "4102444800", "1"))
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			logins := 0
			session, usedCookies, err := authenticateAccount(context.Background(), server.Client(), server.URL, c, func(_ context.Context, client *http.Client, _ string, input Credentials) (loginResult, error) {
				logins++
				if client != server.Client() || input.Username != c.Username || input.Password != c.Password || input.TOTPSecret != c.TOTPSecret {
					t.Error("fallback credentials/route changed")
				}
				return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: "123", Username: "alice"}, nil
			})
			if errorCode(err) != tc.wantError || (logins == 1) != tc.wantLogin || logins > 1 {
				t.Fatal("unexpected fallback outcome", errorCode(err), logins)
			}
			if tc.expired && calls != 0 {
				t.Fatal("expired cookie was sent")
			}
			if err == nil && ((!tc.wantLogin) != usedCookies || usedCookies && session.Authorization != "") {
				t.Fatal("wrong credential persistence mode")
			}
		})
	}
}

func TestCombinedImportPersistsBackupAndUpdatesExistingCookieAccount(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) { testCombinedImport(t, existing) })
	}
}
func testCombinedImport(t *testing.T, existing bool) {
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","user":{"username":"alice","pk":123}}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	m := NewAccountManager(p, store, []string{""})
	m.probe = func(context.Context, *http.Client) (string, error) { return "192.0.2.1", nil }
	m.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		t.Fatal("password used while cookies worked")
		return loginResult{}, nil
	}
	// Existing cookie-only account can gain backup credentials without a second row.
	initial := combinedFixture()
	if existing {
		initial = Credentials{Cookies: cookieFixture("123")}
	}
	results, err := m.Import(context.Background(), []Credentials{initial})
	if err != nil || results[0].Status != "imported" {
		t.Fatal(err)
	}
	id := results[0].AccountID
	results, err = m.Import(context.Background(), []Credentials{combinedFixture()})
	wantStatus := "duplicate"
	if existing {
		wantStatus = "updated"
	}
	if err != nil || results[0].Status != wantStatus || results[0].AccountID != id || p.Count() != 1 {
		t.Fatal("backup update failed", results, err)
	}
	rows, err := store.Load(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Login == nil || rows[0].Login.TOTPSecret != fallbackTOTP || rows[0].Authorization != "" || len(rows[0].Cookies) == 0 {
		t.Fatal("credentials not retained", err)
	}
	resultJSON, _ := json.Marshal(m.Status())
	for _, secret := range []string{"fixture-password", fallbackTOTP, "sessionid"} {
		if strings.Contains(string(resultJSON), secret) {
			t.Fatal("public status leaked credentials")
		}
	}
	p.Close()
	store.Close()
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
	if replacement.accounts[0].login == nil || replacement.accounts[0].login.Password != "fixture-password" {
		t.Fatal("restart lost fallback")
	}
}

func TestConcurrentSessionRecoveryPersistsBeforeReuse(t *testing.T) {
	store, cfg := testStore(t)
	device, _ := newDeviceIdentity("galaxy-s23-android-14")
	proxy := ""
	seed := storedAccount{accountConfig: accountConfig{ID: "a", Cookies: cookieFixture("123"), Login: combinedFixture().backup("123"), Device: device, ProxyURL: &proxy}, Username: "alice", UserID: "123"}
	if ok, err := store.Insert(context.Background(), seed); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store, PerAccountConcurrency: 8, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const workers = 8
	var oldRequests, logins atomic.Int32
	allOld := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-IG-Device-ID") != device.DeviceID {
			t.Error("recovery changed the device")
		}
		if r.Header.Get("Authorization") != "Bearer IGT:2:renewed" {
			if oldRequests.Add(1) == workers {
				close(allOld)
			}
			select {
			case <-allOld:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"status":"ok","items":[]}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	p.login = func(_ context.Context, client *http.Client, origin string, c Credentials) (loginResult, error) {
		logins.Add(1)
		if client != p.accounts[0].client || origin != server.URL || c.device == nil || *c.device != *device || c.Password != "fixture-password" || c.TOTPSecret != fallbackTOTP {
			t.Error("recovery lost saved route/device/login")
		}
		return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: "123", Username: "alice", Device: device}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			if _, err := p.Detail(ctx, "123"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if logins.Load() != 1 {
		t.Fatal("concurrent requests repeated password login", logins.Load())
	}
	rows, err := store.Load(ctx)
	if err != nil || rows[0].Authorization != "Bearer IGT:2:renewed" || rows[0].Cookies != nil || rows[0].Login == nil {
		t.Fatal("replacement session not saved", err)
	}
	p.Close()
	store.Close()
	restored, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	replacement, err := Open(Options{Store: restored})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	replacement.apiOrigin = server.URL
	replacement.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		t.Fatal("restart relogged instead of using saved session")
		return loginResult{}, nil
	}
	if _, err := replacement.Detail(ctx, "123"); err != nil {
		t.Fatal(err)
	}
}

func TestOldAuthenticationFailureDoesNotDisableRenewedSession(t *testing.T) {
	store, _ := testStore(t)
	device, _ := newDeviceIdentity("galaxy-s23-android-14")
	proxy := ""
	seed := storedAccount{accountConfig: accountConfig{ID: "a", Cookies: cookieFixture("123"), Login: combinedFixture().backup("123"), Device: device, ProxyURL: &proxy}, Username: "alice", UserID: "123"}
	if ok, err := store.Insert(context.Background(), seed); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store, PerAccountConcurrency: 2, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	oldStarted, returnOld := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer IGT:2:renewed" {
			if r.URL.Path == "/api/v1/media/111/info/" {
				close(oldStarted)
				select {
				case <-returnOld:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/media/333/info/" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"status":"ok","items":[]}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	var logins atomic.Int32
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		logins.Add(1)
		return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: "123", Username: "alice", Device: device}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldDone := make(chan error, 1)
	go func() { _, err := p.Detail(ctx, "111"); oldDone <- err }()
	select {
	case <-oldStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err = p.Detail(ctx, "222"); err != nil {
		t.Fatal("session recovery failed", err)
	}
	close(returnOld)
	select {
	case err = <-oldDone:
		if errorCode(err) != "upstream_authentication_error" {
			t.Fatal("original request failure was hidden", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rows, err := store.Load(ctx)
	if err != nil || len(rows) != 1 || rows[0].Dead || rows[0].Authorization != "Bearer IGT:2:renewed" {
		t.Fatal("old authentication failure disabled the saved replacement", err)
	}
	if _, err = p.Detail(ctx, "222"); err != nil || logins.Load() != 1 {
		t.Fatal("renewed session was disabled or logged in again", err)
	}
	// A rejection of the current session must still disable it.
	if _, err = p.Detail(ctx, "333"); errorCode(err) != "upstream_authentication_error" {
		t.Fatal("current session rejection was hidden", err)
	}
	rows, err = store.Load(ctx)
	if err != nil || !rows[0].Dead {
		t.Fatal("current session rejection did not persist dead state", err)
	}
}

func TestRecoveryFailureHandling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		loginCode  string
		userID     string
		wantLogins int
		dead       bool
	}{
		{"rate_limit", 429, `{}`, "", "123", 0, false},
		{"server_error", 503, `{}`, "", "123", 0, false},
		{"challenge", 400, `{"challenge_required":true}`, "", "123", 0, true},
		{"bad_password", 401, `{}`, "login_rejected", "", 1, true},
		{"login_throttled", 401, `{}`, "upstream_rate_limited", "", 1, false},
		{"login_network_failure", 401, `{}`, "upstream_network_error", "", 1, false},
		{"wrong_account", 401, `{}`, "", "456", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Open(Options{MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			device, _ := newDeviceIdentity("pixel-8-android-15")
			if err = p.addAccount(accountConfig{ID: "a", Cookies: cookieFixture("123"), Login: combinedFixture().backup("123"), Device: device}, false, "alice", ""); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			p.apiOrigin = server.URL
			calls := 0
			p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
				calls++
				if tc.loginCode != "" {
					return loginResult{}, loginFailure(tc.loginCode)
				}
				return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: tc.userID, Device: device}, nil
			}
			if _, err = p.Detail(context.Background(), "123"); err == nil {
				t.Fatal("failure hidden")
			}
			if calls != tc.wantLogins || p.accounts[0].disabled != tc.dead {
				t.Fatal("wrong recovery/disabling decision", calls, p.accounts[0].disabled)
			}
			if calls > 0 && !tc.dead {
				_, _ = p.Detail(context.Background(), "123")
				if calls != 1 {
					t.Fatal("transient login failure retried immediately")
				}
			}
		})
	}
}

func TestRecoverySaveFailureDoesNotPublishSession(t *testing.T) {
	store, _ := testStore(t)
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	device, _ := newDeviceIdentity("pixel-8-android-15")
	if err = p.addAccount(accountConfig{ID: "missing", Cookies: cookieFixture("123"), Login: combinedFixture().backup("123"), Device: device}, false, "alice", ""); err != nil {
		t.Fatal(err)
	}
	old, _ := p.session(p.accounts[0])
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: "123", Device: device}, nil
	}
	if err = p.renew(context.Background(), p.accounts[0], old); errorCode(err) != "instagram_database_unavailable" {
		t.Fatal("save failure hidden", err)
	}
	auth, _ := p.session(p.accounts[0])
	if auth != old {
		t.Fatal("unsaved session published")
	}
	// A caller waiting for another renewal can cancel without waiting for login.
	p.accounts[0].refreshGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = p.renew(ctx, p.accounts[0], old); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-p.accounts[0].refreshGate
}

func TestExpiredCookieAccountRecoversAfterRestart(t *testing.T) {
	store, _ := testStore(t)
	proxy := ""
	device, _ := newDeviceIdentity("pixel-8-android-15")
	expired := json.RawMessage(strings.ReplaceAll(string(cookieFixture("123")), "4102444800", "1"))
	if ok, err := store.Insert(context.Background(), storedAccount{accountConfig: accountConfig{ID: "a", Cookies: expired, Login: combinedFixture().backup("123"), ProxyURL: &proxy, Device: device}, Username: "alice", UserID: "123"}); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Ready() {
		t.Fatal("expired cookies hid recoverable account")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer IGT:2:renewed" {
			t.Error("expired/anonymous session sent")
		}
		fmt.Fprint(w, `{"status":"ok","items":[]}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	p.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{Authorization: "Bearer IGT:2:renewed", UserID: "123", Device: device}, nil
	}
	if _, err = p.Detail(context.Background(), "123"); err != nil {
		t.Fatal(err)
	}
	var cookiesNull bool
	if err = store.withDB(context.Background(), func(ctx context.Context, db *pgx.Conn) error {
		return db.QueryRow(ctx, `SELECT cookies IS NULL FROM `+store.accounts+` WHERE id='a'`).Scan(&cookiesNull)
	}); err != nil || !cookiesNull {
		t.Fatal("expired cookies survived renewal", err)
	}
}

func TestCookieRejectionAtImportSavesVerifiedPasswordSession(t *testing.T) {
	for _, wrongAccount := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongAccount), func(t *testing.T) {
			store, _ := testStore(t)
			pool, err := Open(Options{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
			defer server.Close()
			pool.apiOrigin = server.URL
			manager := NewAccountManager(pool, store, []string{""})
			manager.probe = func(context.Context, *http.Client) (string, error) { return "192.0.2.1", nil }
			logins := 0
			manager.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
				logins++
				device, _ := newDeviceIdentity("pixel-8-android-15")
				id := "123"
				if wrongAccount {
					id = "456"
				}
				return loginResult{Authorization: "Bearer IGT:2:password-session", Username: "alice", UserID: id, Device: device}, nil
			}
			results, err := manager.Import(context.Background(), []Credentials{combinedFixture()})
			if err != nil || logins != 1 {
				t.Fatal(err)
			}
			rows, err := store.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if wrongAccount {
				if results[0].Status != "failed" || len(rows) != 0 {
					t.Fatal("fallback switched accounts")
				}
			} else if results[0].Status != "imported" || len(rows) != 1 || rows[0].Cookies != nil || rows[0].Authorization != "Bearer IGT:2:password-session" || rows[0].Login == nil {
				t.Fatal("fallback persisted rejected cookies or lost backup")
			}
		})
	}
}
