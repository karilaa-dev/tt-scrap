package instagramapp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

// Store shares the application's schema ownership boundary (identity.Open must
// succeed first). It uses a separate connection, never the TikTok lease connection.
type Store struct {
	mu                 sync.Mutex
	db                 *pgx.Conn
	config             *pgx.ConnConfig
	accounts, attempts string
	pending            map[string]string
	pendingCooldowns   map[string]time.Time
	dirty              atomic.Bool
}
type storedAccount struct {
	accountConfig
	Username, UserID, IP string
	Dead                 bool
	DeadReason           string
	CooldownUntil        time.Time
}

func dbFailure() error {
	return core.NewError("instagram_database_unavailable", "Instagram account database is unavailable", 503)
}
func OpenStore(ctx context.Context, cfg core.Config) (*Store, error) {
	config, e := pgx.ParseConfig(cfg.DBURL)
	if e != nil {
		return nil, dbFailure()
	}
	config.ConnectTimeout = 5 * time.Second
	config.RuntimeParams["statement_timeout"] = "5000"
	s := &Store{config: config, accounts: pgx.Identifier{cfg.DBSchema, "scrap_instagram_accounts"}.Sanitize(), attempts: pgx.Identifier{cfg.DBSchema, "scrap_instagram_attempts"}.Sanitize(), pending: map[string]string{}, pendingCooldowns: map[string]time.Time{}}
	e = s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+s.accounts+` (
   id TEXT PRIMARY KEY, ordinal BIGSERIAL UNIQUE NOT NULL, username TEXT UNIQUE, user_id TEXT UNIQUE,
   android_authorization TEXT NOT NULL UNIQUE, proxy_url TEXT NOT NULL DEFAULT '', egress_ip TEXT NOT NULL DEFAULT '',
   dead BOOLEAN NOT NULL DEFAULT false, dead_reason TEXT, dead_at TIMESTAMPTZ,
   created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`)
		if e != nil {
			return e
		}
		// Nullable for pre-profile sessions: retain their previous user agent and
		// do not invent IDs for a login whose original device state was not saved.
		if _, e = db.Exec(ctx, `ALTER TABLE `+s.accounts+` ADD COLUMN IF NOT EXISTS device JSONB`); e != nil {
			return e
		}
		if _, e = db.Exec(ctx, `ALTER TABLE `+s.accounts+` ADD COLUMN IF NOT EXISTS cookies JSONB, ADD COLUMN IF NOT EXISTS login_credentials JSONB, ALTER COLUMN android_authorization DROP NOT NULL`); e != nil {
			return e
		}
		if _, e = db.Exec(ctx, `ALTER TABLE `+s.accounts+` ADD COLUMN IF NOT EXISTS cooldown_until TIMESTAMPTZ`); e != nil {
			return e
		}
		if _, e = db.Exec(ctx, `ALTER TABLE `+s.accounts+` ADD COLUMN IF NOT EXISTS session_state JSONB`); e != nil {
			return e
		}
		_, e = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+s.attempts+` (
   id UUID PRIMARY KEY, source_id TEXT NOT NULL, legacy_used BOOLEAN NOT NULL DEFAULT false,
   succeeded BOOLEAN, error_code TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), finished_at TIMESTAMPTZ)`)
		return e
	})
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) withDB(ctx context.Context, run func(context.Context, *pgx.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s.db == nil || s.db.IsClosed() {
		var e error
		s.db, e = pgx.ConnectConfig(ctx, s.config)
		if e != nil {
			return dbFailure()
		}
	}
	if e := run(ctx, s.db); e != nil {
		return dbFailure()
	}
	return nil
}
func (s *Store) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Flush(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_ = s.db.Close(ctx)
	}
}
func (s *Store) Seed(ctx context.Context, configs []accountConfig) error {
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tx, e := db.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		for _, a := range configs {
			device, err := storedDeviceJSON(a.Device)
			if err != nil {
				return err
			}
			state, err := storedSessionJSON(a.Session)
			if err != nil {
				return err
			}
			proxy := ""
			if a.ProxyURL != nil {
				proxy = *a.ProxyURL
			}
			_, e = tx.Exec(ctx, `INSERT INTO `+s.accounts+` (id,android_authorization,proxy_url,device,session_state) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, a.ID, a.Authorization, proxy, device, state)
			if e != nil {
				return e
			}
		}
		return tx.Commit(ctx)
	})
}
func (s *Store) Load(ctx context.Context) ([]storedAccount, error) {
	result := []storedAccount{}
	e := s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		rows, e := db.Query(ctx, `SELECT id,COALESCE(android_authorization,''),proxy_url,COALESCE(username,''),COALESCE(user_id,''),egress_ip,dead,device,cookies,login_credentials,COALESCE(dead_reason,''),cooldown_until,session_state FROM `+s.accounts+` ORDER BY ordinal`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a storedAccount
			var proxy string
			var device, backup, state []byte
			var cooldown *time.Time
			if e = rows.Scan(&a.ID, &a.Authorization, &proxy, &a.Username, &a.UserID, &a.IP, &a.Dead, &device, &a.Cookies, &backup, &a.DeadReason, &cooldown, &state); e != nil {
				return e
			}
			if len(state) != 0 {
				if e = json.Unmarshal(state, &a.Session); e != nil {
					return e
				}
			}
			if cooldown != nil {
				a.CooldownUntil = *cooldown
			}
			if len(device) != 0 {
				if e = json.Unmarshal(device, &a.Device); e != nil {
					return e
				}
				if a.Device == nil {
					return errors.New("invalid stored Instagram device identity")
				}
				if e = a.Device.validate(); e != nil {
					return e
				}
			}
			if len(backup) != 0 {
				if e = json.Unmarshal(backup, &a.Login); e != nil {
					return e
				}
				if a.Login != nil && (a.Login.Username == "" || a.Login.Password == "" || a.Login.UserID != a.UserID) {
					return errors.New("invalid stored Instagram login credentials")
				}
			}
			a.ProxyURL = &proxy
			result = append(result, a)
		}
		return rows.Err()
	})
	return result, e
}
func (s *Store) Insert(ctx context.Context, a storedAccount) (bool, error) {
	device, err := storedDeviceJSON(a.Device)
	if err != nil {
		return false, err
	}
	state, err := storedSessionJSON(a.Session)
	if err != nil {
		return false, err
	}
	var backup []byte
	if a.Login != nil {
		backup, err = json.Marshal(a.Login)
		if err != nil {
			return false, err
		}
	}
	inserted := false
	e := s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tag, e := db.Exec(ctx, `INSERT INTO `+s.accounts+` (id,username,user_id,android_authorization,proxy_url,egress_ip,device,cookies,login_credentials,session_state) VALUES($1,lower($2),$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, a.ID, a.Username, a.UserID, a.Authorization, *a.ProxyURL, a.IP, device, a.Cookies, backup, state)
		inserted = tag.RowsAffected() == 1
		return e
	})
	return inserted, e
}
func storedDeviceJSON(device *DeviceIdentity) ([]byte, error) {
	if device == nil {
		return nil, nil
	}
	if err := device.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(device)
}

