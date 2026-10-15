package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

type health struct {
	Failures        int     `json:"failures"`
	CooldownUntil   int64   `json:"cooldownUntil"`
	LastFailureAt   *string `json:"lastFailureAt"`
	LastFailureCode *string `json:"lastFailureCode"`
	LastRecoveryAt  *string `json:"lastRecoveryAt"`
	LastRecovery    *string `json:"lastRecovery"`
}
type proxyState struct {
	id                                int64
	key, address                      string
	slots                             [10]*entry
	health                            health
	healthVersion, savedHealthVersion uint64
	transport                         *http.Transport
	client                            *http.Client
	preparationPace                   preparationPacer
}
type entry struct {
	id          string
	proxy       *proxyState
	ordinal     int
	session     *tiktokapp.Session
	client      *tiktokapp.Client
	assignments int64
	selected    uint64
	// waiting counts operations queued for this identity's own media; new
	// unpinned work skips the identity so those operations run next.
	waiting          int
	busy, registered bool
	health           health
	// pending is the latest finished state not yet saved to PostgreSQL.
	pending *checkpoint
}
type Stats struct {
	Ready       bool   `json:"ready"`
	Proxies     int    `json:"proxies"`
	Identities  int    `json:"identities"`
	Registered  int    `json:"registered"`
	Busy        int    `json:"busy"`
	Queued      int    `json:"queued"`
	Checkpoints uint64 `json:"checkpoints"`
	Unsaved     int    `json:"unsaved_identities"`
	Connected   bool   `json:"database_connected"`
	Reads       uint64 `json:"database_reads"`
	Writes      uint64 `json:"database_writes"`
}
type Pool struct {
	mu sync.Mutex
	// db belongs to the persistence goroutine once Open returns.
	db                            *pgx.Conn
	dbConfig                      *pgx.ConnConfig
	lockKey                       string
	log                           *slog.Logger
	timing                        persistTiming
	persistCtx                    context.Context
	persistCancel                 context.CancelFunc
	persistWake                   chan struct{}
	persistStop, persistDone      chan struct{}
	persistErr                    error
	dirty                         map[*entry]struct{}
	checkpoints, saved            uint64
	savedNotify                   chan struct{}
	connected                     bool
	owner                         backend
	failure                       error
	proxies                       []*proxyState
	entries                       map[string]*entry
	notify                        chan struct{}
	lifetime                      context.Context
	stop                          context.CancelFunc
	workers                       sync.WaitGroup
	ready, closing                bool
	sequence                      uint64
	queued, queueLimit            int
	proxiesTable, identitiesTable string
	account                       tiktokapp.Account
	reads, writes                 atomic.Uint64
	mediaTransport                *http.Transport
	clientFactory                 func(*tiktokapp.Session, *http.Client) *tiktokapp.Client
	closeOnce                     sync.Once
	closeErr                      error
	preparation                   *preparationRun
	preparationDelay              time.Duration
	preparationTimeout            time.Duration
}

var schemaPattern = regexp.MustCompile(`^[a-z_][a-z_0-9]{0,62}$`)

