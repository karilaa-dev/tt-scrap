package instagramapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func TestMetadataCooldownSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 2, 1).options.AccountsFile, ActiveAccounts: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	_, err = p.Detail(ctx, "123")
	assertCode(t, err, "upstream_rate_limited")
	rows, err := store.Load(ctx)
	if err != nil || len(rows) != 2 || time.Until(rows[0].CooldownUntil) < 119*time.Minute {
		t.Fatal("metadata throttle deadline was not saved", err)
	}
	deadline := rows[0].CooldownUntil
	p.Close()
	store.Close()
	reopened, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := Open(Options{Store: reopened, ActiveAccounts: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restored.apiOrigin = server.URL
	if !restored.accounts[0].cooldown.Equal(deadline) || !restored.Ready() {
		t.Fatal("restart lost cooldown or treated temporary throttle as unhealthy service")
	}
	_, err = restored.Detail(ctx, "456")
	assertCode(t, err, "upstream_rate_limited")
	if calls.Load() != 1 {
		t.Fatal("restart sent metadata during cooldown or promoted reserve")
	}
	// A previously signed CDN URL does not use the challenged metadata endpoint.
	called := false
	if err = restored.WithMedia(ctx, "instagram:a", func(core.OpenAppMedia) error { called = true; return nil }); err != nil || !called {
		t.Fatal("metadata cooldown blocked a pinned CDN transfer", err)
	}
}

func TestCooldownPersistenceFailureKeepsLongestDeadline(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	p, err := Open(Options{Store: store, AccountsFile: newTestPool(t, 2, 1).options.AccountsFile})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, err := db.Exec(ctx, "ALTER TABLE "+store.accounts+" RENAME TO cooldown_saved_accounts")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Microsecond)
	for _, until := range []time.Time{deadline, deadline.Add(-time.Hour)} {
		if err = store.MarkCooldown(ctx, "a", until); errorCode(err) != "instagram_database_unavailable" {
			t.Fatal("missing schema did not retain a failed cooldown write", err)
		}
	}
	if err = store.MarkDead(ctx, "b", "feedback_required"); errorCode(err) != "instagram_database_unavailable" {
		t.Fatal("missing schema did not retain a failed restriction write", err)
	}
	if p.Ready() {
		t.Fatal("health write failure did not block metadata admission")
	}
	_, _, err = p.acquire(ctx, "", nil)
	assertCode(t, err, "instagram_database_unavailable")
	prefix := store.accounts[:strings.LastIndex(store.accounts, ".")+1]
	if err = store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, err := db.Exec(ctx, "ALTER TABLE "+prefix+"cooldown_saved_accounts RENAME TO scrap_instagram_accounts")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// A successful later response cannot shorten a deadline already in the DB.
	if err = store.MarkCooldown(ctx, "a", deadline.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
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
	if !restored.accounts[0].cooldown.Equal(deadline) || !restored.accounts[1].disabled || restored.accounts[1].deadReason != "feedback_required" {
		t.Fatal("restart lost pending health state or extended the original cooldown")
	}
}

func TestExpiredCooldownAllowsMetadataAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	fixture := newTestPool(t, 1, 1)
	if err := store.Seed(ctx, []accountConfig{{ID: "a", Authorization: "Bearer IGT:2:a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCooldown(ctx, "a", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store, AccountsFile: fixture.options.AccountsFile})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	a, release, err := p.acquire(ctx, "", nil)
	if err != nil || a.id != "instagram:a" {
		t.Fatal("expired persisted cooldown prevented admission", err)
	}
	release()
}

func TestAccountUpdateRespectsPersistedCooldown(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	proxy := ""
	seed := storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:original", ProxyURL: &proxy}, UserID: "123", Username: "alice"}
	if ok, err := store.Insert(ctx, seed); err != nil || !ok {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if err := store.MarkCooldown(ctx, "a", deadline); err != nil {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"status":"ok","user":{"username":"alice","pk":123}}`)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	m := NewAccountManager(p, store, nil)
	result, err := m.Import(ctx, []Credentials{{Cookies: cookieFixture("123")}})
	if err != nil || len(result) != 1 || result[0].Status != "failed" || result[0].ErrorCode != "upstream_rate_limited" || calls.Load() != 0 {
		t.Fatal("cookie replacement sent verification during account cooldown", result, err)
	}
	result, err = m.Import(ctx, []Credentials{{Username: "alice", Password: "updated-backup"}})
	if err != nil || len(result) != 1 || result[0].Status != "updated" || calls.Load() != 0 {
		t.Fatal("offline backup update was blocked or made a network request", result, err)
	}
	rows, err := store.Load(ctx)
	if err != nil || rows[0].Authorization != seed.Authorization || rows[0].Login == nil || rows[0].Login.Password != "updated-backup" || !rows[0].CooldownUntil.Equal(deadline) || !p.accounts[0].cooldown.Equal(deadline) {
		t.Fatal("backup update lost cooldown or changed session", err)
	}
	// Even the storage-level replacement cannot silently erase an account throttle.
	if err = store.ReplaceSession(ctx, "a", "123", loginResult{Authorization: "Bearer IGT:2:replacement"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Load(ctx)
	if err != nil || !rows[0].CooldownUntil.Equal(deadline) {
		t.Fatal("session replacement cleared a throttle", err)
	}
}

func TestCooldownSchemaMigrationPreservesAccounts(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	if err := store.Seed(ctx, []accountConfig{{ID: "a", Authorization: "Bearer IGT:2:a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, err := db.Exec(ctx, "ALTER TABLE "+store.accounts+" DROP COLUMN cooldown_until")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Load(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "a" || !rows[0].CooldownUntil.IsZero() || rows[0].Dead {
		t.Fatal("cooldown migration changed existing account state", err)
	}
}

func TestRejectedReplacementRetainsThrottleAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, cfg := testStore(t)
	proxy := ""
	seed := storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:original", ProxyURL: &proxy}, Username: "alice", UserID: "123"}
	if ok, err := store.Insert(ctx, seed); err != nil || !ok {
		t.Fatal(err)
	}
	p, err := Open(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(429)
	}))
	defer server.Close()
	p.apiOrigin = server.URL
	manager := NewAccountManager(p, store, nil)
	result, err := manager.Import(ctx, []Credentials{{Cookies: cookieFixture("123")}})
	if err != nil || len(result) != 1 || result[0].ErrorCode != "upstream_rate_limited" {
		t.Fatal(result, err)
	}
	rows, err := store.Load(ctx)
	if err != nil || rows[0].Authorization != seed.Authorization || rows[0].Dead || time.Until(rows[0].CooldownUntil) < 119*time.Minute {
		t.Fatal("failed replacement lost throttle or damaged existing session", err)
	}
	replacement, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	reopened, err := Open(Options{Store: replacement})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.apiOrigin = server.URL
	result, err = NewAccountManager(reopened, replacement, nil).Import(ctx, []Credentials{{Cookies: cookieFixture("123")}})
	if err != nil || len(result) != 1 || result[0].ErrorCode != "upstream_rate_limited" || calls.Load() != 1 {
		t.Fatal("replacement retried during persisted throttle", result, err, calls.Load())
	}
}