func storedSessionJSON(state *SessionState) ([]byte, error) {
	if state == nil {
		return nil, nil
	}
	return json.Marshal(state)
}

// A late response must not replace the context of a newer login. Lock the row
// while comparing the session, including cookie imports whose Android bearer is
// derived rather than stored, so a separate process cannot race this checkpoint.
func (s *Store) SaveSessionState(ctx context.Context, id, auth string, state *SessionState) error {
	raw, err := storedSessionJSON(state)
	if err != nil {
		return err
	}
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		var current string
		var cookies json.RawMessage
		err = tx.QueryRow(ctx, `SELECT COALESCE(android_authorization,''),cookies FROM `+s.accounts+` WHERE id=$1 FOR UPDATE`, id).Scan(&current, &cookies)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(cookies) != 0 {
			current, _, err = cookieAuthorization(cookies)
			if err != nil {
				return nil
			}
		}
		if auth == "" || current != auth {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE `+s.accounts+` SET session_state=$2 WHERE id=$1`, id, raw); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}
func (s *Store) MarkDead(ctx context.Context, id, reason string) error {
	return s.MarkHealth(ctx, id, reason, time.Time{})
}

func (s *Store) MarkCooldown(ctx context.Context, id string, until time.Time) error {
	return s.MarkHealth(ctx, id, "", until)
}

// Record both outcomes together when a response contains a restriction and a
// Retry-After. A verified replacement must not erase either server signal.
func (s *Store) MarkHealth(ctx context.Context, id, reason string, until time.Time) error {
	if reason == "" && until.IsZero() {
		return nil
	}
	s.mu.Lock()
	if reason != "" {
		s.pending[id] = reason
	}
	if !until.IsZero() {
		if s.pendingCooldowns == nil {
			s.pendingCooldowns = make(map[string]time.Time)
		}
		if until.After(s.pendingCooldowns[id]) {
			s.pendingCooldowns[id] = until
		}
	}
	s.dirty.Store(true)
	s.mu.Unlock()
	return s.Flush(ctx)
}

// Failed health writes remain pending. Metadata admission calls Flush before
// leasing again, so a DB outage cannot silently revive disabled or throttled
// accounts. A failed write preserves the original absolute cooldown deadline.
func (s *Store) Flush(ctx context.Context) error {
	if !s.dirty.Load() {
		return nil
	}
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		for id, reason := range s.pending {
			var until *time.Time
			if deadline, ok := s.pendingCooldowns[id]; ok {
				until = &deadline
			}
			tag, e := db.Exec(ctx, `UPDATE `+s.accounts+` SET dead=true,dead_reason=$2,dead_at=COALESCE(dead_at,clock_timestamp()),cooldown_until=GREATEST(cooldown_until,$3::timestamptz) WHERE id=$1`, id, reason, until)
			if e != nil {
				return e
			}
			if tag.RowsAffected() != 1 {
				return errors.New("missing account")
			}
			delete(s.pending, id)
			delete(s.pendingCooldowns, id)
		}
		for id, until := range s.pendingCooldowns {
			tag, e := db.Exec(ctx, `UPDATE `+s.accounts+` SET cooldown_until=GREATEST(cooldown_until,$2::timestamptz) WHERE id=$1`, id, until)
			if e != nil {
				return e
			}
			if tag.RowsAffected() != 1 {
				return errors.New("missing account")
			}
			delete(s.pendingCooldowns, id)
		}
		s.dirty.Store(false)
		return nil
	})
}

// Every actual method attempt has its own row, including failed legacy requests.
// Cache hits and coalesced callers do not generate fictitious upstream attempts.
func (s *Store) Attempt(ctx context.Context, source string, legacy bool, run func() (core.InstagramExtraction, error)) (core.InstagramExtraction, error) {
	id := core.NewID()
	if e := s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, `INSERT INTO `+s.attempts+` (id,source_id,legacy_used) VALUES($1,$2,$3)`, id, source, legacy)
		return e
	}); e != nil {
		return core.InstagramExtraction{}, e
	}
	result, err := run()
	code := ""
	if err != nil {
		code = "upstream_error"
		var failure *core.Error
		if errors.As(err, &failure) {
			code = failure.Code
		}
		if errors.Is(err, context.Canceled) {
			code = "request_canceled"
		}
	}
	// Keep the original request outcome even if the caller has disconnected.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	e := s.withDB(saveCtx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, `UPDATE `+s.attempts+` SET succeeded=$2,error_code=NULLIF($3,''),finished_at=clock_timestamp() WHERE id=$1`, id, err == nil, code)
		return e
	})
	if e != nil {
		return core.InstagramExtraction{}, e
	}
	return result, err
}
