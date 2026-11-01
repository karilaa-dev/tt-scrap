package instagramapp

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSessionStateCheckpointSurvivesRestartAndRejectsOldAuthorization(t *testing.T) {
	for _, cookieAccount := range []bool{false, true} {
		name := "bearer"
		if cookieAccount {
			name = "cookie"
		}
		t.Run(name, func(t *testing.T) {
			store, cfg := testStore(t)
			ctx := context.Background()
			proxy := ""
			auth := "Bearer IGT:2:old"
			a := storedAccount{accountConfig: accountConfig{
				ID: "a", Authorization: auth, ProxyURL: &proxy,
				Session: &SessionState{MID: "initial-mid", WWWClaim: "initial-claim"},
			}, Username: "alice", UserID: "123"}
			if cookieAccount {
				a.Cookies = cookieFixture("123")
				a.Authorization = ""
				var err error
				auth, _, err = cookieAuthorization(a.Cookies)
				if err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := store.Insert(ctx, a); err != nil || !ok {
				t.Fatal("insert failed", err)
			}
			expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			checkpoint := &SessionState{MID: "observed-mid", WWWClaim: "observed-claim", RUR: "observed-rur", Cookies: []SessionCookie{{
				Origin: "https://i.instagram.com",
				Cookie: http.Cookie{Name: "csrftoken", Value: "observed-csrf", Path: "/", Secure: true, HttpOnly: true, Expires: expires},
			}}}
			if err := store.SaveSessionState(ctx, "a", auth, checkpoint); err != nil {
				t.Fatal(err)
			}
			store.Close()
			restored, err := OpenStore(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			rows, err := restored.Load(ctx)
			if err != nil || len(rows) != 1 || rows[0].Session == nil || rows[0].Session.MID != checkpoint.MID || rows[0].Session.WWWClaim != checkpoint.WWWClaim || rows[0].Session.RUR != checkpoint.RUR {
				t.Fatal("server context did not survive restart", err)
			}
			cookies := rows[0].Session.Cookies
			if len(cookies) != 1 || cookies[0].Cookie.Value != "observed-csrf" || !cookies[0].Cookie.Expires.Equal(expires) || !cookies[0].Cookie.Secure || cookies[0].Origin != "https://i.instagram.com" {
				t.Fatal("stored cookie scope or expiry changed on restart")
			}
			fresh := loginResult{Authorization: "Bearer IGT:2:new", UserID: "123", Session: &SessionState{MID: "new-login-mid"}}
			if err := restored.SaveSession(ctx, "a", "123", fresh); err != nil {
				t.Fatal(err)
			}
			if err := restored.SaveSessionState(ctx, "a", auth, &SessionState{MID: "late-old-response"}); err != nil {
				t.Fatal(err)
			}
			rows, err = restored.Load(ctx)
			if err != nil || len(rows) != 1 || rows[0].Session == nil || rows[0].Session.MID != "new-login-mid" || rows[0].Authorization != fresh.Authorization || len(rows[0].Cookies) != 0 {
				t.Fatal("old response changed renewed session", err)
			}
		})
	}
}

func TestReplacingSessionSavesContextWithoutErasingCooldown(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	proxy := ""
	if ok, err := store.Insert(ctx, storedAccount{accountConfig: accountConfig{ID: "a", Authorization: "Bearer IGT:2:old", ProxyURL: &proxy, Session: &SessionState{MID: "old-mid"}}, Username: "alice", UserID: "123"}); err != nil || !ok {
		t.Fatal("insert failed", err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := store.MarkHealth(ctx, "a", "session_expired", until); err != nil {
		t.Fatal(err)
	}
	replacement := loginResult{Authorization: "Bearer IGT:2:replacement", UserID: "123", Session: &SessionState{MID: "replacement-mid", WWWClaim: "replacement-claim"}}
	if err := store.ReplaceSession(ctx, "a", "123", replacement, nil, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Load(ctx)
	if err != nil || len(rows) != 1 || rows[0].Session == nil || rows[0].Session.MID != "replacement-mid" || rows[0].Session.WWWClaim != "replacement-claim" || rows[0].Dead || !rows[0].CooldownUntil.Equal(until) {
		t.Fatal("replacement did not preserve context and account cooldown", err)
	}
}

func TestSessionStateMigrationPreservesLegacyAccountsAndSeedContext(t *testing.T) {
	store, cfg := testStore(t)
	ctx := context.Background()
	if err := store.Seed(ctx, []accountConfig{{ID: "legacy", Authorization: "Bearer IGT:2:legacy"}}); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-context schema within this test's isolated database schema.
	if err := store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, err := db.Exec(ctx, "ALTER TABLE "+store.accounts+" DROP COLUMN session_state")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	restored, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Seed(ctx, []accountConfig{{ID: "new", Authorization: "Bearer IGT:2:new", Session: &SessionState{MID: "seeded-mid", WWWClaim: "seeded-claim"}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := restored.Load(ctx)
	if err != nil || len(rows) != 2 || rows[0].Authorization != "Bearer IGT:2:legacy" || rows[0].Session != nil || rows[1].Session == nil || rows[1].Session.MID != "seeded-mid" || rows[1].Session.WWWClaim != "seeded-claim" {
		t.Fatal("migration changed legacy session or discarded seed context", err)
	}
}
