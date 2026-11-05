package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func TestOptionalMediaFailureDoesNotReplaySuccessfulPreparation(t *testing.T) {
	for _, mode := range []string{"operation_optional", "operation_required", "legacy_optional"} {
		for _, failure := range []string{"invalid signature", "invalid device_id"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				cfg, _ := testDatabase(t)
				p := openTestPool(t, cfg, "")
				active := mustAcquire(t, p, "")
				id := active.entry.id
				active.finish(nil)
				fixture := newProtocolFixture(t, "", 0)
				mainCalls, coverCalls, preparations := 0, 0, 0
				p.clientFactory = func(s *tiktokapp.Session, _ *http.Client) *tiktokapp.Client {
					return tiktokapp.NewClient(s, fixtureDoer(func(r *http.Request) (*http.Response, error) {
						switch r.URL.Path {
						case "/main":
							mainCalls++
							return fixtureResponse(200, []byte{255, 216, 255, 0}), nil
						case "/aweme/v1/play/":
							coverCalls++
							if coverCalls == 1 {
								return fixtureResponse(403, []byte(`{"status_code":1,"status_msg":"`+failure+`"}`)), nil
							}
							return fixtureResponse(200, []byte{255, 216, 255, 0}), nil
						default:
							return fixture.do(r)
						}
					}))
				}
				op := core.NewOperation(1)
				ctx := context.Background()
				if mode != "legacy_optional" {
					ctx = core.WithOperation(ctx, op)
				}
				err := p.WithMedia(ctx, id, func(open core.OpenAppMedia) error {
					preparations++
					response, err := open(ctx, "https://cdn.example/main", nil)
					if err != nil {
						return err
					}
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if err != nil {
						return err
					}
					coverCtx := ctx
					if mode != "operation_required" {
						coverCtx = core.WithoutOperationRetries(coverCtx)
					}
					response, err = open(coverCtx, "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
					if err != nil {
						if mode == "operation_required" {
							return err
						}
						return nil // A missing thumbnail does not fail preparation.
					}
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					return err
				})
				want := 2
				if mode == "operation_optional" {
					want = 1
				}
				wantAttempt := want
				if mode == "legacy_optional" {
					wantAttempt = 1
				}
				if err != nil || preparations != want || mainCalls != want || coverCalls != want || op.Attempt() != wantAttempt {
					t.Fatalf("optional failure changed preparation: err=%v runs=%d main=%d cover=%d attempt=%d", err, preparations, mainCalls, coverCalls, op.Attempt())
				}
				if mode == "operation_optional" && (p.entries[id].health.LastRecovery != nil || p.entries[id].health.Failures != 0) {
					t.Fatal("optional failure changed identity health", p.entries[id].health)
				}
			})
		}
	}
}

type failingOptionalBody struct{ err error }

func (b failingOptionalBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingOptionalBody) Close() error             { return nil }

func TestOptionalBodyFailureDoesNotMaskRequiredRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(core.WithOperation(context.Background(), core.NewOperation(1)))
	defer cancel()
	optionalFailure := &tiktokapp.UpstreamError{Code: "signature_rejected", Recovery: tiktokapp.RecoveryRefresh}
	requiredFailure := &tiktokapp.UpstreamError{Code: "identity_rejected", Recovery: tiktokapp.RecoveryRecreate}
	batch := &mediaBatch{ctx: ctx, cancel: cancel, bodies: make(map[*leasedBody]struct{}), openMedia: func(_ context.Context, address string, _ http.Header) (*http.Response, error) {
		if address == "required" {
			return nil, requiredFailure
		}
		return &http.Response{Body: failingOptionalBody{optionalFailure}}, nil
	}}
	defer batch.close()
	response, err := batch.open(core.WithoutOperationRetries(ctx), "optional", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if !errors.Is(err, optionalFailure) || batch.err != nil {
		t.Fatal("optional body failure entered batch recovery", err, batch.err)
	}
	_, _ = batch.open(ctx, "required", nil)
	if !errors.Is(batch.err, requiredFailure) {
		t.Fatal("required recovery was suppressed", batch.err)
	}
}
