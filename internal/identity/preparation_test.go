package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func TestPreparationPacerSpacesStartsAndHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var pace preparationPacer
		start := time.Now()
		for i := 0; i < 3; i++ {
			if err := pace.wait(context.Background(), preparationInterval); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed != time.Duration(i)*10*time.Second {
				t.Fatalf("start %d at %s", i, elapsed)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := pace.wait(ctx, preparationInterval); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestPreparationFillsEachProxyInParallelAndReusesPersistedIdentities(t *testing.T) {
	cfg, db := testDatabase(t)
	p := openTestPool(t, cfg, "http://same.example:8001", "http://same.example:8002", "http://other.example:8001")
	p.preparationDelay = 100 * time.Millisecond
	seed, err := os.ReadFile("testdata/sdk-seed.bin")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	starts := map[*http.Client][]time.Time{}
	initialized := map[*http.Client]int{}
	registrations, seeds := 0, 0
	allStarted := make(chan struct{})
	p.clientFactory = func(s *tiktokapp.Session, c *http.Client) *tiktokapp.Client {
		mu.Lock()
		slowSetup := initialized[c]%2 == 0
		initialized[c]++
		mu.Unlock()
		if slowSetup {
			// Variable request setup must not consume the next attempt's gap.
			time.Sleep(2 * p.preparationDelay)
		}
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/service/2/device_register/":
				mu.Lock()
				starts[c] = append(starts[c], time.Now())
				registrations++
				number := registrations
				if len(starts) == 3 && len(starts[c]) == 1 {
					close(allStarted)
				}
				mu.Unlock()
				select {
				case <-allStarted:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return fixtureResponse(200, fmt.Appendf(nil, `{"device_id_str":"7500000000000000%03d","install_id_str":"7500000000000001%03d"}`, number, number)), nil
			case "/ms/get_seed":
				mu.Lock()
				seeds++
				mu.Unlock()
				return fixtureResponse(200, seed), nil
			default:
				t.Errorf("preparation fetched media or metadata: %s", r.URL.Path)
				return nil, errors.New("unexpected URL")
			}
		}))
	}
	var callers sync.WaitGroup
	for range 12 {
		callers.Go(func() {
			if _, err := p.StartPreparation(); err != nil {
				t.Error(err)
			}
		})
	}
	callers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err, p.PreparationStatus())
	}
	status := p.PreparationStatus()
	if status.State != "completed" || status.Target != 30 || status.Ready != 30 || status.Failed != 0 {
		t.Fatal(status)
	}
	mu.Lock()
	if registrations != 30 || seeds != 30 || len(starts) != 3 {
		t.Fatal(registrations, seeds, len(starts))
	}
	for _, times := range starts {
		if len(times) != 10 {
			t.Fatal("wrong per-proxy count", len(times))
		}
		for i := 1; i < len(times); i++ {
			if times[i].Sub(times[i-1]) < p.preparationDelay {
				t.Fatalf("registration pacing bypassed: gap %s, minimum %s", times[i].Sub(times[i-1]), p.preparationDelay)
			}
		}
	}
	mu.Unlock()
	initialWrites := p.Stats().Writes
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Writes != initialWrites {
		t.Fatal("prepared identities were rewritten")
	}
	var count, assignments int
	if err := db.QueryRow(ctx, "SELECT count(*),sum(assignment_count) FROM "+p.identitiesTable).Scan(&count, &assignments); err != nil || count != 30 || assignments != 0 {
		t.Fatal("preparation changed request balancing", count, assignments, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestPool(t, cfg)
	reopened.clientFactory = func(*tiktokapp.Session, *http.Client) *tiktokapp.Client {
		t.Error("restart bootstrapped a fresh identity")
		return nil
	}
	if err := reopened.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if status := reopened.PreparationStatus(); status.Ready != 30 || status.State != "completed" {
		t.Fatal(status)
	}
}

func TestPreparationRefreshesExpiredSeedsWithoutRecreatingDevices(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	p.preparationDelay = time.Millisecond
	fixture := newProtocolFixture(t, "", 200)
	fixture.install(p)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, e := range p.entries {
		e.session.SDK.FetchedAt = time.Now().Add(-4 * time.Hour)
		e.signedAt = e.session.SDK.FetchedAt
		e.client = nil
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.registrations != 10 || fixture.seeds != 20 {
		t.Fatal("expired seeds recreated devices", fixture)
	}
	l := mustAcquire(t, p, "")
	if !l.recover(&tiktokapp.UpstreamError{Code: "signature_expired", Recovery: tiktokapp.RecoveryRefresh}) {
		t.Fatal("signature recovery was not accepted")
	}
	if status := p.PreparationStatus(); status.Ready != 9 {
		t.Fatal("preparation still counts invalidated signing state as ready", status)
	}
	finishLease(t, l, nil)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.registrations != 10 || fixture.seeds != 21 {
		t.Fatal("recovery did not retain and prepare the existing device", fixture)
	}
}

func TestPreparationFailureIsRetryableAndCloseCancelsWorkers(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	p.preparationDelay = time.Millisecond
	var attempts int
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) { attempts++; return nil, errors.New("fixture failure") }))
	}
	if err := p.Prepare(context.Background()); err == nil {
		t.Fatal("failed registration reported readiness")
	}
	status := p.PreparationStatus()
	if status.State != "failed" || status.Ready != 0 || status.Failed != 10 || attempts != 30 {
		t.Fatal(status, attempts)
	}
	fixture := newProtocolFixture(t, "", 200)
	fixture.install(p)
	for _, e := range p.entries {
		e.client = nil
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Identities != 10 || p.PreparationStatus().Ready != 10 {
		t.Fatal("retry duplicated identities")
	}
	for _, e := range p.entries {
		e.session.SDK = nil
		e.signedAt = time.Time{}
		e.client = nil
	}
	started := make(chan struct{})
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}))
	}
	if _, err := p.StartPreparation(); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if p.PreparationStatus().State != "failed" {
		t.Fatal("shutdown left a running job")
	}
}

func TestPreparationRetriesAfterIdentityCooldown(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	p.preparationDelay = 10 * time.Millisecond
	fixture := newProtocolFixture(t, "", 200)
	failed := false
	p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
		return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
			if !failed {
				failed = true
				return nil, &tiktokapp.UpstreamError{Code: "upstream_rate_limited", RetryAfter: 150 * time.Millisecond}
			}
			return fixture.do(r)
		}))
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal("retry skipped its pacing wait and hit the same cooldown", err, p.PreparationStatus())
	}
	if fixture.registrations != 10 {
		t.Fatal("cooldown prevented preparation", fixture.registrations)
	}
}

func TestPreparationWaitsForProxyCooldownAndShutdownCancelsWait(t *testing.T) {
	cfg, _ := testDatabase(t)
	p := openTestPool(t, cfg, "")
	p.preparationDelay = time.Millisecond
	fixture := newProtocolFixture(t, "", 200)
	fixture.install(p)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	e := p.proxies[0].slots[0]
	e.session.SDK, e.signedAt = nil, time.Time{}
	e.proxy.health.CooldownUntil = time.Now().Add(time.Hour).UnixMilli()
	p.mu.Unlock()
	if _, err := p.StartPreparation(); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	done := p.preparation.done
	p.mu.Unlock()
	select {
	case <-done:
		t.Fatal("preparation consumed attempts during proxy cooldown")
	case <-time.After(30 * time.Millisecond):
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel cooldown wait")
	}
	if fixture.registrations != 10 || fixture.seeds != 10 {
		t.Fatal("preparation contacted a cooling proxy", fixture.registrations, fixture.seeds)
	}
}
