package platform

import (
	"encoding/binary"
	"errors"
)

type dimensions struct{ width, height int }

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
