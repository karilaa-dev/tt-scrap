package instagramapp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

const appVersion = "452.1.0.53.64"
const appBuild = "1096920925"
const defaultDeviceProfile = "pixel-7-android-17"
const loginTLSProfile = "instagram-android-mns-452"

// Profile IDs are immutable: changing an existing definition would change the
// device of stored accounts. Add a new ID when adding an OS/hardware variant.
type DeviceProfile struct {
	ID             string `json:"id"`
	Model          string `json:"model"`
	Manufacturer   string `json:"manufacturer"`
	Brand          string `json:"brand"`
	Device         string `json:"device"`
	CPU            string `json:"cpu"`
	AndroidVersion int    `json:"android_version"`
	AndroidAPI     int    `json:"android_api"`
	DPI            int    `json:"dpi"`
	Resolution     string `json:"resolution"`
	TLSProfile     string `json:"tls_profile"`
}

// Hardware/display values also appear in the TikTok catalog. The Instagram
// profile describes a client; it does not claim hardware-backed attestation.
var deviceProfiles = []DeviceProfile{
	{defaultDeviceProfile, "Pixel 7", "Google", "google", "panther", "panther", 17, 37, 420, "1080x2400", loginTLSProfile},
	{"pixel-7-android-15", "Pixel 7", "Google", "google", "panther", "panther", 15, 35, 420, "1080x2400", loginTLSProfile},
	{"pixel-8-android-15", "Pixel 8", "Google", "google", "shiba", "shiba", 15, 35, 420, "1080x2400", loginTLSProfile},
	{"galaxy-s23-android-14", "SM-S911B", "samsung", "samsung", "dm1q", "qcom", 14, 34, 420, "1080x2340", loginTLSProfile},
	{"galaxy-s23-android-15", "SM-S911B", "samsung", "samsung", "dm1q", "qcom", 15, 35, 420, "1080x2340", loginTLSProfile},
}

func DeviceProfiles() []DeviceProfile { return append([]DeviceProfile(nil), deviceProfiles...) }
func findDeviceProfile(id string) (DeviceProfile, bool) {
	for _, p := range deviceProfiles {
		if p.ID == id {
			return p, true
		}
	}
	return DeviceProfile{}, false
}
func validDeviceSelection(id string) bool {
	if id == "" || id == "random" {
		return true
	}
	_, ok := findDeviceProfile(id)
	return ok
}
func (p DeviceProfile) userAgent() string {
	manufacturer := p.Manufacturer
	if p.Brand != p.Manufacturer {
		manufacturer += "/" + p.Brand
	}
	return fmt.Sprintf("Instagram %s Android (%d/%d; %ddpi; %s; %s; %s; %s; %s; en_US; %s)", appVersion, p.AndroidAPI, p.AndroidVersion, p.DPI, p.Resolution, manufacturer, p.Model, p.Device, p.CPU, appBuild)
}

// Persisted with the account, never exposed by the public account-list API.
// These locally generated IDs are independent of the research phone.
type DeviceIdentity struct {
	Version   int    `json:"version"`
	ProfileID string `json:"profile_id"`
	DeviceID  string `json:"device_id"`
	FamilyID  string `json:"family_device_id"`
	AndroidID string `json:"android_id"`
}

func newDeviceIdentity(selection string) (*DeviceIdentity, error) {
	if !validDeviceSelection(selection) {
		return nil, invalidDeviceProfile()
	}
	if selection == "" || selection == "random" {
		n, e := rand.Int(rand.Reader, big.NewInt(int64(len(deviceProfiles))))
		if e != nil {
			return nil, e
		}
		selection = deviceProfiles[n.Int64()].ID
	}
	seed := make([]byte, 8)
	if _, e := rand.Read(seed); e != nil {
		return nil, e
	}
	return &DeviceIdentity{Version: 1, ProfileID: selection, DeviceID: core.NewID(), FamilyID: core.NewID(), AndroidID: "android-" + hex.EncodeToString(seed)}, nil
}
func invalidDeviceProfile() error {
	return core.NewError("invalid_device_profile", "Choose an Instagram device_profile from /v1/instagram/device-profiles or use random", 422)
}

var deviceUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var deviceAndroidPattern = regexp.MustCompile(`^android-[0-9a-f]{16}$`)

func (d *DeviceIdentity) validate() error {
	if d == nil {
		return nil
	} // Older saved sessions retain their previous UA.
	_, known := findDeviceProfile(d.ProfileID)
	if d.Version != 1 || !known || !deviceUUIDPattern.MatchString(d.DeviceID) || !deviceUUIDPattern.MatchString(d.FamilyID) || d.DeviceID == d.FamilyID || !deviceAndroidPattern.MatchString(d.AndroidID) {
		return errors.New("invalid stored Instagram device identity")
	}
	return nil
}
func (d *DeviceIdentity) profileID() string {
	if d == nil {
		return defaultDeviceProfile
	}
	return d.ProfileID
}
func (d *DeviceIdentity) userAgent() string {
	p, _ := findDeviceProfile(d.profileID())
	return p.userAgent()
}
func (d *DeviceIdentity) headers(h http.Header) {
	h.Set("User-Agent", d.userAgent())
	if d != nil {
		h.Set("X-IG-Device-ID", d.DeviceID)
		h.Set("X-IG-Family-Device-ID", d.FamilyID)
		h.Set("X-IG-Android-ID", d.AndroidID)
	}
}
