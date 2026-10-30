package instagramapp

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
)

const MaxImportAccounts = 100

func invalidImport() error {
	return core.NewError("invalid_account_import", "Supply up to 100 accounts with browser cookies and/or username, password and a valid TOTP secret", 422)
}
func ParseCSV(reader io.Reader) ([]Credentials, error) {
	r := csv.NewReader(reader)
	header, e := r.Read()
	if e != nil {
		return nil, invalidImport()
	}
	columns := map[string]int{}
	for n, name := range header {
		name = strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))
		if _, ok := columns[name]; ok {
			return nil, invalidImport()
		}
		columns[name] = n
	}
	for _, name := range []string{"User ID / Email Address", "Password", "First Secret Question"} {
		if _, ok := columns[name]; !ok {
			return nil, invalidImport()
		}
	}
	var result []Credentials
	for {
		row, e := r.Read()
		if e == io.EOF {
			break
		}
		if e != nil || len(result) >= MaxImportAccounts {
			return nil, invalidImport()
		}
		c := Credentials{Username: row[columns["User ID / Email Address"]], Password: row[columns["Password"]], TOTPSecret: row[columns["First Secret Question"]]}
		if column, ok := columns["Cookies"]; ok && strings.TrimSpace(row[column]) != "" {
			c.Cookies = json.RawMessage(row[column])
		}
		if column, ok := columns["Device Profile"]; ok {
			c.DeviceProfile = row[column]
		}
		result = append(result, c)
	}
	return validateCredentials(result)
}
func validateCredentials(accounts []Credentials) ([]Credentials, error) {
	if len(accounts) == 0 || len(accounts) > MaxImportAccounts {
		return nil, invalidImport()
	}
	seen := map[string]bool{}
	for n := range accounts {
		a := &accounts[n]
		// Spreadsheet exports prefix usernames with an apostrophe to force text.
		// Strip that marker before duplicate detection and login; passwords are
		// opaque and must not receive the same normalization.
		a.Username = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a.Username), "'"))
		a.DeviceProfile = strings.TrimSpace(a.DeviceProfile)
		if !validDeviceSelection(a.DeviceProfile) {
			return nil, invalidDeviceProfile()
		}
		if a.Cookies != nil {
			_, id, err := cookieSession(a.Cookies, a.Password != "")
			if err != nil || seen["cookie:"+id] {
				return nil, invalidImport()
			}
			seen["cookie:"+id] = true
			if a.Username == "" && a.Password == "" && a.TOTPSecret == "" {
				continue
			}
		}
		key := strings.ToLower(a.Username)
		if a.Username == "" || len(a.Username) > 254 || a.Password == "" || len(a.Password) > 1024 || len(a.TOTPSecret) > 2048 || seen[key] || strings.ContainsAny(a.Username, "\r\n\x00") {
			return nil, invalidImport()
		}
		seen[key] = true
		if a.TOTPSecret != "" {
			key, e := totpKey(a.TOTPSecret)
			clear(key)
			if e != nil {
				return nil, invalidImport()
			}
		}
	}
	return accounts, nil
}

