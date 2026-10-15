package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RAM holds the authoritative identity state. PostgreSQL only preserves it
// across restarts and crashes: one goroutine owns the connection and the
// schema ownership lock, writes checkpoints after operations finish, and
// reconnects after failures. Requests never wait for the database.

type persistTiming struct {
	probe, probeTimeout, connect, write, shutdown time.Duration
	minBackoff, maxBackoff                        time.Duration
}

var defaultPersistTiming = persistTiming{probe: 30 * time.Second, probeTimeout: 5 * time.Second, connect: 15 * time.Second, write: 15 * time.Second, shutdown: 10 * time.Second, minBackoff: time.Second, maxBackoff: 30 * time.Second}

// checkpoint is an identity's complete state when its lease finished.
type checkpoint struct {
	session, health []byte
	assignments     int64
	at              time.Time
}

// backend identifies the server process holding ownership. Its start time
// guards against PID reuse.
type backend struct {
	pid     uint32
	started *time.Time
}

var errOwnershipLost = errors.New("another process owns this identity pool")
var errConflict = errors.New("stored identity no longer matches RAM")
var errPreviousOwner = errors.New("previous ownership connection is still ending")

func (p *Pool) checkpointLocked(e *entry, session []byte) {
	health, err := json.Marshal(e.health)
	if err != nil {
		p.log.Error("identity checkpoint skipped", "identity_id", e.id, "error", err)
		return
	}
	e.pending = &checkpoint{session: session, health: health, assignments: e.assignments, at: time.Now()}
	p.dirty[e] = struct{}{}
	p.checkpoints++
	select {
	case p.persistWake <- struct{}{}:
	default:
	}
}

type saveBatch struct {
	entries     []*entry
	written     []*checkpoint
	proxies     []*proxyState
	versions    []uint64
	proxyHealth [][]byte
	through     uint64
}

func (p *Pool) collectLocked() (saveBatch, error) {
	b := saveBatch{through: p.checkpoints}
	for e := range p.dirty {
		b.entries = append(b.entries, e)
		b.written = append(b.written, e.pending)
	}
	for _, proxy := range p.proxies {
		if proxy.healthVersion > proxy.savedHealthVersion {
			raw, err := json.Marshal(proxy.health)
			if err != nil {
				return b, err
			}
			b.proxies = append(b.proxies, proxy)
			b.versions = append(b.versions, proxy.healthVersion)
			b.proxyHealth = append(b.proxyHealth, raw)
		}
	}
	return b, nil
}

func (p *Pool) write(ctx context.Context, b saveBatch) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queue := &pgx.Batch{}
	for i, e := range b.entries {
		c := b.written[i]
		queue.Queue(`INSERT INTO `+p.identitiesTable+` AS t(id,proxy_id,ordinal,session,assignment_count,revision,health,last_used_at) VALUES($1,$2,$3,$4::jsonb,$5,1,$6::jsonb,$7) ON CONFLICT(id) DO UPDATE SET session=EXCLUDED.session,assignment_count=EXCLUDED.assignment_count,revision=t.revision+1,health=EXCLUDED.health,last_used_at=EXCLUDED.last_used_at,updated_at=clock_timestamp() WHERE t.proxy_id=EXCLUDED.proxy_id AND t.ordinal=EXCLUDED.ordinal`, e.id, e.proxy.id, e.ordinal, c.session, c.assignments, c.health, c.at)
	}
	for i, proxy := range b.proxies {
		queue.Queue(`UPDATE `+p.proxiesTable+` SET health=$2::jsonb,updated_at=clock_timestamp() WHERE id=$1`, proxy.id, b.proxyHealth[i])
	}
	results := tx.SendBatch(ctx, queue)
	for range queue.Len() {
		tag, err := results.Exec()
		if err == nil && tag.RowsAffected() != 1 {
			err = errConflict
		}
		if err != nil {
			results.Close()
			return err
		}
	}
	if err = results.Close(); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	p.writes.Add(uint64(queue.Len()))
	return nil
}

func (p *Pool) savedLocked(b saveBatch) {
	for i, e := range b.entries {
		// A newer checkpoint taken during the write stays dirty.
		if e.pending == b.written[i] {
			e.pending = nil
			delete(p.dirty, e)
		}
	}
	for i, proxy := range b.proxies {
		proxy.savedHealthVersion = max(proxy.savedHealthVersion, b.versions[i])
	}
	p.saved = max(p.saved, b.through)
	p.notifySavedLocked()
}

func (p *Pool) notifySavedLocked() { close(p.savedNotify); p.savedNotify = make(chan struct{}) }

