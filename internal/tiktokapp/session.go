package tiktokapp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const SigningTTL = time.Hour
const APKVersion = "47.1.4"
const APKPackage = "com.zhiliaoapp.musically"
const APKAID = 1233
const APKVersionCode = 470104
const APKManifestVersionCode = 2024701040

type Profile struct {
	Model          string `json:"model"`
	Brand          string `json:"brand"`
	Manufacturer   string `json:"manufacturer"`
	OSVersion      string `json:"osVersion"`
	OSAPI          int    `json:"osApi"`
	Build          string `json:"build"`
	Resolution     string `json:"resolution"`
	DPI            int    `json:"dpi"`
	Language       string `json:"language"`
	Region         string `json:"region"`
	Timezone       string `json:"timezone"`
	TimezoneOffset int    `json:"timezoneOffset"`
	Channel        string `json:"channel"`
}
type Identity struct {
	Kind         string     `json:"kind"`
	ClientUDID   string     `json:"clientudid"`
	OpenUDID     string     `json:"openudid"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeviceID     string     `json:"deviceId,omitempty"`
	InstallID    string     `json:"installId,omitempty"`
	DeviceToken  string     `json:"deviceToken"`
	RegisteredAt *time.Time `json:"registeredAt,omitempty"`
}
type SDKState struct {
	Seed         string    `json:"seed"`
	Algorithm    int       `json:"algorithm"`
	FetchedAt    time.Time `json:"fetchedAt"`
	RequestCount uint64    `json:"requestCount"`
}
type Session struct {
	Version      int           `json:"version"`
	Identity     Identity      `json:"identity"`
	Profile      Profile       `json:"profile"`
	Cookies      string        `json:"cookies"`
	RoutingRules []RoutingRule `json:"routingRules"`
	StoreRegion  string        `json:"storeRegion"`
	SDK          *SDKState     `json:"sdk"`
}

func newUUID() string {
	b := randomBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func NewSession(profile *Profile) (*Session, error) {
	var selected Profile
	var err error
	if profile == nil {
		selected, err = PickProfile()
		if err != nil {
			return nil, err
		}
	} else {
		selected = *profile
	}
	if selected.Model == "" || selected.OSVersion == "" || selected.OSAPI <= 0 || selected.Build == "" {
		return nil, errors.New("invalid device profile")
	}
	b := make([]byte, 10)
	if _, err = rand.Read(b); err != nil {
		return nil, err
	}
	return &Session{Version: 1, Identity: Identity{Kind: "local", ClientUDID: newUUID(), OpenUDID: hex.EncodeToString(b), CreatedAt: time.Now().UTC()}, Profile: selected, Cookies: `{"version":"tough-cookie@6.0.2","storeType":"MemoryCookieStore","rejectPublicSuffixes":true,"enableLooseMode":false,"allowSpecialUseDomain":true,"prefixSecurity":"silent","cookies":[]}`, RoutingRules: []RoutingRule{}}, nil
}
func UserAgent(p Profile) string {
	return fmt.Sprintf("%s/%d (Linux; U; Android %s; %s_%s; %s; Build/%s)", APKPackage, APKManifestVersionCode, p.OSVersion, p.Language, p.Region, p.Model, p.Build)
}
