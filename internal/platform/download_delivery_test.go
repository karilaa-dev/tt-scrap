package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type downloadContract string

const (
	rawDownload      downloadContract = "Download"
	identityDownload downloadContract = "WithIdentity"
	deliveryDownload downloadContract = "WithDeliveryIdentity"
)

func (c downloadContract) withIdentity(d *Downloader) func(context.Context, string, func(core.DownloadFunc) error) error {
	if c == deliveryDownload {
		return d.WithDeliveryIdentity
	}
	return d.WithIdentity
}

func (c downloadContract) download(d *Downloader, ctx context.Context, asset core.AssetContext, maximum int64, started func(*core.Downloaded)) (got *core.Downloaded, err error) {
	if c == rawDownload {
		return d.Download(ctx, asset, maximum)
	}
	err = c.withIdentity(d)(ctx, asset.IdentityID, func(download core.DownloadFunc) error {
		got, err = download(ctx, asset, maximum, started)
		return err
	})
	return got, err
}

func (c downloadContract) sum(data []byte) string {
	if c == deliveryDownload {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type observedDigest struct {
	hash.Hash
	written *atomic.Int64
}

func (h *observedDigest) Write(p []byte) (int, error) {
	h.written.Add(int64(len(p)))
	return h.Hash.Write(p)
}

type deliveryBackend struct {
	core.AppBackend
	withMedia func(context.Context, string, func(core.OpenAppMedia) error) error
}

func (b *deliveryBackend) WithMedia(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
	return b.withMedia(ctx, id, run)
}

func TestDeliveryIdentityOwnershipAndRecovery(t *testing.T) {
	for _, contract := range []downloadContract{identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			data := largeVideo(513)
			var leases, opens int
			backend := &deliveryBackend{withMedia: func(ctx context.Context, id string, run func(core.OpenAppMedia) error) error {
				leases++
				if id != "fixture" {
					t.Fatalf("leased identity %q", id)
				}
				open := func(context.Context, string, http.Header) (*http.Response, error) {
					opens++
					return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
				}
				if err := run(open); err != nil {
					return err
				}
				return run(open)
			}}
			cfg := testConfig()
			cfg.TempDir = t.TempDir()
			d := NewDownloader(cfg, backend)
			defer d.Close()
			var got, previous *core.Downloaded
			defer func() { got.Cleanup() }()
			ctx := context.Background()
			err := contract.withIdentity(d)(ctx, "fixture", func(download core.DownloadFunc) error {
				previous = got
				got.Cleanup()
				asset := core.AssetContext{Kind: "video", Filename: "video.mp4", IdentityID: "another", UpstreamURL: "http://fixture.invalid/media"}
				rejected, err := download(ctx, asset, 0, nil)
				rejected.Cleanup()
				if err == nil || rejected != nil {
					t.Fatal("accepted media owned by another identity")
				}
				asset.IdentityID = "fixture"
				got, err = download(ctx, asset, 0, nil)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if leases != 1 || opens != 2 || previous == nil || previous.Data.Err() == nil || !bytes.Equal(got.Data.Bytes(), data) || got.SHA256 != contract.sum(data) {
				t.Fatal("identity recovery changed ownership, bytes, checksum, or source lifetime", leases, opens)
			}
			got.Cleanup()
			if d.MemoryInUse() != 0 {
				t.Fatal("identity recovery retained source RAM")
			}
		})
	}
}

func TestDeliveryIdentityPropagatesLeaseFailure(t *testing.T) {
	want := errors.New("fixture lease save failed")
	backend := &deliveryBackend{withMedia: func(_ context.Context, _ string, run func(core.OpenAppMedia) error) error {
		if err := run(nil); err != nil {
			return err
		}
		return want
	}}
	d := NewDownloader(testConfig(), backend)
	defer d.Close()
	for _, contract := range []downloadContract{identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			called := false
			err := contract.withIdentity(d)(context.Background(), "fixture", func(core.DownloadFunc) error {
				called = true
				return nil
			})
			if !called || !errors.Is(err, want) {
				t.Fatal("lease failure lost", called, err)
			}
		})
	}
}
