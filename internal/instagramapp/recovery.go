package instagramapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/karilaa-dev/tt-scrap/internal/core"
)

// These credentials are private database state. Public account responses never
// marshal them. UserID binds recovery to the account verified during import.
type passwordCredentials struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret,omitempty"`
	UserID     string `json:"user_id"`
}

func (c Credentials) backup(userID string) *passwordCredentials {
	if c.Password == "" {
		return nil
	}
	return &passwordCredentials{Username: c.Username, Password: c.Password, TOTPSecret: c.TOTPSecret, UserID: userID}
}

type passwordLogin func(context.Context, *http.Client, string, Credentials) (loginResult, error)

func errorCode(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func authenticateAccount(ctx context.Context, client *http.Client, origin string, c Credentials, password passwordLogin) (loginResult, bool, error) {
	expectedID := ""
	if c.Cookies != nil {
		_, expectedID, _ = cookieSession(c.Cookies, true)
		_, _, cookieErr := cookieAuthorization(c.Cookies)
		if cookieErr == nil {
			session, err := loginCookies(ctx, client, origin, c)
			if err == nil {
				return session, true, nil
			}
			if errorCode(err) != "session_expired" || c.Password == "" {
				return loginResult{}, false, err
			}
		} else if c.Password == "" {
			return loginResult{}, false, cookieErr
		}
	}
	session, err := password(ctx, client, origin, c)
	if err == nil && expectedID != "" && session.UserID != expectedID {
		return loginResult{}, false, loginFailure("session_check_failed")
	}
	return session, false, err
}

func (s *Store) SetLogin(ctx context.Context, id string, backup *passwordCredentials) error {
	raw, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tag, err := db.Exec(ctx, `UPDATE `+s.accounts+` SET login_credentials=$2 WHERE id=$1 AND user_id=$3`, id, raw, backup.UserID)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("missing Instagram account")
		}
		return err
	})
}

func (s *Store) SaveSession(ctx context.Context, id, userID string, session loginResult) error {
	device, err := storedDeviceJSON(session.Device)
	if err != nil {
		return err
	}
	return s.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		tag, err := db.Exec(ctx, `UPDATE `+s.accounts+` SET android_authorization=$3,cookies=NULL,device=$4 WHERE id=$1 AND user_id=$2 AND NOT dead`, id, userID, session.Authorization, device)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("missing Instagram account")
		}
		return err
	})
}

func (p *Pool) session(a *account) (string, *DeviceIdentity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return a.authorization, a.device
}

func (p *Pool) lookupRecovering(ctx context.Context, a *account, mediaID string) (data map[string]any, state disposition, err error) {
	auth, device := p.session(a)
	// Tag the final outcome with the session used by that request, including
	// the retry after renewal, so late failures cannot disable its replacement.
	defer func() { state.authorization = auth }()
	state, err = disposition{renew: true, disable: true, retry: true, deadReason: "session_expired"}, loginFailure("session_expired")
	if auth != "" {
		data, state, err = p.lookup(ctx, a, mediaID, auth, device)
	}
	p.mu.Lock()
	canRenew := a.login != nil
	p.mu.Unlock()
	if !state.renew || !canRenew {
		return data, state, err
	}
	if err = p.renew(ctx, a, auth); err != nil {
		state = disposition{retry: true, cooldown: time.Minute}
		switch errorCode(err) {
		case "session_expired", "account_mismatch", "login_rejected", "unsupported_two_factor", "challenge_required", "account_restricted":
			state = disposition{retry: true, disable: true, deadReason: errorCode(err)}
		}
		if ctx.Err() != nil {
			return nil, disposition{}, ctx.Err()
		}
		return nil, state, err
	}
	auth, device = p.session(a)
	return p.lookup(ctx, a, mediaID, auth, device)
}

// One login per rejected session, even with concurrent metadata requests. Waiters
// can cancel independently. A saved replacement is published to RAM only after
// the database commit, so restart cannot resurrect the rejected cookie session.
func (p *Pool) renew(ctx context.Context, a *account, rejected string) error {
	select {
	case a.refreshGate <- struct{}{}:
		defer func() { <-a.refreshGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	auth, backup, device, disabled := a.authorization, a.login, a.device, a.disabled
	p.mu.Unlock()
	if auth != rejected {
		return nil
	}
	if disabled || backup == nil {
		return loginFailure("session_expired")
	}
	if time.Now().Before(a.refreshAfter) {
		return a.refreshErr
	}
	loginCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c := Credentials{Username: backup.Username, Password: backup.Password, TOTPSecret: backup.TOTPSecret, DeviceProfile: device.profileID(), device: device}
	session, err := p.login(loginCtx, a.client, p.apiOrigin, c)
	if err == nil && (session.UserID != backup.UserID || !validAuthorization(session.Authorization)) {
		err = loginFailure("account_mismatch")
	}
	if err == nil && p.options.Store != nil {
		err = p.options.Store.SaveSession(loginCtx, strings.TrimPrefix(a.id, IdentityPrefix), backup.UserID, session)
	}
	if err == nil {
		p.mu.Lock()
		a.authorization, a.device = session.Authorization, session.Device
		p.mu.Unlock()
	}
	if ctx.Err() == nil {
		a.refreshAfter = time.Now().Add(time.Minute)
		a.refreshErr = err
		if err == nil {
			a.refreshErr = loginFailure("session_expired")
		}
	}
	return err
}
