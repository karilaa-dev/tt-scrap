package platform

import (
	"sort"
	"strings"
)

type videoSource struct {
	URLs          []string
	Width, Height int
	fps           float64
	bitrate       int
}

func selectVideo(video object) *videoSource {
	var best videoSource
	found := false
	collect := func(container object) {
		keys := make([]string, 0, len(container))
		for key := range container {
			if key == "play_addr" || strings.HasPrefix(key, "play_addr_") {
				keys = append(keys, key)
			}
		}
		// Stable address order keeps equally ranked renditions deterministic.
		sort.Strings(keys)
		for _, key := range keys {
			address := obj(container[key])
			sources := urls(address)
			if len(sources) == 0 {
				continue
			}
			bitrate := integer(container["bit_rate"])
			if bitrate <= 0 {
				bitrate = integer(address["bit_rate"])
			}
			candidate := videoSource{URLs: sources, Width: integer(address["width"]), Height: integer(address["height"]), fps: decimal(first(container["fps"], address["fps"])), bitrate: bitrate}
			if !found || candidate.betterThan(best) {
				best, found = candidate, true
			}
		}
	}
	collect(video)
	for _, raw := range arr(video["bit_rate"]) {
		collect(obj(raw))
	}
	if !found {
		return nil
	}
	return &best
}

func (v videoSource) betterThan(other videoSource) bool {
	if v.Width*v.Height != other.Width*other.Height {
		return v.Width*v.Height > other.Width*other.Height
	}
	if v.fps != other.fps {
		return v.fps > other.fps
	}
	return v.bitrate > other.bitrate
}
