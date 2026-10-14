package tiktokapp

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emmansun/gmsm/sm3"
)

func seedRequest(i Identity, base string, now time.Time) (*url.URL, []byte, error) {
	u, e := url.Parse(base)
	if e != nil {
		return nil, nil, e
	}
	u.Path = "/ms/get_seed"
	u.RawQuery = url.Values{"lc_id": {"2142840551"}, "platform": {"android"}, "device_platform": {"android"}, "sdk_ver": {"v05.03.01-alpha.2-ov-android"}, "sdk_ver_code": {"84082976"}, "app_ver": {APKVersion}, "version_code": {strconv.Itoa(APKManifestVersionCode)}, "aid": {"1233"}, "iid": {i.InstallID}, "did": {i.DeviceID}, "client_type": {"inhouse"}, "region_type": {"ov"}, "mode": {"2"}}.Encode()
	payload := encodeProto(ps(1, hex.EncodeToString(randomBytes(16))), ps(2, i.DeviceID), ps(3, "android"), ps(4, "v05.03.01"))
	encrypted, e := encryptSDKPayload(payload, randomBytes(4))
	if e != nil {
		return nil, nil, e
	}
	body := encodeProto(pi(1, 1077938244), pi(2, 2), pi(3, 4), pb(4, encrypted), pi(5, uint64(now.UnixMilli())*2))
	return u, body, nil
}
func acceptSeedResponse(response []byte, now time.Time) (*SDKState, error) {
	env, e := decodeProto(response)
	if e != nil {
		return nil, e
	}
	encrypted, e := protoBytes(env, 6)
	if e != nil {
		return nil, errors.New("SDK bootstrap did not return an encrypted seed")
	}
	plain, e := decryptSDKPayload(encrypted)
	if e != nil {
		return nil, e
	}
	fields, e := decodeProto(plain)
	if e != nil {
		return nil, e
	}
	seed, e := protoBytes(fields, 1)
	if e != nil {
		return nil, e
	}
	decoded, e := base64.StdEncoding.DecodeString(string(seed))
	if e != nil || len(decoded) != 98 {
		return nil, errors.New("invalid SDK seed length")
	}
	algorithms, e := protoBytes(fields, 2)
	if e != nil {
		return nil, e
	}
	nested, e := decodeProto(algorithms)
	if e != nil {
		return nil, e
	}
	packed, e := protoBytes(nested, 1)
	if e != nil {
		return nil, e
	}
	algorithm := 0
	for len(packed) > 0 {
		v, n := binary.Uvarint(packed)
		if n <= 0 {
			return nil, errors.New("invalid SDK algorithm list")
		}
		packed = packed[n:]
		if v&1 == 0 && v/2 >= 1 && v/2 <= 8 && algorithm == 0 {
			algorithm = int(v / 2)
		}
	}
	if algorithm == 0 {
		return nil, errors.New("unsupported SDK signing algorithm")
	}
	return &SDKState{Seed: string(seed), Algorithm: algorithm, FetchedAt: now.UTC()}, nil
}
func signAppRequest(u *url.URL, body []byte, i Identity, p Profile, sdk SDKState, now time.Time) (http.Header, error) {
	timestamp := uint64(now.Unix())
	started := uint64(sdk.FetchedAt.Unix())
	nonce := binary.LittleEndian.Uint32(randomBytes(4)) & 0x7fffffff
	digest := make([]byte, 16)
	if len(body) > 0 {
		h := md5.Sum(body)
		copy(digest, h[:])
	}
	query := u.RawQuery
	bodySM := sm3.Sum(digest)
	querySM := sm3.Sum([]byte(query))
	proof, e := dynamicProof(sdk.Algorithm, nonce, query, digest)
	if e != nil {
		return nil, e
	}
	osVersion, _ := strconv.ParseUint(strings.Split(p.OSVersion, ".")[0], 10, 32)
	udid := strings.ReplaceAll(i.ClientUDID, "-", "")
	if len(udid) > 25 {
		udid = udid[:25]
	}
	payload := encodeProto(pi(1, 1077940818), pi(2, 2), pi(3, uint64(nonce)*2), ps(4, "1233"), ps(5, i.DeviceID), ps(6, "2142840551"), ps(7, APKVersion), ps(8, "v05.03.01-ov-android"), pi(9, 168165952), pb(10, mustHex("0004000000000000")), pi(12, timestamp*2), pb(13, bodySM[:6]), pb(14, querySM[:6]), pb(15, encodeProto(pi(1, (sdk.RequestCount+1)*2), pi(2, 2), pi(5, 2), pi(6, 2), pi(7, started*2), pi(8, 2))), ps(16, udid), pi(17, timestamp*2), ps(20, "none"), pi(21, 624), pb(23, encodeProto(ps(1, p.Model), pi(2, osVersion*2), ps(3, p.Channel), pi(4, 539064320), pi(5, 35651584))), ps(24, sdk.Seed), pi(25, 10), pb(26, encodeProto(pi(1, uint64(sdk.Algorithm)*2), pb(2, proof))), pi(27, started*2), pi(28, 1006), pi(29, 540176), pi(30, 6), pi(31, uint64(binary.LittleEndian.Uint32(randomBytes(4)))), pi(33, 4))
	seed := randomBytes(4)
	prefix := concat([]byte{0xec}, randomBytes(4), mustHex("01d00518"))
	argus, e := encryptArgusEnvelope(payload, seed, mustHex("0004000000000000"), prefix, nil)
	if e != nil {
		return nil, e
	}
	lad, e := ladon(int64(timestamp), randomBytes(4))
	if e != nil {
		return nil, e
	}
	return http.Header{"X-Khronos": {strconv.FormatUint(timestamp, 10)}, "X-Ss-Req-Ticket": {strconv.FormatUint(timestamp*1000, 10)}, "X-Ladon": {lad}, "X-Argus": {argus}}, nil
}
