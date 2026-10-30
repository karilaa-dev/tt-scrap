package instagramapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
)

const IdentityPrefix = "instagram:"
const UserAgent = "Instagram 452.1.0.53.64 Android (37/17; 420dpi; 1080x2400; Google/google; Pixel 7; panther; panther; en_US; 1096920925)"
const AppID = "567067343352427"

type Options struct {
	ActiveAccounts                int
	Store                         *Store
	AccountsFile                  string
	AccountsDir                   string
	PerAccountConcurrency         int
	PerAccountDownloadConcurrency int
	MaxQueuedRequests             int
	Timeout                       time.Duration
	MaxAttempts                   int
	RetryDelay                    time.Duration
	HTTPMaxConnections            int
}

type account struct {
	updating      bool
	login         *passwordCredentials
	refreshGate   chan struct{}
	refreshAfter  time.Time
	refreshErr    error
	id            string
	authorization string
	client        *http.Client
	mediaClient   *http.Client
	busy          int
	mediaBusy     int
	disabled      bool
	cooldown      time.Time
	username, ip  string
	device        *DeviceIdentity
}

type Pool struct {
	login         passwordLogin
	options       Options
	mu            sync.Mutex
	accounts      []*account
	byID          map[string]*account
	next          int
	queued        int
	pendingDeaths int
	updating      int
	changed       chan struct{}
	closed        bool
	apiOrigin     string
}

func Open(options Options) (*Pool, error) {
	if options.PerAccountConcurrency == 0 {
		options.PerAccountConcurrency = 1
	}
	if options.PerAccountDownloadConcurrency == 0 {
		options.PerAccountDownloadConcurrency = 1
	}
	if options.MaxQueuedRequests == 0 {
		options.MaxQueuedRequests = 1024
	}
	if options.Timeout == 0 {
		options.Timeout = 60 * time.Second
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
	}
	if options.HTTPMaxConnections == 0 {
		options.HTTPMaxConnections = 128
	}
	if options.ActiveAccounts < 0 || options.ActiveAccounts > 10000 || options.PerAccountConcurrency < 1 || options.PerAccountConcurrency > 64 || options.PerAccountDownloadConcurrency < 1 || options.PerAccountDownloadConcurrency > 64 || options.MaxQueuedRequests < 1 || options.Timeout < 0 || options.MaxAttempts < 1 || options.MaxAttempts > 10 || options.RetryDelay < 0 || options.HTTPMaxConnections < 1 || options.HTTPMaxConnections > 2048 {
		return nil, errors.New("invalid Instagram pool options")
	}
	configs, err := loadAccounts(options)
	if err != nil {
		return nil, err
	}
	p := &Pool{login: login, options: options, changed: make(chan struct{}), byID: make(map[string]*account), apiOrigin: "https://i.instagram.com"}
	if options.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err = options.Store.Seed(ctx, configs); err != nil {
			return nil, err
		}
		saved, e := options.Store.Load(ctx)
		if e != nil {
			return nil, e
		}
		for _, a := range saved {
			if err = p.addAccount(a.accountConfig, a.Dead, a.Username, a.IP); err != nil {
				p.Close()
				return nil, err
			}
		}
	} else {
		for _, config := range configs {
			if err = p.addAccount(config, false, "", ""); err != nil {
				p.Close()
				return nil, err
			}
		}
	}
	return p, nil
}

func (p *Pool) addAccount(config accountConfig, dead bool, username, ip string) error {
	if config.Cookies != nil {
		if config.Authorization != "" {
			return errors.New("invalid stored Instagram account")
		}
		auth, _, err := cookieAuthorization(config.Cookies)
		if err != nil {
			// An expired saved session must not prevent the other accounts from loading.
			if config.Login == nil {
				dead = true
			}
		} else {
			config.Authorization = auth
		}
	}
	if !accountIDPattern.MatchString(config.ID) || (config.Cookies == nil && !validAuthorization(config.Authorization)) {
		return errors.New("invalid stored Instagram account")
	}
	if err := config.Device.validate(); err != nil {
		return err
	}

	proxy := ""
	if config.ProxyURL != nil {
		proxy = *config.ProxyURL
	}
	transport, err := nettransport.New(proxy, p.options.HTTPMaxConnections)
	if err != nil {
		return errors.New("invalid Instagram account proxy configuration")
	}
	// Metadata never follows redirects: bearer credentials remain at the API origin.
	metadata := &http.Client{Transport: transport, Timeout: p.options.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	media := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("media redirect limit exceeded for Instagram")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("invalid Instagram media redirect")
		}
		stripCredentials(req.Header)
		return nil
	}}
	a := &account{login: config.Login, refreshGate: make(chan struct{}, 1), id: IdentityPrefix + config.ID, authorization: config.Authorization, client: metadata, mediaClient: media}
	if config.Device != nil {
		device := *config.Device
		a.device = &device
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		transport.CloseIdleConnections()
		return errors.New("closed Instagram pool")
	}
	if p.byID[a.id] != nil {
		transport.CloseIdleConnections()
		return nil
	}
	a.disabled = dead
	a.username = username
	a.ip = ip
	p.accounts = append(p.accounts, a)
	p.byID[a.id] = a
	p.notify()
	return nil
}

