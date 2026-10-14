package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type fakePreparer struct {
	calls  int
	status core.IdentityPreparation
	err    error
}

func (p *fakePreparer) StartPreparation() (core.IdentityPreparation, error) {
	p.calls++
	return p.status, p.err
}
func (p *fakePreparer) PreparationStatus() core.IdentityPreparation { return p.status }

func TestIdentityPreparationAPIAuthenticationProgressAndAvailability(t *testing.T) {
	dependencies, _, _, _ := fixture(t)
	identities := &fakePreparer{status: core.IdentityPreparation{State: "running", Proxies: 10, Target: 100, Ready: 24}}
	dependencies.Identities = identities
	handler := NewHandler(dependencies)
	for _, method := range []string{"GET", "POST"} {
		if r := request(handler, method, preparationPath, "", false); r.Code != 401 {
			t.Fatal("unprotected identity control", r.Code)
		}
	}
	if identities.calls != 0 {
		t.Fatal("unauthenticated registration")
	}
	for _, body := range []string{"", `{}`} {
		r := request(handler, "POST", preparationPath, body, true)
		if r.Code != 202 {
			t.Fatal(r.Code, r.Body.String())
		}
		var status core.IdentityPreparation
		if err := json.Unmarshal(r.Body.Bytes(), &status); err != nil || status.Target != 100 || status.Ready != 24 || status.State != "running" {
			t.Fatal(status, err)
		}
	}
	if r := request(handler, "GET", preparationPath, "", true); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if identities.calls != 2 {
		t.Fatal("GET started work")
	}
	if r := request(handler, "POST", preparationPath, `{"count":1000}`, true); r.Code != 422 {
		t.Fatal("accepted unbounded job parameters", r.Code)
	}
	if r := request(handler, "DELETE", preparationPath, "", true); r.Code != 405 || r.Header().Get("Allow") != "GET, POST" {
		t.Fatal(r.Code, r.Header())
	}
	identities.status.State = "completed"
	identities.status.Ready = 100
	if r := request(handler, "POST", preparationPath, "", true); r.Code != 200 {
		t.Fatal(r.Code)
	}
	identities.err = core.NewError("service_not_configured", "Pool ownership lost", 503)
	if r := request(handler, "POST", preparationPath, "", true); r.Code != 503 {
		t.Fatal(r.Code)
	}
	dependencies.Identities = nil
	handler = NewHandler(dependencies)
	for _, method := range []string{"GET", "POST"} {
		if r := request(handler, method, preparationPath, "", true); r.Code != 503 {
			t.Fatal("unavailable pool accepted identity control", r.Code)
		}
	}
}

func TestChunkedPreparationAcceptsEmptyBodyAndRetainsValidation(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		auth             bool
		want             int
	}{
		{"empty", preparationPath, "", true, 202},
		{"object", preparationPath, `{}`, true, 202},
		{"authentication", preparationPath, "", false, 401},
		{"null", preparationPath, `null`, true, 422},
		{"malformed", preparationPath, `{`, true, 422},
		{"extra", preparationPath, `{} {}`, true, 422},
		{"parameters", preparationPath, `{"count":1000}`, true, 422},
		{"oversized", preparationPath, strings.Repeat(" ", 1<<20) + `{}`, true, 422},
		{"required", "/v1/tiktok/extractions", "", true, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies, _, _, _ := fixture(t)
			dependencies.Identities = &fakePreparer{status: core.IdentityPreparation{State: "running"}}
			h := NewHandler(dependencies)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ContentLength != -1 {
					t.Errorf("fixture is not chunked: content length %d", r.ContentLength)
				}
				h.ServeHTTP(w, r)
			}))
			defer server.Close()
			r, err := http.NewRequest("POST", server.URL+test.path, io.NopCloser(strings.NewReader(test.body)))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/json")
			if test.auth {
				r.Header.Set("Authorization", "Bearer "+dependencies.Config.APIKey)
			}
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.want {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d want=%d: %s", response.StatusCode, test.want, body)
			}
		})
	}
}
