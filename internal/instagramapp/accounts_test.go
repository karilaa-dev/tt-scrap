package instagramapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountLoadingRelativeSessionAndDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"authorization":"IGT:2:first","proxy_url":"http://127.0.0.1:8080"}`), 0600)
	manifest := filepath.Join(dir, "accounts.json")
	os.WriteFile(manifest, []byte(`{"accounts":[{"id":"one","session_file":"session.json","proxy_url":""}]}`), 0600)
	profiles := filepath.Join(dir, "profiles")
	os.Mkdir(profiles, 0700)
	os.WriteFile(filepath.Join(profiles, "two.json"), []byte(`{"authorization":"Bearer IGT:2:second"}`), 0600)
	accounts, err := loadAccounts(Options{AccountsFile: manifest, AccountsDir: profiles})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != "one" || *accounts[0].ProxyURL != "" || accounts[1].ID != "two" || accounts[1].Authorization != "Bearer IGT:2:second" {
		t.Fatalf("bad accounts: count=%d", len(accounts))
	}
}

func TestAccountErrorsDoNotExposeSecrets(t *testing.T) {
	secret := "IGT:2:PRIVATE_MATERIAL"
	cases := []string{
		`{"accounts":[{"id":"a","authorization":"` + secret + `"`,
		`{"accounts":[{"id":"a","authorization":"` + secret + `"},{"id":"b","authorization":"` + secret + `"}]}`,
		`{"accounts":[{"id":"a","authorization":"` + secret + `"},{"id":"a","authorization":"IGT:2:other"}]}`,
		`{"accounts":[{"id":"a","authorization":"` + secret + `\nINJECT"}]}`,
		`{"accounts":[{"id":"a","authorization":"` + secret + `","proxy_url":"invalid://private:password@example.test"}]}`,
	}
	for n, data := range cases {
		path := filepath.Join(t.TempDir(), "accounts.json")
		os.WriteFile(path, []byte(data), 0600)
		p, err := Open(Options{AccountsFile: path})
		if p != nil {
			p.Close()
		}
		if err == nil {
			t.Fatalf("case %d accepted", n)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "password") {
			t.Fatal("credential exposed")
		}
	}
}

func TestInvalidAuthorizationAndIDs(t *testing.T) {
	for _, token := range []string{"", "Bearer IGT:2:", "Bearer browser-cookie", "IGT:2:has space", "IGT:2:ta\tb", "IGT:2:new\nline"} {
		data, _ := json.Marshal(map[string]any{"accounts": []map[string]string{{"id": "a", "authorization": token}}})
		path := filepath.Join(t.TempDir(), "accounts.json")
		os.WriteFile(path, data, 0600)
		p, err := Open(Options{AccountsFile: path})
		if p != nil {
			p.Close()
		}
		if err == nil {
			t.Fatalf("accepted invalid token format")
		}
	}
}
