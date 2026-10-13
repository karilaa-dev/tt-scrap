package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func testDatabase(t testing.TB) (core.Config, *pgx.Conn) {
	t.Helper()
	address := os.Getenv("TIKTOK_APP_TEST_DB_URL")
	if address == "" {
		address = os.Getenv("DB_URL")
	}
	if address == "" {
		if env, e := godotenv.Read("../../.env"); e == nil {
			address = env["TIKTOK_APP_TEST_DB_URL"]
			if address == "" {
				address = env["DB_URL"]
			}
		}
	}
	if address == "" {
		t.Skip("DB_URL is required for isolated PostgreSQL identity tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, e := pgx.Connect(ctx, address)
	if e != nil {
		t.Fatal("test PostgreSQL connection failed")
	}
	nonce := make([]byte, 12)
	if _, e = rand.Read(nonce); e != nil {
		t.Fatal(e)
	}
	cfg := core.DefaultConfig()
	cfg.DBURL = address
	cfg.DBSchema = "go_identity_test_" + hex.EncodeToString(nonce)
	cfg.MaxQueuedRequests = 2
	schema := pgx.Identifier{cfg.DBSchema}.Sanitize()
	if _, e = db.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		db.Close(context.Background())
		t.Fatal("test schema creation failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, e := db.Exec(ctx, "DROP TABLE IF EXISTS "+schema+".scrap_identities")
		if e == nil {
			_, e = db.Exec(ctx, "DROP TABLE IF EXISTS "+schema+".scrap_proxies")
		}
		if e == nil {
			_, e = db.Exec(ctx, "DROP SCHEMA "+schema)
		}
		db.Close(ctx)
		if e != nil {
			t.Error("test schema cleanup failed")
		}
	})
	return cfg, db
}
func openTestPool(t testing.TB, cfg core.Config, addresses ...string) *Pool {
	t.Helper()
	p, e := Open(context.Background(), cfg, addresses)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := p.Close(); e != nil {
			t.Error(e)
		}
	})
	return p
}
func mustAcquire(t *testing.T, p *Pool, id string) *lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	l, e := p.acquire(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func finishLease(t *testing.T, l *lease, e error) {
	t.Helper()
	if err := l.finish(e); err != nil {
		t.Fatal(err)
	}
}
func waitStats(t *testing.T, p *Pool, predicate func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate(p.Stats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pool did not reach expected state: %+v", p.Stats())
}

func TestPoolLazyBalancedTenPerProxyAndMemoryReads(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "http://user:secret@proxy-one.example:8080", "http://proxy-two.example:8080")
	initial := p.Stats()
	if initial.Proxies != 2 || initial.Identities != 0 || !initial.Ready {
		t.Fatalf("startup was not lazy: %+v", initial)
	}
	var count int
	if e := db.QueryRow(context.Background(), "SELECT count(*) FROM "+p.identitiesTable).Scan(&count); e != nil || count != 0 {
		t.Fatalf("startup created identity rows: %d %v", count, e)
	}
	pinned := map[string]string{}
	counts := map[string]int{}
	for range 43 {
		l := mustAcquire(t, p, "")
		key := l.entry.proxy.key
		id := l.entry.id
		if old, ok := pinned[id]; ok && old != key {
			t.Fatal("identity changed its proxy")
		}
		pinned[id] = key
		counts[key]++
		if len(l.entry.session.Identity.OpenUDID) < 13 {
			t.Fatal("lazy identity missing local device state")
		}
		finishLease(t, l, nil)
	}
	stats := p.Stats()
	if stats.Identities != 20 || stats.Busy != 0 || stats.Registered != 0 {
		t.Fatalf("wrong lazily filled pool:%+v", stats)
	}
	if stats.Reads != initial.Reads {
		t.Fatalf("selection queried database: startup%d after%d", initial.Reads, stats.Reads)
	}
	if stats.Writes != 43 {
		t.Fatalf("expected one identity write after each operation, got%d", stats.Writes)
	}
	low, high := 1000, 0
	for _, e := range p.entries {
		if int(e.assignments) < low {
			low = int(e.assignments)
		}
		if int(e.assignments) > high {
			high = int(e.assignments)
		}
	}
	if high-low > 1 {
		t.Fatalf("identity assignments uneven:%d..%d", low, high)
	}
	if delta := counts["proxy-one.example:8080"] - counts["proxy-two.example:8080"]; delta < -1 || delta > 1 {
		t.Fatalf("proxy assignment uneven:%v", counts)
	}
	rows, e := db.Query(context.Background(), "SELECT proxy_id,count(*) FROM "+p.identitiesTable+" GROUP BY proxy_id")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	servers := 0
	for rows.Next() {
		var proxyID int64
		var identities int
		if e = rows.Scan(&proxyID, &identities); e != nil {
			t.Fatal(e)
		}
		if identities != 10 {
			t.Fatalf("proxy%d has%d identities", proxyID, identities)
		}
		servers++
	}
	if rows.Err() != nil || servers != 2 {
		t.Fatal("per-proxy database slots differ")
	}
}
func TestPoolPersistOnlyAfterOperationAndRestart(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	udid := l.entry.session.Identity.ClientUDID
	l.entry.session.StoreRegion = "fixture"
	var rows int
	if e := db.QueryRow(context.Background(), "SELECT count(*) FROM "+p.identitiesTable).Scan(&rows); e != nil || rows != 0 {
		t.Fatal("identity was persisted before operation finished")
	}
	finishLease(t, l, nil)
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	restored := openTestPool(t, cfg)
	if stats := restored.Stats(); stats.Identities != 1 || stats.Proxies != 1 {
		t.Fatalf("restart did not reload database:%+v", stats)
	}
	next := mustAcquire(t, restored, id)
	if next.entry.session.Identity.ClientUDID != udid || next.entry.session.StoreRegion != "fixture" || next.entry.proxy.key != "direct" {
		t.Fatal("restart lost identity/proxy/session state")
	}
	finishLease(t, next, nil)
}
func TestPoolRejectsSecondOwnerAndReleasesLockOnClose(t *testing.T) {
	cfg, _ := testDatabase(t)
	first := openTestPool(t, cfg, "")
	second, e := Open(context.Background(), cfg, nil)
	if e == nil {
		second.Close()
		t.Fatal("a second process acquired RAM pool ownership")
	}
	if !first.Ready() {
		t.Fatal("second owner damaged first")
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	third := openTestPool(t, cfg)
	if !third.Ready() {
		t.Fatal("owner lock was not released")
	}
}
func TestPoolConcurrentCapacityQueueLimitAndCancellation(t *testing.T) {
	cfg, _ := testDatabase(t)
	cfg.MaxQueuedRequests = 1
	p := openTestPool(t, cfg, "")
	held := make([]*lease, 10)
	seen := map[string]bool{}
	for i := range held {
		held[i] = mustAcquire(t, p, "")
		if seen[held[i].entry.id] {
			t.Fatal("busy identity leased concurrently")
		}
		seen[held[i].entry.id] = true
	}
	if stats := p.Stats(); stats.Identities != 10 || stats.Busy != 10 {
		t.Fatalf("capacity not ten:%+v", stats)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		l, e := p.acquire(ctx, "")
		if e == nil {
			e = l.finish(nil)
		}
		result <- e
	}()
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	overflowCtx, overflowCancel := context.WithTimeout(context.Background(), time.Second)
	defer overflowCancel()
	if l, e := p.acquire(overflowCtx, ""); e == nil {
		l.finish(nil)
		t.Fatal("queue limit accepted overflow")
	}
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("queued cancellation:%v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued cancellation did not wake")
	}
	waitStats(t, p, func(s Stats) bool { return s.Queued == 0 })
	for _, l := range held {
		finishLease(t, l, nil)
	}
	if stats := p.Stats(); stats.Busy != 0 || stats.Identities != 10 {
		t.Fatalf("released capacity leaked:%+v", stats)
	}
}
func TestPoolConcurrentLeasesSerializeOnlyFinishedWrites(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "http://first.example:8080", "http://second.example:8080")
	initialReads := p.Stats().Reads
	var wg sync.WaitGroup
	active := map[string]bool{}
	var mu sync.Mutex
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			l, e := p.acquire(ctx, "")
			if e != nil {
				t.Error(e)
				return
			}
			mu.Lock()
			if active[l.entry.id] {
				t.Error("identity is concurrently assigned")
			}
			active[l.entry.id] = true
			mu.Unlock()
			l.entry.session.StoreRegion = "concurrent"
			time.Sleep(time.Millisecond)
			mu.Lock()
			delete(active, l.entry.id)
			mu.Unlock()
			if e = l.finish(nil); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	stats := p.Stats()
	if stats.Busy != 0 || stats.Writes != 20 || stats.Reads != initialReads {
		t.Fatalf("concurrent completion did not persist once:%+v", stats)
	}
}
func TestPoolRevisionConflictFailsClosed(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	first := mustAcquire(t, p, "")
	id := first.entry.id
	first.entry.session.StoreRegion = "saved"
	finishLease(t, first, nil)
	if _, e := db.Exec(context.Background(), "UPDATE "+p.identitiesTable+" SET revision=revision+1 WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	next := mustAcquire(t, p, id)
	next.entry.session.StoreRegion = "stale"
	if e := next.finish(nil); e == nil {
		t.Fatal("stale RAM snapshot overwrote changed row")
	}
	if p.Ready() {
		t.Fatal("pool remained available after revision conflict")
	}
	if l, e := p.acquire(context.Background(), ""); e == nil {
		l.finish(nil)
		t.Fatal("failed pool granted another identity")
	}
	var region string
	if e := db.QueryRow(context.Background(), "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); e != nil || region != "saved" {
		t.Fatalf("stale session changed database:%s %v", region, e)
	}
}
func TestPoolFailedOperationStillFlushesSession(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	l.entry.session.StoreRegion = "before-failure"
	id := l.entry.id
	operationError := errors.New("fixture operation failed")
	e := l.finish(operationError)
	if e != nil && !errors.Is(e, operationError) {
		t.Fatal(e)
	}
	var region string
	if err := db.QueryRow(context.Background(), "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "before-failure" {
		t.Fatal("failure discarded changed session")
	}
	if p.Stats().Busy != 0 {
		t.Fatal("failed operation kept identity busy")
	}
}
func TestProxyAddressNormalizationAndCredentialPrivacy(t *testing.T) {
	for _, raw := range []string{"http://USER:password@Example.COM:8080", "Example.COM:8080:USER:password", "socks5://Example.COM:8080"} {
		key, address, e := Endpoint(raw)
		if e != nil || key != "example.com:8080" || address == "" {
			t.Fatalf("proxy normalization failed for scheme:%v", e)
		}
		if strings.Contains(key, "password") || strings.Contains(key, "USER") {
			t.Fatal("server key contains credentials")
		}
	}
	for _, raw := range []string{"ftp://example.org:21", "http://example.org/path", "http://example.org/?x=y"} {
		if _, _, e := Endpoint(raw); e == nil {
			t.Fatal("accepted invalid proxy")
		}
	}
	if key, address, e := Endpoint(""); e != nil || key != "direct" || address != "" {
		t.Fatal("direct endpoint changed")
	}
}
