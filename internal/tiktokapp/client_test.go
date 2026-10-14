package tiktokapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func testResponse(status int, body []byte, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body))}
}
func seedResponse(t *testing.T) []byte {
	t.Helper()
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 98))
	plain := encodeProto(ps(1, seed), pb(2, encodeProto(pb(1, []byte{6, 10, 16}))))
	encrypted, e := encryptSDKPayload(plain, []byte{1, 2, 3, 4})
	if e != nil {
		t.Fatal(e)
	}
	return encodeProto(pb(6, encrypted))
}
func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, e := NewSession(nil)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func registeredTestSession(t *testing.T) *Session {
	s := newTestSession(t)
	now := time.Now().UTC()
	s.Identity.Kind = "registered"
	s.Identity.DeviceID = "7500000000000000001"
	s.Identity.InstallID = "7500000000000000002"
	s.Identity.RegisteredAt = &now
	s.SDK = &SDKState{Seed: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 98)), Algorithm: 3, FetchedAt: now}
	return s
}

func TestNativeLifecycleRegistrationSeedDetailAndSessionReload(t *testing.T) {
	s := newTestSession(t)
	seed := seedResponse(t)
	registers, seeds, details := 0, 0, 0
	ctx := context.Background()
	client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/service/2/device_register/":
			registers++
			if r.Header.Get("X-Argus") != "" {
				t.Error("registration unexpectedly signed")
			}
			raw, _ := io.ReadAll(r.Body)
			if !bytes.Equal(raw[:6], mustHex("746305100000")) {
				t.Error("registration not encrypted")
			}
			return testResponse(200, []byte(`{"device_id_str":"7500000000000000001","install_id_str":"7500000000000000002","device_token":"token"}`), http.Header{"Set-Cookie": {"odin_tt=fixture; Domain=.tiktokv.com; Path=/; Secure; HttpOnly"}, "X-Tt-Store-Region": {"us"}}), nil
		case "/ms/get_seed":
			seeds++
			if r.URL.Query().Get("did") != "7500000000000000001" || r.URL.Query().Get("iid") != "7500000000000000002" {
				t.Error("seed request lost 64-bit IDs")
			}
			if cookie, e := r.Cookie("odin_tt"); e != nil || cookie.Value != "fixture" {
				t.Error("registration cookie was not forwarded")
			}
			raw, _ := io.ReadAll(r.Body)
			fields, e := decodeProto(raw)
			if e != nil {
				t.Fatal(e)
			}
			encrypted, e := protoBytes(fields, 4)
			if e != nil {
				t.Fatal(e)
			}
			plain, e := decryptSDKPayload(encrypted)
			if e != nil {
				t.Fatal(e)
			}
			params, e := decodeProto(plain)
			if e != nil {
				t.Fatal(e)
			}
			device, e := protoBytes(params, 2)
			if e != nil || string(device) != "7500000000000000001" {
				t.Fatal("seed encrypted ID differs")
			}
			return testResponse(200, seed, nil), nil
		case "/aweme/v1/aweme/detail/":
			details++
			for _, key := range []string{"X-Argus", "X-Ladon", "X-Khronos", "X-SS-REQ-TICKET"} {
				if r.Header.Get(key) == "" {
					t.Errorf("missing signature header%s", key)
				}
			}
			if r.Header.Get("X-Tt-Store-Region") != "us" {
				t.Error("registration region missing")
			}
			if r.URL.Query().Get("device_id") != "7500000000000000001" {
				t.Error("detail changed identity")
			}
			return testResponse(200, []byte(`{"status_code":0,"aweme_detail":{"aweme_id":"7695962883523464478","create_time":1800000000,"large_id":7695962883523464478}}`), nil), nil
		default:
			t.Fatalf("unexpected request%s", r.URL.Path)
			return nil, nil
		}
	}))
	result, e := client.Detail(ctx, "7695962883523464478")
	if e != nil {
		t.Fatal(e)
	}
	item, e := DetailItem(result, "7695962883523464478")
	if e != nil || String(item["large_id"]) != "7695962883523464478" {
		t.Fatal("JSON ID precision lost")
	}
	if registers != 1 || seeds != 1 || details != 1 || s.SDK.RequestCount != 1 {
		t.Fatalf("wrong lifecycle counts:%d %d %d", registers, seeds, details)
	}
	if !strings.Contains(s.Cookies, "odin_tt") || s.Identity.Kind != "registered" || s.SDK.Algorithm != 3 {
		t.Fatal("session mutations were not retained")
	}
	if e = ValidateSession(s); e != nil {
		t.Fatal(e)
	}
	encoded, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	var restored Session
	if e = json.Unmarshal(encoded, &restored); e != nil {
		t.Fatal(e)
	}
	second := NewClient(&restored, client.transport)
	if _, e = second.Detail(ctx, "7695962883523464478"); e != nil {
		t.Fatal(e)
	}
	if registers != 1 || seeds != 1 || details != 2 {
		t.Fatal("warm session fetched registration/seed")
	}
	restored.SDK.FetchedAt = time.Now().Add(-SigningTTL - time.Second)
	if _, e = second.Detail(ctx, "7695962883523464478"); e != nil {
		t.Fatal(e)
	}
	if registers != 1 || seeds != 2 || details != 3 || restored.SDK.RequestCount != 1 {
		t.Fatal("lazy seed refresh recreated device or failed to reset count")
	}
	restored.SDK.FetchedAt = time.Now().Add(time.Hour)
	if e = ValidateSession(&restored); e != nil {
		t.Fatal("clock skew should refresh the seed on use, not invalidate the saved identity")
	}
	if _, e = second.Detail(ctx, "7695962883523464478"); e != nil {
		t.Fatal(e)
	}
	if registers != 1 || seeds != 3 || details != 4 {
		t.Fatal("future seed timestamp was not refreshed on use")
	}
}
func TestConcurrentBootstrapHasOneRegistrationAndSeed(t *testing.T) {
	s := newTestSession(t)
	seed := seedResponse(t)
	var register, seeds atomic.Int32
	client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/service/2/device_register/":
			register.Add(1)
			return testResponse(200, []byte(`{"device_id_str":"7500000000000000001","install_id_str":"7500000000000000002"}`), nil), nil
		case "/ms/get_seed":
			seeds.Add(1)
			return testResponse(200, seed, nil), nil
		}
		return nil, errors.New("unexpected request")
	}))
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if e := client.Bootstrap(context.Background()); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if register.Load() != 1 || seeds.Load() != 1 {
		t.Fatalf("duplicate bootstrap %d %d", register.Load(), seeds.Load())
	}
}
func TestFailureClassificationRequiresExplicitRecoveryEvidence(t *testing.T) {
	cases := []struct {
		status, app   int
		message, code string
		recovery      Recovery
	}{{403, 0, "access denied", "upstream_blocked", RecoveryNone}, {401, 0, "challenge required", "upstream_blocked", RecoveryNone}, {200, 1, "device_id is invalid", "identity_rejected", RecoveryRecreate}, {200, 1, "expired signature", "signature_rejected", RecoveryRefresh}, {200, 1, "seed rejected", "signature_rejected", RecoveryRefresh}, {429, 0, "invalid signature", "upstream_rate_limited", RecoveryNone}, {503, 0, "invalid device", "upstream_unavailable", RecoveryNone}, {200, 10216, "", "content_private", RecoveryNone}, {200, 10222, "", "content_private", RecoveryNone}, {200, 10204, "", "region_blocked", RecoveryNone}, {404, 0, "", "content_deleted", RecoveryNone}}
	for _, v := range cases {
		err := ClassifyFailure(v.status, v.app, v.message, false, 3*time.Second)
		if err.Code != v.code || err.Recovery != v.recovery || err.RetryAfter != 3*time.Second {
			t.Fatalf("classification%+v: %+v", v, err)
		}
	}
}
func TestFeedClassifiesFailuresAndKeepsIdentity(t *testing.T) {
	s := registeredTestSession(t)
	original := s.Identity.DeviceID
	client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(200, []byte(`{"status_code":1,"status_msg":"invalid seed"}`), nil), nil
	}))
	_, e := client.Feed(context.Background())
	var failure *UpstreamError
	if !errors.As(e, &failure) || failure.Recovery != RecoveryRefresh {
		t.Fatalf("feed recovery:%v", e)
	}
	if s.Identity.DeviceID != original {
		t.Fatal("protocol layer recreated identity")
	}
}
func TestMediaRedirectStripsSecretsAndPreservesScopedCookies(t *testing.T) {
	s := registeredTestSession(t)
	s.StoreRegion = "us"
	var seen int
	client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
		seen++
		switch seen {
		case 1:
			if r.Header.Get("X-Argus") == "" || r.URL.Query().Get("device_id") == "" {
				t.Fatal("play request lacks common parameters or signing")
			}
			return testResponse(302, nil, http.Header{"Location": {"https://video.example.org/file.mp4"}, "Set-Cookie": {"odin_tt=only-app; Domain=.tiktokv.com; Path=/; Secure"}}), nil
		case 2:
			for _, key := range []string{"X-Argus", "X-Ladon", "X-Tt-Store-Region", "Cookie", "Authorization"} {
				if r.Header.Get(key) != "" {
					t.Errorf("leaked%s on CDN redirect", key)
				}
			}
			if r.Header.Get("Range") != "bytes=0-31" {
				t.Error("range header lost")
			}
			return testResponse(206, []byte("mp4"), nil), nil
		}
		return nil, errors.New("extra request")
	}))
	response, e := client.MediaRequest(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/?video_id=fixture", http.Header{"Authorization": {"secret"}, "Cookie": {"external=secret"}, "Range": {"bytes=0-31"}})
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if seen != 2 {
		t.Fatal("media redirect incomplete")
	}
}
func TestCookiesRejectUnrelatedDomainsAndPublicSuffixes(t *testing.T) {
	jar := newPersistentJar(`{"cookies":[]}`)
	u, _ := url.Parse("https://api19-va.tiktokv.com/a/b")
	jar.accept(u, []*http.Cookie{{Name: "good", Value: "one", Domain: ".tiktokv.com", Path: "/", Secure: true}, {Name: "bad", Value: "two", Domain: ".com", Path: "/"}, {Name: "other", Value: "three", Domain: "example.org", Path: "/"}})
	serialized := jar.serialize()
	if strings.Contains(serialized, "\"bad\"") || strings.Contains(serialized, "other") {
		t.Fatal("serialized rejected cookie")
	}
	restored := newPersistentJar(serialized)
	if cookies := restored.jar.Cookies(u); len(cookies) != 1 || cookies[0].Name != "good" {
		t.Fatalf("cookie roundtrip%v", cookies)
	}
	jar.accept(u, []*http.Cookie{{Name: "good", Domain: ".tiktokv.com", Path: "/", MaxAge: -1}})
	if strings.Contains(jar.serialize(), "\"good\"") {
		t.Fatal("deleted cookie retained")
	}
}
func TestShareResolutionChecksRedirectDestination(t *testing.T) {
	s := newTestSession(t)
	client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(302, nil, http.Header{"Location": {"https://example.org/video/7695962883523464478"}}), nil
	}))
	if _, e := client.ResolveVideo(context.Background(), "https://vm.tiktok.com/share"); e == nil {
		t.Fatal("accepted external share redirect")
	}
	for _, input := range []string{"7695962883523464478", "https://www.tiktok.com/@test/video/7695962883523464478", "https://www.tiktok.com/@test/photo/7695962883523464478"} {
		id, e := ParseVideoID(input)
		if e != nil || id != "7695962883523464478" {
			t.Fatal(input, e)
		}
	}
}
func TestJSONResponseRejectsTruncationAndBlocks(t *testing.T) {
	for _, body := range []string{"", `{"status_code":0`, `{"status_code":0} {"other":1}`, "[]"} {
		if _, e := jsonResponse(testResponse(200, []byte(body), nil)); e == nil {
			t.Fatalf("accepted invalid body%q", body)
		}
	}
	_, e := jsonResponse(testResponse(200, []byte(`{"status_code":0}`), http.Header{"Tt_block": {"1"}}))
	var upstream *UpstreamError
	if !errors.As(e, &upstream) || upstream.Code != "upstream_blocked" {
		t.Fatalf("ignored block:%v", e)
	}
}
func TestCatalogPreservesModelAndAndroidCoverage(t *testing.T) {
	devices := Catalog()
	if len(devices) != 75 {
		t.Fatalf("catalog models%d", len(devices))
	}
	variants := 0
	seen := map[string]bool{}
	for _, d := range devices {
		if seen[d.ID] || len(d.Releases) < 1 || len(d.Releases) > 3 {
			t.Fatal("invalid catalog entry", d.ID)
		}
		seen[d.ID] = true
		variants += len(d.Releases)
	}
	if variants != 184 {
		t.Fatalf("catalog variants%d", variants)
	}
	for range 100 {
		s := newTestSession(t)
		if e := ValidateSession(s); e != nil {
			t.Fatal(e)
		}
	}
}

