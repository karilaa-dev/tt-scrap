package identity

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

var fastPersistTiming = persistTiming{probe: 50 * time.Millisecond, probeTimeout: 500 * time.Millisecond, connect: 2 * time.Second, write: 300 * time.Millisecond, shutdown: 3 * time.Second, minBackoff: 20 * time.Millisecond, maxBackoff: 200 * time.Millisecond}

func ownerPID(p *Pool) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owner.pid
}

// lockIdentityRow holds a row lock, as a concurrent writer would, so the
// pool's next save of id blocks until the returned transaction ends. It uses
// its own connection: inside a transaction db would see frozen statistics.
func lockIdentityRow(t *testing.T, db *pgx.Conn, p *Pool, id string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, db.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(ctx) })
	if _, err = tx.Exec(ctx, "SELECT 1 FROM "+p.identitiesTable+" WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	return tx
}

// blockedSave reports when the transaction of server process pid that waits
// for a row lock started.
func blockedSave(t *testing.T, db *pgx.Conn, pid uint32) (time.Time, bool) {
	t.Helper()
	var started *time.Time
	err := db.QueryRow(context.Background(), "SELECT xact_start FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'", pid).Scan(&started)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if started == nil {
		return time.Time{}, false
	}
	return *started, true
}

func waitBlockedSave(t *testing.T, db *pgx.Conn, pid uint32) time.Time {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if started, ok := blockedSave(t, db, pid); ok {
			return started
		}
	}
	t.Fatal("save did not block on the row lock")
	return time.Time{}
}

func holdsOwnership(t *testing.T, db *pgx.Conn, pid uint32) bool {
	t.Helper()
	var holds bool
	if err := db.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)", pid).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	return holds
}

// The production incident: one state save stalled past its deadline and the
// pool stopped serving until restart. Only the stalled statement may end; the
// connection, and so the ownership lock, must survive.
func TestStalledSaveKeepsOwnershipAndServesFromRAM(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	p := openTestPoolTiming(t, cfg, fastPersistTiming, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.entry.session.StoreRegion = "saved"
	l.finish(nil)
	persisted(t, p)
	owner := ownerPID(p)
	tx := lockIdentityRow(t, db, p, id)
	l = mustAcquire(t, p, id)
	l.entry.session.StoreRegion = "during-stall"
	l.finish(nil)
	stalled := waitBlockedSave(t, db, owner)
	// The save retries in a new transaction only after its deadline cancelled
	// the stalled one.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if !holdsOwnership(t, db, owner) {
			t.Fatal("stalled save released ownership")
		}
		if retried, ok := blockedSave(t, db, owner); ok && retried.After(stalled) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stalled save was not cancelled and retried on its connection")
		}
	}
	for range 30 {
		started := time.Now()
		l := mustAcquire(t, p, "")
		l.finish(nil)
		if time.Since(started) > 100*time.Millisecond {
			t.Fatal("an operation waited for the database")
		}
	}
	if stats := p.Stats(); !stats.Ready || !stats.Connected || stats.Unsaved == 0 {
		t.Fatal("pool stopped serving, dropped its connection, or lost track of unsaved state", stats)
	}
	if ownerPID(p) != owner || !holdsOwnership(t, db, owner) {
		t.Fatal("stalled save released ownership while the pool kept serving")
	}
	if second, err := Open(ctx, cfg, nil); err == nil {
		second.Close()
		t.Fatal("another process took ownership during a stalled save")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	persisted(t, p)
	var region string
	if err := db.QueryRow(ctx, "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "during-stall" {
		t.Fatal("RAM state was not saved after recovery", region, err)
	}
	if stats := p.Stats(); ownerPID(p) != owner || !stats.Connected || stats.Unsaved != 0 {
		t.Fatal("pool did not recover on its ownership connection", stats)
	}
}

