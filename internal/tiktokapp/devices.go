package tiktokapp

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strconv"
)

//go:embed devices.json
var deviceCatalogJSON []byte

type DeviceRelease struct {
	Android    int    `json:"android"`
	Build      string `json:"build"`
	ReleasedAt string `json:"releasedAt"`
}
type CatalogDevice struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Model          string          `json:"model"`
	Brand          string          `json:"brand"`
	Manufacturer   string          `json:"manufacturer"`
	Resolution     string          `json:"resolution"`
	DPI            int             `json:"dpi"`
	AvailableSince string          `json:"availableSince"`
	Releases       []DeviceRelease `json:"releases"`
}

var deviceCatalog []CatalogDevice

func init() {
	if e := json.Unmarshal(deviceCatalogJSON, &deviceCatalog); e != nil {
		panic(e)
	}
	if len(deviceCatalog) == 0 {
		panic("empty device catalog")
	}
}
func Catalog() []CatalogDevice {
	out := make([]CatalogDevice, len(deviceCatalog))
	for i, d := range deviceCatalog {
		out[i] = d
		out[i].Releases = append([]DeviceRelease(nil), d.Releases...)
	}
	return out
}
func randomIndex(n int) (int, error) {
	v, e := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if e != nil {
		return 0, e
	}
	return int(v.Int64()), nil
}

var androidAPILevels = map[int]int{12: 31, 13: 33, 14: 34, 15: 35, 16: 36, 17: 37}

func PickProfile() (Profile, error) {
	index, e := randomIndex(len(deviceCatalog))
	if e != nil {
		return Profile{}, e
	}
	device := deviceCatalog[index]
	releaseIndex, e := randomIndex(len(device.Releases))
	if e != nil {
		return Profile{}, e
	}
	release := device.Releases[releaseIndex]
	return Profile{Model: device.Model, Brand: device.Brand, Manufacturer: device.Manufacturer, OSVersion: strconv.Itoa(release.Android), OSAPI: androidAPILevels[release.Android], Build: release.Build, Resolution: device.Resolution, DPI: device.DPI, Language: "en", Region: "US", Timezone: "Etc/UTC", TimezoneOffset: 0, Channel: "googleplay"}, nil
}

var openUDIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{13,20}$`)
var clientUDIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var resolutionPattern = regexp.MustCompile(`^[1-9][0-9]*\*[1-9][0-9]*$`)
var osVersionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)

func ValidateSession(s *Session) error {
	if s == nil || s.Version != 1 {
		return errors.New("unsupported TikTok session version")
	}
	i, p := s.Identity, s.Profile
	if !clientUDIDPattern.MatchString(i.ClientUDID) || !openUDIDPattern.MatchString(i.OpenUDID) || i.CreatedAt.IsZero() {
		return errors.New("invalid TikTok local identity")
	}
	if i.Kind != "local" && i.Kind != "registered" {
		return errors.New("invalid TikTok identity kind")
	}
	if i.Kind == "registered" && (!serverIDPattern.MatchString(i.DeviceID) || !serverIDPattern.MatchString(i.InstallID) || i.RegisteredAt == nil || i.RegisteredAt.IsZero()) {
		return errors.New("invalid TikTok device registration")
	}
	if p.Model == "" || p.Brand == "" || p.Manufacturer == "" || !osVersionPattern.MatchString(p.OSVersion) || p.OSAPI <= 0 || p.Build == "" || !resolutionPattern.MatchString(p.Resolution) || p.DPI <= 0 || len(p.Language) < 2 || len(p.Region) != 2 || p.Timezone == "" || p.Channel == "" {
		return errors.New("invalid TikTok device profile")
	}
	var jar savedJar
	if json.Unmarshal([]byte(s.Cookies), &jar) != nil {
		return errors.New("invalid TikTok cookies")
	}
	if sdk := s.SDK; sdk != nil {
		seed, e := base64.StdEncoding.DecodeString(sdk.Seed)
		if e != nil || len(seed) != 98 || sdk.Algorithm < 1 || sdk.Algorithm > 8 || sdk.FetchedAt.IsZero() {
			return errors.New("invalid TikTok SDK state")
		}
	}
	return nil
}
