package tiktokapp

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Account holds the login token separately from anonymous device sessions.
type Account struct {
	SessionID string
}

func (a Account) Valid() bool { return a.SessionID != "" }

func isAccountCookie(name string) bool {
	switch name {
	case "sid_tt", "sessionid", "sessionid_ss":
		return true
	}
	return false
}

type accountCookieJar struct {
	account Account
	updates *cookiejar.Jar
	scopes  *cookiejar.Jar
}

func newAccountCookieJar(account Account) *accountCookieJar {
	options := &cookiejar.Options{PublicSuffixList: publicsuffix.List}
	updates, _ := cookiejar.New(options)
	scopes, _ := cookiejar.New(options)
	return &accountCookieJar{account: account, updates: updates, scopes: scopes}
}

func (j *accountCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if !j.account.Valid() || !IsAppHost(u) || !strings.HasPrefix(u.Path, "/aweme/") {
		return nil
	}
	var cookies []*http.Cookie
	updatedCookies := j.updates.Cookies(u)
	scopes := j.scopes.Cookies(u)
	for _, name := range []string{"sid_tt", "sessionid", "sessionid_ss"} {
		cookie := &http.Cookie{Name: name, Value: j.account.SessionID}
		for _, scope := range scopes {
			if scope.Name == name {
				cookie = nil
				break
			}
		}
		for _, updated := range updatedCookies {
			if updated.Name == name {
				cookie = updated
				break
			}
		}
		if cookie != nil {
			cookies = append(cookies, cookie)
		}
	}
	return cookies
}

func (j *accountCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		if !isAccountCookie(cookie.Name) {
			continue
		}
		j.updates.SetCookies(u, []*http.Cookie{cookie})
		// Retain each response cookie's scope for this request, even after deletion
		// or expiry, so a redirect cannot restore the configured token there.
		scope := *cookie
		scope.Value = ""
		scope.MaxAge = 0
		scope.Expires = time.Time{}
		j.scopes.SetCookies(u, []*http.Cookie{&scope})
	}
}

func IsContentRestricted(err error) bool {
	var failure *UpstreamError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "content_restricted", "content_private", "region_blocked":
		return true
	}
	return false
}
