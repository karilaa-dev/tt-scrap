package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}
type entry struct {
	id                    string
	proxy                 *proxyState
	ordinal               int
	session               *tiktokapp.Session
	client                *tiktokapp.Client
	assignments, revision int64
	selected              uint64
	busy, registered      bool
	health                health
}
type Stats struct {
	Ready      bool   `json:"ready"`
	Proxies    int    `json:"proxies"`
	Identities int    `json:"identities"`
	Registered int    `json:"registered"`
	Busy       int    `json:"busy"`
	Queued     int    `json:"queued"`
	Reads      uint64 `json:"database_reads"`
	Writes     uint64 `json:"database_writes"`
}
type Pool struct {
	mu                            sync.Mutex
	dbMu                          sync.Mutex
	db                            *pgx.Conn
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
	reads, writes                 atomic.Uint64
	clientFactory                 func(*tiktokapp.Session, *http.Client) *tiktokapp.Client
	closeOnce                     sync.Once
	closeErr                      error
}

func Open(ctx context.Context, cfg core.Config, addresses []string) (*Pool, error) {
	if !regexp.MustCompile(`^[a-z_][a-z_0-9]{0,62}$`).MatchString(cfg.DBSchema) {
		return nil, core.ConfigError("DB_SCHEMA")
	}
	lifetime, stop := context.WithCancel(context.Background())
	p := &Pool{entries: map[string]*entry{}, notify: make(chan struct{}), lifetime: lifetime, stop: stop, queueLimit: cfg.MaxQueuedRequests,
		proxiesTable: fmt.Sprintf(`"%s".scrap_proxies`, cfg.DBSchema), identitiesTable: fmt.Sprintf(`"%s".scrap_identities`, cfg.DBSchema),
		clientFactory: func(s *tiktokapp.Session, c *http.Client) *tiktokapp.Client { return tiktokapp.NewClient(s, c) }}
	if p.queueLimit < 1 {
		p.queueLimit = 1024
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(cfg.DBURL)
	if err != nil {
		stop()
		return nil, errors.New("invalid DB_URL")
	}
	config.ConnectTimeout = 10 * time.Second
	config.RuntimeParams["statement_timeout"] = "10000"
	p.db, err = pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		stop()
		return nil, errors.New("identity database connection failed")
	}
	if err = p.initialize(connectCtx, cfg, addresses); err != nil {
		p.db.Close(context.Background())
		stop()
		return nil, err
	}
	p.ready = true
	go p.monitor()
	return p, nil
}
func (p *Pool) initialize(ctx context.Context, cfg core.Config, addresses []string) error {
	var owned bool
	p.reads.Add(1)
	if err := p.db.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, cfg.DBSchema+".scrap_app.ram_pool").Scan(&owned); err != nil {
		return errors.New("identity ownership check failed")
	}
	if !owned {
		return errors.New("another process owns this identity pool")
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
	rows, err = tx.Query(ctx, `SELECT id::text,proxy_id,ordinal,session,assignment_count,revision,health FROM `+p.identitiesTable+` ORDER BY proxy_id,ordinal`)
	if err != nil {
		return err
	}
	for rows.Next() {
		state := &entry{}
		var proxyID int64
		var raw, rawHealth []byte
		if err = rows.Scan(&state.id, &proxyID, &state.ordinal, &raw, &state.assignments, &state.revision, &rawHealth); err != nil {
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
func (p *Pool) fail()       { p.mu.Lock(); p.ready = false; p.stop(); p.wakeLocked(); p.mu.Unlock() }
func (p *Pool) Ready() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.ready && !p.closing }
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Stats{Ready: p.ready && !p.closing, Proxies: len(p.proxies), Identities: len(p.entries), Queued: p.queued, Reads: p.reads.Load(), Writes: p.writes.Load()}
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
func (p *Pool) monitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.lifetime.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(p.lifetime, 5*time.Second)
			p.dbMu.Lock()
			err := p.db.Ping(ctx)
			p.dbMu.Unlock()
			cancel()
			if err != nil {
				p.fail()
				return
			}
		}
	}
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
		for _, proxy := range p.proxies {
			proxy.transport.CloseIdleConnections()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p.dbMu.Lock()
		p.closeErr = p.db.Close(ctx)
		p.dbMu.Unlock()
	})
	return p.closeErr
}
