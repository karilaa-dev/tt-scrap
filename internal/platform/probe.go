package platform

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type dimensions struct{ width, height int }

func (t *TikTok) selectApp(ctx context.Context, video object, asset core.AssetContext) (*videoSource, error) {
	candidates := appCandidates(video)
	if len(candidates) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pending := make([]*videoSource, 0, len(candidates))
	for _, candidate := range candidates {
		if value, ok := t.probes.get(asset.IdentityID + ":" + candidate.URLs[0]); ok {
			candidate.Width, candidate.Height, candidate.measured = value.width, value.height, true
		} else {
			pending = append(pending, candidate)
		}
	}
	var err error
	if len(pending) > 0 {
		err = t.app.WithMedia(ctx, asset.IdentityID, func(open core.OpenAppMedia) error {
			return t.probeCandidates(ctx, pending, asset, open)
		})
	}
	sortAppCandidates(candidates)
	return candidates[0], err
}

func (t *TikTok) probeCandidates(ctx context.Context, candidates []*videoSource, asset core.AssetContext, open core.OpenAppMedia) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *videoSource)
	var wg sync.WaitGroup
	var fatalMu sync.Mutex
	var fatal error
	for n := 0; n < min(3, len(candidates)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				value, e := t.probes.do(ctx, asset.IdentityID+":"+candidate.URLs[0], false, func() (dimensions, error) {
					for _, source := range candidate.URLs {
						d, e := readMovieDimensions(func(start, length int64) ([]byte, error) { return rangeBytes(ctx, asset, source, start, length, open) })
						if e == nil {
							return d, nil
						}
						var api *core.Error
						if errors.As(e, &api) && api.Code == "service_not_configured" {
							return dimensions{}, e
						}
						if ctx.Err() != nil {
							break
						}
					}
					return dimensions{}, errors.New("MP4 resolution could not be verified")
				})
				var api *core.Error
				if errors.As(e, &api) && api.Code == "service_not_configured" {
					fatalMu.Lock()
					fatal = e
					fatalMu.Unlock()
					cancel()
				}
				if e == nil {
					candidate.Width, candidate.Height, candidate.measured = value.width, value.height, true
				}
			}
		}()
	}
dispatch:
	for _, candidate := range candidates {
		select {
		case jobs <- candidate:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	return fatal
}
func rangeBytes(ctx context.Context, asset core.AssetContext, source string, start, length int64, open core.OpenAppMedia) ([]byte, error) {
	headers := asset.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	headers.Set("Accept-Encoding", "identity")
	res, e := open(ctx, source, headers)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 206 {
		return nil, errors.New("MP4 metadata request failed")
	}
	if start > 0 && res.StatusCode != 206 {
		return nil, errors.New("CDN does not support byte ranges")
	}
	if res.StatusCode == 206 && !strings.HasPrefix(res.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", start)) {
		return nil, errors.New("CDN returned an unexpected byte range")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, length))
	if closeErr := res.Body.Close(); closeErr != nil {
		return nil, closeErr
	}
	return data, err
}
func readMovieDimensions(read func(int64, int64) ([]byte, error)) (dimensions, error) {
	var offset int64
	for count := 0; count < 32; count++ {
		data, e := read(offset, 65536)
		if e != nil {
			return dimensions{}, e
		}
		local := int64(0)
		for local+8 <= int64(len(data)) {
			size := int64(binary.BigEndian.Uint32(data[local : local+4]))
			header := int64(8)
			if size == 1 {
				if local+16 > int64(len(data)) {
					break
				}
				n := binary.BigEndian.Uint64(data[local+8 : local+16])
				if n > 1<<50 {
					return dimensions{}, errors.New("MP4 box is too large")
				}
				size = int64(n)
				header = 16
			}
			if size < header {
				return dimensions{}, errors.New("invalid MP4 box")
			}
			if string(data[local+4:local+8]) == "moov" {
				if size > 8<<20 {
					return dimensions{}, errors.New("MP4 metadata exceeds 8 MiB")
				}
				var movie []byte
				if local+size <= int64(len(data)) {
					movie = data[local : local+size]
				} else {
					movie, e = read(offset+local, size)
					if e != nil {
						return dimensions{}, e
					}
				}
				return movieDimensions(movie)
			}
			local += size
		}
		if local == 0 {
			break
		}
		offset += local
	}
	return dimensions{}, errors.New("MP4 has no movie metadata")
}
