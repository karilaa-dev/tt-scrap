package instagramapp

// Password encryption and session parsing verified against the Pixel 7 capture.
// Passwords and authenticator seeds exist only for the duration of the import.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type Credentials struct {
	Cookies       json.RawMessage `json:"cookies,omitempty"`
	Username      string          `json:"username"`
	Password      string          `json:"password"`
	TOTPSecret    string          `json:"totp_secret"`
	DeviceProfile string          `json:"device_profile,omitempty"`
}
type loginResult struct {
	Authorization, Username, UserID string
	Device                          *DeviceIdentity
}
type loginResponse struct {
	status int
	header http.Header
	value  map[string]any
}

const bloksVersion = "1dad8c6870f627b89627678d48ae07b89573dd4beb9cce4e6c7b4c56e60bf78d"

var bloksStrings = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

func loginFailure(code string) error {
	return core.NewError(code, "Instagram account login failed; check account status before retrying", 502)
}

func totpKey(input string) ([]byte, error) {
	value := strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToLower(value), "otpauth://") {
		u, e := url.Parse(value)
		if e != nil || u.Scheme != "otpauth" || u.Host != "totp" {
			return nil, loginFailure("invalid_totp_secret")
		}
		q := u.Query()
		if (q.Get("algorithm") != "" && !strings.EqualFold(q.Get("algorithm"), "SHA1")) || (q.Get("digits") != "" && q.Get("digits") != "6") || (q.Get("period") != "" && q.Get("period") != "30") {
			return nil, loginFailure("invalid_totp_secret")
		}
		value = q.Get("secret")
	}
	value = strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value))
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(value, "="))
	if e != nil || len(key) < 10 {
		return nil, loginFailure("invalid_totp_secret")
	}
	return key, nil
}
func totp(key []byte, now time.Time) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(counter[:])
	digest := h.Sum(nil)
	offset := digest[len(digest)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}