// claim takes schema ownership on conn and identifies its server process.
func (p *Pool) claim(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var owned bool
	var b backend
	p.reads.Add(1)
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0)),pg_backend_pid(),(SELECT backend_start FROM pg_stat_activity WHERE pid=pg_backend_pid())`, p.lockKey).Scan(&owned, &b.pid, &b.started); err != nil {
		return false, err
	}
	if owned {
		p.mu.Lock()
		p.owner = b
		p.mu.Unlock()
	}
	return owned, nil
}

// connect restores ownership after a lost connection. A connection that
// failed on this side can survive on the server and keep the lock until TCP
// keepalive notices; only that exact backend is ended. Any other holder is a
// different process, so this pool must stop.
func (p *Pool) connect(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, p.dbConfig)
	if err != nil {
		return err
	}
	owned, err := p.claim(ctx, conn)
	if err == nil && !owned {
		p.mu.Lock()
		previous := p.owner
		p.mu.Unlock()
		var ended bool
		err = conn.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid=$1 AND pid<>pg_backend_pid() AND ($2::timestamptz IS NULL OR backend_start=$2)`, previous.pid, previous.started).Scan(&ended)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = errOwnershipLost
		case err == nil:
			p.log.Warn("ended previous identity database connection", "backend_pid", previous.pid, "signalled", ended)
			err = errPreviousOwner
		}
	}
	if err != nil {
		p.closeConn(conn)
		return err
	}
	p.db = conn
	p.mu.Lock()
	p.connected = true
	p.mu.Unlock()
	return nil
}

func (p *Pool) disconnect() {
	if p.db != nil {
		p.closeConn(p.db)
		p.db = nil
	}
	p.mu.Lock()
	p.connected = false
	p.mu.Unlock()
}

func (p *Pool) closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn.Close(ctx)
}

// sync connects when needed and saves every pending checkpoint. With nothing
// to save, probe checks that the connection, and so ownership, still exists.
func (p *Pool) sync(parent context.Context, probe bool) error {
	if p.db == nil {
		ctx, cancel := context.WithTimeout(parent, p.timing.connect)
		err := p.connect(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}
	p.mu.Lock()
	b, err := p.collectLocked()
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if len(b.entries) == 0 && len(b.proxies) == 0 {
		if probe {
			ctx, cancel := context.WithTimeout(parent, p.timing.probeTimeout)
			err = p.db.Ping(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("probe: %w", err)
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(parent, p.timing.write)
		err = p.write(ctx, b)
		cancel()
		if err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
	}
	p.mu.Lock()
	p.savedLocked(b)
	p.mu.Unlock()
	return nil
}

func fatal(err error) bool {
	var pg *pgconn.PgError
	return errors.Is(err, errOwnershipLost) || errors.Is(err, errConflict) || errors.As(err, &pg) && strings.HasPrefix(pg.Code, "23")
}

func (p *Pool) persist() {
	defer close(p.persistDone)
	timer := time.NewTimer(p.timing.probe)
	defer timer.Stop()
	var retryAt, failingSince time.Time
	var backoff time.Duration
	for {
		probe := false
		select {
		case <-p.persistStop:
			p.saveBeforeClose()
			return
		case <-p.persistWake:
		case <-timer.C:
			probe = true
		}
		if p.Err() != nil || time.Now().Before(retryAt) {
			continue
		}
		err := p.sync(p.persistCtx, probe)
		if err == nil {
			if !failingSince.IsZero() {
				p.log.Info("identity database restored; RAM state saved", "unavailable_ms", time.Since(failingSince).Milliseconds())
				failingSince = time.Time{}
			}
			backoff, retryAt = 0, time.Time{}
			timer.Reset(p.timing.probe)
			continue
		}
		p.disconnect()
		if fatal(err) {
			p.log.Error("identity pool ownership lost; stopping identity work", databaseAttributes(err, p.unsaved())...)
			p.fail(err)
			continue
		}
		level := slog.LevelWarn
		if failingSince.IsZero() {
			failingSince, level = time.Now(), slog.LevelError
		}
		backoff = min(max(2*backoff, p.timing.minBackoff), p.timing.maxBackoff)
		retryAt = time.Now().Add(backoff)
		timer.Reset(backoff)
		p.log.Log(context.Background(), level, "identity database unavailable; serving from RAM", append(databaseAttributes(err, p.unsaved()), "retry_ms", backoff.Milliseconds())...)
	}
}

// saveBeforeClose retries saving state left by finished leases until Close's
// deadline cancels persistCtx.
func (p *Pool) saveBeforeClose() {
	defer p.disconnect()
	if p.Err() != nil {
		return
	}
	for {
		err := p.sync(p.persistCtx, false)
		if err == nil {
			return
		}
		p.disconnect()
		if !fatal(err) {
			select {
			case <-time.After(p.timing.minBackoff):
				continue
			case <-p.persistCtx.Done():
			}
		}
		p.log.Error("identity state not saved before shutdown", databaseAttributes(err, p.unsaved())...)
		p.persistErr = errors.New("identity state could not be saved before shutdown")
		return
	}
}

func (p *Pool) unsaved() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dirty)
}

func databaseAttributes(err error, unsaved int) []any {
	attributes := []any{"error", err.Error(), "unsaved_identities", unsaved}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		attributes = append(attributes, "sqlstate", pg.Code)
	}
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) {
		attributes = append(attributes, "timeout", true)
	}
	return attributes
}

// flush waits until checkpoints taken before the call are in PostgreSQL.
func (p *Pool) flush(ctx context.Context) error {
	p.mu.Lock()
	target := p.checkpoints
	p.mu.Unlock()
	select {
	case p.persistWake <- struct{}{}:
	default:
	}
	for {
		p.mu.Lock()
		saved, failure, notify := p.saved, p.failure, p.savedNotify
		p.mu.Unlock()
		if saved >= target {
			return nil
		}
		if failure != nil {
			return failure
		}
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.persistDone:
			return errors.New("identity pool closed before its state was saved")
		}
	}
}