// A save that succeeds after a newer checkpoint of the same identity was
// taken must leave the newer checkpoint for the next save.
func TestNewerCheckpointSurvivesOlderSuccessfulSave(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	timing := fastPersistTiming
	// The blocked save must succeed once released, not time out.
	timing.write = 5 * time.Second
	p := openTestPoolTiming(t, cfg, timing, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	persisted(t, p)
	tx := lockIdentityRow(t, db, p, id)
	l = mustAcquire(t, p, id)
	l.entry.session.StoreRegion = "older"
	l.finish(nil)
	waitBlockedSave(t, db, ownerPID(p))
	l = mustAcquire(t, p, id)
	l.entry.session.StoreRegion = "newer"
	l.finish(nil)
	writes := p.Stats().Writes
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	persisted(t, p)
	var region string
	if err := db.QueryRow(ctx, "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "newer" {
		t.Fatal("older save discarded the newer checkpoint", region, err)
	}
	if stats := p.Stats(); stats.Unsaved != 0 || stats.Writes != writes+2 {
		t.Fatal("expected the older save and then the newer one", stats, writes)
	}
}

// A network failure can leave the server side of the ownership connection
// open, holding the lock until TCP keepalive notices.
func TestPoolEndsItsOrphanedOwnershipConnection(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	proxy := proxyDatabase(t, &cfg)
	p := openTestPoolTiming(t, cfg, fastPersistTiming, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	persisted(t, p)
	orphaned := ownerPID(p)
	// Running operations finish; new ones wait until ownership is retaken.
	l = mustAcquire(t, p, id)
	proxy.cut()
	if !holdsOwnership(t, db, orphaned) {
		t.Fatal("fixture did not leave the server connection holding ownership")
	}
	l.entry.session.StoreRegion = "after-cut"
	l.finish(nil)
	persisted(t, p)
	if ownerPID(p) == orphaned {
		t.Fatal("pool did not move ownership to a new connection")
	}
	var region string
	if err := db.QueryRow(ctx, "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "after-cut" {
		t.Fatal("RAM state was not saved after reconnecting", region, err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var alive bool
		if err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", orphaned).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphaned ownership connection was not ended")
		}
	}
}

// Once the ownership connection is lost, the server may release the lock to
// another process, so the pool must not start identity work it may no longer
// own. It resumes after retaking ownership.
func TestLostOwnershipConnectionPausesIdentityWork(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	proxy := proxyDatabase(t, &cfg)
	p := openTestPoolTiming(t, cfg, fastPersistTiming, "")
	running := mustAcquire(t, p, "")
	// Close waits for running operations.
	t.Cleanup(func() { running.finish(context.Canceled) })
	id := running.entry.id
	owner := ownerPID(p)
	proxy.block()
	// The server notices the failure and releases the lock.
	if _, err := db.Exec(ctx, "SELECT pg_terminate_backend($1)", owner); err != nil {
		t.Fatal(err)
	}
	waitStats(t, p, func(s Stats) bool { return !s.Connected })
	var api *core.Error
	if l, err := p.acquire(ctx, ""); !errors.As(err, &api) || api.Status != 503 {
		if err == nil {
			l.finish(nil)
		}
		t.Fatal("pool started identity work without ownership", err)
	}
	var free bool
	if err := db.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,0))", p.lockKey).Scan(&free); err != nil || !free {
		t.Fatal("fixture did not release ownership", err)
	}
	if _, err := db.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", p.lockKey); err != nil {
		t.Fatal(err)
	}
	// Failing readiness would invite a restart that discards unsaved RAM state.
	if !p.Ready() || p.Err() != nil {
		t.Fatal("pause stopped the pool")
	}
	running.entry.session.StoreRegion = "while-paused"
	running.finish(nil)
	proxy.restore()
	persisted(t, p)
	var region string
	if err := db.QueryRow(ctx, "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "while-paused" {
		t.Fatal("operation finished during the pause was not saved", region, err)
	}
	if ownerPID(p) == owner || !holdsOwnership(t, db, ownerPID(p)) {
		t.Fatal("pool resumed without retaking ownership")
	}
	mustAcquire(t, p, "").finish(nil)
}

func TestOwnershipTakenDuringOutageStopsPool(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	timing := fastPersistTiming
	// Only the next save may notice the lost connection.
	timing.probe = time.Hour
	p := openTestPoolTiming(t, cfg, timing, "")
	l := mustAcquire(t, p, "")
	l.finish(nil)
	persisted(t, p)
	if _, err := db.Exec(ctx, "SELECT pg_terminate_backend($1)", ownerPID(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", p.lockKey); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", p.lockKey)
	l = mustAcquire(t, p, "")
	l.finish(nil)
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("pool kept serving after another process took ownership")
	}
	if !errors.Is(p.Err(), errOwnershipLost) || p.Ready() {
		t.Fatal("ownership loss was not reported", p.Err())
	}
	var api *core.Error
	if _, err := p.acquire(ctx, ""); !errors.As(err, &api) || api.Code != "service_not_configured" {
		t.Fatal("pool granted an identity it no longer owns", err)
	}
}

// cutProxy forwards database connections. cut drops the client sides as a
// network failure would, while the server sides stay open.
type cutProxy struct {
	mu      sync.Mutex
	blocked bool
	clients []net.Conn
	servers []net.Conn
}

func proxyDatabase(t *testing.T, cfg *core.Config) *cutProxy {
	t.Helper()
	address, err := url.Parse(cfg.DBURL)
	if err != nil || address.Host == "" {
		t.Skip("proxy tests need a URL-form database address")
	}
	target := address.Host
	if address.Port() == "" {
		target = net.JoinHostPort(address.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x := &cutProxy{}
	t.Cleanup(func() {
		listener.Close()
		x.mu.Lock()
		defer x.mu.Unlock()
		for _, c := range append(x.clients, x.servers...) {
			c.Close()
		}
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			x.mu.Lock()
			blocked := x.blocked
			x.mu.Unlock()
			if blocked {
				client.Close()
				continue
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			x.mu.Lock()
			x.clients, x.servers = append(x.clients, client), append(x.servers, server)
			x.mu.Unlock()
			go io.Copy(server, client)
			go io.Copy(client, server)
		}
	}()
	address.Host = listener.Addr().String()
	cfg.DBURL = address.String()
	return x
}

func (x *cutProxy) cut() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, c := range x.clients {
		c.Close()
	}
	x.clients = nil
}

// block cuts the client sides and refuses new connections until restore.
func (x *cutProxy) block() {
	x.mu.Lock()
	x.blocked = true
	x.mu.Unlock()
	x.cut()
}

func (x *cutProxy) restore() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.blocked = false
}
