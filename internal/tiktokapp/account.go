package tiktokapp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
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

func (a Account) cookies(u *url.URL) []*http.Cookie {
	if !a.Valid() || !IsAppHost(u) || !strings.HasPrefix(u.Path, "/aweme/") {
		return nil
	}
	return []*http.Cookie{{Name: "sid_tt", Value: a.SessionID}, {Name: "sessionid", Value: a.SessionID}, {Name: "sessionid_ss", Value: a.SessionID}}
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
