package instagramapp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTOTPAndCSV(t *testing.T) {
	key, e := totpKey("GEZDGNBV GY3TQOJQ GEZDGNBV GY3TQOJQ")
	if e != nil {
		t.Fatal(e)
	}
	if got := totp(key, time.Unix(59, 0)); got != "287082" {
		t.Fatal(got)
	}
	input := "\ufeffUser ID / Email Address,Password,First Secret Question,First Secret Answer\r\nalice,\"p,ass\",GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ,ignored\r\n"
	accounts, e := ParseCSV(strings.NewReader(input))
	if e != nil || len(accounts) != 1 || accounts[0].Password != "p,ass" || accounts[0].TOTPSecret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatal("CSV mapping failed", e)
	}
	for _, bad := range []string{"bad-header\nvalue", input + "ALICE,pw,GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ,x\n", strings.Replace(input, "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "not a secret!", 1)} {
		if _, e := ParseCSV(strings.NewReader(bad)); e == nil {
			t.Fatal("accepted malformed CSV")
		}
	}
}

func TestSpreadsheetUsernamePrefixPreservesCredentials(t *testing.T) {
	input := "User ID / Email Address,Password,First Secret Question\r\n'Account.Name,\" 'p,ass' \",GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\r\n"
	accounts, err := ParseCSV(strings.NewReader(input))
	if err != nil || len(accounts) != 1 {
		t.Fatal("CSV parsing failed", err)
	}
	if accounts[0].Username != "Account.Name" || accounts[0].Password != " 'p,ass' " || accounts[0].TOTPSecret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatal("spreadsheet normalization changed credential data")
	}
	jsonAccounts, err := validateCredentials([]Credentials{{Username: "  'Account.Name  ", Password: " 'p,ass' "}})
	if err != nil || jsonAccounts[0].Username != "Account.Name" || jsonAccounts[0].Password != " 'p,ass' " {
		t.Fatal("JSON credentials normalized inconsistently")
	}
	_, err = validateCredentials([]Credentials{{Username: "'Alice", Password: "first"}, {Username: "alice", Password: "second"}})
	if err == nil {
		t.Fatal("duplicate username accepted after normalization")
	}
	_, err = validateCredentials([]Credentials{{Username: "'", Password: "password"}})
	if err == nil {
		t.Fatal("empty normalized username accepted")
	}
}