func TestMediaFailureRetainsReasonAndRetryAfter(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		retry    string
		recovery Recovery
		code     string
	}{
		{403, `{"status_code":1,"status_msg":"invalid signature"}`, "", RecoveryRefresh, "signature_rejected"},
		{403, `{"status_code":1,"status_msg":"device_id is invalid"}`, "", RecoveryRecreate, "identity_rejected"},
		{403, `<html>challenge required</html>`, "", RecoveryNone, "upstream_blocked"},
		{429, `{"status_code":1}`, "7", RecoveryNone, "upstream_rate_limited"},
	}
	for _, test := range cases {
		s := registeredTestSession(t)
		client := NewClient(s, doerFunc(func(r *http.Request) (*http.Response, error) {
			return testResponse(test.status, []byte(test.body), http.Header{"Retry-After": {test.retry}}), nil
		}))
		response, err := client.MediaRequest(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/?video_id=fixture", nil)
		var failure *UpstreamError
		if response != nil || !errors.As(err, &failure) || failure.Code != test.code || failure.Recovery != test.recovery {
			t.Fatalf("media classification: %v", err)
		}
		if test.retry != "" && failure.RetryAfter != 7*time.Second {
			t.Fatalf("lost media retry-after: %s", failure.RetryAfter)
		}
	}
}

