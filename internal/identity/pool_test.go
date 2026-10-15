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
	"github.com/jackc/pgx/v5/pgconn"
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
	return openTestPoolTiming(t, cfg, defaultPersistTiming, addresses...)
}
func openTestPoolTiming(t testing.TB, cfg core.Config, timing persistTiming, addresses ...string) *Pool {
	t.Helper()
	p, e := open(context.Background(), cfg, addresses, timing)
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

// persisted waits until the pool saves every finished operation.
func persisted(t testing.TB, p *Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := p.flush(ctx); e != nil {
		t.Fatal(e)
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
		l.finish(nil)
	}
	persisted(t, p)
	stats := p.Stats()
	if stats.Identities != 20 || stats.Busy != 0 || stats.Registered != 0 || stats.Unsaved != 0 {
		t.Fatalf("wrong lazily filled pool:%+v", stats)
	}
	if stats.Reads != initial.Reads {
		t.Fatalf("selection queried database: startup%d after%d", initial.Reads, stats.Reads)
	}
	if stats.Checkpoints != 43 || stats.Writes < 20 || stats.Writes > 43 {
		t.Fatalf("expected one checkpoint per operation and at most one write each:%+v", stats)
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
	l.finish(nil)
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
	next.finish(nil)
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
			l.finish(nil)
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
		l.finish(nil)
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
			l.finish(nil)
		})
	}
	wg.Wait()
	persisted(t, p)
	stats := p.Stats()
	if stats.Busy != 0 || stats.Checkpoints != 20 || stats.Unsaved != 0 || stats.Reads != initialReads {
		t.Fatalf("concurrent completion did not persist once:%+v", stats)
	}
}
func TestPoolRAMStateOverwritesExternalRowChanges(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	first := mustAcquire(t, p, "")
	id := first.entry.id
	first.entry.session.StoreRegion = "saved"
	first.finish(nil)
	persisted(t, p)
	if _, e := db.Exec(context.Background(), "UPDATE "+p.identitiesTable+" SET revision=revision+5,session=jsonb_set(session,'{storeRegion}','\"external\"') WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	next := mustAcquire(t, p, id)
	next.entry.session.StoreRegion = "ram"
	next.finish(nil)
	persisted(t, p)
	if !p.Ready() {
		t.Fatal("an external row change stopped the pool")
	}
	var region string
	if e := db.QueryRow(context.Background(), "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); e != nil || region != "ram" {
		t.Fatalf("RAM state was not saved over the row:%s %v", region, e)
	}
}
func TestPoolStopsWhenAnotherWriterTakesItsSlot(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	if _, e := db.Exec(context.Background(), "INSERT INTO "+p.identitiesTable+"(id,proxy_id,ordinal,session) VALUES(gen_random_uuid(),$1,$2,'{}')", l.entry.proxy.id, l.entry.ordinal); e != nil {
		t.Fatal(e)
	}
	l.finish(nil)
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("pool kept running after another writer took its slot")
	}
	var pg *pgconn.PgError
	if !errors.As(p.Err(), &pg) || pg.Code != "23505" || p.Ready() {
		t.Fatal("slot conflict was not reported as an ownership failure", p.Err())
	}
}
func TestPoolFailedOperationStillFlushesSession(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	l.entry.session.StoreRegion = "before-failure"
	id := l.entry.id
	l.finish(errors.New("fixture operation failed"))
	persisted(t, p)
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

func TestPinnedWaiterRunsBeforeNewWorkAndReleasesReservation(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	var mu sync.Mutex
	var leases []*lease
	var waiters sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	// Pool shutdown waits for leases, so a failed assertion must release them.
	t.Cleanup(func() {
		cancel()
		waiters.Wait()
		for _, l := range leases {
			l.finish(nil)
		}
	})
	track := func(l *lease) *lease {
		mu.Lock()
		defer mu.Unlock()
		leases = append(leases, l)
		return l
	}
	held := make([]*lease, 10)
	for i := range held {
		held[i] = track(mustAcquire(t, p, ""))
	}
	pinned := held[0].entry.id
	acquire := func(ctx context.Context, id string) chan *lease {
		result := make(chan *lease, 1)
		waiters.Go(func() {
			l, err := p.acquire(ctx, id)
			if err != nil {
				result <- nil
				return
			}
			result <- track(l)
		})
		return result
	}
	media := acquire(ctx, pinned)
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	// Model the moment after release, before the waiter runs: new work must
	// not take the identity its queued media is waiting for.
	p.mu.Lock()
	held[0].entry.busy = false
	p.mu.Unlock()
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if l, err := p.acquire(short, ""); err == nil {
		l.finish(nil)
		t.Fatal("new work took an identity reserved for queued media")
	}
	stop()
	p.mu.Lock()
	held[0].entry.busy = true
	p.mu.Unlock()
	unpinned := acquire(ctx, "")
	waitStats(t, p, func(s Stats) bool { return s.Queued == 2 })
	held[0].finish(nil)
	var l *lease
	select {
	case l = <-media:
	case <-time.After(2 * time.Second):
		t.Fatal("pinned waiter did not receive its identity")
	}
	if l == nil || l.entry.id != pinned {
		t.Fatal("pinned waiter lost its identity to new work")
	}
	// A cancelled pinned waiter releases its reservation for queued work.
	abandon, cancelWaiter := context.WithCancel(ctx)
	abandoned := acquire(abandon, pinned)
	waitStats(t, p, func(s Stats) bool { return s.Queued == 2 })
	cancelWaiter()
	if <-abandoned != nil {
		t.Fatal("cancelled waiter acquired an identity")
	}
	l.finish(nil)
	select {
	case l = <-unpinned:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled reservation kept blocking new work")
	}
	if l == nil || l.entry.id != pinned {
		t.Fatal("queued work did not take the released identity")
	}
	l.finish(nil)
	for _, l := range held[1:] {
		l.finish(nil)
	}
	if stats := p.Stats(); stats.Busy != 0 || stats.Queued != 0 {
		t.Fatalf("leases leaked: %+v", stats)
	}
}
