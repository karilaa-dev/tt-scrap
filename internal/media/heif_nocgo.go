//go:build !cgo

package media

import (
	"fmt"
	"image"
	"io"
)

func decodeHEIF(io.ReaderAt, int64, func(int, int) error) (image.Image, error) {
	return nil, fmt.Errorf("HEIF and AVIF require a build with CGO and libheif")
}
