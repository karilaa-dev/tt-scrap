package instagramapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestImportCredentialsIntoExistingAccount(t *testing.T) {
	for _, source := range []string{"cookies", "password"} {
		for _, target := range []string{"cookies", "password", "both"} {
			t.Run(source+"_to_"+target, func(t *testing.T) {
				ctx := context.Background()
				store, cfg := testStore(t)
				p, err := Open(Options{Store: store})
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				var expectedDevice *DeviceIdentity
				checks := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					checks++
					if expectedDevice != nil && r.Header.Get("X-IG-Device-ID") != expectedDevice.DeviceID {
						t.Error("existing account device changed")
					}
					fmt.Fprint(w, `{"status":"ok","user":{"username":"alice","pk":123}}`)
				}))
				defer server.Close()
				p.apiOrigin = server.URL
				m := NewAccountManager(p, store, []string{""})
				probes, logins := 0, 0
				m.probe = func(context.Context, *http.Client) (string, error) { probes++; return "192.0.2.1", nil }
				m.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
					logins++
					device, _ := newDeviceIdentity("galaxy-s23-android-14")
					return loginResult{Authorization: "Bearer IGT:2:original", Username: "alice", UserID: "123", Device: device}, nil
				}
				initial := Credentials{Cookies: cookieFixture("123")}
				if source == "password" {
					initial = Credentials{Username: "alice", Password: "old-password", TOTPSecret: fallbackTOTP}
				}
				first, err := m.Import(ctx, []Credentials{initial})
				if err != nil || first[0].Status != "imported" {
					t.Fatal(first, err)
				}
				before, err := store.Load(ctx)
				if err != nil {
					t.Fatal(err)
				}
				expectedDevice = before[0].Device
				probes, logins, checks = 0, 0, 0
				replacement := json.RawMessage(strings.ReplaceAll(string(cookieFixture("123")), "fixture", "replacement"))
				incoming := Credentials{Cookies: replacement}
				if target == "password" {
					incoming = Credentials{Username: " ALICE ", Password: "new-password", TOTPSecret: fallbackTOTP}
				}
				if target == "both" {
					incoming = Credentials{Cookies: replacement, Username: "alice", Password: "new-password", TOTPSecret: fallbackTOTP}
				}
				result, err := m.Import(ctx, []Credentials{incoming})
				if err != nil || result[0].Status != "updated" || result[0].AccountID != first[0].AccountID || p.Count() != 1 {
					t.Fatal("existing account was not updated", result, err)
				}
				if probes != 0 || logins != 0 {
					t.Fatal("credential update selected another route or attempted password login")
				}
				wantChecks := 1
				if target == "password" {
					wantChecks = 0
				}
				if checks != wantChecks {
					t.Fatal("cookie update was not verified", checks)
				}
				rows, err := store.Load(ctx)
				if err != nil || len(rows) != 1 {
					t.Fatal(err)
				}
				after := rows[0]
				if after.ID != before[0].ID || after.IP != before[0].IP || *after.ProxyURL != *before[0].ProxyURL || !reflect.DeepEqual(after.Device, before[0].Device) {
					t.Fatal("credential update changed account identity/route/device")
				}
				if target == "password" {
					if !sameCookies(after.Cookies, before[0].Cookies) && source == "cookies" || after.Authorization != before[0].Authorization {
						t.Fatal("backup update replaced active session")
					}
				} else if !sameCookies(after.Cookies, replacement) || after.Authorization != "" {
					t.Fatal("cookie update retained bearer or lost cookies")
				}
				wantBackup := before[0].Login
				if target != "cookies" {
					wantBackup = Credentials{Username: strings.TrimSpace(incoming.Username), Password: "new-password", TOTPSecret: fallbackTOTP}.backup("123")
				}
				if !reflect.DeepEqual(after.Login, wantBackup) {
					t.Fatal("credential update lost backup")
				}
				auth, _ := p.session(p.accounts[0])
				if target != "password" {
					want, _, _ := cookieAuthorization(replacement)
					if auth != want {
						t.Fatal("RAM did not use replacement cookies")
					}
				}
				duplicate, err := m.Import(ctx, []Credentials{incoming})
				if err != nil || duplicate[0].Status != "duplicate" || checks != wantChecks {
					t.Fatal("unchanged import reverified", err)
				}
				p.Close()
				store.Close()
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
				restoredAuth, _ := restored.session(restored.accounts[0])
				if restored.Count() != 1 || restoredAuth != auth || !reflect.DeepEqual(restored.accounts[0].login, wantBackup) {
					t.Fatal("restart lost updated credentials")
				}
			})
		}
	}
}

