package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/instagramapp"
)

type fakeInstagramAccounts struct {
	calls    int
	accounts []instagramapp.Credentials
}

func (f *fakeInstagramAccounts) Status() []instagramapp.AccountStatus {
	return []instagramapp.AccountStatus{{ID: "fixture", State: "active"}}
}
func (f *fakeInstagramAccounts) Import(_ context.Context, a []instagramapp.Credentials) ([]instagramapp.ImportResult, error) {
	f.calls++
	f.accounts = a
	return []instagramapp.ImportResult{{Row: 1, Status: "imported", AccountID: "fixture"}}, nil
}
func TestInstagramAccountImportRoutes(t *testing.T) {
	csv := "User ID / Email Address,Password,First Secret Question,Device Profile\n'alice,fixture-password,GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ,galaxy-s23-android-14\n"
	for _, format := range []string{"text/csv", "application/json", "multipart/form-data"} {
		t.Run(format, func(t *testing.T) {
			d, _, _, _ := fixture(t)
			manager := &fakeInstagramAccounts{}
			d.InstagramAccounts = manager
			handler := NewHandler(d)
			var body bytes.Buffer
			contentType := format
			switch format {
			case "text/csv":
				body.WriteString(csv)
			case "application/json":
				json.NewEncoder(&body).Encode(map[string]any{"accounts": []instagramapp.Credentials{{Username: "alice", Password: "fixture-password", DeviceProfile: "galaxy-s23-android-14"}}})
			case "multipart/form-data":
				mw := multipart.NewWriter(&body)
				file, _ := mw.CreateFormFile("file", "accounts.csv")
				file.Write([]byte(csv))
				mw.Close()
				contentType = mw.FormDataContentType()
			}
			data := append([]byte(nil), body.Bytes()...)
			for _, authenticated := range []bool{false, true} {
				req := httptest.NewRequest("POST", instagramImportPath, bytes.NewReader(data))
				req.Header.Set("Content-Type", contentType)
				if authenticated {
					req.Header.Set("Authorization", "Bearer "+d.Config.APIKey)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				expected := 401
				if authenticated {
					expected = 200
				}
				if response.Code != expected {
					t.Fatal(response.Code, response.Body.String())
				}
				if strings.Contains(response.Body.String(), "fixture-password") {
					t.Fatal("credential leaked")
				}
			}
			if manager.calls != 1 || manager.accounts[0].Username != "alice" || manager.accounts[0].DeviceProfile != "galaxy-s23-android-14" {
				t.Fatal("import authentication or mapping failed")
			}
			req := httptest.NewRequest("GET", instagramAccountsPath, nil)
			req.Header.Set("Authorization", "Bearer "+d.Config.APIKey)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"active_limit":10`) {
				t.Fatal(response.Body.String())
			}
		})
	}
}

func TestInstagramDeviceProfilesRequireAuthentication(t *testing.T) {
	d, _, _, _ := fixture(t)
	// Catalog is available before accounts have been loaded.
	d.InstagramAccounts = nil
	handler := NewHandler(d)
	for _, authenticated := range []bool{false, true} {
		req := httptest.NewRequest("GET", instagramDeviceProfilesPath, nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+d.Config.APIKey)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if !authenticated {
			if res.Code != 401 {
				t.Fatal("catalog missing authentication")
			}
			continue
		}
		var result struct {
			Profiles []instagramapp.DeviceProfile `json:"profiles"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &result) != nil || len(result.Profiles) != len(instagramapp.DeviceProfiles()) {
			t.Fatal("catalog response invalid")
		}
		if strings.Contains(res.Body.String(), "android_id") || strings.Contains(res.Body.String(), "family_device_id") {
			t.Fatal("account identity in catalog")
		}
	}
}

func TestInstagramCookieImportRoutes(t *testing.T) {
	cookies := `[{"domain":".instagram.com","path":"/","name":"sessionid","value":"123%3Afixture","secure":true},{"domain":".instagram.com","path":"/","name":"ds_user_id","value":"123","secure":true}]`
	for _, format := range []string{"text/plain", "text/plain-bom", "application/json", "json-envelope", "multipart/form-data", "multipart/form-data-bom"} {
		t.Run(format, func(t *testing.T) {
			d, _, _, _ := fixture(t)
			manager := &fakeInstagramAccounts{}
			d.InstagramAccounts = manager
			var body bytes.Buffer
			contentType := strings.TrimSuffix(format, "-bom")
			export := cookies
			if strings.HasSuffix(format, "-bom") {
				export = "\ufeff" + cookies
			}
			switch contentType {
			case "json-envelope":
				contentType = "application/json"
				body.WriteString(`{"accounts":[{"cookies":` + cookies + `}]}`)
			case "multipart/form-data":
				mw := multipart.NewWriter(&body)
				f, _ := mw.CreateFormFile("file", "accounts.txt")
				f.Write([]byte(export))
				mw.Close()
				contentType = mw.FormDataContentType()
			default:
				body.WriteString(export)
			}
			data := append([]byte(nil), body.Bytes()...)
			for _, authenticated := range []bool{false, true} {
				req := httptest.NewRequest("POST", instagramImportPath, bytes.NewReader(data))
				req.Header.Set("Content-Type", contentType)
				if authenticated {
					req.Header.Set("Authorization", "Bearer "+d.Config.APIKey)
				}
				res := httptest.NewRecorder()
				NewHandler(d).ServeHTTP(res, req)
				want := 401
				if authenticated {
					want = 200
				}
				if res.Code != want || strings.Contains(res.Body.String(), "123%3Afixture") {
					t.Fatal(res.Code)
				}
			}
			if manager.calls != 1 || len(manager.accounts) != 1 || len(manager.accounts[0].Cookies) == 0 || manager.accounts[0].Password != "" {
				t.Fatal("cookie upload not mapped")
			}
		})
	}
}
