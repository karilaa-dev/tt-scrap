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

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

var fastPersistTiming = persistTiming{probe: 50 * time.Millisecond, probeTimeout: 500 * time.Millisecond, connect: 2 * time.Second, write: 300 * time.Millisecond, shutdown: 3 * time.Second, minBackoff: 20 * time.Millisecond, maxBackoff: 200 * time.Millisecond}

func ownerPID(p *Pool) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owner.pid
}

// The production incident: one state save stalled past its deadline and the
// pool stopped serving until restart.
func TestStalledDatabaseKeepsServingFromRAMAndRecovers(t *testing.T) {
	cfg, db := testDatabase(t)
	ctx := context.Background()
	p := openTestPoolTiming(t, cfg, fastPersistTiming, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.entry.session.StoreRegion = "saved"
	l.finish(nil)
	persisted(t, p)
	stalled := ownerPID(p)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT 1 FROM "+p.identitiesTable+" WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	l = mustAcquire(t, p, id)
	l.entry.session.StoreRegion = "during-outage"
	l.finish(nil)
	waitStats(t, p, func(s Stats) bool { return !s.Connected })
	for range 30 {
		started := time.Now()
		l := mustAcquire(t, p, "")
		l.finish(nil)
		if time.Since(started) > 100*time.Millisecond {
			t.Fatal("an operation waited for the database")
		}
	}
	if stats := p.Stats(); !stats.Ready || stats.Unsaved == 0 {
		t.Fatal("pool stopped serving or lost track of unsaved state", stats)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	persisted(t, p)
	var region string
	if err = db.QueryRow(ctx, "SELECT session->>'storeRegion' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&region); err != nil || region != "during-outage" {
		t.Fatal("RAM state was not saved after recovery", region, err)
	}
	if ownerPID(p) == stalled {
		t.Fatal("pool kept using the connection whose save timed out")
	}
	if second, err := Open(ctx, cfg, nil); err == nil {
		second.Close()
		t.Fatal("reconnected pool did not retake ownership")
	}
	if stats := p.Stats(); !stats.Ready || !stats.Connected || stats.Unsaved != 0 {
		t.Fatal("pool did not recover", stats)
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
	proxy.cut()
	var holds bool
	if err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)", orphaned).Scan(&holds); err != nil || !holds {
		t.Fatal("fixture did not leave the server connection holding ownership", err)
	}
	l = mustAcquire(t, p, id)
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
