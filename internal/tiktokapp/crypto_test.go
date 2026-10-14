package tiktokapp

import (
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
)

type goldenHex struct {
	Payload   string
	Seed      string
	Prefix    string
	Padding   string
	Expected  string
	IV        string
	Timestamp int64
}

func TestOriginalNodeCryptoVectors(t *testing.T) {
	raw, e := os.ReadFile("testdata/node-crypto-goldens.json")
	if e != nil {
		t.Fatal(e)
	}
	var vectors struct {
		Dynamic []struct {
			Nonce    uint32
			Query    string
			Body     string
			Version  int
			Expected string
		}
		Ladon     goldenHex
		Argus     goldenHex
		TTEncrypt goldenHex
		XTEA      []goldenHex
		SDK       []goldenHex
	}
	if e = json.Unmarshal(raw, &vectors); e != nil {
		t.Fatal(e)
	}
	for _, v := range vectors.Dynamic {
		out, e := dynamicProof(v.Version, v.Nonce, v.Query, mustHex(v.Body))
		if e != nil || hex.EncodeToString(out) != v.Expected {
			t.Fatalf("dynamic proof v%d nonce%d: %x, %v, expected%s", v.Version, v.Nonce, out, e, v.Expected)
		}
	}
	lad, e := ladon(vectors.Ladon.Timestamp, mustHex(vectors.Ladon.Seed))
	if e != nil || lad != vectors.Ladon.Expected {
		t.Fatalf("Ladon differs: %s, %v", lad, e)
	}
	a := vectors.Argus
	argus, e := encryptArgusEnvelope(mustHex(a.Payload), mustHex(a.Seed), mustHex("0004000000000000"), mustHex(a.Prefix), mustHex(a.Padding))
	if e != nil || argus != a.Expected {
		t.Fatalf("Argus differs: %s, %v", argus, e)
	}
	tt := vectors.TTEncrypt
	enc, e := ttEncrypt(mustHex(tt.Payload), mustHex(tt.Seed))
	if e != nil || hex.EncodeToString(enc) != tt.Expected {
		t.Fatalf("TTEncrypt differs: %x, %v", enc, e)
	}
	for index, x := range vectors.XTEA {
		enc, e := xteaCBC(mustHex(x.Payload), sdkXTEAKey, mustHex(x.IV), false)
		if e != nil || hex.EncodeToString(enc) != x.Expected {
			t.Fatalf("XTEA branch%d differs:%x,%v", index, enc, e)
		}
		plain, e := xteaCBC(enc, sdkXTEAKey, mustHex(x.IV), true)
		if e != nil || hex.EncodeToString(plain) != x.Payload {
			t.Fatalf("XTEA decrypt branch%d:%x,%v", index, plain, e)
		}
	}
	for index, s := range vectors.SDK {
		plain, e := decryptSDKPayload(mustHex(s.Expected))
		if e != nil || hex.EncodeToString(plain) != s.Payload {
			t.Fatalf("Original Node SDK envelope branch%d decode:%x,%v", index, plain, e)
		}
	}
}
func TestRegistrationIndependentDecryption(t *testing.T) {
	payload := map[string]any{"magic_tag": "ss_app_log", "header": map[string]any{"clientudid": "unit-test"}, "_gen_time": float64(1800000000000)}
	encrypted, e := encryptRegistration(payload)
	if e != nil {
		t.Fatal(e)
	}
	if hex.EncodeToString(encrypted[:6]) != "746305100000" {
		t.Fatal("registration envelope prefix")
	}
	seedHash := sha512.Sum512(encrypted[6:38])
	material := sha512.Sum512(concat(seedHash[:], ttSalt))
	plain, e := aesCBC(encrypted[38:], material[:16], material[16:32], true)
	if e != nil {
		t.Fatal(e)
	}
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	compressed := plain[64:]
	sum := sha512.Sum512(compressed)
	if !bytes.Equal(sum[:], plain[:64]) {
		t.Fatal("registration payload digest mismatch")
	}
	if hex.EncodeToString(compressed[:10]) != "1f8b0800000000000000" {
		t.Fatal("gzip header differs from Java")
	}
	reader, e := gzip.NewReader(bytes.NewReader(compressed))
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close()
	data, e := io.ReadAll(reader)
	if e != nil {
		t.Fatal(e)
	}
	var decoded map[string]any
	if e = json.Unmarshal(data, &decoded); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(decoded, payload) {
		t.Fatalf("registration JSON mismatch: %v", decoded)
	}
}
func TestSDKRoundTripsAndRejectsOversize(t *testing.T) {
	for i := range uint32(5) {
		nonce := make([]byte, 4)
		binary.LittleEndian.PutUint32(nonce, i)
		payload := []byte("zero\x00 UTF-8 café")
		encrypted, e := encryptSDKPayload(payload, nonce)
		if e != nil {
			t.Fatal(e)
		}
		plain, e := decryptSDKPayload(encrypted)
		if e != nil || !bytes.Equal(plain, payload) {
			t.Fatalf("branch%d: %v", i, e)
		}
	}
	for _, in := range [][]byte{nil, {1}, make([]byte, 16), make([]byte, maxSDKPayload+16)} {
		if _, e := decryptSDKPayload(in); e == nil {
			t.Fatal("accepted invalid SDK envelope")
		}
	}
}
func TestProtobufExactUint64AndBounds(t *testing.T) {
	fields := []protoField{pi(1, 150), ps(2, "abc"), pi(3, ^uint64(0))}
	encoded := encodeProto(fields...)
	if hex.EncodeToString(encoded) != "089601120361626318ffffffffffffffffff01" {
		t.Fatalf("protobuf=%x", encoded)
	}
	decoded, e := decodeProto(encoded)
	if e != nil || !reflect.DeepEqual(decoded, fields) {
		t.Fatalf("protobuf decode:%v %v", decoded, e)
	}
	for _, input := range []string{"120561", "ffffffffffffffffffffff", "080180", "0000", "0b00"} {
		if _, e := decodeProto(mustHex(input)); e == nil {
			t.Fatalf("accepted malformed protobuf%s", input)
		}
	}
}