func encryptPassword(password, encodedKey string, keyID byte, now time.Time) (string, error) {
	raw, e := base64.StdEncoding.DecodeString(encodedKey)
	if e != nil {
		return "", loginFailure("invalid_login_key")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", loginFailure("invalid_login_key")
	}
	public, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		return "", loginFailure("invalid_login_key")
	}
	key, ok := public.(*rsa.PublicKey)
	if !ok || key.Size() < 256 || key.Size() > 8192 {
		return "", loginFailure("invalid_login_key")
	}
	secret := make([]byte, 32)
	defer clear(secret)
	iv := make([]byte, 12)
	if _, e = rand.Read(secret); e != nil {
		return "", e
	}
	if _, e = rand.Read(iv); e != nil {
		return "", e
	}
	//lint:ignore SA1019 Instagram's password envelope requires PKCS#1 v1.5; OAEP is not wire-compatible.
	wrapped, e := rsa.EncryptPKCS1v15(rand.Reader, key, secret)
	if e != nil {
		return "", loginFailure("invalid_login_key")
	}
	aesBlock, _ := aes.NewCipher(secret)
	gcm, _ := cipher.NewGCM(aesBlock)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	encrypted := gcm.Seal(nil, iv, []byte(password), []byte(timestamp))
	envelope := make([]byte, 16)
	envelope[0] = 1
	envelope[1] = keyID
	copy(envelope[2:], iv)
	binary.LittleEndian.PutUint16(envelope[14:], uint16(len(wrapped)))
	envelope = append(envelope, wrapped...)
	envelope = append(envelope, encrypted[len(encrypted)-16:]...)
	envelope = append(envelope, encrypted[:len(encrypted)-16]...)
	return "#PWD_INSTAGRAM:4:" + timestamp + ":" + base64.StdEncoding.EncodeToString(envelope), nil
}
func jsonObject(value any) (map[string]any, error) {
	if s, ok := value.(string); ok {
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		if d.Decode(&m) != nil || m == nil {
			return nil, loginFailure("invalid_login_response")
		}
		return m, nil
	}
	if m, ok := value.(map[string]any); ok && m != nil {
		return m, nil
	}
	return nil, loginFailure("invalid_login_response")
}
func parseLogin(status int, header http.Header, body []byte) (loginResponse, error) {
	value, e := jsonObject(string(body))
	if e != nil {
		if status == 429 {
			return loginResponse{}, loginFailure("upstream_rate_limited")
		}
		return loginResponse{}, e
	}
	payload := value["payload"]
	if payload == nil {
		layout, _ := value["layout"].(map[string]any)
		bloks, _ := layout["bloks_payload"].(map[string]any)
		action, _ := bloks["action"].(string)
		var previous string
		for _, literal := range bloksStrings.FindAllString(action, -1) {
			var s string
			if json.Unmarshal([]byte(literal), &s) != nil {
				continue
			}
			candidate, e := jsonObject(s)
			if e != nil || candidate["login_response"] == nil {
				continue
			}
			canonical, _ := json.Marshal(candidate)
			if previous != "" && previous != string(canonical) {
				return loginResponse{}, loginFailure("invalid_login_response")
			}
			previous = string(canonical)
			payload = candidate
		}
	}
	header = header.Clone()
	if payload != nil {
		nested, e := jsonObject(payload)
		if e != nil {
			return loginResponse{}, e
		}
		if nested["headers"] != nil {
			headers, e := jsonObject(nested["headers"])
			if e != nil {
				return loginResponse{}, e
			}
			for k, v := range headers {
				if s, ok := v.(string); ok {
					header.Set(k, s)
				}
			}
		}
		value = nested
		if nested["login_response"] != nil {
			value, e = jsonObject(nested["login_response"])
			if e != nil {
				return loginResponse{}, e
			}
		}
	}
	return loginResponse{status, header, value}, nil
}
func loginCheck(r loginResponse) error {
	text := strings.ToLower(stringField(r.value, "message") + " " + stringField(r.value, "error_type"))
	if r.status == http.StatusTooManyRequests || strings.Contains(text, "rate_limit") || strings.Contains(text, "please wait") {
		return loginFailure("upstream_rate_limited")
	}
	if truthy(r.value["challenge"]) || truthy(r.value["challenge_required"]) || truthy(r.value["checkpoint_required"]) || strings.Contains(text, "challenge") || strings.Contains(text, "checkpoint") || strings.Contains(text, "security_requirement") {
		return loginFailure("challenge_required")
	}
	return nil
}
func verifyLogin(ctx context.Context, client *http.Client, origin, auth string, device *DeviceIdentity) (loginResult, error) {
	// Verify that the issued session is usable on the selected route before saving it.
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/api/v1/accounts/current_user/?edit=true", nil)
	req.Header.Set("Authorization", auth)
	device.headers(req.Header)
	req.Header.Set("X-IG-App-ID", AppID)
	res, e := client.Do(req)
	if e != nil {
		return loginResult{}, loginFailure("session_check_failed")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return loginResult{}, loginFailure("upstream_rate_limited")
	}
	body, e := io.ReadAll(io.LimitReader(res.Body, metadataLimit+1))
	if e != nil || len(body) > metadataLimit || res.StatusCode != 200 {
		return loginResult{}, loginFailure("session_check_failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var verified struct {
		Status string `json:"status"`
		User   struct {
			Username string      `json:"username"`
			PK       json.Number `json:"pk"`
		} `json:"user"`
	}
	if decoder.Decode(&verified) != nil || verified.Status != "ok" || verified.User.Username == "" || verified.User.PK == "" {
		return loginResult{}, loginFailure("session_check_failed")
	}
	return loginResult{Authorization: auth, Username: verified.User.Username, UserID: verified.User.PK.String(), Device: device}, nil
}
func validAuthorization(auth string) bool {
	if !strings.HasPrefix(auth, "Bearer IGT:2:") || len(auth) <= len("Bearer IGT:2:") {
		return false
	}
	for _, r := range strings.TrimPrefix(auth, "Bearer ") {
		if r <= 32 || r >= 127 {
			return false
		}
	}
	return true
}
