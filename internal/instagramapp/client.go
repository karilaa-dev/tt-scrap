package instagramapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

const metadataLimit = 16 << 20

type disposition struct {
	authorization string
	renew         bool
	retry         bool
	disable       bool
	deadReason    string
	cooldown      time.Duration
}

func (p *Pool) Detail(ctx context.Context, mediaID string) (core.AppDetail, error) {
	if mediaID == "" || len(mediaID) > 40 || strings.Trim(mediaID, "0123456789") != "" {
		return core.AppDetail{}, core.InvalidLink("A numeric Instagram media ID is required")
	}
	if p.options.Store != nil {
		if e := p.options.Store.Flush(ctx); e != nil {
			return core.AppDetail{}, e
		}
	}
	tried := map[string]bool{}
	var last error
	for attempt := 0; attempt < p.options.MaxAttempts; attempt++ {
		a, release, err := p.acquire(ctx, "", tried)
		if err != nil {
			if last != nil && ctx.Err() == nil {
				return core.AppDetail{}, last
			}
			return core.AppDetail{}, err
		}
		tried[a.id] = true
		data, state, err := p.lookupRecovering(ctx, a, mediaID)
		// Renewal saves to the database before publishing its session in RAM.
		// Wait for that transition before applying a response's health state.
		guardSession := state.disable || state.cooldown > 0
		if guardSession {
			// Once a response establishes account health, caller cancellation must
			// not discard it. Renewal holding this gate has its own timeout.
			a.refreshGate <- struct{}{}
		}
		var cooldownUntil time.Time
		p.mu.Lock()
		if state.authorization != a.authorization {
			// Authentication failures belong to the rejected session. Throttle
			// deadlines belong to the account and survive session replacement.
			state.disable = false
		}
		if state.disable {
			a.disabled = true
			// A later explicit restriction must not be downgraded to ordinary
			// session expiry by a concurrent response.
			if a.deadReason == "" || (!state.retry && a.deadReason == "session_expired") {
				a.deadReason = state.deadReason
			}
			state.deadReason = a.deadReason
		}
		if state.cooldown > 0 {
			if until := time.Now().Add(state.cooldown); until.After(a.cooldown) {
				a.cooldown = until
				cooldownUntil = until
			}
		}
		persist := p.options.Store != nil && (state.disable || !cooldownUntil.IsZero())
		if persist {
			p.pendingHealthWrites++
		}
		p.mu.Unlock()
		if state.disable {
			slog.WarnContext(ctx, "Instagram account disabled", "account_id", strings.TrimPrefix(a.id, IdentityPrefix), "reason", state.deadReason)
		}
		if !cooldownUntil.IsZero() {
			reason := "temporary_failure"
			if errorCode(err) == "upstream_rate_limited" {
				reason = "rate_limited"
			}
			slog.WarnContext(ctx, "Instagram account cooldown applied", "account_id", strings.TrimPrefix(a.id, IdentityPrefix), "reason", reason, "cooldown_until", cooldownUntil.UTC())
		}
		var persistErr error
		if persist {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			id := strings.TrimPrefix(a.id, IdentityPrefix)
			reason := ""
			if state.disable {
				reason = state.deadReason
			}
			persistErr = p.options.Store.MarkHealth(saveCtx, id, reason, cooldownUntil)
			cancel()
			p.mu.Lock()
			p.pendingHealthWrites--
			p.notify()
			p.mu.Unlock()
		}
		if guardSession {
			<-a.refreshGate
		}
		if persistErr == nil {
			persistErr = p.persistSessionContext(ctx, a)
		}
		release()
		if persistErr != nil {
			return core.AppDetail{}, persistErr
		}
		if err == nil {
			_, device := p.session(a)
			return core.AppDetail{Response: data, IdentityID: a.id, UserAgent: device.userAgent()}, nil
		}
		last = err
		if ctx.Err() != nil {
			return core.AppDetail{}, ctx.Err()
		}
		if !state.retry {
			break
		}
		if attempt+1 < p.options.MaxAttempts && p.options.RetryDelay > 0 {
			timer := time.NewTimer(p.options.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return core.AppDetail{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return core.AppDetail{}, last
}

func (p *Pool) lookup(ctx context.Context, a *account, mediaID, auth string, device *DeviceIdentity, sessionState *sessionContext) (map[string]any, disposition, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiOrigin+"/api/v1/media/"+mediaID+"/info/", nil)
	if err != nil {
		return nil, disposition{}, core.Upstream("Invalid Instagram API endpoint")
	}
	req.Header.Set("Authorization", auth)
	device.headers(req.Header)
	req.Header.Set("X-IG-App-ID", AppID)
	client := *a.client
	client.Jar = sessionState
	if sessionState != nil {
		sessionState.Apply(req)
	} else {
		client.Jar = nil
	}
	// net/http negotiates and decompresses gzip when Accept-Encoding is omitted.
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, disposition{}, ctx.Err()
		}
		return nil, disposition{retry: true}, core.NewError("upstream_network_error", "Instagram metadata request failed", 502)
	}
	defer response.Body.Close()
	if sessionState != nil {
		sessionState.Observe(req.URL, response.Header)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, metadataLimit+1))
	classify := func(record map[string]any) (disposition, error) {
		state, err := apiFailure(record)
		if (state.cooldown > 0 || state.disable && !state.renew) && response.Header.Get("Retry-After") != "" {
			state.cooldown = max(state.cooldown, retryAfter(response.Header.Get("Retry-After")))
		}
		return state, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		var record map[string]any
		if readErr == nil && len(body) <= metadataLimit && json.Unmarshal(body, &record) == nil {
			if state, err := classify(record); state.disable && !state.renew {
				state.cooldown = retryAfter(response.Header.Get("Retry-After"))
				return nil, state, err
			}
		}
		return nil, disposition{retry: true, cooldown: retryAfter(response.Header.Get("Retry-After"))}, core.NewError("upstream_rate_limited", "Instagram account is rate limited", 429)
	}
	if response.StatusCode == 404 || response.StatusCode == 410 {
		return nil, disposition{}, core.NewError("content_deleted", "Instagram post is unavailable", 404)
	}
	// Instagram also reports unavailable media and session challenges as HTTP
	// 400, 401 or 403. A status alone cannot prove an account is unusable.
	// Classify only known fields; never include upstream text in errors.
	if (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && readErr == nil && len(body) <= metadataLimit {
		var record map[string]any
		if json.Unmarshal(body, &record) == nil {
			if state, err := classify(record); err != nil {
				return nil, state, err
			}
		}
	}
	if response.StatusCode != 200 {
		return nil, disposition{retry: true}, core.Upstream("Instagram metadata returned an unsuccessful response")
	}
	if readErr != nil || len(body) > metadataLimit {
		return nil, disposition{retry: true}, core.Upstream("Instagram metadata response is incomplete or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	merged := map[string]any{}
	count := 0
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil || record == nil {
			return nil, disposition{retry: true}, core.Upstream("Instagram returned malformed metadata")
		}
		count++
		if state, err := classify(record); err != nil {
			return nil, state, err
		}
		for key, value := range record {
			if key == "items" || key == "clips_items" {
				if list, ok := value.([]any); ok {
					old, _ := merged[key].([]any)
					merged[key] = append(old, list...)
					continue
				}
			}
			merged[key] = value
		}
	}
	if count == 0 {
		return nil, disposition{retry: true}, core.Upstream("Instagram returned empty metadata")
	}
	return merged, disposition{}, nil
}

// Serialize with renewal/replacement: a delayed old response must never overwrite
// a new session's saved context. The jar itself remains safe for concurrent reads.
func (p *Pool) persistSessionContext(ctx context.Context, a *account) error {
	if p.options.Store == nil {
		return nil
	}
	a.refreshGate <- struct{}{}
	defer func() { <-a.refreshGate }()
	auth, _, state := p.sessionContext(a)
	if state == nil {
		return nil
	}
	snapshot := state.Snapshot()
	serialized := jsonText(snapshot)
	if serialized == a.sessionSaved {
		return nil
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.options.Store.SaveSessionState(saveCtx, strings.TrimPrefix(a.id, IdentityPrefix), auth, snapshot); err != nil {
		return err
	}
	a.sessionSaved = serialized
	return nil
}

func apiFailure(record map[string]any) (disposition, error) {
	text := strings.ToLower(stringField(record, "message") + " " + stringField(record, "error_type") + " " + stringField(record, "error_title"))
	if truthy(record["challenge_required"]) || truthy(record["checkpoint_required"]) || truthy(record["challenge"]) || failureMarker(record, "challenge_required", "checkpoint_required", "checkpoint_challenge_required", "security_requirement") {
		return disposition{disable: true, deadReason: "challenge_required"}, core.NewError("upstream_authentication_error", "Instagram session requires verification before further requests", 502)
	}
	if truthy(record["feedback_required"]) || failureMarker(record, "feedback_required") {
		return disposition{disable: true, deadReason: "feedback_required"}, core.NewError("upstream_authentication_error", "Instagram account requires review before further requests", 502)
	}
	if failureMarker(record, "login_required", "session_expired") {
		return disposition{retry: true, disable: true, renew: true, deadReason: "session_expired"}, core.NewError("upstream_authentication_error", "Instagram session requires renewal", 502)
	}
	if failureMarker(record, "account_disabled", "account_suspended", "inactive_user") {
		return disposition{disable: true, deadReason: "account_restricted"}, core.NewError("upstream_authentication_error", "Instagram account is restricted", 502)
	}
	if strings.Contains(text, "rate_limit") || strings.Contains(text, "throttl") || strings.Contains(text, "please wait") {
		return disposition{retry: true, cooldown: time.Minute}, core.NewError("upstream_rate_limited", "Instagram account is rate limited", 429)
	}
	normalized := strings.ReplaceAll(text, "_", " ")
	if strings.Contains(normalized, "media") && (strings.Contains(normalized, "not found") || strings.Contains(normalized, "unavailable") || strings.Contains(normalized, "not available") || strings.Contains(normalized, "deleted")) {
		return disposition{}, core.NewError("content_deleted", "Instagram post is unavailable", 404)
	}
	if record["status"] == "fail" {
		return disposition{retry: true}, core.Upstream("Instagram returned an API failure")
	}
	return disposition{}, nil
}

// Permanent account decisions require explicit API markers, not incidental
// words in an error (for example, a proxy's "challenge service unavailable").
func failureMarker(record map[string]any, markers ...string) bool {
	for _, field := range []string{"message", "error_type", "error_title"} {
		value := strings.ToLower(strings.TrimSpace(stringField(record, field)))
		for _, marker := range markers {
			if value == marker {
				return true
			}
		}
	}
	return false
}

func stringField(m map[string]any, key string) string { value, _ := m[key].(string); return value }
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	case map[string]any:
		return len(v) > 0
	case json.Number:
		return v == "1"
	case float64:
		return v == 1
	default:
		return false
	}
}
func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	// Saturate unrepresentable delays rather than wrapping or retrying early.
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	if date, err := http.ParseTime(value); err == nil {
		if remaining := time.Until(date); remaining > 0 {
			return remaining
		}
	}
	return time.Minute
}
