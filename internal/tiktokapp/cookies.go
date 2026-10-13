package tiktokapp

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

type savedCookie struct {
	Key          string `json:"key"`
	Value        string `json:"value"`
	Domain       string `json:"domain"`
	Path         string `json:"path"`
	Secure       bool   `json:"secure,omitempty"`
	HTTPOnly     bool   `json:"httpOnly,omitempty"`
	HostOnly     bool   `json:"hostOnly"`
	Expires      string `json:"expires,omitempty"`
	SameSite     string `json:"sameSite,omitempty"`
	Creation     string `json:"creation,omitempty"`
	LastAccessed string `json:"lastAccessed,omitempty"`
}
type savedJar struct {
	Version              string        `json:"version"`
	StoreType            string        `json:"storeType"`
	RejectPublicSuffixes bool          `json:"rejectPublicSuffixes"`
	Cookies              []savedCookie `json:"cookies"`
}
type persistentJar struct {
	jar        *cookiejar.Jar
	saved      map[string]savedCookie
	encoded    string
	dirty      bool
	nextExpiry time.Time
}

func cookieKey(c savedCookie) string { return c.Domain + "\n" + c.Path + "\n" + c.Key }
func newPersistentJar(serialized string) *persistentJar {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	p := &persistentJar{jar: jar, saved: make(map[string]savedCookie), dirty: true}
	var stored savedJar
	if json.Unmarshal([]byte(serialized), &stored) != nil {
		return p
	}
	now := time.Now()
	for _, c := range stored.Cookies {
		if c.Key == "" || c.Domain == "" {
			continue
		}
		h := &http.Cookie{Name: c.Key, Value: c.Value, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if !c.HostOnly {
			h.Domain = c.Domain
		}
		if c.Expires != "" && c.Expires != "Infinity" {
			expiry, e := time.Parse(time.RFC3339Nano, c.Expires)
			if e != nil || !expiry.After(now) {
				continue
			}
			h.Expires = expiry
		}
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		u := &url.URL{Scheme: scheme, Host: strings.TrimPrefix(c.Domain, "."), Path: c.Path}
		jar.SetCookies(u, []*http.Cookie{h})
		p.saved[cookieKey(c)] = c
	}
	return p
}
func defaultCookiePath(path string) string {
	if path == "" || path[0] != '/' {
		return "/"
	}
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "/"
	}
	return path[:idx]
}
func (p *persistentJar) accept(u *url.URL, cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}
	p.jar.SetCookies(u, cookies)
	now := time.Now().UTC()
	for _, c := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		hostOnly := domain == ""
		if hostOnly {
			domain = strings.ToLower(u.Hostname())
		}
		host := strings.ToLower(u.Hostname())
		if host != domain && !strings.HasSuffix(host, "."+domain) {
			continue
		}
		if !hostOnly && publicsuffix.List.PublicSuffix(domain) == domain {
			continue
		}
		path := c.Path
		if path == "" || path[0] != '/' {
			path = defaultCookiePath(u.Path)
		}
		record := savedCookie{Key: c.Name, Value: c.Value, Domain: domain, Path: path, Secure: c.Secure, HTTPOnly: c.HttpOnly, HostOnly: hostOnly, Creation: now.Format(time.RFC3339Nano), LastAccessed: now.Format(time.RFC3339Nano)}
		key := cookieKey(record)
		p.dirty = true
		if c.MaxAge < 0 || (!c.Expires.IsZero() && !c.Expires.After(now)) {
			delete(p.saved, key)
			continue
		}
		if old, ok := p.saved[key]; ok {
			record.Creation = old.Creation
		}
		if c.MaxAge > 0 {
			record.Expires = now.Add(time.Duration(c.MaxAge) * time.Second).Format(time.RFC3339Nano)
		} else if !c.Expires.IsZero() {
			record.Expires = c.Expires.UTC().Format(time.RFC3339Nano)
		}
		switch c.SameSite {
		case http.SameSiteLaxMode:
			record.SameSite = "lax"
		case http.SameSiteStrictMode:
			record.SameSite = "strict"
		case http.SameSiteNoneMode:
			record.SameSite = "none"
		}
		p.saved[key] = record
	}
}
func (p *persistentJar) serialize() string {
	now := time.Now()
	if !p.dirty && (p.nextExpiry.IsZero() || now.Before(p.nextExpiry)) {
		return p.encoded
	}
	stored := savedJar{Version: "tough-cookie@6.0.2", StoreType: "MemoryCookieStore", RejectPublicSuffixes: true, Cookies: make([]savedCookie, 0, len(p.saved))}
	p.nextExpiry = time.Time{}
	for key, c := range p.saved {
		if c.Expires != "" && c.Expires != "Infinity" {
			expiry, e := time.Parse(time.RFC3339Nano, c.Expires)
			if e != nil || !expiry.After(now) {
				delete(p.saved, key)
				continue
			}
			if p.nextExpiry.IsZero() || expiry.Before(p.nextExpiry) {
				p.nextExpiry = expiry
			}
		}
		stored.Cookies = append(stored.Cookies, c)
	}
	sort.Slice(stored.Cookies, func(i, j int) bool { return cookieKey(stored.Cookies[i]) < cookieKey(stored.Cookies[j]) })
	b, _ := json.Marshal(stored)
	p.encoded, p.dirty = string(b), false
	return p.encoded
}
