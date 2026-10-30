package instagramapp

// CAA request sequence observed on the user's Pixel 7 (452.1.0.53.64),
// 2026-10-29. New installations register their own signing key; captured phone
// tokens, account access contexts and attestation proofs are never replayed.
import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"golang.org/x/net/publicsuffix"
)

const caaProcess = "com.bloks.www.bloks.caa.login.process_client_data_and_redirect"
const caaOAuth = "com.bloks.www.caa.login.oauth.token.fetch.async"
const caaPassword = "com.bloks.www.bloks.caa.login.async.send_login_request"
const usdidDocID = "124930351917786857261002920888"
const twoStepApp = "com.bloks.www.two_step_verification.entrypoint"

type loginSession struct {
	client                                 *http.Client
	origin, bootstrapOrigin                string
	guid, phone, device, waterfall, pigeon string
	mid, aac, nonce, usdidHeader           string
	keyID                                  byte
	passwordKey                            string
	identity                               *DeviceIdentity
}

func jsonText(value any) string { b, _ := json.Marshal(value); return string(b) }
func signedForm(value any) url.Values {
	return url.Values{"signed_body": {"SIGNATURE." + jsonText(value)}}
}
func newLoginSession(client *http.Client, origin, selection string) (*loginSession, error) {
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	isolated.Jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	identity, e := newDeviceIdentity(selection)
	if e != nil {
		return nil, e
	}
	s := &loginSession{client: &isolated, origin: origin, bootstrapOrigin: origin, guid: identity.DeviceID, phone: identity.FamilyID, device: identity.AndroidID, identity: identity, waterfall: core.NewID(), pigeon: "UFS-" + core.NewID() + "-1"}
	if origin == "https://i.instagram.com" {
		s.bootstrapOrigin = "https://b.i.instagram.com"
	}
	return s, nil
}
func (s *loginSession) headers() http.Header {
	h := http.Header{}
	for k, v := range map[string]string{"X-IG-App-ID": AppID, "X-IG-App-Locale": "en_US", "X-IG-Device-Locale": "en_US", "X-IG-Mapped-Locale": "en_US", "X-Bloks-Version-Id": bloksVersion, "X-IG-Capabilities": "3brTv10=", "X-IG-Connection-Type": "WIFI", "X-IG-Timezone-Offset": "0", "X-IG-WWW-Claim": "0", "IG-INTENDED-USER-ID": "0", "X-Bloks-Is-Layout-RTL": "false", "X-IG-Bandwidth-Speed-KBPS": "-1.000", "X-IG-Bandwidth-TotalBytes-B": "0", "X-IG-Bandwidth-TotalTime-MS": "0", "X-Pigeon-Session-Id": s.pigeon, "X-Pigeon-Rawclienttime": fmt.Sprintf("%.3f", float64(time.Now().UnixMilli())/1000), "Accept-Language": "en-US", "Priority": "u=3", "X-Tigon-Is-Retry": "False", "X-FB-Client-IP": "True", "X-FB-Server-Cluster": "True", "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8"} {
		h.Set(k, v)
	}
	if s.mid != "" {
		h.Set("X-Mid", s.mid)
	}
	s.identity.headers(h)
	if s.usdidHeader != "" {
		h.Set("X-Meta-Usdid", s.usdidHeader)
	}
	return h
}
func (s *loginSession) post(ctx context.Context, origin, path string, form url.Values, extra http.Header) (loginResponse, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", origin+path, strings.NewReader(form.Encode()))
	if e != nil {
		return loginResponse{}, loginFailure("invalid_login_request")
	}
	req.Header = s.headers()
	for k, v := range extra {
		req.Header[k] = v
	}
	res, e := s.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return loginResponse{}, ctx.Err()
		}
		return loginResponse{}, loginFailure("upstream_network_error")
	}
	defer res.Body.Close()
	// Throttling must stop the import batch even if the body is a challenge,
	// malformed, or cut off before it can be parsed.
	if res.StatusCode == http.StatusTooManyRequests {
		return loginResponse{}, loginFailure("upstream_rate_limited")
	}
	// An upstream outage is not a rejected credential. Recovery must retain
	// the account and cool down, including when the error body is not JSON.
	if res.StatusCode >= 500 && res.StatusCode < 600 {
		return loginResponse{}, loginFailure("upstream_server_error")
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, metadataLimit+1))
	if e != nil || len(raw) > metadataLimit {
		return loginResponse{}, loginFailure("invalid_login_response")
	}
	if mid := res.Header.Get("ig-set-x-mid"); mid != "" {
		s.mid = mid
	}
	result, e := parseLogin(res.StatusCode, res.Header, raw)
	if e != nil {
		return result, e
	}
	if e = loginCheck(result); e != nil {
		return result, e
	}
	return result, nil
}
func requireLoginOK(r loginResponse) error {
	if r.status != 200 || r.value["status"] == "fail" || r.value["errors"] != nil {
		// Bootstrap/protocol failures are not evidence of bad credentials.
		if failureMarker(r.value, "bad_password", "invalid_user", "invalid_credentials") {
			return loginFailure("login_rejected")
		}
		return loginFailure("invalid_login_response")
	}
	return nil
}
func (s *loginSession) bloks(ctx context.Context, origin, kind, action string, params any, attest bool) (loginResponse, error) {
	form := url.Values{"params": {jsonText(params)}, "bk_client_context": {jsonText(map[string]string{"bloks_version": bloksVersion, "styles_id": "instagram"})}, "bloks_versioning_id": {bloksVersion}}
	h := http.Header{}
	h.Set("X-FB-Friendly-Name", "IgApi: bloks/"+kind+"/"+action+"/")
	h.Set("X-IG-Bloks-Serialize-Payload", "true")
	if attest {
		h.Set("X-IG-Attest-Params", jsonText(map[string]any{"attestation": []any{map[string]any{"version": 2, "type": "keystore", "errors": []int{-1013}, "challenge_nonce": s.nonce, "signed_nonce": "", "key_hash": ""}}}))
	}
	return s.post(ctx, origin, "/api/v1/bloks/"+kind+"/"+action+"/", form, h)
}
func (s *loginSession) register(ctx context.Context) error {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	public, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return e
	}
	b64 := base64.RawURLEncoding.EncodeToString
	sign := func(value string) (string, error) {
		hash := sha256.Sum256([]byte(value))
		signature, e := ecdsa.SignASN1(rand.Reader, key, hash[:])
		return b64(signature), e
	}
	usdid := core.NewID()
	kid := make([]byte, 32)
	if _, e = rand.Read(kid); e != nil {
		return e
	}
	now := time.Now().Unix()
	payload := b64([]byte(jsonText(map[string]any{"sub": usdid, "iat": now, "aud": AppID, "exp": now + 3600, "pub": base64.StdEncoding.EncodeToString(public), "alg": "ES256"})))
	protected := b64([]byte(jsonText(map[string]any{"typ": "JWT", "alg": "ES256", "kid": b64(kid), "aid": AppID, "ver": "1"})))
	signature, e := sign(protected + "." + payload)
	if e != nil {
		return e
	}
	token := b64([]byte(jsonText(map[string]any{"payload": payload, "signatures": []any{map[string]string{"protected": protected, "signature": signature}}})))
	prefix := usdid + "." + strconv.FormatInt(now+3600, 10)
	signature, e = sign(prefix)
	if e != nil {
		return e
	}
	s.usdidHeader = prefix + "." + signature
	variables := map[string]any{"input": map[string]any{"usdid_token": map[string]string{"sensitive_string_value": token}, "fdid": map[string]string{"sensitive_string_value": s.phone}}}
	form := url.Values{"method": {"post"}, "pretty": {"false"}, "format": {"json"}, "server_timestamps": {"true"}, "locale": {"user"}, "fb_api_req_friendly_name": {"IGUSDIDRegistrationMutation"}, "enable_canonical_naming": {"true"}, "enable_canonical_variable_overrides": {"true"}, "enable_canonical_naming_ambiguous_type_prefixing": {"true"}, "variables": {jsonText(variables)}, "purpose": {"fetch"}, "client_doc_id": {usdidDocID}}
	h := http.Header{}
	h.Set("X-FB-Friendly-Name", "IGUSDIDRegistrationMutation")
	h.Set("X-Client-Doc-Id", usdidDocID)
	h.Set("X-Root-Field-Name", "usdid_registration")
	h.Set("X-Graphql-Client-Library", "pando")
	r, e := s.post(ctx, s.bootstrapOrigin, "/graphql_www", form, h)
	if e != nil {
		return e
	}
	if e = requireLoginOK(r); e != nil {
		return e
	}
	data, _ := r.value["data"].(map[string]any)
	for key, v := range data {
		m, _ := v.(map[string]any)
		if strings.Contains(key, "usdid_registration") && m["success"] == true {
			return nil
		}
	}
	return loginFailure("device_registration_failed")
}
func (s *loginSession) prepare(ctx context.Context, username string) error {
	r, e := s.post(ctx, s.origin, "/api/v1/launcher/mobileconfig/", signedForm(map[string]string{"device_id": s.guid, "unit_id": s.guid, "unit_type": "2", "api_version": "10", "fetch_type": "SYNC_FULL", "fetch_mode": "CONFIG_SYNC_ONLY", "bool_opt_policy": "0"}), nil)
	if e != nil {
		return e
	}
	if e = requireLoginOK(r); e != nil {
		return e
	}
	id, e := strconv.ParseUint(r.header.Get("ig-set-password-encryption-key-id"), 10, 8)
	if e != nil || s.mid == "" {
		return loginFailure("invalid_login_key")
	}
	s.keyID = byte(id)
	s.passwordKey = r.header.Get("ig-set-password-encryption-pub-key")
	if e = s.register(ctx); e != nil {
		return e
	}
	r, e = s.post(ctx, s.bootstrapOrigin, "/api/v1/attestation/create_android_keystore/", url.Values{"app_scoped_device_id": {s.guid}, "key_hash": {""}}, nil)
	if e != nil {
		return e
	}
	if e = requireLoginOK(r); e != nil {
		return e
	}
	s.nonce = stringField(r.value, "challenge_nonce")
	if s.nonce == "" {
		return loginFailure("missing_login_context")
	}
	params := map[string]any{"is_from_logged_out": true, "logged_out_user": "", "qpl_join_id": nil, "family_device_id": s.phone, "device_id": s.device, "offline_experiment_group": "caa_iteration_v3_perf_ig_4", "waterfall_id": s.waterfall, "logout_source": "", "show_internal_settings": false, "last_auto_login_time": 0, "disable_auto_login": true, "qe_device_id": s.guid, "use_auto_login_interstitial": true, "disable_recursive_auto_login_interstitial": true, "auto_login_interstitial_experiment_group_name": "", "is_from_logged_in_switcher": false, "switcher_logged_in_uid": "", "account_list": []any{}, "blocked_uid": []any{}, "INTERNAL_INFRA_THEME": "THREE_NEUTRAL_GRAY", "layered_homepage_experiment_group": "Deploy: Not in Experiment", "launched_url": "", "sim_phone_numbers": []any{}, "is_from_registration_reminder": false}
	r, e = s.bloks(ctx, s.bootstrapOrigin, "async_action", caaProcess, params, false)
	if e != nil {
		return e
	}
	if e = requireLoginOK(r); e != nil {
		return e
	}
	s.aac = extractAAC(r.value)
	if s.aac == "" {
		return loginFailure("missing_login_context")
	}
	server := s.serverParams()
	client := map[string]any{"username_input": username, "si_device_param_network_info": deviceNetworkInfo(), "aac": s.aac, "lois_settings": map[string]string{"lois_token": ""}, "cloud_trust_token": nil, "zero_balance_state": "", "network_bssid": nil}
	r, e = s.bloks(ctx, s.origin, "async_action", caaOAuth, map[string]any{"client_input_params": client, "server_params": server}, false)
	if e != nil {
		return e
	}
	return requireLoginOK(r)
}
func deviceNetworkInfo() map[string]any {
	return map[string]any{"active_subscriptions_info": nil, "default_subscription_info": nil, "is_airplane_mode": 0, "is_active_network_cellular": 0, "is_device_sms_capable": 1, "sim_count": 0, "is_wifi": 1}
}
func (s *loginSession) serverParams() map[string]any {
	return map[string]any{"is_from_logged_out": 1, "layered_homepage_experiment_group": "Deploy: Not in Experiment", "device_id": s.device, "login_surface": "login_home", "waterfall_id": s.waterfall, "INTERNAL__latency_qpl_instance_id": time.Now().UnixMilli(), "is_platform_login": 0, "login_entry_point": "logged_out", "INTERNAL__latency_qpl_marker_id": 36707139, "family_device_id": s.phone, "offline_experiment_group": "caa_iteration_v3_perf_ig_4", "access_flow_version": "pre_mt_behavior", "is_from_logged_in_switcher": 0, "qe_device_id": s.guid}
}
func (s *loginSession) password(ctx context.Context, c Credentials) (loginResponse, error) {
	encrypted, e := encryptPassword(c.Password, s.passwordKey, s.keyID, time.Now())
	if e != nil {
		return loginResponse{}, e
	}
	nonASCII := false
	for _, r := range c.Password {
		if r > 127 {
			nonASCII = true
		}
	}
	client := map[string]any{"blocked_uids": []any{}, "aac": s.aac, "sim_phones": []any{}, "aymh_accounts": []any{}, "network_bssid": nil, "secure_family_device_id": "", "has_granted_read_contacts_permissions": 0, "auth_secure_device_id": "", "has_whatsapp_installed": 0, "si_device_param_network_info": deviceNetworkInfo(), "password": encrypted, "sso_token_map_json_string": "", "block_store_machine_id": "", "ig_vetted_device_nonces": nil, "cloud_trust_token": nil, "event_flow": "login_manual", "password_contains_non_ascii": strconv.FormatBool(nonASCII), "client_known_key_hash": "", "sso_accounts_auth_data": []any{}, "encrypted_msisdn": "", "has_granted_read_phone_permissions": 0, "app_manager_id": "", "should_show_nested_nta_from_aymh": 0, "device_id": s.device, "zero_balance_state": "", "login_attempt_count": 1, "machine_id": s.mid, "flash_call_permission_status": map[string]string{"READ_PHONE_STATE": "DENIED", "READ_CALL_LOG": "DENIED", "ANSWER_PHONE_CALLS": "DENIED"}, "accounts_list": []any{}, "gms_incoming_call_retriever_eligibility": "eligible", "family_device_id": s.phone, "fb_ig_device_id": []any{}, "device_emails": []any{}, "try_num": 1, "lois_settings": map[string]string{"lois_token": ""}, "event_step": "home_page", "headers_infra_flow_id": "", "openid_tokens": map[string]any{}, "contact_point": c.Username}
	server := s.serverParams()
	inputID := strings.ReplaceAll(core.NewID(), "-", "")[:4] + "ig"
	for k, v := range map[string]any{"should_trigger_override_login_2fa_action": 0, "should_trigger_override_login_success_action": 0, "login_credential_type": "none", "server_login_source": "login", "two_step_login_type": "one_step_login", "login_source": "Login", "is_from_aymh": 0, "is_from_landing_page": 0, "left_nav_button_action": "NONE", "password_text_input_id": inputID + ":82", "is_from_empty_password": 0, "is_from_msplit_fallback": 0, "ar_event_source": "login_home_page", "username_text_input_id": inputID + ":81", "reg_flow_source": "login_home_native_integration_point", "is_caa_perf_enabled": 1, "credential_type": "password", "is_from_password_entry_page": 0, "caller": "gslr", "is_from_assistive_id": 0} {
		server[k] = v
	}
	return s.bloks(ctx, s.origin, "async_action", caaPassword, map[string]any{"client_input_params": client, "server_params": server}, true)
}
func login(ctx context.Context, client *http.Client, origin string, c Credentials) (loginResult, error) {
	var key []byte
	var e error
	if c.TOTPSecret != "" {
		key, e = totpKey(c.TOTPSecret)
		if e != nil {
			return loginResult{}, e
		}
		defer clear(key)
	}
	s, e := newLoginSession(client, origin, c.DeviceProfile)
	if e != nil {
		return loginResult{}, e
	}
	if c.device != nil {
		if e = c.device.validate(); e != nil {
			return loginResult{}, e
		}
		device := *c.device
		s.identity = &device
		s.guid, s.phone, s.device = device.DeviceID, device.FamilyID, device.AndroidID
	}
	if origin == "https://i.instagram.com" {
		base, ok := client.Transport.(*http.Transport)
		if !ok {
			return loginResult{}, loginFailure("invalid_login_transport")
		}
		transport := newLoginTransport(base)
		defer transport.CloseIdleConnections()
		s.client.Transport = transport
	}
	if e = s.prepare(ctx, c.Username); e != nil {
		return loginResult{}, e
	}
	r, e := s.password(ctx, c)
	if e != nil {
		return loginResult{}, e
	}
	if contextValue := extractTwoStepContext(r.value); contextValue != "" && (r.status == 200 || r.status == 400) {
		if len(key) == 0 {
			return loginResult{}, loginFailure("unsupported_two_factor")
		}
		r, e = s.twoStep(ctx, contextValue, key)
	} else if r.value["two_factor_required"] == true && (r.status == 200 || r.status == 400) {
		info, _ := r.value["two_factor_info"].(map[string]any)
		id := stringField(info, "two_factor_identifier")
		if len(key) == 0 || info["totp_two_factor_on"] != true || id == "" {
			return loginResult{}, loginFailure("unsupported_two_factor")
		}
		r, e = s.post(ctx, s.origin, "/api/v1/accounts/two_factor_login/", signedForm(map[string]string{"username": c.Username, "verification_method": "3", "two_factor_identifier": id, "device_id": s.device, "phone_id": s.phone, "guid": s.guid, "verification_code": totp(key, time.Now()), "trust_this_device": "0"}), nil)
	}
	if e != nil {
		return loginResult{}, e
	}
	if e = requireLoginOK(r); e != nil {
		return loginResult{}, e
	}
	auth := strings.TrimSpace(r.header.Get("ig-set-authorization"))
	if strings.HasPrefix(auth, "IGT:2:") {
		auth = "Bearer " + auth
	}
	if !validAuthorization(auth) {
		return loginResult{}, loginFailure("login_session_not_returned")
	}
	// Verify with the persisted device headers and ordinary transport, without
	// login cookies or temporary device proofs: this is the route used after restart.
	return verifyLogin(ctx, client, origin, auth, s.identity)
}
