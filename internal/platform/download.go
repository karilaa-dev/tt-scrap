package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type Downloader struct {
	cfg        core.Config
	app        core.AppBackend
	transports *transports
	gate       *core.Limiter
	buffers    sync.Pool
}

func NewDownloader(cfg core.Config, backend core.AppBackend) *Downloader {
	return &Downloader{cfg: cfg, app: backend, transports: newTransports(cfg.HTTPMaxConnections), gate: core.NewLimiter(cfg.DownloadConcurrency, cfg.MaxQueuedRequests), buffers: sync.Pool{New: func() any { return make([]byte, 128<<10) }}}
}
func (d *Downloader) Close() { d.transports.close() }
func (d *Downloader) Download(ctx context.Context, asset core.AssetContext, maxBytes int64) (*core.Downloaded, error) {
	release, e := d.gate.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, d.cfg.DownloadTimeout)
	defer cancel()
	if d.cfg.MaxAssetBytes > 0 && (maxBytes <= 0 || d.cfg.MaxAssetBytes < maxBytes) {
		maxBytes = d.cfg.MaxAssetBytes
	}
	video, e := d.download(ctx, asset, maxBytes)
	if e != nil {
		return nil, e
	}
	if asset.Audio == nil {
		return video, nil
	}
	audioAsset := asset
	audioAsset.Kind = "audio"
	audioAsset.Filename = "audio.m4a"
	audioAsset.ContentType = asset.Audio.ContentType
	audioAsset.UpstreamURL = asset.Audio.UpstreamURL
	audioAsset.AlternateURLs = asset.Audio.AlternateURLs
	audioAsset.Cookies = asset.Audio.Cookies
	audioAsset.Audio = nil
	audioAsset.ExpectedWidth = 0
	audioAsset.ExpectedHeight = 0
	audio, e := d.download(ctx, audioAsset, maxBytes)
	if e != nil {
		video.Cleanup()
		return nil, e
	}
	defer video.Cleanup()
	defer audio.Cleanup()
	output, e := os.CreateTemp(d.cfg.TempDir, "tt-scrap-mux-*.mp4")
	if e != nil {
		return nil, e
	}
	path := output.Name()
	output.Close()
	ok := false
	defer func() {
		if !ok {
			os.Remove(path)
		}
	}()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", video.Path, "-i", audio.Path, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-movflags", "+faststart"}
	args = append(args, path)
	command := exec.CommandContext(ctx, "ffmpeg", args...)
	if e = command.Run(); e != nil {
		return nil, core.NewError("upstream_network_error", "Video and audio could not be combined", 502)
	}
	file, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil {
		return nil, e
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, tooLarge()
	}
	digest := sha256.New()
	if _, e = io.Copy(digest, file); e != nil {
		return nil, e
	}
	w, h, e := mediaDimensions(file)
	if e != nil {
		return nil, e
	}
	ok = true
	return &core.Downloaded{Path: path, Filename: asset.Filename, ContentType: "video/mp4", Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil)), Width: w, Height: h, Remove: func() { os.Remove(path) }}, nil
}
func (d *Downloader) download(ctx context.Context, asset core.AssetContext, maximum int64) (*core.Downloaded, error) {
	sources := append([]string{asset.UpstreamURL}, asset.AlternateURLs...)
	var last error
	for attempt := 0; attempt < max(len(sources), d.cfg.DownloadRetries); attempt++ {
		if e := ctx.Err(); e != nil {
			return nil, networkError(ctx, e)
		}
		result, e := d.downloadOnce(ctx, asset, sources[attempt%len(sources)], maximum)
		if e == nil {
			return result, nil
		}
		last = e
		var api *core.Error
		if errors.As(e, &api) && (api.Code == "asset_too_large" || api.Code == "service_not_configured") {
			return nil, e
		}
		if attempt+1 < max(len(sources), d.cfg.DownloadRetries) {
			if e = sleepContext(ctx, d.cfg.RetryDelay); e != nil {
				return nil, networkError(ctx, e)
			}
		}
	}
	return nil, last
}
func (d *Downloader) open(ctx context.Context, asset core.AssetContext, source string) (*http.Response, error) {
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
		if d.app == nil {
			return nil, core.Upstream("TikTok app backend is unavailable")
		}
		return d.app.OpenMedia(ctx, asset.IdentityID, source, headers)
	}
	proxy := asset.ProxyURL
	if asset.Platform == "tiktok" && d.cfg.ProxyDataOnly {
		proxy = ""
	}
	transport, e := d.transports.get(proxy)
	if e != nil {
		return nil, e
	}
	client := &http.Client{Transport: transport}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if e != nil {
		return nil, core.NewError("upstream_network_error", "Media URL is invalid", 502)
	}
	req.Header = headers
	for name, value := range asset.Cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	return client.Do(req)
}
func (d *Downloader) downloadOnce(ctx context.Context, asset core.AssetContext, source string, maximum int64) (*core.Downloaded, error) {
	res, e := d.open(ctx, asset, source)
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
			if maximum > 0 && received+int64(n) > maximum {
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