func Open(ctx context.Context, cfg core.Config, addresses []string) (*Pool, error) {
	return open(ctx, cfg, addresses, defaultPersistTiming)
}
func open(ctx context.Context, cfg core.Config, addresses []string, timing persistTiming) (*Pool, error) {
	if !schemaPattern.MatchString(cfg.DBSchema) {
		return nil, core.ConfigError("DB_SCHEMA")
	}
	lifetime, stop := context.WithCancel(context.Background())
	persistCtx, persistCancel := context.WithCancel(context.Background())
	p := &Pool{persistCtx: persistCtx, persistCancel: persistCancel, entries: map[string]*entry{}, notify: make(chan struct{}), lifetime: lifetime, stop: stop, queueLimit: cfg.MaxQueuedRequests,
		preparationDelay: preparationInterval, preparationTimeout: cfg.UpstreamTimeout,
		proxiesTable: fmt.Sprintf(`"%s".scrap_proxies`, cfg.DBSchema), identitiesTable: fmt.Sprintf(`"%s".scrap_identities`, cfg.DBSchema),
		lockKey: cfg.DBSchema + ".scrap_app.ram_pool", log: slog.Default(), timing: timing,
		persistWake: make(chan struct{}, 1), persistStop: make(chan struct{}), persistDone: make(chan struct{}),
		dirty: map[*entry]struct{}{}, savedNotify: make(chan struct{}),
		account:       tiktokapp.Account{SessionID: cfg.TikTokSessionID},
		clientFactory: func(s *tiktokapp.Session, c *http.Client) *tiktokapp.Client { return tiktokapp.NewClient(s, c) }}
	if p.queueLimit < 1 {
		p.queueLimit = 1024
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(cfg.DBURL)
	if err != nil {
		stop()
		persistCancel()
		return nil, errors.New("invalid DB_URL")
	}
	config.ConnectTimeout = 10 * time.Second
	config.RuntimeParams["statement_timeout"] = "10000"
	config.RuntimeParams["application_name"] = "tt-scrap"
	p.dbConfig = config
	p.db, err = pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		stop()
		persistCancel()
		return nil, errors.New("identity database connection failed")
	}
	if err = p.initialize(connectCtx, cfg, addresses); err != nil {
		p.closeConn(p.db)
		stop()
		persistCancel()
		return nil, err
	}
	p.connected = true
	go p.persist()
	if cfg.ProxyDataOnly {
		p.mediaTransport, err = newTransport("", cfg.HTTPMaxConnections)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	p.ready = true
	return p, nil
}
func (p *Pool) initialize(ctx context.Context, cfg core.Config, addresses []string) error {
	owned, err := p.claim(ctx, p.db)
	if err != nil {
		return errors.New("identity ownership check failed")
	}
	if !owned {
		return errOwnershipLost
	}
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + p.proxiesTable + ` (id BIGSERIAL PRIMARY KEY,server_key TEXT NOT NULL UNIQUE,proxy_url TEXT,created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`,
		`CREATE TABLE IF NOT EXISTS ` + p.identitiesTable + ` (id UUID PRIMARY KEY,proxy_id BIGINT NOT NULL REFERENCES ` + p.proxiesTable + `(id),ordinal SMALLINT NOT NULL CHECK(ordinal BETWEEN 0 AND 9),session JSONB NOT NULL,assignment_count BIGINT NOT NULL DEFAULT 0 CHECK(assignment_count>=0),revision BIGINT NOT NULL DEFAULT 0 CHECK(revision>=0),last_used_at TIMESTAMPTZ,created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),UNIQUE(proxy_id,ordinal))`,
		`ALTER TABLE ` + p.identitiesTable + ` ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE ` + p.identitiesTable + ` ADD COLUMN IF NOT EXISTS health JSONB NOT NULL DEFAULT '{}'::jsonb`,
		`ALTER TABLE ` + p.proxiesTable + ` ADD COLUMN IF NOT EXISTS health JSONB NOT NULL DEFAULT '{}'::jsonb`,
		`UPDATE ` + p.identitiesTable + ` SET session=jsonb_set(session,'{identity,deviceToken}','""'::jsonb),revision=revision+1,updated_at=clock_timestamp() WHERE session->'identity'->>'kind'='registered' AND NOT(session->'identity' ? 'deviceToken')`,
	}
	for _, sql := range statements {
		if _, err = tx.Exec(ctx, sql); err != nil {
			return errors.New("identity schema migration failed")
		}
	}
	for _, address := range addresses {
		key, normalized, e := Endpoint(address)
		if e != nil {
			return e
		}
		var value any = normalized
		if normalized == "" {
			value = nil
		}
		if _, err = tx.Exec(ctx, `INSERT INTO `+p.proxiesTable+`(server_key,proxy_url) VALUES($1,$2) ON CONFLICT(server_key) DO UPDATE SET proxy_url=EXCLUDED.proxy_url,updated_at=clock_timestamp()`, key, value); err != nil {
			return errors.New("proxy synchronization failed")
		}
	}
	// Only unused local placeholders from the original eager pool may be removed.
	_, err = tx.Exec(ctx, `DELETE FROM `+p.identitiesTable+` WHERE assignment_count=0 AND last_used_at IS NULL AND session->'identity'->>'kind'='local' AND (session->'sdk' IS NULL OR session->'sdk'='null'::jsonb) AND NOT(session->'identity' ? 'deviceId') AND NOT(session->'identity' ? 'installId')`)
	if err != nil {
		return errors.New("identity placeholder migration failed")
	}
	p.reads.Add(1)
	rows, err := tx.Query(ctx, `SELECT id,server_key,proxy_url,health FROM `+p.proxiesTable+` ORDER BY id`)
	if err != nil {
		return err
	}
	byID := map[int64]*proxyState{}
	for rows.Next() {
		state := &proxyState{}
		var address *string
		var raw []byte
		if err = rows.Scan(&state.id, &state.key, &address, &raw); err != nil {
			rows.Close()
			return err
		}
		if address != nil {
			state.address = *address
		}
		key, normalized, e := Endpoint(state.address)
		if e != nil || key != state.key {
			rows.Close()
			return errors.New("stored proxy does not match its permanent server key")
		}
		state.address = normalized
		if err = json.Unmarshal(raw, &state.health); err != nil {
			rows.Close()
			return errors.New("stored proxy health is invalid")
		}
		state.transport, err = newTransport(state.address, cfg.HTTPMaxConnections)
		if err != nil {
			rows.Close()
			return err
		}
		state.client = &http.Client{Transport: state.transport}
		p.proxies = append(p.proxies, state)
		byID[state.id] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(p.proxies) == 0 {
		return errors.New("no proxy servers configured; set PROXY_FILE or PROXY_INCLUDE_HOST")
	}
	p.reads.Add(1)
	rows, err = tx.Query(ctx, `SELECT id::text,proxy_id,ordinal,session,assignment_count,health FROM `+p.identitiesTable+` ORDER BY proxy_id,ordinal`)
	if err != nil {
		return err
	}
	for rows.Next() {
		state := &entry{}
		var proxyID int64
		var raw, rawHealth []byte
		if err = rows.Scan(&state.id, &proxyID, &state.ordinal, &raw, &state.assignments, &rawHealth); err != nil {
			rows.Close()
			return err
		}
		state.proxy = byID[proxyID]
		if state.proxy == nil || state.ordinal < 0 || state.ordinal >= 10 || state.proxy.slots[state.ordinal] != nil {
			rows.Close()
			return errors.New("stored identity has an invalid proxy slot")
		}
		if err = json.Unmarshal(raw, &state.session); err != nil {
			rows.Close()
			return errors.New("stored identity session is invalid")
		}
		if err = tiktokapp.ValidateSession(state.session); err != nil {
			rows.Close()
			return errors.New("stored identity session failed validation")
		}
		if err = json.Unmarshal(rawHealth, &state.health); err != nil {
			rows.Close()
			return errors.New("stored identity health is invalid")
		}
		state.registered = state.session.Identity.Kind == "registered"
		state.proxy.slots[state.ordinal] = state
		p.entries[state.id] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (p *Pool) wakeLocked() { close(p.notify); p.notify = make(chan struct{}) }

// fail stops identity work after this process loses schema ownership.
func (p *Pool) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure == nil {
		p.failure = err
	}
	p.ready = false
	p.stop()
	p.wakeLocked()
	p.notifySavedLocked()
}
func (p *Pool) Ready() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.ready && !p.closing }

// Done is closed when the pool stops serving: after Close or lost ownership.
func (p *Pool) Done() <-chan struct{} { return p.lifetime.Done() }

// Err reports why the pool stopped serving before Close.
func (p *Pool) Err() error { p.mu.Lock(); defer p.mu.Unlock(); return p.failure }
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Stats{Ready: p.ready && !p.closing, Proxies: len(p.proxies), Identities: len(p.entries), Queued: p.queued, Checkpoints: p.checkpoints, Unsaved: len(p.dirty), Connected: p.connected, Reads: p.reads.Load(), Writes: p.writes.Load()}
	for _, e := range p.entries {
		if e.busy {
			s.Busy++
		}
		if e.registered {
			s.Registered++
		}
	}
	return s
}
func (p *Pool) ProxyAddresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.proxies))
	for i, v := range p.proxies {
		out[i] = v.address
	}
	return out
}
func (p *Pool) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.ready = false
		p.stop()
		p.wakeLocked()
		p.mu.Unlock()
		p.workers.Wait()
		if p.mediaTransport != nil {
			p.mediaTransport.CloseIdleConnections()
		}
		for _, proxy := range p.proxies {
			proxy.transport.CloseIdleConnections()
		}
		// Finished leases left checkpoints; save them before releasing
		// ownership, within one deadline that also bounds a save in progress.
		deadline := time.AfterFunc(p.timing.shutdown, p.persistCancel)
		close(p.persistStop)
		<-p.persistDone
		deadline.Stop()
		p.persistCancel()
		p.closeErr = p.persistErr
	})
	return p.closeErr
}
