package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/spool"
)

// This file uses only the existing preparation API so the same benchmark can
// be compiled unchanged with the frozen original source. Each iteration decodes
// fresh pixels, including alpha, before conversion; fixture encoding is untimed.
func BenchmarkPrepare(b *testing.B) {
	for _, test := range []struct {
		name, format, fixture string
		width, height         int
		alpha                 bool
		orientation           int
		m                     mode
		outW, outH            int
		owned                 bool
	}{
		{name: "NativeJPEG1080p", format: "jpeg", width: 1920, height: 1080, m: photoMode, outW: 1920, outH: 1080},
		{name: "ThumbnailJPEG1080p", format: "jpeg", width: 1920, height: 1080, m: thumbnailMode, outW: 320, outH: 180, owned: true},
		{name: "ThumbnailOpaquePNG1080p", format: "png", width: 1920, height: 1080, m: thumbnailMode, outW: 320, outH: 180, owned: true},
		{name: "ThumbnailAlphaPNG1080p", format: "png", width: 1920, height: 1080, alpha: true, m: thumbnailMode, outW: 320, outH: 180, owned: true},
		{name: "DocumentJPEG1080p", format: "jpeg", width: 1920, height: 1080, m: documentMode, outW: 1920, outH: 1080, owned: true},
		{name: "DocumentOrientedJPEG1080p", format: "jpeg", width: 1920, height: 1080, orientation: 6, m: documentMode, outW: 1080, outH: 1920, owned: true},
		{name: "DocumentOpaquePNG1080p", format: "png", width: 1920, height: 1080, m: documentMode, outW: 1920, outH: 1080, owned: true},
		{name: "DocumentAlphaPNG1080p", format: "png", width: 1920, height: 1080, alpha: true, m: documentMode, outW: 1920, outH: 1080, owned: true},
		{name: "PhotoWidePNG", format: "png", width: 4000, height: 40, alpha: true, m: photoMode, outW: 4000, outH: 200, owned: true},
		{name: "DocumentWebP", fixture: "static.webp", m: documentMode, outW: 400, outH: 301, owned: true},
		{name: "PhotoAnimatedWebP", fixture: "animated.webp", m: photoMode, outW: 16, outH: 12, owned: true},
		{name: "PhotoAnimatedPNG", fixture: "animated.png", m: photoMode, outW: 16, outH: 12, owned: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			var data []byte
			if test.fixture != "" {
				var err error
				data, err = os.ReadFile("testdata/" + test.fixture)
				if err != nil {
					b.Fatal(err)
				}
			} else {
				img := image.NewNRGBA(image.Rect(0, 0, test.width, test.height))
				for y := range test.height {
					for x := range test.width {
						a := uint8(255)
						if test.alpha {
							a = uint8(x*13 + y*7)
						}
						img.SetNRGBA(x, y, color.NRGBA{R: uint8(x*3 + y), G: uint8(x + y*5), B: uint8(x*7 + y*11), A: a})
					}
				}
				var encoded bytes.Buffer
				var err error
				if test.format == "jpeg" {
					err = jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90})
				} else {
					err = png.Encode(&encoded, img)
				}
				if err != nil {
					b.Fatal(err)
				}
				data = encoded.Bytes()
			}
			if test.orientation != 0 {
				exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, byte(test.orientation), 0, 0, 0, 0, 0, 0, 0}
				marker := []byte{0xff, 0xe1, 0, 0}
				binary.BigEndian.PutUint16(marker[2:], uint16(len(exif)+2))
				data = append(append(append(bytes.Clone(data[:2]), marker...), exif...), data[2:]...)
			}
			source := spool.Bytes(data)
			defer source.Close()
			p := NewProcessor(1, 64<<20)
			ctx := context.Background()
			prepare := func() (Prepared, error) {
				if test.m == documentMode {
					return p.PrepareDocument(ctx, source, "fixture.bin")
				}
				return p.Prepare(ctx, source, "fixture.bin", test.m == thumbnailMode)
			}
			sample, err := prepare()
			if err != nil {
				b.Fatal(err)
			}
			decoded, _, err := image.Decode(bytes.NewReader(sample.Data.Bytes()))
			if err != nil {
				b.Fatal(err)
			}
			if decoded.Bounds().Size() != image.Pt(test.outW, test.outH) || sample.Owned != test.owned || sample.ContentType != "image/jpeg" {
				b.Fatalf("unexpected output: %v %+v", decoded.Bounds(), sample)
			}
			wantSize := sample.Size
			sample.Cleanup()
			b.ReportAllocs()
			for b.Loop() {
				prepared, err := prepare()
				if err != nil {
					b.Fatal(err)
				}
				if prepared.Size != wantSize || prepared.Owned != test.owned || prepared.Data.Size() != prepared.Size {
					b.Fatal("inconsistent prepared result")
				}
				prepared.Cleanup()
			}
		})
	}
}
