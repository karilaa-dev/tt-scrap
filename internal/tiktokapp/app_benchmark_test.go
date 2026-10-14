package tiktokapp

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"testing"
	"time"
)

func BenchmarkAppSigning(b *testing.B) {
	s, err := NewSession(&Profile{Model: "Pixel 8", Brand: "google", Manufacturer: "Google", OSVersion: "15", OSAPI: 35, Build: "AP3A.241105.007", Channel: "googleplay", Resolution: "1080*2400", DPI: 420, Language: "en", Region: "US"})
	if err != nil {
		b.Fatal(err)
	}
	s.Identity.Kind, s.Identity.DeviceID, s.Identity.InstallID = "registered", "7500000000000000001", "7500000000000000002"
	now := time.Now()
	sdk := SDKState{Seed: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 98)), Algorithm: 3, FetchedAt: now}
	u, _ := url.Parse("https://api19-va.tiktokv.com/aweme/v1/aweme/detail/")
	q := commonParams(s.Identity, s.Profile, now)
	q.Set("aweme_id", "7695962883523464478")
	u.RawQuery = q.Encode()
	b.ReportAllocs()
	for b.Loop() {
		h, err := signAppRequest(u, nil, s.Identity, s.Profile, sdk, now)
		if err != nil || h.Get("X-Argus") == "" || h.Get("X-Ladon") == "" {
			b.Fatal("signing failed", err)
		}
	}
}
