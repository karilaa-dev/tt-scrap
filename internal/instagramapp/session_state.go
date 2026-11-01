package instagramapp

import (
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

const maxSessionCookies = 128
const maxSessionValue = 8192

// SessionState contains private, server-issued state for one account. It must
// be stored with the same protections as that account's authorization token.
// Cookies include their scope and absolute expiry; saving and restoring a
// session never grants a cookie a fresh Max-Age interval.
type SessionState struct {
	MID      string          `json:"mid,omitempty"`
	WWWClaim string          `json:"www_claim,omitempty"`
	RUR      string          `json:"rur,omitempty"`
	Cookies  []SessionCookie `json:"cookies,omitempty"`
}

type SessionCookie struct {
	Origin string      `json:"origin"`
	Cookie http.Cookie `json:"cookie"`
}

type sessionContext struct {
	mu      sync.Mutex
	jar     *cookiejar.Jar
	allowed map[string]bool
	state   SessionState
}

// extraOrigins are for an internally overridden API endpoint in local tests.
// They are never loaded from persisted state or supplied by an upstream server.
func newSessionContext(saved *SessionState, extraOrigins ...string) (*sessionContext, error) {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	s := &sessionContext{jar: jar, allowed: map[string]bool{"https://i.instagram.com": true, "https://b.i.instagram.com": true}}
	for _, origin := range extraOrigins {
		u, err := url.Parse(origin)
		if err != nil || sessionOrigin(u) == "" {
			return nil, errors.New("invalid Instagram session origin")
		}
		s.allowed[sessionOrigin(u)] = true
	}
	if saved == nil {
		return s, nil
	}
	if len(saved.Cookies) > maxSessionCookies || !validSessionValue(saved.MID) || !validSessionValue(saved.WWWClaim) || !validSessionValue(saved.RUR) {
		return nil, errors.New("invalid stored Instagram session state")
	}
	s.state.MID, s.state.WWWClaim, s.state.RUR = saved.MID, saved.WWWClaim, saved.RUR
	now := time.Now()
	for _, item := range saved.Cookies {
		u, err := url.Parse(item.Origin)
		if err != nil || !s.accepts(u) || item.Cookie.MaxAge != 0 {
			return nil, errors.New("invalid stored Instagram session cookie")
		}
		cookie, ok := normalizeSessionCookie(u, &item.Cookie, now)
		if !ok {
			return nil, errors.New("invalid stored Instagram session cookie")
		}
		s.setCookieLocked(u, cookie, now)
	}
	return s, nil
}

func sessionOrigin(u *url.URL) string {
	if u == nil || u.User != nil || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if u.Scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	return u.Scheme + "://" + host
}

func (s *sessionContext) accepts(u *url.URL) bool {
	return s != nil && s.allowed[sessionOrigin(u)]
}

func validSessionValue(value string) bool {
	if len(value) > maxSessionValue {
		return false
	}
	for _, c := range value {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

// Apply adds only supported session headers. The client's Jar handles cookies;
// a CDN client must not receive this context or these headers.
func (s *sessionContext) Apply(req *http.Request) {
	if req == nil || !s.accepts(req.URL) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for name, value := range map[string]string{"X-MID": s.state.MID, "X-IG-WWW-Claim": s.state.WWWClaim, "IG-U-RUR": s.state.RUR} {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
}

// Observe never copies arbitrary upstream headers or rotates authorization.
// Cookies are observed separately through http.Client's CookieJar contract.
func (s *sessionContext) Observe(origin *url.URL, header http.Header) {
	if !s.accepts(origin) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, target := range map[string]*string{"ig-set-x-mid": &s.state.MID, "x-ig-set-www-claim": &s.state.WWWClaim, "ig-set-ig-u-rur": &s.state.RUR} {
		values := header.Values(name)
		if len(values) == 1 && validSessionValue(values[0]) {
			*target = values[0]
		}
	}
}

func (s *sessionContext) Cookies(u *url.URL) []*http.Cookie {
	if !s.accepts(u) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jar.Cookies(u)
}

func (s *sessionContext) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !s.accepts(u) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneLocked(now)
	for _, input := range cookies {
		cookie, ok := normalizeSessionCookie(u, input, now)
		if ok {
			s.setCookieLocked(u, cookie, now)
		}
	}
}

func cookieScope(u *url.URL, cookie *http.Cookie) string {
	domain := cookie.Domain
	if domain == "" {
		domain = strings.ToLower(u.Hostname())
	}
	return domain + "\n" + cookie.Path + "\n" + cookie.Name
}

func (s *sessionContext) setCookieLocked(u *url.URL, cookie *http.Cookie, now time.Time) {
	key := cookieScope(u, cookie)
	index := -1
	for i := range s.state.Cookies {
		old := &s.state.Cookies[i]
		oldURL, _ := url.Parse(old.Origin)
		if cookieScope(oldURL, &old.Cookie) == key {
			index = i
			break
		}
	}
	remove := cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(now))
	if remove {
		s.jar.SetCookies(u, []*http.Cookie{cookie})
		if index >= 0 {
			s.state.Cookies = append(s.state.Cookies[:index], s.state.Cookies[index+1:]...)
		}
		return
	}
	if index < 0 && len(s.state.Cookies) == maxSessionCookies {
		return // Do not let persistence and the live jar diverge at the bound.
	}
	s.jar.SetCookies(u, []*http.Cookie{cookie})
	item := SessionCookie{Origin: sessionOrigin(u), Cookie: *cookie}
	if index >= 0 {
		s.state.Cookies[index] = item // Retain creation order for equal-path cookies.
	} else {
		s.state.Cookies = append(s.state.Cookies, item)
	}
}

func (s *sessionContext) pruneLocked(now time.Time) {
	remaining := s.state.Cookies[:0]
	for _, item := range s.state.Cookies {
		if item.Cookie.Expires.IsZero() || item.Cookie.Expires.After(now) {
			remaining = append(remaining, item)
		} else {
			u, _ := url.Parse(item.Origin)
			cookie := item.Cookie
			cookie.MaxAge = -1
			s.jar.SetCookies(u, []*http.Cookie{&cookie})
		}
	}
	s.state.Cookies = remaining
}

func (s *sessionContext) Snapshot() *SessionState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	result := s.state
	result.Cookies = append([]SessionCookie(nil), s.state.Cookies...)
	return &result
}

// Normalize the RFC 6265 scope before indexing cookies. The standard library's
// public-suffix-aware jar still performs cookie selection and path matching.
// Raw/unparsed attributes are deliberately not retained in private snapshots.
func normalizeSessionCookie(u *url.URL, input *http.Cookie, now time.Time) (*http.Cookie, bool) {
	if input == nil || len(input.Name)+len(input.Value)+len(input.Domain)+len(input.Path) > maxSessionValue || input.Partitioned {
		return nil, false
	}
	c := *input
	c.Raw, c.RawExpires, c.Unparsed = "", "", nil
	if c.Path == "" || !strings.HasPrefix(c.Path, "/") {
		c.Path = "/"
		if i := strings.LastIndex(u.Path, "/"); i > 0 {
			c.Path = u.Path[:i]
		}
	}
	host := strings.ToLower(u.Hostname())
	domain := c.Domain
	if domain != "" {
		if net.ParseIP(host) != nil {
			if domain != host {
				return nil, false
			}
			c.Domain = ""
		} else {
			domain = strings.ToLower(strings.TrimPrefix(domain, "."))
			if domain == "" || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || (host != domain && !strings.HasSuffix(host, "."+domain)) {
				return nil, false
			}
			if suffix, _ := publicsuffix.PublicSuffix(domain); suffix == domain {
				if host != domain {
					return nil, false
				}
				c.Domain = ""
			} else {
				c.Domain = domain
			}
		}
	}
	if c.MaxAge > 0 {
		seconds := min(int64(c.MaxAge), int64((1<<63-1)/time.Second))
		c.Expires = now.Add(time.Duration(seconds) * time.Second)
		c.MaxAge = 0
	}
	if c.Valid() != nil {
		return nil, false
	}
	return &c, true
}
