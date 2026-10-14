package tiktokapp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestCookieSnapshotTracksChangesDeletionsAndExpiry(t *testing.T) {
	u, _ := url.Parse("https://api19-va.tiktokv.com/")
	p := newPersistentJar("")
	p.accept(u, []*http.Cookie{{Name: "keep", Value: "one", Path: "/", Secure: true}, {Name: "remove", Value: "gone", Path: "/", Secure: true}, {Name: "expire", Value: "soon", Path: "/", Secure: true, Expires: time.Now().Add(30 * time.Millisecond)}})
	first := p.serialize()
	if first != p.serialize() {
		t.Fatal("unchanged cookies changed snapshot")
	}
	p.accept(u, []*http.Cookie{{Name: "keep", Value: "two", Path: "/", Secure: true}, {Name: "remove", MaxAge: -1, Path: "/", Secure: true}})
	time.Sleep(40 * time.Millisecond)
	var stored savedJar
	if err := json.Unmarshal([]byte(p.serialize()), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Cookies) != 1 || stored.Cookies[0].Key != "keep" || stored.Cookies[0].Value != "two" {
		t.Fatal("stale cookie snapshot", stored.Cookies)
	}
	restored := newPersistentJar(p.serialize())
	got := restored.jar.Cookies(u)
	if len(got) != 1 || got[0].Name != "keep" || got[0].Value != "two" {
		t.Fatal("snapshot did not survive reload", got)
	}
	p.accept(u, []*http.Cookie{{Name: "expireOnly", Value: "gone", Path: "/", Secure: true, Expires: time.Now().Add(30 * time.Millisecond)}})
	_ = p.serialize()
	time.Sleep(40 * time.Millisecond)
	if err := json.Unmarshal([]byte(p.serialize()), &stored); err != nil || len(stored.Cookies) != 1 {
		t.Fatal("unchanged snapshot retained expired cookie", err, stored.Cookies)
	}
}
