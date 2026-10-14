package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type Downloader struct {
	cfg     core.Config
	app     core.AppBackend
	client  *http.Client
	gate    *core.Limiter
	buffers sync.Pool
}

func NewDownloader(cfg core.Config, backend core.AppBackend) *Downloader {
	return &Downloader{cfg: cfg, app: backend, client: &http.Client{Transport: newTransport(cfg.HTTPMaxConnections)}, gate: core.NewLimiter(cfg.DownloadConcurrency, cfg.MaxQueuedRequests), buffers: sync.Pool{New: func() any { return make([]byte, 128<<10) }}}
}
func (d *Downloader) Close() { d.client.CloseIdleConnections() }
func (d *Downloader) WithIdentity(ctx context.Context, id string, run func(core.DownloadFunc) error) error {
	if id == "" {
		return run(d.Download)
	}
	if d.app == nil {
		return core.Upstream("TikTok app backend is unavailable")
	}
	return d.app.WithMedia(ctx, id, func(open core.OpenAppMedia) error {
		return run(func(ctx context.Context, asset core.AssetContext, maxBytes int64) (*core.Downloaded, error) {
			if asset.IdentityID != id {
				return nil, core.Upstream("Media does not belong to the leased identity")
			}
			return d.downloadAsset(ctx, asset, maxBytes, open)
		})
	})
}
func (d *Downloader) Download(ctx context.Context, asset core.AssetContext, maxBytes int64) (*core.Downloaded, error) {
	return d.downloadAsset(ctx, asset, maxBytes, nil)
}
func (d *Downloader) downloadAsset(ctx context.Context, asset core.AssetContext, maxBytes int64, open core.OpenAppMedia) (*core.Downloaded, error) {
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	if d.cfg.MaxAssetBytes > 0 && (maxBytes <= 0 || d.cfg.MaxAssetBytes < maxBytes) {
		maxBytes = d.cfg.MaxAssetBytes
	}
	var downloaded *core.Downloaded
	transfer := func(open core.OpenAppMedia) error {
		// Identity waits must not occupy transfer slots or the download budget.
		release, err := d.gate.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		if cancel == nil {
			ctx, cancel = context.WithTimeout(ctx, d.cfg.DownloadTimeout)
		}
		// Identity recovery can rerun the transfer before returning to the caller.
		downloaded.Cleanup()
		downloaded, err = d.download(ctx, asset, maxBytes, open)
		return err
	}
	var err error
	if open != nil {
		err = transfer(open)
	} else if asset.IdentityID != "" {
		if d.app == nil {
			return nil, core.Upstream("TikTok app backend is unavailable")
		}
		err = d.app.WithMedia(ctx, asset.IdentityID, transfer)
	} else {
		err = transfer(nil)
	}
	if err != nil {
		downloaded.Cleanup()
		return nil, err
	}
	return downloaded, nil
}

