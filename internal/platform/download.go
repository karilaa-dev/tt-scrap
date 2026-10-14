package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

type Downloader struct {
	cfg     core.Config
	app     core.AppBackend
	client  *http.Client
	gate    *core.Limiter
	store   *spool.Store
	buffers sync.Pool
}

// The first request asks for one part; its Content-Range reveals the size and
// whether the remaining parts can download in parallel.
const firstPart = 1 << 20
const minPart = 1 << 20

func NewDownloader(cfg core.Config, backend core.AppBackend) *Downloader {
	return &Downloader{cfg: cfg, app: backend, client: &http.Client{Transport: newTransport(cfg.HTTPMaxConnections)}, gate: core.NewLimiter(cfg.DownloadConcurrency, cfg.MaxQueuedRequests), store: spool.NewStore(cfg.TempDir, cfg.MediaMemoryFileBytes, cfg.MediaMemoryBudgetBytes), buffers: sync.Pool{New: func() any { buffer := make([]byte, 128<<10); return &buffer }}}
}
func (d *Downloader) Close() { d.client.CloseIdleConnections() }

// MemoryInUse reports RAM reserved by open downloads.
func (d *Downloader) MemoryInUse() int64 { return d.store.Used() }
func (d *Downloader) WithIdentity(ctx context.Context, id string, run func(core.DownloadFunc) error) error {
	if id == "" {
		return run(func(ctx context.Context, asset core.AssetContext, maxBytes int64, started func(*core.Downloaded)) (*core.Downloaded, error) {
			return d.downloadAsset(ctx, asset, maxBytes, nil, started)
		})
	}
	if d.app == nil {
		return core.Upstream("TikTok app backend is unavailable")
	}
	return d.app.WithMedia(ctx, id, func(open core.OpenAppMedia) error {
		return run(func(ctx context.Context, asset core.AssetContext, maxBytes int64, started func(*core.Downloaded)) (*core.Downloaded, error) {
			if asset.IdentityID != id {
				return nil, core.Upstream("Media does not belong to the leased identity")
			}
			return d.downloadAsset(ctx, asset, maxBytes, open, started)
		})
	})
}
func (d *Downloader) Download(ctx context.Context, asset core.AssetContext, maxBytes int64) (*core.Downloaded, error) {
	return d.downloadAsset(ctx, asset, maxBytes, nil, nil)
}
func (d *Downloader) downloadAsset(ctx context.Context, asset core.AssetContext, maxBytes int64, open core.OpenAppMedia, started func(*core.Downloaded)) (*core.Downloaded, error) {
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
		downloaded, err = d.download(ctx, asset, maxBytes, open, started)
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

func (d *Downloader) download(ctx context.Context, asset core.AssetContext, maximum int64, open core.OpenAppMedia, started func(*core.Downloaded)) (*core.Downloaded, error) {
	sources := append([]string{asset.UpstreamURL}, asset.AlternateURLs...)
	var last error
	for attempt := 0; attempt < max(len(sources), d.cfg.DownloadRetries); attempt++ {
		if e := ctx.Err(); e != nil {
			return nil, networkError(ctx, e)
		}
		result, e := d.downloadOnce(ctx, asset, sources[attempt%len(sources)], maximum, open, started)
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
func (d *Downloader) open(ctx context.Context, asset core.AssetContext, source, byteRange string, appOpen core.OpenAppMedia) (*http.Response, error) {
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
	if byteRange != "" {
		headers.Set("Range", byteRange)
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
func (d *Downloader) downloadOnce(ctx context.Context, asset core.AssetContext, source string, maximum int64, open core.OpenAppMedia, started func(*core.Downloaded)) (*core.Downloaded, error) {
	ranged := d.cfg.DownloadParts > 1
	byteRange := ""
	if ranged {
		byteRange = "bytes=0-" + strconv.Itoa(firstPart-1)
	}
	res, e := d.open(ctx, asset, source, byteRange, open)
	if e != nil {
		return nil, networkError(ctx, e)
	}
	defer res.Body.Close()
	// total is the file size and end the bytes this response carries; -1 if unknown.
	total, end := res.ContentLength, res.ContentLength
	switch {
	case res.StatusCode == http.StatusPartialContent && ranged:
		first, last, size, ok := contentRange(res.Header.Get("Content-Range"))
		if !ok || first != 0 || res.ContentLength >= 0 && res.ContentLength != last+1 {
			return nil, invalidRange()
		}
		total, end = size, last+1
	case res.StatusCode == http.StatusOK:
		ranged = false
	default:
		return nil, core.NewError("upstream_network_error", "Media download returned an unsuccessful response", 502)
	}
	if maximum > 0 && total > maximum {
		return nil, tooLarge()
	}
	data, e := d.store.New(total)
	if e != nil {
		return nil, e
	}
	succeeded := false
	var parts *rangeJob
	defer func() {
		if !succeeded {
			data.Close()
		}
		parts.stop()
	}()
	digest := sha256.New()
	hashed := make(chan error, 1)
	go func() {
		pooled := d.buffers.Get().(*[]byte)
		defer d.buffers.Put(pooled)
		_, err := io.CopyBuffer(digest, data.Reader(ctx, 0, -1), *pooled)
		hashed <- err
	}()
	writer := data.Writer(0, end)
	pooled := d.buffers.Get().(*[]byte)
	defer d.buffers.Put(pooled)
	buffer := *pooled
	prefix := make([]byte, 0, 512)
	mime := ""
	// Validate the media type before requesting other parts or announcing bytes.
	announce := func() error {
		mime = detectMedia(prefix, asset.Kind)
		if mime == "" {
			return core.NewError("upstream_network_error", "Upstream returned unsupported media bytes", 502)
		}
		if asset.Kind == "video" && mime != "video/mp4" {
			return core.NewError("upstream_network_error", "Upstream video is not MP4", 502)
		}
		if ranged && total > end {
			parts = d.startRanges(ctx, asset, partSources(res, source), open, data, end, total)
		}
		if started != nil && total >= 0 {
			started(&core.Downloaded{Data: data, Filename: detectedFilename(asset.Filename, mime), ContentType: mime, Size: total})
		}
		return nil
	}
	var received int64
	for {
		n, readErr := res.Body.Read(buffer)
		if n > 0 {
			if end >= 0 && int64(n) > end-received {
				return nil, core.NewError("upstream_network_error", "Media download exceeds its declared length", 502)
			}
			if maximum > 0 && int64(n) > maximum-received {
				return nil, tooLarge()
			}
			if len(prefix) < 512 {
				prefix = append(prefix, buffer[:min(n, 512-len(prefix))]...)
				if len(prefix) == 512 {
					if e = announce(); e != nil {
						return nil, e
					}
				}
			}
			if _, e = writer.Write(buffer[:n]); e != nil {
				return nil, e
			}
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
	if received == 0 || (end >= 0 && received != end) {
		return nil, core.NewError("upstream_network_error", "Media download is empty or truncated", 502)
	}
	if mime == "" {
		if e = announce(); e != nil {
			return nil, e
		}
	}
	if e = res.Body.Close(); e != nil {
		return nil, e
	}
	if parts != nil {
		// The first connection keeps its transfer slot for the remaining parts.
		if e = parts.finish(); e != nil {
			return nil, e
		}
	}
	if e = data.Complete(); e != nil {
		return nil, core.NewError("upstream_network_error", "Media download is empty or truncated", 502)
	}
	if e = <-hashed; e != nil {
		return nil, e
	}
	size := data.Size()
	width, height := 0, 0
	if asset.Kind == "video" {
		width, height, e = mediaDimensions(data)
		if e != nil {
			return nil, core.NewError("upstream_network_error", "Upstream MP4 is invalid or incomplete", 502)
		}
		if asset.ExpectedWidth > 0 && asset.ExpectedHeight > 0 && (width != asset.ExpectedWidth || height != asset.ExpectedHeight) {
			return nil, core.NewError("upstream_network_error", "Video dimensions differ from the selected rendition", 502)
		}
	}
	succeeded = true
	return &core.Downloaded{Data: data, Filename: detectedFilename(asset.Filename, mime), ContentType: mime, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil)), Width: width, Height: height}, nil
}

// partSources prefers the post-redirect address, so parts skip app redirects;
// part retries alternate with the original source in case that address fails.
func partSources(res *http.Response, source string) []string {
	if res.Request != nil && res.Request.URL != nil && (res.Request.URL.Scheme == "https" || res.Request.URL.Scheme == "http") {
		if final := res.Request.URL.String(); final != source {
			return []string{final, source}
		}
	}
	return []string{source}
}

func contentRange(value string) (first, last, size int64, ok bool) {
	span, found := strings.CutPrefix(value, "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	span, total, found := strings.Cut(span, "/")
	from, to, separated := strings.Cut(span, "-")
	if !found || !separated {
		return 0, 0, 0, false
	}
	var errs [3]error
	first, errs[0] = strconv.ParseInt(from, 10, 64)
	last, errs[1] = strconv.ParseInt(to, 10, 64)
	size, errs[2] = strconv.ParseInt(total, 10, 64)
	return first, last, size, errors.Join(errs[:]...) == nil && first >= 0 && first <= last && last < size
}

func invalidRange() error {
	return core.NewError("upstream_network_error", "Media download returned an invalid range", 502)
}

// rangeJob downloads the parts after the first response. Extra connections use
// free transfer slots only, so a busy gate downloads with fewer connections.
type rangeJob struct {
	d       *Downloader
	ctx     context.Context
	cancel  context.CancelFunc
	asset   core.AssetContext
	sources []string
	open    core.OpenAppMedia
	data    *spool.Buffer
	from    int64
	total   int64
	part    int64
	count   int64
	next    atomic.Int64
	workers sync.WaitGroup
	once    sync.Once
	err     error
}

func (d *Downloader) startRanges(ctx context.Context, asset core.AssetContext, sources []string, open core.OpenAppMedia, data *spool.Buffer, from, total int64) *rangeJob {
	connections := int64(d.cfg.DownloadParts)
	// Two parts per connection keep the ready prefix moving for streaming readers.
	part := max(minPart, (total-from+connections*2-1)/(connections*2))
	j := &rangeJob{d: d, asset: asset, sources: sources, open: open, data: data, from: from, total: total, part: part, count: (total - from + part - 1) / part}
	j.ctx, j.cancel = context.WithCancel(ctx)
	for range min(connections-1, j.count) {
		release, ok := d.gate.TryAcquire()
		if !ok {
			break
		}
		j.workers.Go(func() {
			defer release()
			j.work()
		})
	}
	return j
}

func (j *rangeJob) work() {
	for j.ctx.Err() == nil {
		index := j.next.Add(1) - 1
		if index >= j.count {
			return
		}
		start := j.from + index*j.part
		if err := j.fetch(start, min(start+j.part, j.total)); err != nil {
			j.once.Do(func() { j.err = err; j.cancel() })
			return
		}
	}
}

func (j *rangeJob) finish() error {
	j.work()
	j.workers.Wait()
	if j.err != nil {
		return j.err
	}
	if err := j.ctx.Err(); err != nil {
		return networkError(j.ctx, err)
	}
	return nil
}

func (j *rangeJob) stop() {
	if j != nil {
		j.cancel()
		j.workers.Wait()
	}
}

// fetch resumes a part from its last received byte, so retries never rewrite
// bytes that readers may already have streamed.
func (j *rangeJob) fetch(start, end int64) error {
	writer := j.data.Writer(start, end)
	var last error
	for attempt := 0; attempt < max(1, j.d.cfg.DownloadRetries); attempt++ {
		if attempt > 0 {
			if err := sleepContext(j.ctx, j.d.retryDelay(attempt-1)); err != nil {
				return networkError(j.ctx, err)
			}
		}
		if last = j.fetchOnce(writer, j.sources[attempt%len(j.sources)], end); last == nil {
			return nil
		}
		if j.ctx.Err() != nil {
			return networkError(j.ctx, j.ctx.Err())
		}
	}
	return last
}

func (j *rangeJob) fetchOnce(writer *spool.Writer, source string, end int64) error {
	offset := writer.Offset()
	res, err := j.d.open(j.ctx, j.asset, source, "bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(end-1, 10), j.open)
	if err != nil {
		return networkError(j.ctx, err)
	}
	defer res.Body.Close()
	first, last, size, ok := contentRange(res.Header.Get("Content-Range"))
	if res.StatusCode != http.StatusPartialContent || !ok || first != offset || last != end-1 || size != j.total {
		return invalidRange()
	}
	pooled := j.d.buffers.Get().(*[]byte)
	defer j.d.buffers.Put(pooled)
	// Reading to EOF lets the transport reuse this connection for the next part;
	// the range writer rejects bytes beyond the requested range.
	n, err := io.CopyBuffer(writer, res.Body, *pooled)
	if err != nil {
		return networkError(j.ctx, err)
	}
	if n != end-offset {
		return core.NewError("upstream_network_error", "Media download is empty or truncated", 502)
	}
	return nil
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
