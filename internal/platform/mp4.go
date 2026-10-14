package platform

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

type box struct {
	kind       string
	start, end int64
}

func boxes(r io.ReaderAt, start, end int64) ([]box, error) {
	var result []box
	for start < end {
		if end-start < 8 {
			return nil, errors.New("truncated MP4 box")
		}
		var h [16]byte
		if _, e := r.ReadAt(h[:8], start); e != nil {
			return nil, e
		}
		size := int64(binary.BigEndian.Uint32(h[:4]))
		header := int64(8)
		if size == 1 {
			if _, e := r.ReadAt(h[8:], start+8); e != nil {
				return nil, e
			}
			n := binary.BigEndian.Uint64(h[8:])
			if n > uint64(end-start) {
				return nil, errors.New("MP4 box exceeds file")
			}
			size = int64(n)
			header = 16
		} else if size == 0 {
			size = end - start
		}
		if size < header || size > end-start {
			return nil, errors.New("invalid MP4 box size")
		}
		result = append(result, box{kind: string(h[4:8]), start: start + header, end: start + size})
		start += size
		if len(result) > 10000 {
			return nil, errors.New("MP4 contains too many boxes")
		}
	}
	return result, nil
}
func mediaDimensions(file io.ReaderAt) (int, int, error) {
	d, e := readMovieDimensions(func(start, length int64) ([]byte, error) {
		data := make([]byte, length)
		n, err := file.ReadAt(data, start)
		if err == io.EOF {
			err = nil
		}
		return data[:n], err
	})
	return d.width, d.height, e
}
func movieDimensions(movie []byte) (dimensions, error) {
	r := bytes.NewReader(movie)
	top, e := boxes(r, 0, int64(len(movie)))
	if e != nil {
		return dimensions{}, e
	}
	for _, moov := range top {
		if moov.kind != "moov" {
			continue
		}
		tracks, e := boxes(r, moov.start, moov.end)
		if e != nil {
			return dimensions{}, e
		}
		for _, track := range tracks {
			if track.kind != "trak" {
				continue
			}
			parts, e := boxes(r, track.start, track.end)
			if e != nil {
				return dimensions{}, e
			}
			for _, mdia := range parts {
				if mdia.kind != "mdia" {
					continue
				}
				children, e := boxes(r, mdia.start, mdia.end)
				if e != nil {
					return dimensions{}, e
				}
				isVideo := false
				var minf *box
				for n := range children {
					b := &children[n]
					if b.kind == "hdlr" && b.start+12 <= b.end && string(movie[b.start+8:b.start+12]) == "vide" {
						isVideo = true
					}
					if b.kind == "minf" {
						minf = b
					}
				}
				if !isVideo || minf == nil {
					continue
				}
				minfChildren, e := boxes(r, minf.start, minf.end)
				if e != nil {
					return dimensions{}, e
				}
				for _, stbl := range minfChildren {
					if stbl.kind != "stbl" {
						continue
					}
					stblChildren, e := boxes(r, stbl.start, stbl.end)
					if e != nil {
						return dimensions{}, e
					}
					for _, stsd := range stblChildren {
						if stsd.kind != "stsd" || stsd.start+8 > stsd.end {
							continue
						}
						entries, e := boxes(r, stsd.start+8, stsd.end)
						if e != nil {
							return dimensions{}, e
						}
						for _, entry := range entries {
							switch entry.kind {
							case "avc1", "avc3", "hvc1", "hev1", "hvc2", "av01", "mp4v", "encv", "bvc1", "bvc2", "dvhe", "dvh1":
							default:
								continue
							}
							if entry.start+28 > entry.end {
								continue
							}
							w, h := int(binary.BigEndian.Uint16(movie[entry.start+24:entry.start+26])), int(binary.BigEndian.Uint16(movie[entry.start+26:entry.start+28]))
							if w > 0 && h > 0 {
								return dimensions{w, h}, nil
							}
						}
					}
				}
			}
		}
	}
	return dimensions{}, errors.New("MP4 has no supported video sample description")
}