type ImportResult struct {
	Row       int    `json:"row"`
	AccountID string `json:"account_id,omitempty"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}
type AccountStatus struct {
	ID            string `json:"id"`
	Username      string `json:"username,omitempty"`
	EgressIP      string `json:"egress_ip,omitempty"`
	State         string `json:"state"`
	DeviceProfile string `json:"device_profile"`
}
type AccountManager struct {
	pool    *Pool
	store   *Store
	proxies []string
	gate    chan struct{}
	probe   func(context.Context, *http.Client) (string, error)
	login   func(context.Context, *http.Client, string, Credentials) (loginResult, error)
}

func NewAccountManager(pool *Pool, store *Store, proxies []string) *AccountManager {
	return &AccountManager{pool: pool, store: store, proxies: append([]string(nil), proxies...), gate: make(chan struct{}, 1), probe: probeIP, login: login}
}
func probeIP(ctx context.Context, client *http.Client) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=json", nil)
	res, e := client.Do(req)
	if e != nil {
		return "", loginFailure("proxy_check_failed")
	}
	defer res.Body.Close()
	var value struct {
		IP string `json:"ip"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 1024)).Decode(&value) != nil || net.ParseIP(value.IP) == nil {
		return "", loginFailure("proxy_check_failed")
	}
	return value.IP, nil
}
func (m *AccountManager) Status() []AccountStatus {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]AccountStatus, 0, len(p.accounts))
	active := 0
	for _, a := range p.accounts {
		state := "dead"
		if !a.disabled {
			state = "reserve"
			if p.options.ActiveAccounts == 0 || active < p.options.ActiveAccounts {
				state = "active"
			}
			active++
		}
		result = append(result, AccountStatus{ID: strings.TrimPrefix(a.id, IdentityPrefix), Username: a.username, EgressIP: a.ip, State: state, DeviceProfile: a.device.profileID()})
	}
	return result
}
func (m *AccountManager) Import(ctx context.Context, credentials []Credentials) ([]ImportResult, error) {
	credentials, e := validateCredentials(credentials)
	if e != nil {
		return nil, e
	}
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	default:
		return nil, core.NewError("import_busy", "An Instagram account import is already running", 409)
	}
	existing, e := m.store.Load(ctx)
	if e != nil {
		return nil, e
	}
	known := map[string]string{}
	knownIDs := map[string]string{}
	counts := map[string]int{}
	for _, a := range existing {
		if err := m.pool.addAccount(a.accountConfig, a.Dead, a.Username, a.IP); err != nil {
			return nil, err
		}
		knownIDs[a.UserID] = a.ID
		if a.Username != "" {
			known[strings.ToLower(a.Username)] = a.ID
		}
		if !a.Dead {
			counts[*a.ProxyURL]++
		}
	}
	results := make([]ImportResult, 0, len(credentials))
	unavailable := map[string]bool{}
	for n, c := range credentials {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		row := ImportResult{Row: n + 1}
		id := known[strings.ToLower(c.Username)]
		if c.Cookies != nil {
			_, userID, _ := cookieSession(c.Cookies, true)
			id = knownIDs[userID]
		}
		if id != "" {
			row.AccountID = id
			for n := range existing {
				if existing[n].ID == id {
					wasDead := existing[n].Dead
					row.Status, e = m.updateExisting(ctx, &existing[n], c)
					if e == nil && wasDead && !existing[n].Dead {
						counts[*existing[n].ProxyURL]++
					}
					break
				}
			}
			if e != nil {
				if errorCode(e) == "instagram_database_unavailable" || ctx.Err() != nil {
					return results, e
				}
				var stop bool
				results, stop = appendImportFailure(results, row, len(credentials), e)
				if stop {
					break
				}
			} else {
				results = append(results, row)
			}
			continue
		}
		// Only routes are retried here; credentials are submitted on one checked
		// route once. Least assigned route first; multiple accounts per IP allowed.
		proxies := append([]string(nil), m.proxies...)
		sort.SliceStable(proxies, func(i, j int) bool { return counts[proxies[i]] < counts[proxies[j]] })
		var client *http.Client
		var selected, ip string
		for _, proxy := range proxies {
			if unavailable[proxy] {
				continue
			}
			transport, err := nettransport.New(proxy, m.pool.options.HTTPMaxConnections)
			if err != nil {
				continue
			}
			candidate := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			address, err := m.probe(checkCtx, candidate)
			cancel()
			if err != nil {
				candidate.CloseIdleConnections()
				unavailable[proxy] = true
				continue
			}
			client, selected, ip = candidate, proxy, address
			break
		}
		if client == nil {
			row.Status = "failed"
			row.ErrorCode = "proxy_check_failed"
			results = append(results, row)
			continue
		}
		loginCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		session, usedCookies, err := authenticateAccount(loginCtx, client, m.pool.apiOrigin, c, m.login)
		cancel()
		client.CloseIdleConnections()
		if err != nil {
			var stop bool
			results, stop = appendImportFailure(results, row, len(credentials), err)
			if stop {
				break
			}
			continue
		}
		// A renamed username or email can identify an existing cookie account
		// only after login. Add its backup instead of losing it to a DB conflict.
		if id := knownIDs[session.UserID]; id != "" {
			row.AccountID = id
			for n := range existing {
				if existing[n].ID == id {
					row.Status, e = m.updateExisting(ctx, &existing[n], Credentials{Username: c.Username, Password: c.Password, TOTPSecret: c.TOTPSecret})
					break
				}
			}
			if e != nil {
				return results, e
			}
			known[strings.ToLower(c.Username)] = id
			results = append(results, row)
			continue
		}
		var cookies json.RawMessage
		if usedCookies {
			cookies = c.Cookies
		}
		account := storedAccount{accountConfig: accountConfig{ID: core.NewID(), Login: c.backup(session.UserID), Cookies: cookies, Authorization: session.Authorization, ProxyURL: &selected, Device: session.Device}, Username: strings.ToLower(session.Username), UserID: session.UserID, IP: ip}
		inserted, err := m.store.Insert(ctx, account)
		if err != nil {
			return results, err
		}
		if !inserted {
			row.Status = "duplicate"
		} else {
			if err = m.pool.addAccount(account.accountConfig, false, account.Username, ip); err != nil {
				return results, err
			}
			row.Status = "imported"
			row.AccountID = account.ID
			counts[selected]++
			if c.Username != "" {
				known[strings.ToLower(c.Username)] = account.ID
			}
			known[account.Username] = account.ID
			knownIDs[account.UserID] = account.ID
			existing = append(existing, account)
		}
		results = append(results, row)
	}
	return results, nil
}

func appendImportFailure(results []ImportResult, row ImportResult, total int, err error) ([]ImportResult, bool) {
	row.Status, row.ErrorCode = "failed", errorCode(err)
	if row.ErrorCode == "" {
		row.ErrorCode = "login_failed"
	}
	results = append(results, row)
	stop := row.ErrorCode == "upstream_rate_limited"
	if stop {
		for rest := row.Row + 1; rest <= total; rest++ {
			results = append(results, ImportResult{Row: rest, Status: "skipped", ErrorCode: "batch_rate_limited"})
		}
	}
	return results, stop
}
