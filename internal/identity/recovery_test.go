package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

type fixtureDoer func(*http.Request) (*http.Response, error)

func (f fixtureDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureResponse(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, ContentLength: int64(len(body)), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

type protocolFixture struct {
	t                             *testing.T
	seed                          []byte
	failure                       string
	status                        int
	registrations, seeds, details int
	deviceIDs                     []string
}

func newProtocolFixture(t *testing.T, failure string, status int) *protocolFixture {
	t.Helper()
	seed, e := os.ReadFile("testdata/sdk-seed.bin")
	if e != nil {
		t.Fatal(e)
	}
	return &protocolFixture{t: t, seed: seed, failure: failure, status: status}
}
func (f *protocolFixture) do(r *http.Request) (*http.Response, error) {
	switch r.URL.Path {
	case "/service/2/device_register/":
		f.registrations++
		return fixtureResponse(200, fmt.Appendf(nil, `{"device_id_str":"750000000000000000%d","install_id_str":"750000000000000010%d"}`, f.registrations, f.registrations)), nil
	case "/ms/get_seed":
		f.seeds++
		return fixtureResponse(200, f.seed), nil
	case "/aweme/v1/aweme/detail/":
		f.details++
		f.deviceIDs = append(f.deviceIDs, r.URL.Query().Get("device_id"))
		if f.details == 1 && f.failure != "" {
			body, _ := json.Marshal(map[string]any{"status_code": 1, "status_msg": f.failure})
			return fixtureResponse(f.status, body), nil
		}
		return fixtureResponse(200, []byte(`{"status_code":0,"aweme_detail":{"aweme_id":"7695962883523464478"}}`)), nil
	}
	f.t.Errorf("unexpected native protocol request:%s", r.URL.Path)
	return nil, errors.New("unexpected fixture URL")
}
func (f *protocolFixture) install(p *Pool) {
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(f.do))
	}
}
func TestBackendRefreshesSeedOnlyForExplicitSignatureFailure(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "http://one.example:8080")
	fixture := newProtocolFixture(t, "signature is expired", 403)
	fixture.install(p)
	result, e := p.Detail(context.Background(), "7695962883523464478")
	if e != nil {
		t.Fatal(e)
	}
	if fixture.registrations != 1 || fixture.seeds != 2 || fixture.details != 2 || fixture.deviceIDs[0] != fixture.deviceIDs[1] {
		t.Fatalf("signature recovery changed identity or skipped seed: %+v", fixture)
	}
	entry := p.entries[result.IdentityID]
	if entry == nil || entry.session.SDK.RequestCount != 1 || entry.health.LastRecovery == nil || *entry.health.LastRecovery != "refresh" {
		t.Fatal("signature recovery state missing")
	}
	persisted(t, p)
	var recovery string
	if e = db.QueryRow(context.Background(), "SELECT health->>'lastRecovery' FROM "+p.identitiesTable+" WHERE id=$1", result.IdentityID).Scan(&recovery); e != nil || recovery != "refresh" {
		t.Fatalf("recovery was not persisted:%s %v", recovery, e)
	}
	if stats := p.Stats(); stats.Checkpoints != 1 || stats.Writes != 1 {
		t.Fatal("bootstrap/retry wrote intermediate database state", stats)
	}
}
func TestBackendRecreatesDeviceInSameProxySlotOnlyWhenRejected(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "http://one.example:8080")
	fixture := newProtocolFixture(t, "invalid device_id", 403)
	fixture.install(p)
	result, e := p.Detail(context.Background(), "7695962883523464478")
	if e != nil {
		t.Fatal(e)
	}
	if fixture.registrations != 2 || fixture.seeds != 2 || fixture.details != 2 || fixture.deviceIDs[0] == fixture.deviceIDs[1] {
		t.Fatalf("device rejection recovery wrong:%+v", fixture)
	}
	entry := p.entries[result.IdentityID]
	if entry == nil || entry.ordinal != 0 || entry.proxy.key != "one.example:8080" || entry.health.LastRecovery == nil || *entry.health.LastRecovery != "recreate" {
		t.Fatal("device recreation changed permanent proxy slot")
	}
	if stats := p.Stats(); stats.Identities != 1 || stats.Registered != 1 || stats.Checkpoints != 1 {
		t.Fatalf("device recreation duplicated pool rows:%+v", stats)
	}
}
func TestBackendDoesNotRecreateOrResignForGenericForbidden(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	fixture := newProtocolFixture(t, "challenge required", 403)
	fixture.install(p)
	_, e := p.Detail(context.Background(), "7695962883523464478")
	var apiErr *core.Error
	if !errors.As(e, &apiErr) || apiErr.Code != "upstream_extraction_error" {
		t.Fatalf("wrong public upstream failure:%v", e)
	}
	if fixture.registrations != 1 || fixture.seeds != 1 || fixture.details != 1 {
		t.Fatal("generic block caused recovery churn")
	}
	for _, entry := range p.entries {
		if entry.health.LastRecovery != nil || entry.health.CooldownUntil <= time.Now().UnixMilli() {
			t.Fatal("blocked identity recovery/cooldown wrong")
		}
	}
}
func TestBackendDoesNotLoopOnRepeatedSignatureRejection(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	fixture := newProtocolFixture(t, "invalid signature", 403)
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/aweme/v1/aweme/detail/" {
				fixture.details++
				return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"invalid signature"}`)), nil
			}
			return fixture.do(r)
		}))
	}
	if _, e := p.Detail(context.Background(), "7695962883523464478"); e == nil {
		t.Fatal("persistent signature rejection succeeded")
	}
	if fixture.registrations != 1 || fixture.seeds != 2 || fixture.details != 2 {
		t.Fatalf("unbounded recovery or device churn:%+v", fixture)
	}
}
func TestProxyNetworkCooldownSurvivesOlderConcurrentSuccess(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	first, second := mustAcquire(t, p, ""), mustAcquire(t, p, "")
	network := &net.DNSError{Err: "fixture network timeout", Name: "example.invalid", IsTimeout: true}
	first.finish(network)
	second.finish(nil)
	proxy := first.entry.proxy
	if proxy.health.Failures != 1 || proxy.health.CooldownUntil <= time.Now().UnixMilli() {
		t.Fatal("older success erased a newer proxy failure")
	}
	persisted(t, p)
	var failures int
	if e := db.QueryRow(context.Background(), "SELECT (health->>'failures')::int FROM "+p.proxiesTable+" WHERE id=$1", proxy.id).Scan(&failures); e != nil || failures != 1 {
		t.Fatal("proxy cooldown persistence was overwritten")
	}
	if l, e := p.acquire(context.Background(), ""); e == nil {
		l.finish(nil)
		t.Fatal("cooled proxy accepted new request")
	}
	p.mu.Lock()
	proxy.health.CooldownUntil = 0
	p.mu.Unlock()
	recovered := mustAcquire(t, p, first.entry.id)
	recovered.finish(nil)
	if proxy.health.Failures != 0 || proxy.health.CooldownUntil != 0 {
		t.Fatal("fresh success failed to restore proxy")
	}
}
func TestOpenMediaKeepsLeaseUntilBodyCloseThenPersistsCookies(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "")
	l := mustAcquire(t, p, "")
	id := l.entry.id
	l.finish(nil)
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			response := fixtureResponse(200, []byte("video fixture"))
			response.Header.Set("Set-Cookie", "cdn=downloaded; Path=/; Secure")
			return response, nil
		}))
	}
	initial := p.Stats().Checkpoints
	response, e := p.OpenMedia(context.Background(), id, "https://video.example.org/file.mp4", nil)
	if e != nil {
		t.Fatal(e)
	}
	if stats := p.Stats(); stats.Busy != 1 || stats.Checkpoints != initial {
		t.Fatal("media operation finished before body")
	}
	body, e := io.ReadAll(response.Body)
	if e != nil || string(body) != "video fixture" {
		t.Fatal("media body changed")
	}
	if p.Stats().Busy != 1 {
		t.Fatal("lease returned before caller closes stream")
	}
	if e = response.Body.Close(); e != nil {
		t.Fatal(e)
	}
	if e = response.Body.Close(); e != nil {
		t.Fatal(e)
	}
	if stats := p.Stats(); stats.Busy != 0 || stats.Checkpoints != initial+1 {
		t.Fatal("media completion did not flush exactly once")
	}
	persisted(t, p)
	var cookies string
	if e = db.QueryRow(context.Background(), "SELECT session->>'cookies' FROM "+p.identitiesTable+" WHERE id=$1", id).Scan(&cookies); e != nil || !bytes.Contains([]byte(cookies), []byte("downloaded")) {
		t.Fatal("media response cookie was not persisted")
	}
}

func TestPoolCloseCancelsQueuedAcquisitionAndActiveOperation(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	active := mustAcquire(t, p, "")
	queued := make(chan error, 1)
	go func() {
		l, e := p.acquire(context.Background(), active.entry.id)
		if e == nil {
			l.finish(nil)
		}
		queued <- e
	}()
	waitStats(t, p, func(s Stats) bool { return s.Queued == 1 })
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-active.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("closing pool did not cancel active operation")
	}
	select {
	case e := <-queued:
		if e == nil {
			t.Fatal("closing pool granted queued identity")
		}
	case <-time.After(time.Second):
		t.Fatal("closing pool did not cancel queue")
	}
	active.finish(context.Canceled)
	select {
	case e := <-closed:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("pool close did not finish after releasing operation")
	}
}
