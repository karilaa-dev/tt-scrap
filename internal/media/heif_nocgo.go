//go:build !cgo

package media

import (
	"fmt"
	"image"
)

func decodeHEIF([]byte) (image.Image, error) {
	return nil, fmt.Errorf("HEIF and AVIF require a build with CGO and libheif")
}