func TestTOTPRFC6238SHA1Vectors(t *testing.T) {
	// RFC 6238 Appendix B uses eight digits. The same SHA-1 dynamic truncation
	// reduced modulo 10^6 gives the six-digit codes required by Instagram.
	key := []byte("12345678901234567890")
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		if got := totp(key, time.Unix(tc.seconds, 0)); got != tc.want {
			t.Errorf("TOTP at %d: got %s, want %s", tc.seconds, got, tc.want)
		}
	}
}
func TestEncryptedLoginTOTPAndSessionCheck(t *testing.T) {
	for _, method := range []string{"classic", "bloks"} {
		t.Run(method, func(t *testing.T) { testEncryptedLogin(t, method) })
	}
}
func testEncryptedLogin(t *testing.T, method string) {
	private, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&private.PublicKey)
	encoded := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))
	steps := 0
	savedDevice, e := newDeviceIdentity("galaxy-s23-android-14")
	if e != nil {
		t.Fatal(e)
	}
	var device, phone string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		profile, _ := findDeviceProfile("galaxy-s23-android-14")
		if r.Header.Get("User-Agent") != profile.userAgent() {
			t.Error("selected device profile not used")
		}
		if r.Header.Get("X-IG-Device-ID") != savedDevice.DeviceID {
			t.Error("re-login changed saved device identity")
		}
		if steps == 1 {
			device = r.Header.Get("X-IG-Android-ID")
			phone = r.Header.Get("X-IG-Family-Device-ID")
		} else if device != r.Header.Get("X-IG-Android-ID") || phone != r.Header.Get("X-IG-Family-Device-ID") {
			t.Error("changed device")
		}
		switch r.URL.Path {
		case "/api/v1/launcher/mobileconfig/":
			w.Header().Set("ig-set-password-encryption-key-id", "23")
			w.Header().Set("ig-set-password-encryption-pub-key", encoded)
			w.Header().Set("ig-set-x-mid", "machine")
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/graphql_www":
			if r.Header.Get("X-Meta-Usdid") == "" {
				t.Error("missing device proof")
			}
			fmt.Fprint(w, `{"data":{"usdid_registration":{"success":true}}}`)
		case "/api/v1/attestation/create_android_keystore/":
			fmt.Fprint(w, `{"status":"ok","challenge_nonce":"fixture-nonce"}`)
		case "/api/v1/bloks/async_action/com.bloks.www.bloks.caa.login.process_client_data_and_redirect/":
			fmt.Fprint(w, `{"layout":{"bloks_payload":{"data":[{"data":{"key":"CAA_ACCOUNT_ACCESS_CONTEXT:aac","initial":"fixture-aac"}}]}}}`)
		case "/api/v1/bloks/async_action/com.bloks.www.caa.login.oauth.token.fetch.async/":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/api/v1/bloks/async_action/com.bloks.www.bloks.caa.login.async.send_login_request/":
			r.ParseForm()
			var params struct {
				Client map[string]any `json:"client_input_params"`
			}
			json.Unmarshal([]byte(r.Form.Get("params")), &params)
			if params.Client["aac"] != "fixture-aac" || r.Header.Get("X-IG-Attest-Params") == "" {
				t.Error("password sent without preflight context")
			}
			password, _ := params.Client["password"].(string)
			pieces := strings.Split(password, ":")
			if len(pieces) != 4 {
				t.Error("password format")
				w.WriteHeader(500)
				return
			}
			blob, e := base64.StdEncoding.DecodeString(pieces[3])
			if e != nil || len(blob) < 16 {
				t.Error("envelope")
				return
			}
			keyLen := int(binary.LittleEndian.Uint16(blob[14:16]))
			//lint:ignore SA1019 This local fixture verifies Instagram's required PKCS#1 v1.5 envelope.
			key, e := rsa.DecryptPKCS1v15(rand.Reader, private, blob[16:16+keyLen])
			if e != nil {
				t.Error(e)
				return
			}
			block, _ := aes.NewCipher(key)
			gcm, _ := cipher.NewGCM(block)
			offset := 16 + keyLen
			encrypted := append(append([]byte(nil), blob[offset+16:]...), blob[offset:offset+16]...)
			cleartext, e := gcm.Open(nil, blob[2:14], encrypted, []byte(pieces[2]))
			if e != nil || string(cleartext) != "fixture-password" || blob[1] != 23 {
				t.Error("password encryption incompatible", e)
			}
			if method == "classic" {
				fmt.Fprint(w, `{"payload":{"login_response":"{\"status\":\"fail\",\"two_factor_required\":true,\"two_factor_info\":{\"totp_two_factor_on\":true,\"two_factor_identifier\":\"fixture-id\"}}"}}`)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"layout": map[string]any{"bloks_payload": map[string]string{"action": `(action "` + twoStepApp + `" (f4i (dkc "server_params") (dkc (f4i (dkc "two_step_verification_context") (dkc "fixture-context")))))`}}})
			}
		case "/api/v1/bloks/apps/com.bloks.www.two_step_verification.entrypoint/", "/api/v1/bloks/apps/com.bloks.www.two_step_verification.method_picker/", "/api/v1/bloks/async_action/com.bloks.www.two_step_verification.method_picker.navigation.async/", "/api/v1/bloks/async_action/com.bloks.www.two_step_verification.verify_code.async/":
			r.ParseForm()
			var params struct {
				Client map[string]any `json:"client_input_params"`
				Server map[string]any `json:"server_params"`
			}
			json.Unmarshal([]byte(r.Form.Get("params")), &params)
			if params.Server["two_step_verification_context"] != "fixture-context" {
				t.Error("lost Bloks 2FA context")
			}
			if strings.Contains(r.URL.Path, "navigation.async") && params.Client["selected_method"] != "totp" {
				t.Error("did not select authenticator")
			}
			if strings.Contains(r.URL.Path, "verify_code.async") {
				key, _ := totpKey("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
				if params.Client["code"] != totp(key, time.Now()) {
					t.Error("incorrect authenticator code")
				}
				fmt.Fprint(w, `{"payload":{"login_response":"{\"status\":\"ok\"}","headers":{"ig-set-authorization":"Bearer IGT:2:fixture"}}}`)
			} else {
				fmt.Fprint(w, `{"status":"ok"}`)
			}
		case "/api/v1/accounts/two_factor_login/":
			r.ParseForm()
			var values map[string]string
			json.Unmarshal([]byte(strings.TrimPrefix(r.Form.Get("signed_body"), "SIGNATURE.")), &values)
			key, _ := totpKey("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
			if values["phone_id"] != phone || values["device_id"] != device || values["two_factor_identifier"] != "fixture-id" || values["verification_code"] != totp(key, time.Now()) {
				t.Error("2FA payload mismatch")
			}
			payload := map[string]any{"login_response": `{"status":"ok","logged_in_user":{"username":"alice","pk":"123"}}`, "headers": map[string]string{"ig-set-authorization": "Bearer IGT:2:fixture"}}
			encoded, _ := json.Marshal(payload)
			literal, _ := json.Marshal(string(encoded))
			json.NewEncoder(w).Encode(map[string]any{"layout": map[string]any{"bloks_payload": map[string]any{"action": "(action " + string(literal) + ")"}}})
		case "/api/v1/accounts/current_user/":
			if r.Header.Get("Authorization") != "Bearer IGT:2:fixture" {
				t.Error("session check missing")
			}
			fmt.Fprint(w, `{"status":"ok","user":{"username":"alice","pk":123}}`)
		default:
			t.Error("unexpected path")
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	result, e := login(context.Background(), server.Client(), server.URL, Credentials{Username: "alice", Password: "fixture-password", TOTPSecret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", DeviceProfile: "galaxy-s23-android-14", device: savedDevice})
	wantSteps := 8
	if method == "bloks" {
		wantSteps = 11
	}
	if e != nil || steps != wantSteps || result.UserID != "123" {
		t.Fatal("login flow failed", e, steps)
	}
	if result.Device == nil || result.Device.ProfileID != "galaxy-s23-android-14" || result.Device.AndroidID != device || result.Device.FamilyID != phone {
		t.Fatal("login identity was not retained")
	}
}
func TestLoginStopsAtUpstreamFailure(t *testing.T) {
	for _, test := range []struct {
		name, body, code string
		status           int
		truncated        bool
	}{
		{"rate_limited_challenge", `{"status":"fail","challenge_required":true}`, "upstream_rate_limited", 429, false},
		{"rate_limited_nested_error", `{"payload":"invalid JSON"}`, "upstream_rate_limited", 429, false},
		{"rate_limited_truncated", "", "upstream_rate_limited", 429, true},
		{"forbidden_challenge", `{"challenge_required":true}`, "challenge_required", 403, false},
		{"bad_request_challenge", `{"challenge_required":true}`, "challenge_required", 400, false},
		{"server_error", `{"status":"fail"}`, "upstream_server_error", 500, false},
		{"service_unavailable", `{"status":"fail"}`, "upstream_server_error", 503, false},
		{"gateway_error_html", `<html>Bad Gateway</html>`, "upstream_server_error", 502, false},
		{"server_error_truncated", "", "upstream_server_error", 503, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if test.truncated {
					w.Header().Set("Content-Length", "1024")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			_, e := login(context.Background(), server.Client(), server.URL, Credentials{Username: "alice", Password: "password"})
			if e == nil || calls != 1 {
				t.Fatal("login retried rejected bootstrap")
			}
			assertCode(t, e, test.code)
		})
	}
}
