package instagramapp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
)

// Explicit opt-in: never prints passwords, tokens, proxy URLs or raw responses.
func TestLivePasswordLogin(t *testing.T) {
	path := os.Getenv("TT_SCRAP_LIVE_LOGIN_CREDENTIALS")
	if path == "" {
		t.Skip("explicit credentials file required")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("credentials unreadable")
	}
	var c Credentials
	if json.Unmarshal(raw, &c) != nil {
		t.Fatal("invalid credentials JSON")
	}
	if c.Username == "" {
		c.Username = os.Getenv("TT_SCRAP_LIVE_LOGIN_USERNAME")
	}
	if c.Username == "" {
		t.Fatal("explicit login username required")
	}
	if profile := os.Getenv("TT_SCRAP_LIVE_LOGIN_DEVICE_PROFILE"); profile != "" {
		c.DeviceProfile = profile
	}
	var route struct {
		Proxy string `json:"proxy"`
	}
	raw, e = os.ReadFile(os.Getenv("TT_SCRAP_LIVE_LOGIN_ROUTE"))
	if e != nil || json.Unmarshal(raw, &route) != nil {
		t.Fatal("route unreadable")
	}
	transport, e := nettransport.New(route.Proxy, 4)
	if e != nil {
		t.Fatal(e)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result, e := login(ctx, client, "https://i.instagram.com", c)
	if e != nil {
		t.Fatal(e)
	}
	path = os.Getenv("TT_SCRAP_LIVE_LOGIN_OUTPUT")
	if path != "" {
		data, _ := json.Marshal(map[string]any{"authorization": result.Authorization, "username": result.Username, "user_id": result.UserID, "proxy_url": route.Proxy, "device": result.Device})
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("cannot create private session file")
		}
		defer file.Close()
		if _, err := file.Write(data); err != nil {
			t.Fatal("cannot save private session")
		}
	}
	t.Logf("password login and session verification succeeded with device profile %s", result.Device.profileID())
}
