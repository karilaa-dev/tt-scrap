package tiktokapp

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var videoIDPattern = regexp.MustCompile(`^[1-9][0-9]{9,24}$`)
var videoPathPattern = regexp.MustCompile(`/(?:video|photo)/([0-9]+)`)

func ParseVideoID(input string) (string, error) {
	if videoIDPattern.MatchString(input) {
		return input, nil
	}
	u, e := url.Parse(input)
	if e != nil || u.Scheme != "https" || !(u.Hostname() == "tiktok.com" || strings.HasSuffix(u.Hostname(), ".tiktok.com")) {
		return "", errors.New("expected a TikTok URL or numeric video ID")
	}
	m := videoPathPattern.FindStringSubmatch(u.Path)
	if len(m) == 2 && videoIDPattern.MatchString(m[1]) {
		return m[1], nil
	}
	return "", errors.New("TikTok URL does not contain a video ID")
}
func commonParams(i Identity, p Profile, now time.Time) url.Values {
	q := url.Values{"device_platform": {"android"}, "os": {"android"}, "ssmix": {"a"}, "_rticket": {strconv.FormatInt(now.UnixMilli(), 10)}, "channel": {p.Channel}, "aid": {"1233"}, "app_name": {"musical_ly"}, "version_code": {strconv.Itoa(APKVersionCode)}, "version_name": {APKVersion}, "manifest_version_code": {strconv.Itoa(APKManifestVersionCode)}, "update_version_code": {strconv.Itoa(APKManifestVersionCode)}, "ab_version": {APKVersion}, "resolution": {p.Resolution}, "dpi": {strconv.Itoa(p.DPI)}, "device_type": {p.Model}, "device_brand": {p.Brand}, "language": {p.Language}, "os_api": {strconv.Itoa(p.OSAPI)}, "os_version": {p.OSVersion}, "ac": {"wifi"}, "app_type": {"normal"}, "app_language": {p.Language}, "region": {p.Region}, "sys_region": {p.Region}, "timezone_name": {p.Timezone}, "timezone_offset": {strconv.Itoa(p.TimezoneOffset)}, "host_abi": {"arm64-v8a"}, "locale": {p.Language + "-" + p.Region}, "ts": {strconv.FormatInt(now.Unix(), 10)}, "openudid": {i.OpenUDID}}
	if i.Kind == "registered" {
		q.Set("device_id", i.DeviceID)
		q.Set("iid", i.InstallID)
	}
	return q
}
func registrationPayload(i Identity, p Profile, requestID string, now time.Time) map[string]any {
	parts := strings.Split(p.Resolution, "*")
	resolution := p.Resolution
	if len(parts) == 2 {
		resolution = parts[1] + "x" + parts[0]
	}
	header := map[string]any{"aid": APKAID, "app_name": "musical_ly", "display_name": "TikTok", "package": APKPackage, "app_version": APKVersion, "version_code": APKVersionCode, "manifest_version_code": APKManifestVersionCode, "update_version_code": APKManifestVersionCode, "app_version_minor": "", "channel": p.Channel, "device_platform": "android", "os": "Android", "os_version": p.OSVersion, "os_api": p.OSAPI, "device_model": p.Model, "device_brand": p.Brand, "device_manufacturer": p.Manufacturer, "cpu_abi": "arm64-v8a", "density_dpi": p.DPI, "resolution": resolution, "language": p.Language, "app_language": p.Language, "region": p.Region, "app_region": p.Region, "timezone": float64(p.TimezoneOffset) / 3600, "tz_name": p.Timezone, "tz_offset": p.TimezoneOffset, "access": "wifi", "not_request_sender": 1, "openudid": i.OpenUDID, "clientudid": i.ClientUDID, "sdk_version": "2.5.14.6", "sdk_target_version": 30, "sdk_flavor": "i18nInner", "git_hash": "43cd1bf", "req_id": requestID, "apk_first_install_time": i.CreatedAt.UnixMilli(), "is_system_app": 0, "user_agent": UserAgent(p)}
	if i.Kind == "registered" {
		header["device_id"] = i.DeviceID
		header["install_id"] = i.InstallID
	}
	return map[string]any{"magic_tag": "ss_app_log", "header": header, "_gen_time": now.UnixMilli()}
}

type RoutingSelectors struct {
	HostGroup        []string          `json:"host_group"`
	ContainGroup     []string          `json:"contain_group"`
	EqualGroup       []string          `json:"equal_group"`
	PrefixesGroup    []string          `json:"prefixes_group"`
	PatternGroup     []string          `json:"pattern_group"`
	HostReplace      string            `json:"host_replace,omitempty"`
	DispatchStrategy int               `json:"dispatch_strategy,omitempty"`
	StrategyInfo     map[string]string `json:"strategy_info,omitempty"`
}
type RoutingRule struct {
	Priority int              `json:"act_priority"`
	Action   string           `json:"action"`
	Param    RoutingSelectors `json:"param"`
}

var appHostPattern = regexp.MustCompile(`(?:^|\.)tiktok(?:v|w)?\.(com|us|eu)$`)
var routeHostPattern = regexp.MustCompile(`^[a-z0-9.-]+\.tiktok(?:v|w)?\.(com|us|eu)$`)

// Routing patterns arrive with registrations and repeat across identities, so
// each is compiled once. The bound caps memory if upstream varies them.
const maxRoutePatterns = 1024

var routePatterns sync.Map
var routePatternCount atomic.Int32

func routePattern(pattern string) *regexp.Regexp {
	if cached, ok := routePatterns.Load(pattern); ok {
		return cached.(*regexp.Regexp)
	}
	re, _ := regexp.Compile(pattern)
	if routePatternCount.Load() < maxRoutePatterns {
		if _, loaded := routePatterns.LoadOrStore(pattern, re); !loaded {
			routePatternCount.Add(1)
		}
	}
	return re
}

func IsAppHost(u *url.URL) bool {
	return u.Scheme == "https" && appHostPattern.MatchString(u.Hostname())
}
func routeHostMatches(hosts []string, hostname string) bool {
	for _, host := range hosts {
		if host == "*" || host == hostname || (strings.HasPrefix(host, "*.") && strings.HasSuffix(hostname, host[1:])) {
			return true
		}
	}
	return false
}
func routePathMatches(p RoutingSelectors, path string) bool {
	for _, part := range p.ContainGroup {
		if strings.Contains(path, part) {
			return true
		}
	}
	for _, part := range p.EqualGroup {
		if path == part {
			return true
		}
	}
	for _, part := range p.PrefixesGroup {
		if strings.HasPrefix(path, part) {
			return true
		}
	}
	for _, part := range p.PatternGroup {
		if re := routePattern(part); re != nil && re.MatchString(path) {
			return true
		}
	}
	return false
}
func routeURL(original *url.URL, rules []RoutingRule) *url.URL {
	u := *original
	for _, rule := range rules {
		p := rule.Param
		if !routeHostMatches(p.HostGroup, u.Hostname()) || !routePathMatches(p, u.Path) {
			continue
		}
		target := ""
		if rule.Action == "tc" {
			target = p.HostReplace
		} else if rule.Action == "dispatch" && p.DispatchStrategy == 1 {
			target = p.StrategyInfo[u.Hostname()]
		}
		if routeHostPattern.MatchString(target) {
			u.Host = target
		}
	}
	return &u
}
