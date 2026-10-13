package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var postIDPattern = regexp.MustCompile(`/(?:video|photo)/(\d+)`)
var scriptPattern = regexp.MustCompile(`(?is)<script\b[^>]*\bid\s*=\s*["'](__UNIVERSAL_DATA_FOR_REHYDRATION__|SIGI_STATE|sigi-persisted-data)["'][^>]*>`)
var challengeElementPattern = regexp.MustCompile(`(?is)<[^/!][^>]*\bid\s*=\s*["'](cs|wci|rci|rs)["'][^>]*>`)
var classPattern = regexp.MustCompile(`(?is)\bclass\s*=\s*["']([^"']*)["']`)

func validTikTokURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return u.Scheme == "https" && (host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com")) && u.User == nil
}
func videoID(raw string) string {
	if m := postIDPattern.FindStringSubmatch(raw); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ParseWebDetail preserves integer IDs by decoding JSON numbers losslessly.
func ParseWebDetail(body []byte, id string) (object, int, error) {
	for _, loc := range scriptPattern.FindAllSubmatchIndex(body, -1) {
		start := loc[1]
		end := bytes.Index(bytes.ToLower(body[start:]), []byte("</script>"))
		if end < 0 {
			continue
		}
		data, e := decode(body[start : start+end])
		if e != nil {
			return nil, 0, e
		}
		kind := string(body[loc[2]:loc[3]])
		if kind == "__UNIVERSAL_DATA_FOR_REHYDRATION__" {
			scope := obj(data["__DEFAULT_SCOPE__"])
			detail := obj(scope["webapp.video-detail"])
			item := obj(obj(detail["itemInfo"])["itemStruct"])
			if len(item) > 0 {
				return item, integer(detail["statusCode"]), nil
			}
			if status := integer(detail["statusCode"]); status != 0 {
				return nil, status, nil
			}
			continue
		}
		if item := obj(obj(data["ItemModule"])[id]); len(item) > 0 {
			return item, 0, nil
		}
	}
	return nil, 0, errors.New("TikTok page contains no post metadata")
}

func solveWebChallenge(ctx context.Context, body []byte, jar http.CookieJar) (bool, error) {
	fields := map[string]string{}
	for _, m := range challengeElementPattern.FindAllSubmatch(body, -1) {
		c := classPattern.FindSubmatch(m[0])
		if len(c) > 1 {
			fields[string(m[1])] = html.UnescapeString(string(c[1]))
		}
	}
	if fields["cs"] == "" || fields["wci"] == "" {
		return false, nil
	}
	raw, e := base64.StdEncoding.DecodeString(padBase64(fields["cs"]))
	if e != nil {
		return false, e
	}
	data, e := decode(raw)
	if e != nil {
		return false, e
	}
	values := obj(data["v"])
	prefix, e := base64.StdEncoding.DecodeString(padBase64(str(values["a"])))
	if e != nil {
		return false, e
	}
	expected, e := base64.StdEncoding.DecodeString(padBase64(str(values["c"])))
	if e != nil || len(expected) != sha256.Size {
		return false, errors.New("invalid TikTok challenge")
	}
	buffer := make([]byte, len(prefix), len(prefix)+16)
	copy(buffer, prefix)
	solution := ""
	for i := 0; i <= 1000000; i++ {
		if i%4096 == 0 {
			if e = ctx.Err(); e != nil {
				return false, e
			}
		}
		value := strconv.AppendInt(buffer[:len(prefix)], int64(i), 10)
		digest := sha256.Sum256(value)
		if bytes.Equal(digest[:], expected) {
			solution = strconv.Itoa(i)
			break
		}
	}
	if solution == "" {
		return false, errors.New("TikTok challenge has no solution")
	}
	data["d"] = base64.StdEncoding.EncodeToString([]byte(solution))
	encoded, e := json.Marshal(data)
	if e != nil {
		return false, e
	}
	u, _ := url.Parse("https://www.tiktok.com/")
	cookies := []*http.Cookie{{Name: fields["wci"], Value: base64.StdEncoding.EncodeToString(encoded), Domain: ".tiktok.com", Path: "/", Secure: true, Expires: time.Now().Add(2 * time.Minute)}}
	if fields["rci"] != "" && fields["rs"] != "" {
		cookies = append(cookies, &http.Cookie{Name: fields["rci"], Value: fields["rs"], Domain: ".tiktok.com", Path: "/", Secure: true, Expires: time.Now().Add(2 * time.Minute)})
	}
	jar.SetCookies(u, cookies)
	return true, nil
}
func padBase64(s string) string {
	return strings.TrimRight(s, "=") + strings.Repeat("=", (4-len(strings.TrimRight(s, "="))%4)%4)
}

func newWebClient(transport http.RoundTripper) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("TikTok redirect limit exceeded")
		}
		if !validTikTokURL(req.URL.String()) {
			return errors.New("TikTok redirected outside its domain")
		}
		return nil
	}}
}
