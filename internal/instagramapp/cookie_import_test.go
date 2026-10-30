package instagramapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func cookieFixture(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`[{"domain":".instagram.com","name":"sessionid","value":"%s%%3Afixture%%3A0","path":"/","secure":true,"httpOnly":true,"expirationDate":4102444800,"storeId":"0"},{"domain":".instagram.com","name":"ds_user_id","value":"%s","path":"/","secure":true,"session":true}]`, id, id))
}

func TestCookieExportParsingAndValidation(t *testing.T) {
	raw := string(cookieFixture("123"))
	accounts, err := ParseImportFile(strings.NewReader("\ufeff\r\n" + raw + "\r\n\r\n" + string(cookieFixture("456")) + "\n"))
	if err != nil || len(accounts) != 2 {
		t.Fatal("cookie export did not parse", err)
	}
	auth, id, err := cookieAuthorization(accounts[0].Cookies)
	if err != nil || id != "123" {
		t.Fatal("cookie credentials missing", err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Bearer IGT:2:"))
	var value map[string]string
	if err != nil || json.Unmarshal(data, &value) != nil || value["sessionid"] != "123:fixture:0" {
		t.Fatal("encoded browser session was not decoded")
	}
	cases := []string{
		"", "[]", "null", raw + raw, raw + "garbage",
		strings.ReplaceAll(raw, ".instagram.com", "evil.example"),
		strings.ReplaceAll(raw, "4102444800", "1"),
		strings.ReplaceAll(raw, `"path":"/"`, `"path":"/other/"`),
		strings.ReplaceAll(raw, `"secure":true`, `"secure":true,"hostOnly":true`),
		strings.ReplaceAll(raw, `"name":"sessionid"`, `"name":"other"`),
		strings.ReplaceAll(raw, `123%3Afixture%3A0`, `bad%escape`),
		strings.Repeat(raw+"\n", MaxImportAccounts+1),
	}
	for n, input := range cases {
		if _, err := ParseCookieExport(strings.NewReader(input)); err == nil {
			t.Errorf("invalid case %d accepted", n)
		}
	}
	for _, c := range []Credentials{
		{Cookies: cookieFixture("123"), Username: "alice"},
		{Cookies: cookieFixture("123"), Password: "secret"},
		{Cookies: cookieFixture("123"), TOTPSecret: "secret"},
	} {
		if _, err := validateCredentials([]Credentials{c}); err == nil {
			t.Error("mixed credentials accepted")
		}
	}
}

func TestCookieImportPersistsAndRestoresWithoutBearer(t *testing.T) {
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer IGT:2:"))
		var auth map[string]string
		if err != nil || json.Unmarshal(raw, &auth) != nil || auth["sessionid"] != auth["ds_user_id"]+":fixture:0" {
			t.Error("wrong cookie authorization")
		}
		id := auth["ds_user_id"]
		if id == "789" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/api/v1/accounts/current_user/" {
			fmt.Fprintf(w, `{"status":"ok","user":{"pk":%s,"username":"user%s"}}`, id, id)
			return
		}
		fmt.Fprint(w, `{"status":"ok","items":[]}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	manager := NewAccountManager(p, store, []string{""})
	manager.probe = func(context.Context, *http.Client) (string, error) { return "192.0.2.1", nil }
	manager.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		t.Fatal("cookie import attempted password login")
		return loginResult{}, nil
	}
	input := []Credentials{{Cookies: cookieFixture("123")}, {Cookies: cookieFixture("789")}, {Cookies: cookieFixture("456")}}
	result, err := manager.Import(context.Background(), input)
	if err != nil || len(result) != 3 || result[0].Status != "imported" || result[1].Status != "failed" || result[2].Status != "imported" {
		t.Fatal("cookie batch result", result, err)
	}
	result, err = manager.Import(context.Background(), input[:1])
	if err != nil || result[0].Status != "duplicate" || calls != 3 {
		t.Fatal("duplicate reverified", err)
	}
	var count int
	err = store.withDB(context.Background(), func(ctx context.Context, db *pgx.Conn) error {
		return db.QueryRow(ctx, `SELECT count(*) FROM `+store.accounts+` WHERE android_authorization IS NULL AND jsonb_typeof(cookies)='array'`).Scan(&count)
	})
	if err != nil || count != 2 {
		t.Fatal("cookie import persisted a bearer or lost cookies", err)
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
	replacement.apiOrigin = server.URL
	if replacement.Count() != 2 {
		t.Fatal("restart lost cookie accounts")
	}
	if _, err = replacement.Detail(context.Background(), "123"); err != nil {
		t.Fatal("restored cookie metadata failed", err)
	}
	rows, err := restored.Load(context.Background())
	if err != nil || len(rows) != 2 || !strings.Contains(string(rows[0].Cookies), `"storeId"`) {
		t.Fatal("browser metadata not preserved", err)
	}
	// Existing password-session accounts coexist with nullable cookie authorization.
	proxy := ""
	ok, err := restored.Insert(context.Background(), storedAccount{accountConfig: accountConfig{ID: "password", Authorization: "Bearer IGT:2:fixture", ProxyURL: &proxy}, Username: "password", UserID: "999"})
	if err != nil || !ok {
		t.Fatal("password import regressed", err)
	}
}

func TestCookieVerificationRejectsMismatchAndThrottling(t *testing.T) {
	for _, code := range []int{200, 429} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			fmt.Fprint(w, `{"status":"ok","user":{"pk":456,"username":"wrong"}}`)
		}))
		_, err := loginCookies(context.Background(), server.Client(), server.URL, Credentials{Cookies: cookieFixture("123")})
		server.Close()
		want := "session_check_failed"
		if code == 429 {
			want = "upstream_rate_limited"
		}
		assertCode(t, err, want)
	}
}

func TestExpiredStoredCookiesDoNotBlockHealthyAccounts(t *testing.T) {
	p, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	expired := json.RawMessage(strings.ReplaceAll(string(cookieFixture("123")), "4102444800", "1"))
	if err := p.addAccount(accountConfig{ID: "expired", Cookies: expired}, false, "expired", ""); err != nil {
		t.Fatal(err)
	}
	if p.Ready() || !p.accounts[0].disabled {
		t.Fatal("expired session admitted")
	}
	if err := p.addAccount(accountConfig{ID: "healthy", Cookies: cookieFixture("456")}, false, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	a, release, err := p.acquire(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !p.Ready() || a.id != "instagram:healthy" {
		t.Fatal("expired session blocked healthy account")
	}
}
