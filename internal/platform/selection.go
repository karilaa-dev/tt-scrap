package platform

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

type videoSource struct {
	URLs             []string
	Width, Height    int
	AudioURLs        []string
	score            float64
	known            bool
	quality, bitrate int
	fps              float64
	measured         bool
}

func selectVideo(video object, app bool) *videoSource {
	if app {
		candidates := appCandidates(video)
		if len(candidates) == 0 {
			return nil
		}
		return candidates[0]
	}
	var regular *videoSource
	for _, key := range []string{"playAddr", "downloadAddr"} {
		if u := urls(video[key]); len(u) > 0 {
			regular = &videoSource{URLs: u, Width: integer(video["width"]), Height: integer(video["height"])}
			break
		}
	}
	audioTracks := arr(first(video["bitrateAudioInfo"], video["bit_rate_audio_info"]))
	var audioURLs []string
	audioRate := -1
	for _, r := range audioTracks {
		v := obj(r)
		rate := integer(first(v["Bitrate"], v["bit_rate"]))
		if u := urls(first(v["UrlList"], v["url_list"])); len(u) > 0 && rate > audioRate {
			audioURLs = u
			audioRate = rate
		}
	}
	var candidates []*videoSource
	for _, r := range arr(video["bitrateInfo"]) {
		v := obj(r)
		address := obj(first(v["PlayAddr"], v["play_addr"]))
		u := urls(address)
		if len(u) == 0 {
			continue
		}
		gear := strings.ToLower(str(first(v["GearName"], v["gear_name"])))
		muxed := strings.HasPrefix(gear, "normal_")
		if len(audioTracks) > 0 && !muxed && len(audioURLs) == 0 {
			continue
		}
		c := &videoSource{URLs: u, Width: integer(first(address["Width"], address["width"])), Height: integer(first(address["Height"], address["height"])), quality: integer(first(v["QualityType"], v["quality_type"])), bitrate: integer(first(v["Bitrate"], v["bit_rate"])), known: gear == "adapt_lowest_1080_1" || strings.Contains(str(first(address["UrlKey"], address["url_key"])), "bytevc1_1080p"), score: math.Inf(-1)}
		if c.quality <= 0 {
			c.quality = 10000
		}
		if len(audioTracks) > 0 && !muxed {
			c.AudioURLs = audioURLs
		}
		c.score = mvmaf(first(v["MVMAF"], v["mvmaf"]), min(c.Width, c.Height))
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return regular
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Width*a.Height != b.Width*b.Height {
			return a.Width*a.Height > b.Width*b.Height
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.known != b.known {
			return a.known
		}
		if a.quality != b.quality {
			return a.quality < b.quality
		}
		return a.bitrate > b.bitrate
	})
	best := candidates[0]
	if regular != nil && (regular.Width*regular.Height > best.Width*best.Height || (regular.Width*regular.Height == best.Width*best.Height && math.IsInf(best.score, -1))) {
		return regular
	}
	return best
}
func mvmaf(v any, edge int) float64 {
	if edge <= 0 {
		return math.Inf(-1)
	}
	if s, ok := v.(string); ok {
		_ = json.Unmarshal([]byte(s), &v)
	}
	m := obj(v)
	if nested, ok := m["v2.0"].(map[string]any); ok {
		m = nested
	}
	scores := obj(first(m["ori"], m["srv1"]))
	distance := math.MaxInt
	selectedTarget := 0
	result := math.Inf(-1)
	for k, v := range scores {
		if !strings.HasPrefix(k, "v") {
			continue
		}
		target := integer(k[1:])
		d := target - edge
		if d < 0 {
			d = -d
		}
		if target <= 0 || d > distance || (d == distance && target < selectedTarget) {
			continue
		}
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case json.Number:
			f, _ = x.Float64()
		default:
			continue
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		distance = d
		selectedTarget = target
		result = f
	}
	return result
}

func appCandidates(video object) []*videoSource {
	var result []*videoSource
	collect := func(container object, rate object) {
		keys := make([]string, 0, len(container))
		for key := range container {
			if key == "play_addr" || strings.HasPrefix(key, "play_addr_") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			address := obj(container[key])
			sources := urls(address)
			if len(sources) == 0 {
				continue
			}
			result = append(result, &videoSource{URLs: sources, Width: integer(address["width"]), Height: integer(address["height"]), fps: decimal(first(rate["fps"], address["fps"])), bitrate: integer(first(rate["bit_rate"], address["bit_rate"]))})
		}
	}
	collect(video, video)
	for _, raw := range arr(video["bit_rate"]) {
		rate := obj(raw)
		collect(rate, rate)
	}
	sortAppCandidates(result)
	return result
}
func sortAppCandidates(result []*videoSource) {
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Width*a.Height != b.Width*b.Height {
			return a.Width*a.Height > b.Width*b.Height
		}
		if a.fps != b.fps {
			return a.fps > b.fps
		}
		return a.bitrate > b.bitrate
	})
}