func (d *Downloader) download(ctx context.Context, asset core.AssetContext, maximum int64, open core.OpenAppMedia) (*core.Downloaded, error) {
	sources := append([]string{asset.UpstreamURL}, asset.AlternateURLs...)
	var last error
	for attempt := 0; attempt < max(len(sources), d.cfg.DownloadRetries); attempt++ {
		if e := ctx.Err(); e != nil {
			return nil, networkError(ctx, e)
		}
		result, e := d.downloadOnce(ctx, asset, sources[attempt%len(sources)], maximum, open)
		if e == nil {
			return result, nil
		}
		last = e
		var api *core.Error
		if errors.As(e, &api) && (api.Code == "asset_too_large" || api.Code == "service_not_configured") {
			return nil, e
		}
		if attempt+1 < max(len(sources), d.cfg.DownloadRetries) {
			if e = sleepContext(ctx, d.retryDelay(attempt)); e != nil {
				return nil, networkError(ctx, e)
			}
		}
	}
	return nil, last
}
func (d *Downloader) retryDelay(attempt int) time.Duration {
	// Configured retries stop before this cap; extra fallback URLs must not overflow.
	delay := min(d.cfg.RetryDelay*time.Duration(1<<min(attempt, 10)), d.cfg.DownloadTimeout)
	return min(delay+time.Duration(rand.Float64()*float64(delay)*0.1), d.cfg.DownloadTimeout)
}
func (d *Downloader) open(ctx context.Context, asset core.AssetContext, source string, appOpen core.OpenAppMedia) (*http.Response, error) {
	headers := asset.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Accept-Encoding", "identity")
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", browserAgent)
	}
	if asset.Referer != "" {
		headers.Set("Referer", asset.Referer)
	}
	if asset.IdentityID != "" {
		return appOpen(ctx, source, headers)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if e != nil {
		return nil, core.NewError("upstream_network_error", "Media URL is invalid", 502)
	}
	req.Header = headers
	return d.client.Do(req)
}
func (d *Downloader) downloadOnce(ctx context.Context, asset core.AssetContext, source string, maximum int64, open core.OpenAppMedia) (*core.Downloaded, error) {
	res, e := d.open(ctx, asset, source, open)
	if e != nil {
		return nil, networkError(ctx, e)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, core.NewError("upstream_network_error", "Media download returned an unsuccessful response", 502)
	}
	if maximum > 0 && res.ContentLength > maximum {
		return nil, tooLarge()
	}
	file, e := os.CreateTemp(d.cfg.TempDir, "tt-scrap-media-*")
	if e != nil {
		return nil, e
	}
	path := file.Name()
	good := false
	defer func() {
		file.Close()
		if !good {
			os.Remove(path)
		}
	}()
	digest := sha256.New()
	buffer := d.buffers.Get().([]byte)
	defer d.buffers.Put(buffer)
	prefix := make([]byte, 0, 512)
	var received int64
	for {
		n, readErr := res.Body.Read(buffer)
		if n > 0 {
			if maximum > 0 && int64(n) > maximum-received {
				return nil, tooLarge()
			}
			if len(prefix) < 512 {
				prefix = append(prefix, buffer[:min(n, 512-len(prefix))]...)
			}
			if _, e = file.Write(buffer[:n]); e != nil {
				return nil, e
			}
			digest.Write(buffer[:n])
			received += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, networkError(ctx, readErr)
		}
		if e = ctx.Err(); e != nil {
			return nil, networkError(ctx, e)
		}
	}
	if received == 0 || (res.ContentLength >= 0 && received != res.ContentLength) {
		return nil, core.NewError("upstream_network_error", "Media download is empty or truncated", 502)
	}
	mime := detectMedia(prefix, asset.Kind)
	if mime == "" {
		return nil, core.NewError("upstream_network_error", "Upstream returned unsupported media bytes", 502)
	}
	width, height := 0, 0
	if asset.Kind == "video" {
		if mime != "video/mp4" {
			return nil, core.NewError("upstream_network_error", "Upstream video is not MP4", 502)
		}
		width, height, e = mediaDimensions(file)
		if e != nil {
			return nil, core.NewError("upstream_network_error", "Upstream MP4 is invalid or incomplete", 502)
		}
		if asset.ExpectedWidth > 0 && asset.ExpectedHeight > 0 && (width != asset.ExpectedWidth || height != asset.ExpectedHeight) {
			return nil, core.NewError("upstream_network_error", "Video dimensions differ from the selected rendition", 502)
		}
	}
	if e = res.Body.Close(); e != nil {
		return nil, e
	}
	good = true
	return &core.Downloaded{Path: path, Filename: detectedFilename(asset.Filename, mime), ContentType: mime, Size: received, SHA256: hex.EncodeToString(digest.Sum(nil)), Width: width, Height: height, Remove: func() { os.Remove(path) }}, nil
}
func detectMedia(prefix []byte, kind string) string {
	if imageType := media.DetectImageType(prefix); imageType != "" {
		return imageType
	}
	if len(prefix) >= 12 && string(prefix[4:8]) == "ftyp" {
		if kind == "audio" {
			return "audio/mp4"
		}
		return "video/mp4"
	}
	if len(prefix) >= 3 && (string(prefix[:3]) == "ID3" || (prefix[0] == 255 && prefix[1]&224 == 224)) {
		return "audio/mpeg"
	}
	if len(prefix) >= 4 && string(prefix[:4]) == "OggS" {
		return "audio/ogg"
	}
	return ""
}
func detectedFilename(name, mime string) string { return media.FilenameForType(name, mime) }

func tooLarge() error {
	return core.NewError("asset_too_large", "Media exceeds the configured download limit", 413)
}
