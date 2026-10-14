package tiktokapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}
type Recovery string

const (
	RecoveryNone     Recovery = "none"
	RecoveryRefresh  Recovery = "refresh"
	RecoveryRecreate Recovery = "recreate"
)

type UpstreamError struct {
	Code          string
	Recovery      Recovery
	HTTPStatus    int
	AppStatusCode int
	RetryAfter    time.Duration
}

func (e *UpstreamError) Error() string {
	switch e.Code {
	case "upstream_rate_limited":
		return "The upstream service rate limited the request."
	case "upstream_unavailable":
		return "The upstream service is temporarily unavailable."
	case "upstream_blocked":
		return "The upstream service did not accept the request."
	case "upstream_invalid_response":
		return "The upstream service returned an invalid response."
	case "signature_rejected":
		return "The upstream service rejected the signing credentials."
	case "identity_rejected":
		return "The upstream service rejected the device registration."
	case "content_private":
		return "The requested content is private."
	case "content_restricted":
		return "The requested content requires an account with access."
	case "content_deleted":
		return "The requested content is unavailable or deleted."
	case "region_blocked":
		return "The requested content is unavailable in this region."
	}
	return "The upstream service rejected the request."
}

var identityFailurePattern = regexp.MustCompile(`\b(?:invalid|expired|unregistered|revoked)[ _-]+(?:device(?:[ _-]?id)?|install[ _-]?id)\b|\b(?:device(?:[ _-]?id)?|install[ _-]?id)[ _-]+(?:is[ _-]+)?(?:invalid|expired|unregistered|revoked|not registered)\b`)
var signatureFailurePattern = regexp.MustCompile(`\b(?:invalid|expired)[ _-]+(?:signature|seed|sdk[ _-]?token|x[ _-]argus)\b|\b(?:signature|seed|sdk[ _-]?token|x[ _-]argus)[ _-]+(?:is[ _-]+)?(?:invalid|expired|error|rejected)\b`)
var rateFailurePattern = regexp.MustCompile(`rate[ _-]?limit|too many requests`)
var contentRestrictionPattern = regexp.MustCompile(`age[ _-]?(?:restrict|limit|verif)|(?:content|video|post)[ _-]+(?:is[ _-]+)?(?:restricted|private)|(?:log[ _-]?in|sign[ _-]?in|authentication)[ _-]+(?:is[ _-]+)?required|(?:requires?|must)[ _-]+(?:log[ _-]?in|sign[ _-]?in)|(?:log[ _-]?in|sign[ _-]?in) to (?:view|watch|access|see)|not (?:authorized|allowed|permitted) to (?:view|watch)|not available (?:in your|for your) (?:country|region)`)

func ClassifyFailure(status, appStatus int, message string, blocked bool, retry time.Duration) *UpstreamError {
	message = strings.ToLower(message)
	e := &UpstreamError{Code: "upstream_rejected", Recovery: RecoveryNone, HTTPStatus: status, AppStatusCode: appStatus, RetryAfter: retry}
	switch {
	case status == 429 || rateFailurePattern.MatchString(message):
		e.Code = "upstream_rate_limited"
	case status >= 500 || status == 408:
		e.Code = "upstream_unavailable"
	case status == 404:
		e.Code = "content_deleted"
	case !blocked && (appStatus == 10216 || appStatus == 10222):
		e.Code = "content_private"
	case !blocked && appStatus == 10204:
		e.Code = "region_blocked"
	case identityFailurePattern.MatchString(message):
		e.Code = "identity_rejected"
		e.Recovery = RecoveryRecreate
	case signatureFailurePattern.MatchString(message):
		e.Code = "signature_rejected"
		e.Recovery = RecoveryRefresh
	case !blocked && contentRestrictionPattern.MatchString(message):
		e.Code = "content_restricted"
	case blocked || status == 401 || status == 403:
		e.Code = "upstream_blocked"
	}
	return e
}

type Client struct {
	Session          *Session
	RegistrationBase string
	APIBase          string
	SDKBase          string
	transport        Doer
	mediaTransport   Doer
	mu               sync.Mutex
	registerMu       sync.Mutex
	bootstrapMu      sync.Mutex
	jar              *persistentJar
}