type trackedMediaBody struct {
	io.ReadCloser
	read   int
	closed bool
}

func (b *trackedMediaBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += n
	return n, err
}

func (b *trackedMediaBody) Close() error {
	b.closed = true
	return b.ReadCloser.Close()
}

func TestMediaHTTP200JSONFailures(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, blocked, code string
	}{
		{"private", "application/json", `{"status_code":10216}`, "", "content_private"},
		{"age", "application/json; charset=utf-8", `{"status_code":1,"status_msg":"Age restricted"}`, "", "content_restricted"},
		{"region untyped", "", " \n" + `{"status_code":10204}`, "", "region_blocked"},
		{"octet stream", "application/octet-stream", `{"status_code":10222}`, "", "content_private"},
		{"json suffix", "application/problem+json", `{"status_code":10216}`, "", "content_private"},
		{"no error status", "application/json", `{"status_code":0}`, "", "upstream_invalid_response"},
		{"malformed", "application/json", `{`, "", "upstream_invalid_response"},
		{"oversized", "application/json", `{"padding":"` + strings.Repeat("x", 300<<10) + `"}`, "", "upstream_invalid_response"},
		{"blocked private post", "application/json", `{"status_code":10216}`, "1", "upstream_blocked"},
		{"blocked private account", "application/json", `{"status_code":10222}`, "1", "upstream_blocked"},
		{"blocked region", "application/json", `{"status_code":10204}`, "1", "upstream_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedMediaBody{ReadCloser: io.NopCloser(strings.NewReader(tc.body))}
			client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.contentType}, "Tt_block": {tc.blocked}}, Body: body}, nil
			}))
			response, err := client.MediaRequest(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
			if response != nil {
				defer response.Body.Close()
			}
			var failure *UpstreamError
			if response != nil || !errors.As(err, &failure) || failure.Code != tc.code || failure.Recovery != RecoveryNone {
				t.Errorf("media error=%v, want %s with no identity recovery", err, tc.code)
			}
			if !body.closed || body.read > (256<<10)+1 {
				t.Errorf("JSON body closed=%t read=%d, want closed with bounded reads", body.closed, body.read)
			}
		})
	}
}

