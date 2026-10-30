package instagramapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
			select {
			case a.refreshGate <- struct{}{}:
			case <-ctx.Done():
				release()
				return core.AppDetail{}, ctx.Err()
			}
		}
		p.mu.Lock()
		if state.authorization != a.authorization {
			state.disable, state.cooldown = false, 0
		}
		if state.disable {
			a.disabled = true
			if p.options.Store != nil {
				p.pendingDeaths++
			}
		}
		if state.cooldown > 0 {
			a.cooldown = time.Now().Add(state.cooldown)
		}
		p.mu.Unlock()
		if guardSession {
			<-a.refreshGate
		}
		release()
		if state.disable && p.options.Store != nil {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			persistErr := p.options.Store.MarkDead(saveCtx, strings.TrimPrefix(a.id, IdentityPrefix), state.deadReason)
			cancel()
			p.mu.Lock()
			p.pendingDeaths--
			p.notify()
			p.mu.Unlock()
			if persistErr != nil {
				return core.AppDetail{}, persistErr
			}
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

func (p *Pool) lookup(ctx context.Context, a *account, mediaID, auth string, device *DeviceIdentity) (map[string]any, disposition, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiOrigin+"/api/v1/media/"+mediaID+"/info/", nil)
	if err != nil {
		return nil, disposition{}, core.Upstream("Invalid Instagram API endpoint")
	}
	req.Header.Set("Authorization", auth)
	device.headers(req.Header)
	req.Header.Set("X-IG-App-ID", AppID)
	// net/http negotiates and decompresses gzip when Accept-Encoding is omitted.
	response, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, disposition{}, ctx.Err()
		}
		return nil, disposition{retry: true}, core.NewError("upstream_network_error", "Instagram metadata request failed", 502)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, metadataLimit+1))
	if response.StatusCode == 429 {
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
			if state, err := apiFailure(record); err != nil {
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
		if state, err := apiFailure(record); err != nil {
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

func apiFailure(record map[string]any) (disposition, error) {
	text := strings.ToLower(stringField(record, "message") + " " + stringField(record, "error_type") + " " + stringField(record, "error_title"))
	if truthy(record["challenge_required"]) || truthy(record["checkpoint_required"]) || truthy(record["challenge"]) || failureMarker(record, "challenge_required", "checkpoint_required", "checkpoint_challenge_required", "security_requirement") {
		return disposition{retry: true, disable: true, deadReason: "challenge_required"}, core.NewError("upstream_authentication_error", "Instagram session requires renewal or verification", 502)
	}
	if failureMarker(record, "login_required", "session_expired") {
		return disposition{retry: true, disable: true, renew: true, deadReason: "session_expired"}, core.NewError("upstream_authentication_error", "Instagram session requires renewal", 502)
	}
	if failureMarker(record, "account_disabled", "account_suspended", "inactive_user") {
		return disposition{retry: true, disable: true, deadReason: "account_restricted"}, core.NewError("upstream_authentication_error", "Instagram account is restricted", 502)
	}
	if strings.Contains(text, "rate_limit") || strings.Contains(text, "throttl") || strings.Contains(text, "please wait") || strings.Contains(text, "feedback_required") {
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
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400)) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		if remaining := time.Until(date); remaining > 0 {
			return min(remaining, 24*time.Hour)
		}
	}
	return time.Minute
}
