package instagramapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func testStore(t *testing.T) (*Store, core.Config) {
	t.Helper()
	address := os.Getenv("TIKTOK_APP_TEST_DB_URL")
	if address == "" {
		address = os.Getenv("DB_URL")
	}
	if address == "" {
		env, _ := godotenv.Read("../../.env")
		address = env["DB_URL"]
	}
	if address == "" {
		t.Skip("TIKTOK_APP_TEST_DB_URL or DB_URL required for isolated PostgreSQL tests")
	}
	cfg := core.DefaultConfig()
	cfg.DBURL = address
	cfg.DBSchema = "go_instagram_test_" + strings.ReplaceAll(core.NewID(), "-", "")
	db, e := pgx.Connect(context.Background(), address)
	if e != nil {
		t.Fatal("database connection failed")
	}
	schema := pgx.Identifier{cfg.DBSchema}.Sanitize()
	if _, e = db.Exec(context.Background(), "CREATE SCHEMA "+schema); e != nil {
		t.Fatal("create schema failed")
	}
	t.Cleanup(func() {
		db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		db.Close(context.Background())
	})
	store, e := OpenStore(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	return store, cfg
}
func TestImportBalancesSharedRoutesAndSurvivesRestart(t *testing.T) {
	store, cfg := testStore(t)
	p, e := Open(Options{Store: store, ActiveAccounts: 2})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	manager := NewAccountManager(p, store, []string{"http://route-a.invalid:80", "http://route-b.invalid:80"})
	manager.probe = func(context.Context, *http.Client) (string, error) { return "192.0.2.1", nil }
	calls := 0
	manager.login = func(_ context.Context, _ *http.Client, _ string, c Credentials) (loginResult, error) {
		calls++
		device, err := newDeviceIdentity(c.DeviceProfile)
		return loginResult{Authorization: "Bearer IGT:2:" + c.Username, Username: c.Username, UserID: c.Username, Device: device}, err
	}
	credentials := []Credentials{{Username: "old", Password: "x", DeviceProfile: "pixel-8-android-15"}, {Username: "middle", Password: "x", DeviceProfile: "galaxy-s23-android-14"}, {Username: "new", Password: "x"}}
	results, e := manager.Import(context.Background(), credentials)
	if e != nil || len(results) != 3 {
		t.Fatal(e)
	}
	for _, r := range results {
		if r.Status != "imported" {
			t.Fatal(r)
		}
	}
	rows, e := store.Load(context.Background())
	if e != nil || len(rows) != 3 || *rows[0].ProxyURL != *rows[2].ProxyURL || *rows[0].ProxyURL == *rows[1].ProxyURL {
		t.Fatal("routes not balanced/shared", e)
	}
	for n, a := range rows {
		if a.Device == nil || a.Device.validate() != nil || *a.Device != *p.accounts[n].device {
			t.Fatal("import lost device identity")
		}
		if credentials[n].DeviceProfile != "" && a.Device.ProfileID != credentials[n].DeviceProfile {
			t.Fatal("import changed selected profile")
		}
	}
	result, e := manager.Import(context.Background(), credentials[:1])
	if e != nil || result[0].Status != "duplicate" || calls != 3 {
		t.Fatal("duplicate relogged", e)
	}
	status := manager.Status()
	if status[0].State != "active" || status[1].State != "active" || status[2].State != "reserve" {
		t.Fatal(status)
	}
	for n := 0; n < 6; n++ {
		a, release, e := p.acquire(context.Background(), "", nil)
		if e != nil {
			t.Fatal(e)
		}
		release()
		if a.username == "new" {
			t.Fatal("reserve used prematurely")
		}
	}
	// Exercise real HTTP failure classification and durable automatic replacement.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer IGT:2:")
		for _, a := range rows {
			if a.Username == username && (r.Header.Get("User-Agent") != a.Device.userAgent() || r.Header.Get("X-IG-Device-ID") != a.Device.DeviceID) {
				t.Error("metadata changed account device")
			}
		}
		if r.Header.Get("Authorization") == "Bearer IGT:2:old" {
			w.WriteHeader(401)
		} else {
			fmt.Fprint(w, `{"status":"ok","items":[]}`)
		}
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	p.mu.Lock()
	p.next = 0
	for _, a := range p.accounts {
		a.client = server.Client()
	}
	p.mu.Unlock()
	if _, e = p.Detail(context.Background(), "123"); e != nil {
		t.Fatal(e)
	}
	status = manager.Status()
	if status[0].State != "dead" || status[2].State != "active" {
		t.Fatal(status)
	}
	p.Close()
	store.Close()
	restored, e := OpenStore(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	replacement, e := Open(Options{Store: restored, ActiveAccounts: 2})
	if e != nil {
		t.Fatal(e)
	}
	defer replacement.Close()
	if replacement.Count() != 3 || !replacement.accounts[0].disabled {
		t.Fatal("dead account revived on restart")
	}
	for n, a := range replacement.accounts {
		if a.device == nil || *a.device != *rows[n].Device {
			t.Fatal("restart changed account device")
		}
	}
	for n := 0; n < 4; n++ {
		a, release, e := replacement.acquire(context.Background(), "", nil)
		if e != nil {
			t.Fatal(e)
		}
		release()
		if a.username == "old" {
			t.Fatal("dead account used")
		}
	}
}
func TestCooldownDoesNotConsumeReserve(t *testing.T) {
	p := newTestPool(t, 3, 1)
	p.options.ActiveAccounts = 2
	p.accounts[0].cooldown = time.Now().Add(time.Minute)
	p.accounts[1].cooldown = time.Now().Add(time.Minute)
	_, _, e := p.acquire(context.Background(), "", nil)
	assertCode(t, e, "upstream_rate_limited")
}
func TestAttemptRowsAndFailedDeathPersistence(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	for _, legacy := range []bool{false, true, true} {
		_, e := store.Attempt(ctx, "shortcode", legacy, func() (core.InstagramExtraction, error) { return core.InstagramExtraction{}, core.Upstream("fixture") })
		if e == nil {
			t.Fatal("lost upstream failure")
		}
	}
	var total, legacy int
	if e := store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		return db.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE legacy_used) FROM `+store.attempts+` WHERE succeeded=false`).Scan(&total, &legacy)
	}); e != nil || total != 3 || legacy != 2 {
		t.Fatal("attempt metrics incorrect", e, total, legacy)
	}
	p, e := Open(Options{Store: store, AccountsFile: newTestPool(t, 1, 1).options.AccountsFile})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	// Missing table mimics an unavailable schema. Pending death must fail closed.
	if e = store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, "ALTER TABLE "+store.accounts+" RENAME TO saved_accounts")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = store.MarkDead(ctx, "a", "fixture"); e == nil {
		t.Fatal("death write should fail")
	}
	if p.Ready() {
		t.Fatal("ready with pending death")
	}
	if _, e = p.Detail(ctx, "123"); e == nil {
		t.Fatal("admitted while death could not persist")
	}
	// Restore using the qualified table prefix, then flush and verify.
	prefix := store.accounts[:strings.LastIndex(store.accounts, ".")+1]
	if e = store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, "ALTER TABLE "+prefix+"saved_accounts RENAME TO scrap_instagram_accounts")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = store.Flush(ctx); e != nil {
		t.Fatal(e)
	}
	rows, e := store.Load(ctx)
	if e != nil || !rows[0].Dead {
		t.Fatal("death was not recovered", e)
	}
}

func TestImportStopsBatchOnSessionRateLimitAndSkipsBrokenProxy(t *testing.T) {
	store, _ := testStore(t)
	p, e := Open(Options{Store: store, ActiveAccounts: 10})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	manager := NewAccountManager(p, store, []string{"http://bad.invalid:80", "http://good.invalid:80"})
	probes := 0
	manager.probe = func(context.Context, *http.Client) (string, error) {
		probes++
		if probes == 1 {
			return "", loginFailure("proxy_check_failed")
		}
		return "192.0.2.1", nil
	}
	logins := 0
	verification := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/accounts/current_user/" {
			t.Error("unexpected verification path")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer verification.Close()
	manager.login = func(context.Context, *http.Client, string, Credentials) (loginResult, error) {
		logins++
		return verifyLogin(context.Background(), verification.Client(), verification.URL, "Bearer IGT:2:fixture", nil)
	}
	result, e := manager.Import(context.Background(), []Credentials{{Username: "a", Password: "x"}, {Username: "b", Password: "y"}})
	if e != nil || len(result) != 2 || result[0].Status != "failed" || result[1].Status != "skipped" || logins != 1 || probes != 2 || p.Count() != 0 {
		t.Fatal("rate-limited batch continued", e, result)
	}
	if result[0].ErrorCode != "upstream_rate_limited" || result[1].ErrorCode != "batch_rate_limited" {
		t.Fatal("lost verification rate limit classification", result)
	}
	rows, e := store.Load(context.Background())
	if e != nil || len(rows) != 0 {
		t.Fatal("failed login saved", e)
	}
}