func (p *Pool) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.accounts) }

// Ready reports session availability, independent of transient capacity or cooldown.
func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.pendingDeaths > 0 || (p.options.Store != nil && p.options.Store.dirty.Load()) {
		return false
	}
	for _, a := range p.accounts {
		if !a.disabled {
			return true
		}
	}
	return false
}
func (p *Pool) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.notify()
	}
	accounts := append([]*account(nil), p.accounts...)
	p.mu.Unlock()
	for _, a := range accounts {
		a.client.CloseIdleConnections()
	}
}
func (p *Pool) notify() {
	if p.queued == 0 && p.updating == 0 {
		return
	}
	close(p.changed)
	p.changed = make(chan struct{})
}

// acquire balances metadata requests across accounts. CDN batches use an
// independent per-account capacity: a slow transfer cannot occupy metadata slots.
// Both queues share the configured waiting-work bound and immutable account route.
func (p *Pool) acquire(ctx context.Context, pinned string, avoid map[string]bool) (*account, func(), error) {
	queued := false
	p.mu.Lock()
	defer func() {
		if queued {
			p.queued--
		}
		p.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if p.closed {
			return nil, nil, core.NewError("service_not_configured", "Instagram account pool is closed", 503)
		}
		if pinned == "" && (p.pendingDeaths > 0 || (p.options.Store != nil && p.options.Store.dirty.Load())) {
			return nil, nil, dbFailure()
		}
		if len(p.accounts) == 0 {
			return nil, nil, core.NewError("service_not_configured", "Instagram accounts are not configured", 503)
		}
		var selected *account
		var busy *int
		available := false
		if pinned != "" {
			a := p.byID[pinned]
			if a == nil {
				return nil, nil, core.MissingAsset()
			}
			available = true
			// Signed CDN resources remain usable when metadata authentication
			// is challenged or cooling down. Never move them to another route.
			if a.mediaBusy < p.options.PerAccountDownloadConcurrency {
				selected, busy = a, &a.mediaBusy
			}
		} else {
			// Only the oldest N non-dead accounts are eligible. Cooldowns and
			// saturation do not promote reserves; permanent failure does.
			eligibleEnd := len(p.accounts)
			if p.options.ActiveAccounts > 0 {
				count := 0
				for index, a := range p.accounts {
					if !a.disabled {
						count++
						if count == p.options.ActiveAccounts {
							eligibleEnd = index + 1
							break
						}
					}
				}
			}
			chosen := -1
			now := time.Now()
			for pass := 0; pass < 2 && chosen < 0; pass++ {
				for n := 0; n < eligibleEnd; n++ {
					index := (p.next + n) % eligibleEnd
					a := p.accounts[index]
					if a.disabled || a.cooldown.After(now) {
						continue
					}
					available = true
					if a.updating || pass == 0 && avoid[a.id] || a.busy >= p.options.PerAccountConcurrency {
						continue
					}
					if chosen < 0 || a.busy < p.accounts[chosen].busy ||
						a.busy == p.accounts[chosen].busy && a.mediaBusy < p.accounts[chosen].mediaBusy {
						chosen = index
						if a.busy == 0 && a.mediaBusy == 0 {
							// This is the minimum possible load; scan order already
							// breaks ties fairly, so further accounts cannot win.
							break
						}
					}
				}
			}
			if chosen >= 0 {
				selected = p.accounts[chosen]
				busy = &selected.busy
				p.next = (chosen + 1) % len(p.accounts)
			}
		}
		if selected != nil {
			*busy++
			var once sync.Once
			release := func() { once.Do(func() { p.mu.Lock(); *busy--; p.notify(); p.mu.Unlock() }) }
			return selected, release, nil
		}
		if !available {
			return nil, nil, core.NewError("upstream_rate_limited", "No Instagram account is currently available; refresh challenged sessions or retry after cooldown", 503)
		}
		if !queued {
			if p.queued >= p.options.MaxQueuedRequests {
				return nil, nil, core.NewError("service_busy", "The Instagram account queue is full", 503)
			}
			p.queued++
			queued = true
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		p.mu.Lock()
	}
}

func stripCredentials(h http.Header) {
	for k := range h {
		lower := strings.ToLower(k)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || strings.HasPrefix(lower, "x-ig-") || strings.HasPrefix(lower, "ig-") {
			delete(h, k)
		}
	}
}

// WithMedia holds one download slot on the metadata account for a batch.
// Parallel ranges and carousel children retain that account's fixed egress route.
func (p *Pool) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	if !strings.HasPrefix(id, IdentityPrefix) {
		return core.MissingAsset()
	}
	a, release, err := p.acquire(ctx, id, nil)
	if err != nil {
		return err
	}
	defer release()
	return run(func(ctx context.Context, source string, headers http.Header) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil || req.URL.Scheme != "https" || req.URL.Hostname() == "" || req.URL.User != nil {
			return nil, core.Upstream("Invalid Instagram media URL")
		}
		req.Header = headers.Clone()
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		stripCredentials(req.Header)
		_, device := p.session(a)
		req.Header.Set("User-Agent", device.userAgent())
		response, err := a.mediaClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, core.NewError("upstream_network_error", "Instagram media request failed", 502)
		}
		return response, nil
	})
}
