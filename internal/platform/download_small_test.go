package platform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type smallResponseTransport func(*http.Request) (*http.Response, error)

func (f smallResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type smallResponseBody struct {
	*bytes.Reader
	chunk    int
	final    error
	closed   bool
	closeErr error
	onClose  func()
}

func (b *smallResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p[:min(len(p), b.chunk)])
	if b.Len() == 0 && b.final != nil {
		return n, b.final
	}
	return n, err
}

func (b *smallResponseBody) Close() error {
	b.closed = true
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

func TestDownloadSmallResponseValidation(t *testing.T) {
	for _, contract := range []downloadContract{identityDownload, deliveryDownload} {
		t.Run(string(contract), func(t *testing.T) {
			for _, test := range []struct {
				name                string
				size                int
				declared            int64
				span                string
				maximum, configured int64
				cancel              bool
				closeCancel         bool
				data                []byte
				closeErr            error
				final               error
				want                string
			}{
				{name: "fragmented", size: 513, declared: 513},
				{name: "unsupported_mime", data: []byte("<html>denied</html>"), declared: 19, want: "upstream_network_error"},
				{name: "image_as_video", data: []byte{255, 216, 255, 0, 1, 2}, declared: 6, want: "upstream_network_error"},
				{name: "invalid_mp4", data: boxBytes("ftyp", []byte("isom0000")), declared: 16, want: "upstream_network_error"},
				{name: "invalid_range", size: 513, declared: 513, span: "bytes 1-513/514", want: "upstream_network_error"},
				{name: "close_failure", size: 513, declared: 513, closeErr: core.NewError("service_not_configured", "fixture close failed", 503), want: "service_not_configured"},
				{name: "cancel_on_close", size: 513, declared: 513, closeCancel: true},
				{name: "bytes_with_eof", size: 513, declared: 513, final: io.EOF},
				{name: "unknown", size: 513, declared: -1},
				{name: "range", size: 513, declared: -1, span: "bytes 0-512/513"},
				{name: "extra_byte", size: 514, declared: 513, want: "upstream_network_error"},
				{name: "range_extra_byte", size: 514, declared: -1, span: "bytes 0-512/513", want: "upstream_network_error"},
				{name: "range_length_mismatch", size: 513, declared: 512, span: "bytes 0-512/513", want: "upstream_network_error"},
				{name: "truncated", size: 512, declared: 513, final: io.EOF, want: "upstream_network_error"},
				{name: "unexpected_eof", size: 512, declared: 513, final: io.ErrUnexpectedEOF, want: "upstream_network_error"},
				{name: "empty", declared: 0, want: "upstream_network_error"},
				{name: "empty_unknown", declared: -1, want: "upstream_network_error"},
				{name: "zero_with_bytes", size: 513, declared: 0, want: "upstream_network_error"},
				{name: "exact_limit", size: 513, declared: 513, maximum: 513},
				{name: "caller_limit", size: 513, declared: 513, maximum: 512, want: "asset_too_large"},
				{name: "configured_limit", size: 513, declared: 513, maximum: 1024, configured: 512, want: "asset_too_large"},
				{name: "unknown_limit", size: 513, declared: -1, maximum: 512, want: "asset_too_large"},
				{name: "range_total_limit", size: 513, declared: 513, span: "bytes 0-512/1024", maximum: 513, want: "asset_too_large"},
				{name: "cancel_final_bytes", size: 512, declared: 512, cancel: true, final: io.EOF},
				{name: "cancel_after_eof", size: 511, declared: 511, cancel: true, final: io.EOF},
			} {
				for _, disk := range []bool{false, true} {
					t.Run(test.name+"/disk="+strconv.FormatBool(disk), func(t *testing.T) {
						data := test.data
						if test.size > 0 {
							data = largeVideo(test.size)
						}
						cfg := testConfig()
						cfg.TempDir = t.TempDir()
						if disk {
							cfg.MediaMemoryFileBytes = 0
						}
						if test.configured > 0 {
							cfg.MaxAssetBytes = test.configured
						}
						d := NewDownloader(cfg, nil)
						defer d.Close()
						body := &smallResponseBody{Reader: bytes.NewReader(data), chunk: 171, final: test.final, closeErr: test.closeErr}
						d.client.Transport = smallResponseTransport(func(*http.Request) (*http.Response, error) {
							status, headers := http.StatusOK, make(http.Header)
							if test.span != "" {
								status = http.StatusPartialContent
								headers.Set("Content-Range", test.span)
								headers.Set("ETag", `"fixture"`)
							}
							return &http.Response{StatusCode: status, Header: headers, ContentLength: test.declared, Body: body}, nil
						})
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						if test.closeCancel {
							body.onClose = cancel
						}
						var got *core.Downloaded
						err := contract.withIdentity(d)(ctx, "", func(download core.DownloadFunc) error {
							var err error
							got, err = download(ctx, core.AssetContext{Kind: "video", UpstreamURL: "http://fixture.invalid/media"}, test.maximum, func(*core.Downloaded) {
								if test.cancel {
									cancel()
								}
							})
							return err
						})
						defer got.Cleanup()
						switch {
						case test.cancel || test.closeCancel:
							if !errors.Is(err, context.Canceled) {
								t.Fatal("cancellation lost", err)
							}
						case test.want != "":
							var api *core.Error
							if !errors.As(err, &api) || api.Code != test.want {
								t.Fatal("unexpected rejection", err, test.want)
							}
						default:
							if err != nil {
								t.Fatal(err)
							}
							actual, readErr := io.ReadAll(got.Data.Reader(ctx, 0, -1))
							if readErr != nil || !bytes.Equal(actual, data) || got.Size != int64(len(data)) || got.SHA256 != contract.sum(data) || got.Data.InMemory() == disk {
								t.Fatal("download changed", readErr, got)
							}
						}
						got.Cleanup()
						if !body.closed || d.MemoryInUse() != 0 {
							t.Fatal("response resources retained", body.closed, d.MemoryInUse())
						}
					})
				}
			}
		})
	}
}
