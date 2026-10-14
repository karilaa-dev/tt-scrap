package tiktokapp

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"

	"github.com/emmansun/gmsm/sm3"
)

var apkSignKey = mustHex("c02f250f86cc4f198d5706398d292a8b74169aba61affe7cba02e4a3b5198163")
var sdkAESKey = mustHex("b8d72ddec05142948bbf2dc81d63759c")
var sdkAESIV = mustHex("d6c3969582f9ac5313d39c180b54a2bc")
var sdkXTEAKey = mustHex("782399bdfacedead3230313030343034")
var ttSalt = mustHex("4dd4c2e6b83162090e52b3c7a6733ba41cb2462b829ab58a196b39db57177524f49baf7f08e8d68d26a72e37c1a95a2f1f05a51892aef2949732b62a38aadd58")

const maxSDKPayload = 16 << 20

func mustHex(s string) []byte {
	b, e := hex.DecodeString(s)
	if e != nil {
		panic(e)
	}
	return b
}
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return b
}
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}
func padded(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(bytes.Clone(b), bytes.Repeat([]byte{byte(n)}, n)...)
}
func aesCBC(b, key, iv []byte, decrypt bool) ([]byte, error) {
	if len(b)%16 != 0 || len(iv) != 16 {
		return nil, errors.New("invalid AES block size")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	o := make([]byte, len(b))
	if decrypt {
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(o, b)
	} else {
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(o, b)
	}
	return o, nil
}

func ttEncrypt(payload, seed []byte) ([]byte, error) {
	if len(seed) != 32 {
		return nil, errors.New("TTEncrypt requires a 32-byte seed")
	}
	h := sha512.Sum512(seed)
	material := sha512.Sum512(concat(h[:], ttSalt))
	digest := sha512.Sum512(payload)
	enc, e := aesCBC(padded(concat(digest[:], payload), 16), material[:16], material[16:32], false)
	if e != nil {
		return nil, e
	}
	return concat(mustHex("746305100000"), seed, enc), nil
}
func encryptRegistration(payload any) ([]byte, error) {
	plain, e := json.Marshal(payload)
	if e != nil {
		return nil, e
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, e = gz.Write(plain); e != nil {
		return nil, e
	}
	if e = gz.Close(); e != nil {
		return nil, e
	}
	compressed := b.Bytes()
	copy(compressed[:10], mustHex("1f8b0800000000000000"))
	return ttEncrypt(compressed, randomBytes(32))
}
func xteaBlock(in, key []byte, rounds uint32, decrypt bool) []byte {
	first, second := binary.BigEndian.Uint32(in), binary.BigEndian.Uint32(in[4:])
	var sum uint32
	const delta uint32 = 0x9e3779b9
	if decrypt {
		sum = rounds * delta
	}
	mix := func(word, total, index uint32) uint32 {
		return (((word << 4) ^ (word >> 5)) + word) ^ (total + binary.BigEndian.Uint32(key[index*4:]))
	}
	for r := uint32(0); r < rounds; r++ {
		if decrypt {
			second -= mix(first, sum, (sum>>11)&3)
			sum -= delta
			first -= mix(second, sum, sum&3)
		} else {
			first += mix(second, sum, sum&3)
			sum += delta
			second += mix(first, sum, (sum>>11)&3)
		}
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, first)
	binary.BigEndian.PutUint32(out[4:], second)
	return out
}
func xteaCBC(in, key, iv []byte, decrypt bool) ([]byte, error) {
	if len(in)%8 != 0 || len(key) != 16 || len(iv) != 8 {
		return nil, errors.New("invalid SDK XTEA lengths")
	}
	previous := bytes.Clone(iv)
	rounds := 32 + (binary.LittleEndian.Uint32(iv)%5)*8
	out := make([]byte, len(in))
	for off := 0; off < len(in); off += 8 {
		current := in[off : off+8]
		if decrypt {
			decoded := xteaBlock(current, key, rounds, true)
			for i := range 8 {
				out[off+i] = decoded[i] ^ previous[i]
			}
			copy(previous, current)
		} else {
			mixed := make([]byte, 8)
			for i := range 8 {
				mixed[i] = current[i] ^ previous[i]
			}
			previous = xteaBlock(mixed, key, rounds, false)
			copy(out[off:], previous)
		}
	}
	return out, nil
}
func encryptSDKPayload(payload, nonce []byte) ([]byte, error) {
	if len(payload) > maxSDKPayload || len(nonce) != 4 {
		return nil, errors.New("invalid SDK payload or nonce")
	}
	header := make([]byte, 5)
	header[0] = (randomBytes(1)[0] & 0xfc) | 1
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	var b bytes.Buffer
	z, e := zlib.NewWriterLevel(&b, 1)
	if e != nil {
		return nil, e
	}
	if _, e = z.Write(payload); e != nil {
		return nil, e
	}
	if e = z.Close(); e != nil {
		return nil, e
	}
	used := len(header) + b.Len()
	inner, e := xteaCBC(concat(header, b.Bytes(), make([]byte, 8+(8-used%8)%8)), sdkXTEAKey, concat(nonce, mustHex("27042020")), false)
	if e != nil {
		return nil, e
	}
	envelope := concat([]byte{inner[0] ^ 3}, inner, nonce)
	n := 16 - len(envelope)%16
	pad := randomBytes(n)
	pad[n-1] = byte(n)
	return aesCBC(concat(envelope, pad), sdkAESKey, sdkAESIV, false)
}
func decryptSDKPayload(payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload)%16 != 0 || len(payload) > maxSDKPayload {
		return nil, errors.New("invalid SDK encrypted payload length")
	}
	p, e := aesCBC(payload, sdkAESKey, sdkAESIV, true)
	if e != nil {
		return nil, e
	}
	pad := int(p[len(p)-1])
	if pad < 1 || pad > 16 {
		return nil, errors.New("invalid SDK outer padding")
	}
	env := p[:len(p)-pad]
	if len(env) < 13 || len(env)%8 != 5 || env[0] != (env[1]^3) {
		return nil, errors.New("invalid SDK payload envelope")
	}
	inner, e := xteaCBC(env[1:len(env)-4], sdkXTEAKey, concat(env[len(env)-4:], mustHex("27042020")), true)
	if e != nil {
		return nil, e
	}
	off := int(inner[0] & 3)
	if off == 0 {
		off = 4
	}
	if off+4 > len(inner) {
		return nil, errors.New("truncated SDK header")
	}
	size := binary.LittleEndian.Uint32(inner[off:])
	if size > maxSDKPayload {
		return nil, errors.New("SDK decompressed payload too large")
	}
	reader, e := zlib.NewReader(bytes.NewReader(inner[off+4:]))
	if e != nil {
		return nil, e
	}
	defer reader.Close()
	plain, e := io.ReadAll(io.LimitReader(reader, int64(size)+1))
	if e != nil {
		return nil, e
	}
	if len(plain) != int(size) {
		return nil, errors.New("SDK decompressed payload length mismatch")
	}
	return plain, nil
}
func argusXORMask(seed []byte) []byte {
	first := uint32(seed[2])
	v := ^((first >> 5) ^ (uint32(seed[3]) | (first << 11)) ^ first)
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, v)
	return out
}
func simon(in, key []byte) []byte {
	var keys [72]uint64
	for i := range 4 {
		keys[i] = binary.LittleEndian.Uint64(key[i*8:])
	}
	for r := 4; r < 72; r++ {
		mixed := bits.RotateLeft64(keys[r-1], -3) ^ keys[r-3]
		mixed ^= bits.RotateLeft64(mixed, -1)
		keys[r] = (^keys[r-4]) ^ mixed ^ ((uint64(0x3dc94c3a046d678b) >> ((r - 4) % 62)) & 1) ^ 3
	}
	out := make([]byte, len(in))
	for off := 0; off < len(in); off += 16 {
		left, right := binary.LittleEndian.Uint64(in[off:]), binary.LittleEndian.Uint64(in[off+8:])
		for _, k := range keys {
			left, right = right, left^(bits.RotateLeft64(right, 1)&bits.RotateLeft64(right, 8))^bits.RotateLeft64(right, 2)^k
		}
		binary.LittleEndian.PutUint64(out[off:], left)
		binary.LittleEndian.PutUint64(out[off+8:], right)
	}
	return out
}
func encryptArgusEnvelope(payload, seed, flags, prefix, padding []byte) (string, error) {
	if len(seed) != 4 || len(flags) != 8 || len(prefix) != 9 {
		return "", errors.New("invalid Argus material")
	}
	h := sm3.Sum(concat(apkSignKey, seed, apkSignKey))
	mixed := concat(flags, simon(padded(payload, 16), h[:]))
	mask := argusXORMask(seed)
	for i := range mixed {
		mixed[i] ^= mask[i%4]
	}
	for i, j := 0, len(mixed)-1; i < j; i, j = i+1, j-1 {
		mixed[i], mixed[j] = mixed[j], mixed[i]
	}
	plain := concat(prefix, mixed, seed[2:])
	n := 16 - len(plain)%16
	if padding == nil {
		padding = randomBytes(n)
		padding[n-1] = byte(n)
	}
	if len(padding) != n || int(padding[n-1]) != n {
		return "", errors.New("invalid Argus padding")
	}
	key, iv := md5.Sum(apkSignKey[:16]), md5.Sum(apkSignKey[16:])
	enc, e := aesCBC(concat(plain, padding), key[:], iv[:], false)
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(concat(seed[:2], enc)), nil
}
func ladon(timestamp int64, seed []byte) (string, error) {
	if timestamp < 0 || len(seed) != 4 {
		return "", errors.New("invalid Ladon timestamp or seed")
	}
	h := md5.Sum(concat(seed, []byte("1233")))
	material := []byte(hex.EncodeToString(h[:]))
	key, first, second, third := binary.LittleEndian.Uint64(material), binary.LittleEndian.Uint64(material[8:]), binary.LittleEndian.Uint64(material[16:]), binary.LittleEndian.Uint64(material[24:])
	keys := []uint64{key}
	for r := range 33 {
		next := (bits.RotateLeft64(first, -8) + key) ^ uint64(r)
		key = next ^ bits.RotateLeft64(key, 3)
		first, second, third = second, third, next
		keys = append(keys, key)
	}
	in := padded([]byte(fmt.Sprintf("%d-2142840551-1233", timestamp)), 16)
	out := make([]byte, 4+len(in))
	copy(out, seed)
	for off := 0; off < len(in); off += 16 {
		left, right := binary.LittleEndian.Uint64(in[off:]), binary.LittleEndian.Uint64(in[off+8:])
		for _, k := range keys {
			right = k ^ (left + bits.RotateLeft64(right, -8))
			left = right ^ bits.RotateLeft64(left, 3)
		}
		binary.LittleEndian.PutUint64(out[off+4:], left)
		binary.LittleEndian.PutUint64(out[off+12:], right)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}
func dynamicProof(version int, nonce uint32, query string, bodyDigest []byte) ([]byte, error) {
	if len(bodyDigest) != 16 || version < 1 || version > 8 {
		return nil, errors.New("invalid dynamic proof parameters")
	}
	marker := []byte{0, 0, 0, 1}
	inputs := [][]byte{marker, bodyDigest, []byte(query)}
	var words [3]uint32
	var nb [4]byte
	binary.BigEndian.PutUint32(nb[:], nonce)
	keyhash := md5.Sum(nb[:])
	key := []byte(hex.EncodeToString(keyhash[:]))
	for i, in := range inputs {
		hash := md5.Sum(in)
		switch version {
		case 1:
			words[i] = binary.LittleEndian.Uint32(hash[:])
		case 2:
			for j := range hash {
				hash[j] = (hash[j] << 4) | (hash[j] >> 4)
			}
			words[i] = binary.LittleEndian.Uint32(hash[:])
		case 3:
			words[i] = binary.LittleEndian.Uint32(hash[:]) ^ 0x5a5a5a5a
		case 4:
			words[i] = bits.Reverse32(binary.BigEndian.Uint32(hash[:]))
		case 5:
			h := sm3.Sum(in)
			words[i] = binary.LittleEndian.Uint32(h[28:])
		case 6:
			block, e := aes.NewCipher(key[:16])
			if e != nil {
				return nil, e
			}
			encrypted := padded(in, 16)
			//lint:ignore SA1019 signing variant 6 is defined by TikTok's SDK as AES-OFB.
			cipher.NewOFB(block, key[16:]).XORKeyStream(encrypted, encrypted)
			words[i] = binary.LittleEndian.Uint32(encrypted[len(encrypted)-4:])
		}
	}
	if version == 7 {
		stream, e := rc4.NewCipher(key)
		if e != nil {
			return nil, e
		}
		enc := make([]byte, 4)
		stream.XORKeyStream(enc, marker)
		body := md5.Sum(bodyDigest)
		q := sha256.Sum256([]byte(query))
		words = [3]uint32{bits.Reverse32(binary.BigEndian.Uint32(enc)), binary.LittleEndian.Uint32(body[12:]), binary.LittleEndian.Uint32(q[:]) ^ 0x5a5a5a5a}
	}
	if version == 8 {
		h := sha256.Sum256(marker)
		for j := range h {
			h[j] = (h[j] << 4) | (h[j] >> 4)
		}
		q := sha1.Sum([]byte(query))
		words = [3]uint32{binary.LittleEndian.Uint32(h[:]), bits.Reverse32(crc32.ChecksumIEEE(bodyDigest)), binary.LittleEndian.Uint32(q[:]) ^ 0x5a5a5a5a}
	}
	out := make([]byte, 12)
	for i, w := range words {
		binary.BigEndian.PutUint32(out[i*4:], w^nonce)
	}
	return out, nil
}