func NewClient(session *Session, doer Doer) *Client {
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	doer = withoutRedirects(doer)
	return &Client{Session: session, transport: doer, RegistrationBase: "https://log-va.tiktokv.com", APIBase: "https://api19-va.tiktokv.com", SDKBase: "https://mssdk-va.tiktokv.com", jar: newPersistentJar(session.Cookies)}
}

// SetMediaTransport configures direct CDN traffic before the client is used.
// Signed app endpoints continue to use the identity's primary transport.
func (c *Client) SetMediaTransport(doer Doer) { c.mediaTransport = withoutRedirects(doer) }
func withoutRedirects(doer Doer) Doer {
	if h, ok := doer.(*http.Client); ok {
		copy := *h
		copy.Jar = nil
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &copy
	}
	return doer
}
func (c *Client) SyncCookies() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Session.Cookies = c.jar.serialize()
}
func (c *Client) request(ctx context.Context, original *url.URL, method string, body []byte, headers http.Header) (*http.Response, error) {
	return c.requestWithAccount(ctx, original, method, body, headers, false, Account{})
}
func (c *Client) requestWithAccount(ctx context.Context, original *url.URL, method string, body []byte, headers http.Header, media bool, account Account) (*http.Response, error) {
	c.mu.Lock()
	u := routeURL(original, c.Session.RoutingRules)
	jar := c.jar
	var accountCookies *accountCookieJar
	if account.Valid() {
		jar = newPersistentJar(c.jar.serialize())
		accountCookies = newAccountCookieJar(account)
	}
	c.mu.Unlock()
	for hop := 0; hop < 6; hop++ {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return nil, errors.New("unsupported app request URL scheme")
		}
		out := headers.Clone()
		if out == nil {
			out = make(http.Header)
		}
		for _, key := range []string{"Cookie", "Authorization", "X-Argus", "X-Ladon", "X-Khronos", "X-SS-REQ-TICKET", "X-Tt-Store-Region", "X-Tt-Store-Region-Src"} {
			out.Del(key)
		}
		c.mu.Lock()
		out.Set("User-Agent", UserAgent(c.Session.Profile))
		cookies := jar.jar.Cookies(u)
		region := c.Session.StoreRegion
		c.mu.Unlock()
		appHost := IsAppHost(u)
		if appHost && region != "" {
			out.Set("X-Tt-Store-Region", region)
			out.Set("X-Tt-Store-Region-Src", "did")
		}
		if appHost && strings.HasPrefix(original.Path, "/aweme/") && strings.HasPrefix(u.Path, "/aweme/") {
			if e := c.Bootstrap(ctx); e != nil {
				return nil, e
			}
			c.mu.Lock()
			signed, e := signAppRequest(u, body, c.Session.Identity, c.Session.Profile, *c.Session.SDK, time.Now())
			if e == nil {
				c.Session.SDK.RequestCount++
			}
			c.mu.Unlock()
			if e != nil {
				return nil, e
			}
			for key, values := range signed {
				out[key] = values
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header = out
		for _, cookie := range cookies {
			if !isAccountCookie(cookie.Name) {
				req.AddCookie(cookie)
			}
		}
		if accountCookies != nil {
			for _, cookie := range accountCookies.Cookies(u) {
				req.AddCookie(cookie)
			}
		}
		doer := c.transport
		if media && !appHost && c.mediaTransport != nil {
			doer = c.mediaTransport
		}
		response, e := doer.Do(req)
		if e != nil {
			return nil, e
		}
		c.mu.Lock()
		jar.accept(u, response.Cookies())
		if accountCookies != nil {
			accountCookies.SetCookies(u, response.Cookies())
		}
		c.Session.Cookies = c.jar.serialize()
		c.mu.Unlock()
		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
		default:
			return response, nil
		}
		location := response.Header.Get("Location")
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if location == "" {
			return nil, errors.New("app redirect lacks a location")
		}
		next, e := u.Parse(location)
		if e != nil || !(next.Scheme == "https" || next.Scheme == "http") || (u.Scheme == "https" && next.Scheme != "https") {
			return nil, errors.New("invalid or downgraded app redirect")
		}
		bootstrapRedirect := (u.Path == "/service/2/device_register/" || u.Path == "/ms/get_seed") && u.Path == next.Path && next.Scheme == "https" && bootstrapHost(u.Hostname()) && bootstrapHost(next.Hostname())
		if len(body) > 0 && next.Scheme+"://"+next.Host != u.Scheme+"://"+u.Host && !bootstrapRedirect {
			return nil, errors.New("refused app body redirect to an unrelated origin")
		}
		if response.StatusCode == 303 || ((response.StatusCode == 301 || response.StatusCode == 302) && method == "POST") {
			method = "GET"
			body = nil
			headers = headers.Clone()
			headers.Del("Content-Type")
		}
		u = next
	}
	return nil, errors.New("too many app redirects")
}
func bootstrapHost(host string) bool {
	return strings.HasSuffix(host, ".tiktokv.com") || strings.HasSuffix(host, ".tiktokv.us") || strings.HasSuffix(host, ".tiktokv.eu")
}
func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	var delay time.Duration
	if seconds, e := strconv.ParseFloat(header, 64); e == nil {
		delay = time.Duration(seconds * float64(time.Second))
	} else if date, e := http.ParseTime(header); e == nil {
		delay = time.Until(date)
	}
	if delay < 0 {
		return 0
	}
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}
func jsonResponse(response *http.Response) (map[string]any, error) {
	return jsonResponseBounded(response, maxSDKPayload)
}

