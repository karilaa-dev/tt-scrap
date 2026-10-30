package instagramapp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
)

func sameCookies(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

// Pause new metadata leases while old responses and death checkpoints drain.
// CDN downloads retain their route and can continue. The manager serializes
// imports; runtime recovery finishes within its existing metadata lease.
func (p *Pool) beginUpdate(ctx context.Context, a *account) (func(), error) {
	p.mu.Lock()
	a.updating = true
	p.updating++
	p.notify()
	done := func() { p.mu.Lock(); a.updating = false; p.updating--; p.notify(); p.mu.Unlock() }
	for a.busy > 0 || p.pendingDeaths > 0 {
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			done()
			return nil, ctx.Err()
		}
		p.mu.Lock()
	}
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		done()
		return nil, err
	}
	return done, nil
}

func (m *AccountManager) updateExisting(ctx context.Context, saved *storedAccount, c Credentials) (string, error) {
	p := m.pool
	p.mu.Lock()
	a := p.byID[IdentityPrefix+saved.ID]
	p.mu.Unlock()
	done, err := p.beginUpdate(ctx, a)
	if err != nil {
		return "", err
	}
	defer done()
	// A failed old death write must finish before a verified update can revive it.
	if err = m.store.Flush(ctx); err != nil {
		return "", err
	}
	select {
	case a.refreshGate <- struct{}{}:
		defer func() { <-a.refreshGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	p.mu.Lock()
	currentLogin, device, dead, auth := a.login, a.device, a.disabled, a.authorization
	p.mu.Unlock()
	backup := c.backup(saved.UserID)
	backupChanged := backup != nil && (currentLogin == nil || *backup != *currentLogin)
	cookieAuth, _, _ := cookieAuthorization(c.Cookies)
	sessionChanged := c.Cookies != nil && (dead || auth != cookieAuth || !sameCookies(c.Cookies, saved.Cookies))
	if !sessionChanged && !backupChanged {
		return "duplicate", nil
	}
	if sessionChanged {
		verifyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		c.device, c.DeviceProfile = device, device.profileID()
		session, usedCookies, err := authenticateAccount(verifyCtx, a.client, p.apiOrigin, c, m.login)
		if err != nil {
			return "", err
		}
		if session.UserID != saved.UserID {
			return "", loginFailure("account_mismatch")
		}
		var cookies json.RawMessage
		newAuth := session.Authorization
		if usedCookies {
			cookies = c.Cookies
			newAuth, _, err = cookieAuthorization(cookies)
			if err != nil {
				return "", err
			}
		}
		if err = m.store.ReplaceSession(ctx, saved.ID, saved.UserID, session, cookies, backup); err != nil {
			return "", err
		}
		p.mu.Lock()
		a.authorization, a.device = newAuth, session.Device
		a.disabled, a.cooldown = false, time.Time{}
		if backup != nil {
			a.login = backup
		}
		p.mu.Unlock()
		saved.Cookies, saved.Authorization, saved.Device, saved.Dead = cookies, session.Authorization, session.Device, false
	} else {
		if err = m.store.SetLogin(ctx, saved.ID, backup); err != nil {
			return "", err
		}
		p.mu.Lock()
		a.login = backup
		p.mu.Unlock()
	}
	if backup != nil {
		saved.Login = backup
	}
	a.refreshAfter, a.refreshErr = time.Time{}, nil
	return "updated", nil
}

func (s *Store) ReplaceSession(ctx context.Context, id, userID string, session loginResult, cookies json.RawMessage, backup *passwordCredentials) error {
	device, err := storedDeviceJSON(session.Device)
	if err != nil {
		return err
	}
	var loginJSON []byte
	if backup != nil {
		loginJSON, err = json.Marshal(backup)
		if err != nil {
			return err
		}
	}
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tag, err := db.Exec(ctx, `UPDATE `+s.accounts+` SET android_authorization=NULLIF($3,''),cookies=$4,device=$5,login_credentials=COALESCE($6::jsonb,login_credentials),dead=false,dead_reason=NULL,dead_at=NULL WHERE id=$1 AND user_id=$2`, id, userID, session.Authorization, cookies, device, loginJSON)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("missing Instagram account")
		}
		return err
	})
}
