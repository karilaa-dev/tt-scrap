package instagramapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestFreshDeviceProfiles(t *testing.T) {
	seen := map[string]bool{}
	for _, profile := range DeviceProfiles() {
		if seen[profile.ID] {
			t.Fatal("duplicate profile")
		}
		seen[profile.ID] = true
		first, e := newDeviceIdentity(profile.ID)
		if e != nil {
			t.Fatal(e)
		}
		second, e := newDeviceIdentity(profile.ID)
		if e != nil {
			t.Fatal(e)
		}
		if first.validate() != nil || second.validate() != nil || first.DeviceID == second.DeviceID || first.FamilyID == second.FamilyID || first.AndroidID == second.AndroidID {
			t.Fatal("device identity reused")
		}
		raw, _ := json.Marshal(first)
		var restored DeviceIdentity
		if json.Unmarshal(raw, &restored) != nil || restored != *first || restored.userAgent() != first.userAgent() {
			t.Fatal("identity round trip changed profile")
		}
		if profile.TLSProfile != loginTLSProfile {
			t.Fatal("device uses unsupported app TLS profile")
		}
	}
	for _, selection := range []string{"", "random"} {
		d, e := newDeviceIdentity(selection)
		if e != nil || !seen[d.ProfileID] {
			t.Fatal("random profile outside catalog")
		}
	}
	var legacy *DeviceIdentity
	if legacy.userAgent() != UserAgent {
		t.Fatal("pre-profile user agent changed")
	}
	h := http.Header{}
	legacy.headers(h)
	if h.Get("X-IG-Device-ID") != "" {
		t.Fatal("invented an ID for a pre-profile session")
	}
	profiles := DeviceProfiles()
	profiles[0].Model = "changed"
	if DeviceProfiles()[0].Model == "changed" {
		t.Fatal("catalog exposed mutable state")
	}
}

func TestDeviceProfileImportValidation(t *testing.T) {
	csv := "User ID / Email Address,Password,First Secret Question,Device Profile\nalice,fixture,,galaxy-s23-android-14\n"
	accounts, e := ParseCSV(strings.NewReader(csv))
	if e != nil || accounts[0].DeviceProfile != "galaxy-s23-android-14" {
		t.Fatal("CSV profile mapping failed", e)
	}
	_, e = ParseCSV(strings.NewReader(strings.Replace(csv, "galaxy-s23-android-14", "unknown", 1)))
	assertCode(t, e, "invalid_device_profile")
	_, e = validateCredentials([]Credentials{{Username: "alice", Password: "fixture", DeviceProfile: "random"}, {Username: "bob", Password: "fixture", DeviceProfile: "unknown"}})
	assertCode(t, e, "invalid_device_profile")
	device, _ := newDeviceIdentity(defaultDeviceProfile)
	for _, mutate := range []func(*DeviceIdentity){func(d *DeviceIdentity) { d.Version = 2 }, func(d *DeviceIdentity) { d.ProfileID = "unknown" }, func(d *DeviceIdentity) { d.DeviceID = "invalid" }, func(d *DeviceIdentity) { d.FamilyID = d.DeviceID }, func(d *DeviceIdentity) { d.AndroidID = "android-untrusted\r\n" }} {
		bad := *device
		mutate(&bad)
		if bad.validate() == nil {
			t.Fatal("invalid saved identity accepted")
		}
	}
}

func TestDeviceMigrationPreservesExistingSessions(t *testing.T) {
	store, cfg := testStore(t)
	ctx := context.Background()
	if e := store.Seed(ctx, []accountConfig{{ID: "legacy-session", Authorization: "Bearer IGT:2:fixture"}}); e != nil {
		t.Fatal(e)
	}
	// Simulate the schema from before device persistence, in this test's unique schema.
	if e := store.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, "ALTER TABLE "+store.accounts+" DROP COLUMN device")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	store.Close()
	restored, e := OpenStore(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	rows, e := restored.Load(ctx)
	if e != nil || len(rows) != 1 || rows[0].Device != nil || rows[0].Authorization != "Bearer IGT:2:fixture" {
		t.Fatal("migration altered existing session", e)
	}
	if e = restored.withDB(ctx, func(ctx context.Context, db *pgx.Conn) error {
		_, e := db.Exec(ctx, "UPDATE "+restored.accounts+" SET device=$1", `{"version":1,"profile_id":"unknown"}`)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = restored.Load(ctx); e == nil {
		t.Fatal("corrupt saved device silently replaced")
	}
}
