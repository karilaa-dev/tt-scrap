package instagramapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ParseImportFile accepts CSV or browser exports: one JSON cookie array per account.
func ParseImportFile(reader io.Reader) ([]Credentials, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, credentialLimit+1))
	if err != nil || len(raw) > credentialLimit {
		return nil, invalidImport()
	}
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if !bytes.HasPrefix(raw, []byte("[")) {
		return ParseCSV(bytes.NewReader(raw))
	}
	return ParseCookieExport(bytes.NewReader(raw))
}

// ParseCookieExport also accepts a single pretty-printed array. Cookie objects
// stay intact for storage; browser-specific metadata is not an API credential.
func ParseCookieExport(reader io.Reader) ([]Credentials, error) {
	decoder := json.NewDecoder(reader)
	var accounts []Credentials
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil || len(accounts) >= MaxImportAccounts {
			return nil, invalidImport()
		}
		accounts = append(accounts, Credentials{Cookies: raw})
	}
	return validateCredentials(accounts)
}

// cookieAuthorization bridges a browser session to Android API authorization
// in RAM only. The original cookies, never this derived bearer, are persisted.
func cookieAuthorization(raw json.RawMessage) (string, string, error) {
	var cookies []struct {
		Name           string  `json:"name"`
		Value          string  `json:"value"`
		Domain         string  `json:"domain"`
		Path           string  `json:"path"`
		Secure         bool    `json:"secure"`
		HTTPOnly       bool    `json:"httpOnly"`
		HostOnly       bool    `json:"hostOnly"`
		Session        bool    `json:"session"`
		ExpirationDate float64 `json:"expirationDate"`
	}
	if json.Unmarshal(raw, &cookies) != nil || len(cookies) == 0 {
		return "", "", invalidImport()
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	for _, c := range cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain != "instagram.com" && domain != "www.instagram.com" && domain != "i.instagram.com" {
			return "", "", invalidImport()
		}
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if cookie.Valid() != nil || !strings.HasPrefix(c.Path, "/") {
			return "", "", invalidImport()
		}
		if c.HostOnly {
			cookie.Domain = ""
		}
		if !c.Session && c.ExpirationDate != 0 {
			cookie.Expires = time.Unix(int64(c.ExpirationDate), 0)
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: domain, Path: "/"}, []*http.Cookie{cookie})
	}
	endpoint, _ := url.Parse("https://i.instagram.com/api/v1/accounts/current_user/")
	values := map[string]string{}
	for _, c := range jar.Cookies(endpoint) {
		if c.Name == "sessionid" || c.Name == "ds_user_id" {
			if values[c.Name] != "" {
				return "", "", invalidImport()
			}
			values[c.Name] = c.Value
		}
	}
	id := values["ds_user_id"]
	session, err := url.PathUnescape(values["sessionid"])
	if err != nil || session == "" || id == "" || strings.Trim(id, "0123456789") != "" {
		return "", "", invalidImport()
	}
	encoded, _ := json.Marshal(map[string]string{"ds_user_id": id, "sessionid": session})
	return "Bearer IGT:2:" + base64.StdEncoding.EncodeToString(encoded), id, nil
}

func loginCookies(ctx context.Context, client *http.Client, origin string, c Credentials) (loginResult, error) {
	auth, id, err := cookieAuthorization(c.Cookies)
	if err != nil {
		return loginResult{}, err
	}
	device, err := newDeviceIdentity(c.DeviceProfile)
	if err != nil {
		return loginResult{}, err
	}
	result, err := verifyLogin(ctx, client, origin, auth, device)
	if err != nil {
		return loginResult{}, err
	}
	if result.UserID != id {
		return loginResult{}, loginFailure("session_check_failed")
	}
	result.Authorization = ""
	return result, nil
}
