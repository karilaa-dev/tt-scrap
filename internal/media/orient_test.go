package media

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// referenceOrient is the straightforward per-pixel transform that orient must match.
func referenceOrient(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if orientation >= 5 {
		ow, oh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := range h {
		for x := range w {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = w - 1 - x
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dy = h - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

func orientationSources() map[string]image.Image {
	const w, h = 37, 23
	ycbcr := image.NewYCbCr(image.Rect(0, 0, w+4, h+4), image.YCbCrSubsampleRatio420)
	gray := image.NewGray(image.Rect(0, 0, w, h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h + 4 {
		for x := range w + 4 {
			ycbcr.Y[ycbcr.YOffset(x, y)] = uint8(x*7 + y*3)
			ycbcr.Cb[ycbcr.COffset(x, y)] = uint8(x*5 + 40)
			ycbcr.Cr[ycbcr.COffset(x, y)] = uint8(y*11 + 90)
			if x < w && y < h {
				gray.SetGray(x, y, color.Gray{Y: uint8(x*9 + y)})
				rgba.SetRGBA(x, y, color.RGBA{R: uint8(x * 6), G: uint8(y * 10), B: uint8(x + y), A: 255})
				nrgba.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 6), G: uint8(y * 10), B: 200, A: 255})
			}
		}
	}
	return map[string]image.Image{
		"ycbcr":       ycbcr,
		"ycbcr-sub":   ycbcr.SubImage(image.Rect(3, 1, w+3, h+1)),
		"gray":        gray,
		"rgba":        rgba,
		"rgba-sub":    rgba.SubImage(image.Rect(2, 2, w, h)),
		"nrgba-other": nrgba,
	}
}

func TestOrientMatchesPerPixelTransform(t *testing.T) {
	for name, source := range orientationSources() {
		for orientation := 1; orientation <= 8; orientation++ {
			want, got := referenceOrient(source, orientation), orient(source, orientation)
			if orientation == 1 {
				if got != source {
					t.Fatalf("%s: orientation 1 copied the image", name)
				}
				continue
			}
			wantRGBA, gotRGBA := want.(*image.RGBA), got.(*image.RGBA)
			if gotRGBA.Bounds() != wantRGBA.Bounds() || !bytes.Equal(gotRGBA.Pix, wantRGBA.Pix) {
				t.Fatalf("%s orientation %d differs from the per-pixel transform", name, orientation)
			}
		}
	}
}

func BenchmarkOrientJPEG(b *testing.B) {
	img := image.NewYCbCr(image.Rect(0, 0, 1080, 1920), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = uint8(i)
	}
	b.ReportAllocs()
	for b.Loop() {
		orient(img, 6)
	}
}
