// Package instagramapp implements session-authenticated Instagram Android metadata
// and immutable account routes for metadata and subsequent CDN transfers.
package instagramapp

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const credentialLimit = 4 << 20

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type accountConfig struct {
	Login         *passwordCredentials `json:"-"`
	Cookies       json.RawMessage      `json:"cookies,omitempty"`
	ID            string               `json:"id"`
	Authorization string               `json:"authorization"`
	SessionFile   string               `json:"session_file"`
	ProxyURL      *string              `json:"proxy_url"`
	Device        *DeviceIdentity      `json:"device,omitempty"`
}

func readPrivateJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot read Instagram account configuration")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, credentialLimit+1))
	if err != nil || len(b) > credentialLimit {
		return errors.New("cannot read Instagram account configuration")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("invalid Instagram account configuration JSON")
	}
	return nil
}

func loadAccounts(options Options) ([]accountConfig, error) {
	var accounts []accountConfig
	if options.AccountsFile != "" {
		var manifest struct {
			Accounts []accountConfig `json:"accounts"`
		}
		if err := readPrivateJSON(options.AccountsFile, &manifest); err != nil {
			return nil, err
		}
		for _, entry := range manifest.Accounts {
			if entry.SessionFile != "" {
				path := entry.SessionFile
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(options.AccountsFile), path)
				}
				var session accountConfig
				if err := readPrivateJSON(path, &session); err != nil {
					return nil, err
				}
				if entry.Authorization != "" {
					return nil, errors.New("account must use authorization or session_file, not both")
				}
				entry.Authorization = session.Authorization
				if entry.Device == nil {
					entry.Device = session.Device
				}
				if entry.ProxyURL == nil {
					entry.ProxyURL = session.ProxyURL
				}
			}
			accounts = append(accounts, entry)
		}
	}
	if options.AccountsDir != "" {
		entries, err := os.ReadDir(options.AccountsDir)
		if err != nil {
			return nil, errors.New("cannot read Instagram accounts directory")
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var account accountConfig
			if err := readPrivateJSON(filepath.Join(options.AccountsDir, entry.Name()), &account); err != nil {
				return nil, err
			}
			if account.SessionFile != "" {
				return nil, errors.New("account directory profiles require authorization")
			}
			if account.ID == "" {
				account.ID = strings.TrimSuffix(entry.Name(), ".json")
			}
			accounts = append(accounts, account)
		}
	}
	ids := map[string]bool{}
	tokens := map[string]bool{}
	for n := range accounts {
		a := &accounts[n]
		if err := a.Device.validate(); err != nil {
			return nil, err
		}
		if !accountIDPattern.MatchString(a.ID) || ids[a.ID] {
			return nil, errors.New("invalid or duplicate Instagram account ID")
		}
		ids[a.ID] = true
		a.Authorization = strings.TrimSpace(a.Authorization)
		if strings.HasPrefix(a.Authorization, "IGT:2:") {
			a.Authorization = "Bearer " + a.Authorization
		}
		if !strings.HasPrefix(a.Authorization, "Bearer IGT:2:") || len(a.Authorization) <= len("Bearer IGT:2:") {
			return nil, errors.New("invalid Instagram Android authorization")
		}
		token := strings.TrimPrefix(a.Authorization, "Bearer ")
		for _, r := range token {
			if r <= 32 || r >= 127 {
				return nil, errors.New("invalid Instagram Android authorization")
			}
		}
		if tokens[token] {
			return nil, errors.New("duplicate Instagram account authorization")
		}
		tokens[token] = true
	}
	return accounts, nil
}