func jsonResponseBounded(response *http.Response, maxBytes int64) (map[string]any, error) {
	defer response.Body.Close()
	body, e := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if e != nil {
		return nil, e
	}
	blocked := response.Header.Get("tt_block") != ""
	retry := retryAfter(response.Header.Get("Retry-After"))
	if int64(len(body)) > maxBytes {
		if response.StatusCode < 200 || response.StatusCode >= 300 || blocked {
			return nil, ClassifyFailure(response.StatusCode, 0, "", blocked, retry)
		}
		return nil, &UpstreamError{Code: "upstream_invalid_response", Recovery: RecoveryNone, HTTPStatus: response.StatusCode}
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	e = decoder.Decode(&value)
	if e == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			e = errors.New("trailing JSON payload")
		}
	}
	if e != nil || value == nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 || blocked {
			return nil, ClassifyFailure(response.StatusCode, 0, "", blocked, retry)
		}
		return nil, &UpstreamError{Code: "upstream_invalid_response", Recovery: RecoveryNone, HTTPStatus: response.StatusCode}
	}
	code := int(Number(value["status_code"]))
	message, _ := value["status_msg"].(string)
	if message == "" {
		message, _ = value["message"].(string)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || code != 0 || blocked {
		return nil, ClassifyFailure(response.StatusCode, code, message, blocked, retry)
	}
	return value, nil
}
func Number(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}
func String(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	}
	return ""
}
func (c *Client) Register(ctx context.Context) error {
	c.registerMu.Lock()
	defer c.registerMu.Unlock()
	c.mu.Lock()
	i, p := c.Session.Identity, c.Session.Profile
	c.mu.Unlock()
	if i.Kind == "registered" {
		return nil
	}
	now := time.Now()
	requestID := newUUID()
	u, e := url.Parse(c.RegistrationBase)
	if e != nil {
		return e
	}
	u.Path = "/service/2/device_register/"
	q := commonParams(i, p, now)
	q.Set("req_id", requestID)
	q.Set("tt_data", "a")
	u.RawQuery = q.Encode()
	encrypted, e := encryptRegistration(registrationPayload(i, p, requestID, now))
	if e != nil {
		return e
	}
	response, e := c.request(ctx, u, "POST", encrypted, http.Header{"Content-Type": {"application/octet-stream;tt-data=a"}, "X-Tt-Request-Tag": {"t=0;n=1"}})
	if e != nil {
		return e
	}
	data, e := jsonResponse(response)
	if e != nil {
		return e
	}
	device, install := String(data["device_id_str"]), String(data["install_id_str"])
	if !serverIDPattern.MatchString(device) || !serverIDPattern.MatchString(install) {
		return &UpstreamError{Code: "upstream_invalid_response", Recovery: RecoveryNone, HTTPStatus: response.StatusCode}
	}
	registered := time.Now().UTC()
	i.Kind = "registered"
	i.DeviceID = device
	i.InstallID = install
	i.DeviceToken = String(data["device_token"])
	i.RegisteredAt = &registered
	var rules []RoutingRule
	if tnc, ok := data["tnc_data"].(map[string]any); ok {
		if raw, ok := tnc["ttnet_dispatch_actions"].([]any); ok {
			for _, entry := range raw {
				b, e := json.Marshal(entry)
				if e != nil {
					continue
				}
				var r RoutingRule
				if json.Unmarshal(b, &r) == nil && (r.Action == "tc" || r.Action == "dispatch" && r.Param.DispatchStrategy == 1) {
					rules = append(rules, r)
				}
			}
		}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	c.mu.Lock()
	c.Session.Identity = i
	c.Session.RoutingRules = rules
	c.Session.StoreRegion = response.Header.Get("X-Tt-Store-Region")
	c.mu.Unlock()
	return nil
}

var serverIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

func (c *Client) Bootstrap(ctx context.Context) error {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if e := c.Register(ctx); e != nil {
		return e
	}
	c.mu.Lock()
	sdk := c.Session.SDK
	i := c.Session.Identity
	c.mu.Unlock()
	now := time.Now()
	if sdk != nil && now.Sub(sdk.FetchedAt) >= 0 && now.Sub(sdk.FetchedAt) < SigningTTL {
		return nil
	}
	u, body, e := seedRequest(i, c.SDKBase, now)
	if e != nil {
		return e
	}
	response, e := c.request(ctx, u, "POST", body, http.Header{"Content-Type": {"application/octet-stream"}})
	if e != nil {
		return e
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get("tt_block") != "" {
		_, e = jsonResponse(response)
		return e
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, maxSDKPayload+1))
	if e != nil {
		return e
	}
	if len(raw) > maxSDKPayload {
		return errors.New("SDK response exceeds size limit")
	}
	sdk, e = acceptSeedResponse(raw, time.Now())
	if e != nil {
		return e
	}
	c.mu.Lock()
	c.Session.SDK = sdk
	c.mu.Unlock()
	return nil
}
func (c *Client) Detail(ctx context.Context, input string) (map[string]any, error) {
	return c.DetailWithAccount(ctx, input, Account{})
}
func (c *Client) DetailWithAccount(ctx context.Context, input string, account Account) (map[string]any, error) {
	id, e := c.ResolveVideo(ctx, input)
	if e != nil {
		return nil, e
	}
	if e = c.Bootstrap(ctx); e != nil {
		return nil, e
	}
	u, e := url.Parse(c.APIBase)
	if e != nil {
		return nil, e
	}
	u.Path = "/aweme/v1/aweme/detail/"
	c.mu.Lock()
	q := commonParams(c.Session.Identity, c.Session.Profile, time.Now())
	c.mu.Unlock()
	q.Set("aweme_id", id)
	q.Set("origin_type", "others")
	q.Set("request_source", "0")
	q.Set("share_scene", "1")
	u.RawQuery = q.Encode()
	response, e := c.requestWithAccount(ctx, u, "GET", nil, nil, false, account)
	if e != nil {
		return nil, e
	}
	data, e := jsonResponse(response)
	if e != nil {
		return nil, e
	}
	if _, e = DetailItem(data, id); e != nil {
		return nil, e
	}
	return data, nil
}
func DetailItem(data map[string]any, id string) (map[string]any, error) {
	if item, ok := data["aweme_detail"].(map[string]any); ok && String(item["aweme_id"]) == id {
		return item, nil
	}
	if items, ok := data["aweme_details"].([]any); ok {
		for _, entry := range items {
			if item, ok := entry.(map[string]any); ok && String(item["aweme_id"]) == id {
				return item, nil
			}
		}
	}
	message := String(data["status_msg"])
	if message == "" {
		message = String(data["message"])
	}
	if failure := ClassifyFailure(http.StatusOK, int(Number(data["status_code"])), message, false, 0); IsContentRestricted(failure) {
		return nil, failure
	}
	return nil, &UpstreamError{Code: "content_deleted", Recovery: RecoveryNone, HTTPStatus: 200}
}
func (c *Client) Feed(ctx context.Context) (map[string]any, error) { return c.FeedPage(ctx, "0", 0) }
func (c *Client) FeedPage(ctx context.Context, cursor string, page int) (map[string]any, error) {
	if e := c.Bootstrap(ctx); e != nil {
		return nil, e
	}
	u, e := url.Parse(c.APIBase)
	if e != nil {
		return nil, e
	}
	u.Path = "/aweme/v1/feed/"
	c.mu.Lock()
	q := commonParams(c.Session.Identity, c.Session.Profile, time.Now())
	c.mu.Unlock()
	q.Set("type", "0")
	q.Set("count", "12")
	q.Set("max_cursor", cursor)
	q.Set("min_cursor", "-1")
	q.Set("pull_type", "0")
	q.Set("is_cold_start", "1")
	if page > 0 {
		q.Set("pull_type", "2")
		q.Set("is_cold_start", "0")
	}
	q.Set("req_from", "enter_auto")
	q.Set("feed_count", strconv.Itoa(page*12))
	q.Set("volume", "0")
	u.RawQuery = q.Encode()
	response, e := c.request(ctx, u, "GET", nil, nil)
	if e != nil {
		return nil, e
	}
	return jsonResponse(response)
}
func (c *Client) ResolveVideo(ctx context.Context, input string) (string, error) {
	if id, e := ParseVideoID(input); e == nil {
		return id, nil
	}
	u, e := url.Parse(input)
	if e != nil {
		return "", e
	}
	for hop := 0; hop < 6; hop++ {
		if u.Scheme != "https" || !(u.Hostname() == "tiktok.com" || strings.HasSuffix(u.Hostname(), ".tiktok.com")) {
			return "", errors.New("expected a TikTok video or share URL")
		}
		if id, e := ParseVideoID(u.String()); e == nil {
			return id, nil
		}
		req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if e != nil {
			return "", e
		}
		req.Header.Set("User-Agent", UserAgent(c.Session.Profile))
		response, e := c.transport.Do(req)
		if e != nil {
			return "", e
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
			location := response.Header.Get("Location")
			if location == "" {
				return "", errors.New("share URL redirect lacks a location")
			}
			u, e = u.Parse(location)
			if e != nil {
				return "", e
			}
		default:
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return "", ClassifyFailure(response.StatusCode, 0, "", false, 0)
			}
			return ParseVideoID(u.String())
		}
	}
	return "", errors.New("too many TikTok share redirects")
}
func (c *Client) MediaRequest(ctx context.Context, address string, headers http.Header) (*http.Response, error) {
	return c.MediaRequestWithAccount(ctx, address, headers, Account{})
}
func (c *Client) MediaRequestWithAccount(ctx context.Context, address string, headers http.Header, account Account) (*http.Response, error) {
	u, e := url.Parse(address)
	if e != nil {
		return nil, e
	}
	if IsAppHost(u) && strings.Contains(u.Path, "/aweme/v1/play") {
		if e = c.Bootstrap(ctx); e != nil {
			return nil, e
		}
		q := u.Query()
		c.mu.Lock()
		common := commonParams(c.Session.Identity, c.Session.Profile, time.Now())
		c.mu.Unlock()
		for key, value := range common {
			if !q.Has(key) {
				q[key] = value
			}
		}
		u.RawQuery = q.Encode()
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Accept-Encoding", "identity")
	response, err := c.requestWithAccount(ctx, u, "GET", nil, headers, true, account)
	if err != nil {
		return nil, err
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	jsonBody := contentType == "application/json" || strings.HasSuffix(contentType, "+json")
	if response.StatusCode == http.StatusOK && !jsonBody && response.Header.Get("tt_block") == "" {
		reader := bufio.NewReaderSize(response.Body, 512)
		prefix, _ := reader.Peek(512)
		response.Body = struct {
			io.Reader
			io.Closer
		}{reader, response.Body}
		jsonBody = bytes.HasPrefix(bytes.TrimSpace(prefix), []byte("{"))
	}
	if response.StatusCode >= 400 || response.Header.Get("tt_block") != "" || jsonBody {
		_, err = jsonResponseBounded(response, 256<<10)
		if err == nil {
			err = &UpstreamError{Code: "upstream_invalid_response", Recovery: RecoveryNone, HTTPStatus: response.StatusCode}
		}
		return nil, err
	}
	return response, nil
}