func TestMediaDetectionPreservesStreamingBody(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, prefix string
		status                    int
	}{
		{"video", "video/mp4", "\x00\x00\x00\x18ftyp", 200},
		{"untyped", "", "\x00\x00\x00\x18ftyp", 200},
		{"octet stream", "application/octet-stream", "\x00\x00\x00\x18ftyp", 200},
		{"partial", "application/octet-stream", `{"status_code":10216}`, 206},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.prefix + strings.Repeat("media", 10000)
			body := &trackedMediaBody{ReadCloser: io.NopCloser(strings.NewReader(payload))}
			client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}, Body: body}, nil
			}))
			response, err := client.MediaRequest(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if body.closed || body.read > 512 {
				t.Errorf("media was closed or buffered beyond its prefix: closed=%t read=%d", body.closed, body.read)
			}
			got, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if err != nil || closeErr != nil || string(got) != payload || !body.closed {
				t.Fatal("media bytes or body lifecycle changed", err, closeErr)
			}
		})
	}
}

func TestRegisteredSessionSerializesEmptyDeviceTokenForPythonCompatibility(t *testing.T) {
	session := registeredTestSession(t)
	session.Identity.DeviceToken = ""
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		Identity map[string]json.RawMessage `json:"identity"`
	}
	if err = json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	token, present := encoded.Identity["deviceToken"]
	if !present || string(token) != `""` {
		t.Fatal("registered session omitted required empty deviceToken, breaking preserved Python/Node readers")
	}
}