func TestCookieUpdateFailureKeepsExistingCredentials(t *testing.T) {
	for _, status := range []int{401, 429, 503, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store, _ := testStore(t)
			proxy := ""
			device, _ := newDeviceIdentity("pixel-8-android-15")
			seed := storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:original", Login: combinedFixture().backup("123"), Device: device, ProxyURL: &proxy}, UserID: "123", Username: "alice"}
			if ok, err := store.Insert(context.Background(), seed); err != nil || !ok {
				t.Fatal(err)
			}
			p, err := Open(Options{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"status":"ok","user":{"username":"other","pk":456}}`)
			}))
			defer server.Close()
			p.apiOrigin = server.URL
			m := NewAccountManager(p, store, nil)
			result, err := m.Import(context.Background(), []Credentials{{Cookies: cookieFixture("123")}, {Cookies: cookieFixture("456")}})
			if err != nil || result[0].Status != "failed" {
				t.Fatal("failed cookie verification accepted", result, err)
			}
			if status == 429 && result[1].Status != "skipped" {
				t.Fatal("rate-limited update continued batch")
			}
			rows, err := store.Load(context.Background())
			if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], seed) {
				t.Fatal("failed update changed saved credentials", err)
			}
			auth, _ := p.session(p.accounts[0])
			if auth != seed.Authorization {
				t.Fatal("failed update changed RAM session")
			}
		})
	}
}

func TestPasswordImportMatchesExistingCookieAccountByVerifiedID(t *testing.T) {
	store, _ := testStore(t)
	proxy := ""
	if ok, err := store.Insert(context.Background(), storedAccount{accountConfig: accountConfig{ID: "a", Cookies: cookieFixture("123"), ProxyURL: &proxy}, Username: "alice", UserID: "123"}); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	m := NewAccountManager(p, store, []string{""})
	m.probe = func(context.Context, *http.Client) (string, error) { return "192.0.2.1", nil }
	m.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		return loginResult{Authorization: "Bearer IGT:2:verified", UserID: "123", Username: "alice"}, nil
	}
	result, err := m.Import(context.Background(), []Credentials{{Username: "alice@example.invalid", Password: "backup", TOTPSecret: fallbackTOTP}})
	if err != nil || result[0].Status != "updated" || result[0].AccountID != "a" || p.Count() != 1 {
		t.Fatal("verified alias lost backup to duplicate detection", result, err)
	}
	rows, err := store.Load(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Login == nil || rows[0].Login.Username != "alice@example.invalid" || !sameCookies(rows[0].Cookies, cookieFixture("123")) {
		t.Fatal("alias update lost cookies or login", err)
	}
}

func TestCookieUpdateWaitsForOldRequestsBeforeRevivingAccount(t *testing.T) {
	store, _ := testStore(t)
	proxy := ""
	device, _ := newDeviceIdentity("pixel-8-android-15")
	if ok, err := store.Insert(context.Background(), storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:old", ProxyURL: &proxy, Device: device}, Username: "alice", UserID: "123"}); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	oldStarted, finishOld := make(chan struct{}), make(chan struct{})
	verified := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/accounts/current_user/" {
			verified <- struct{}{}
			fmt.Fprint(w, `{"status":"ok","user":{"username":"alice","pk":123}}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer IGT:2:old" {
			close(oldStarted)
			<-finishOld
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"status":"ok","items":[]}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldDone := make(chan error, 1)
	go func() { _, err := p.Detail(ctx, "123"); oldDone <- err }()
	<-oldStarted
	m := NewAccountManager(p, store, nil)
	updateDone := make(chan error, 1)
	p.mu.Lock()
	changing := p.changed
	p.mu.Unlock()
	go func() {
		r, err := m.Import(ctx, []Credentials{{Cookies: cookieFixture("123")}})
		if err == nil && r[0].Status != "updated" {
			err = fmt.Errorf("cookie update: %s", r[0].Status)
		}
		updateDone <- err
	}()
	select {
	case <-changing:
	case <-ctx.Done():
		close(finishOld)
		t.Fatal("update did not start")
	}
	premature := false
	select {
	case <-verified:
		premature = true
	default:
	}
	close(finishOld)
	if premature {
		t.Error("cookie update did not wait for old metadata")
	}
	if err := <-oldDone; err == nil {
		t.Fatal("old request unexpectedly succeeded")
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	rows, err := store.Load(ctx)
	if err != nil || rows[0].Dead || p.accounts[0].disabled {
		t.Fatal("old failure disabled new cookie session", err)
	}
	if _, err = p.Detail(ctx, "123"); err != nil {
		t.Fatal("replacement session unavailable", err)
	}
}

func TestCancelledAccountUpdateRestoresAdmission(t *testing.T) {
	p := newTestPool(t, 1, 1)
	a, release, err := p.acquire(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done, err := p.beginUpdate(ctx, a)
	if err == nil || done != nil {
		t.Fatal("cancelled update was admitted")
	}
	release()
	p.mu.Lock()
	blocked := a.updating || p.updating != 0
	p.mu.Unlock()
	if blocked {
		t.Fatal("cancelled update left admission blocked")
	}
	_, release, err = p.acquire(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