func TestDirectMediaTransportKeepsAppSigningAndCDNRedirectCookies(t *testing.T) {
	primary, direct := 0, 0
	client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
		primary++
		if r.URL.Host != "api19-va.tiktokv.com" || r.Header.Get("X-Argus") == "" {
			t.Error("app play did not use the signed primary route", r.URL)
		}
		return testResponse(302, nil, http.Header{"Location": {"https://cdn.example/start"}}), nil
	}))
	client.SetMediaTransport(doerFunc(func(r *http.Request) (*http.Response, error) {
		direct++
		if r.URL.Host != "cdn.example" || r.Header.Get("X-Argus") != "" || r.Header.Get("X-Tt-Store-Region") != "" {
			t.Error("CDN received app credentials", r.URL)
		}
		if r.URL.Path == "/start" {
			return testResponse(302, nil, http.Header{"Location": {"/final"}, "Set-Cookie": {"cdn=saved; Path=/; Secure"}}), nil
		}
		cookie, err := r.Cookie("cdn")
		if err != nil || cookie.Value != "saved" {
			t.Error("direct redirect lost cookies", err)
		}
		return testResponse(200, []byte("video"), nil), nil
	}))
	response, err := client.MediaRequest(context.Background(), "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "video" || primary != 1 || direct != 2 {
		t.Fatal(string(body), err, primary, direct)
	}
}
