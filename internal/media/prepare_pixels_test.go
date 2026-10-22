package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/spool"
	"golang.org/x/image/draw"
)

// referencePreparePixels preserves the original two-canvas conversion so pixel
// rounding and white compositing can be checked independently of the fast paths.
func referencePreparePixels(img image.Image, width, height, paddedW, paddedH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	if paddedW != width || paddedH != height {
		padded := image.NewRGBA(image.Rect(0, 0, paddedW, paddedH))
		stddraw.Draw(padded, padded.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
		offset := image.Pt((paddedW-width)/2, (paddedH-height)/2)
		stddraw.Draw(padded, dst.Bounds().Add(offset), dst, image.Point{}, stddraw.Src)
		dst = padded
	}
	return dst
}

type genericImage struct{ image.Image }

func preparationSources() map[string]image.Image {
	r := image.Rect(-3, 7, 38, 36)
	nrgba := image.NewNRGBA(r)
	rgba := image.NewRGBA(r)
	opaque := image.NewNRGBA(r)
	rgba64 := image.NewRGBA64(r)
	nrgba64 := image.NewNRGBA64(r)
	gray := image.NewGray(r)
	gray16 := image.NewGray16(r)
	cmyk := image.NewCMYK(r)
	paletted := image.NewPaletted(r, color.Palette{color.Transparent, color.White, color.NRGBA{R: 130, G: 79, B: 40, A: 127}})
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBA{R: uint8(x*7 + y*11), G: uint8(x*13 - y*3), B: uint8(x*19 + y*5), A: uint8(x*31 + y*17)}
			nrgba.SetNRGBA(x, y, c)
			rgba.Set(x, y, c)
			opaque.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: 255})
			c64 := color.NRGBA64{R: uint16(c.R)*257 + 27, G: uint16(c.G)*257 + 71, B: uint16(c.B)*257 + 97, A: uint16(c.A)*257 + 51}
			rgba64.Set(x, y, c64)
			nrgba64.SetNRGBA64(x, y, c64)
			gray.Set(x, y, c)
			gray16.Set(x, y, c64)
			cmyk.Set(x, y, c)
			paletted.SetColorIndex(x, y, uint8((x-r.Min.X+y)%3))
		}
	}
	sub := image.Rect(1, 10, 38, 33)
	sources := map[string]image.Image{
		"rgba": rgba.SubImage(sub), "nrgba": nrgba.SubImage(sub), "opaque-nrgba": opaque.SubImage(sub),
		"rgba64": rgba64.SubImage(sub), "nrgba64": nrgba64.SubImage(sub),
		"gray": gray.SubImage(sub), "gray16": gray16.SubImage(sub), "cmyk": cmyk.SubImage(sub),
		"paletted": paletted.SubImage(sub), "generic": genericImage{nrgba},
	}
	for _, ratio := range []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440, image.YCbCrSubsampleRatio411, image.YCbCrSubsampleRatio410,
	} {
		src := image.NewYCbCr(r, ratio)
		for i := range src.Y {
			src.Y[i] = uint8(i * 31)
		}
		for i := range src.Cb {
			src.Cb[i], src.Cr[i] = uint8(i*7), uint8(i*19)
		}
		sources[ratio.String()] = src.SubImage(sub)
	}
	return sources
}

func assertSamePixels(t *testing.T, got, want *image.RGBA) {
	t.Helper()
	if got.Bounds().Size() != want.Bounds().Size() {
		t.Fatalf("got size %v, want %v", got.Bounds().Size(), want.Bounds().Size())
	}
	for y := range want.Bounds().Dy() {
		for x := range want.Bounds().Dx() {
			g := got.RGBAAt(got.Rect.Min.X+x, got.Rect.Min.Y+y)
			w := want.RGBAAt(want.Rect.Min.X+x, want.Rect.Min.Y+y)
			if g != w {
				t.Fatalf("pixel %d,%d: got %v, want %v", x, y, g, w)
			}
		}
	}
}

func TestPreparePixelsMatchesOriginal(t *testing.T) {
	for _, shape := range []struct {
		name                            string
		width, height, paddedW, paddedH int
	}{
		{"unchanged", 37, 23, 37, 23},
		{"downscale", 17, 11, 17, 11},
		{"single-column", 1, 17, 1, 17},
		{"single-row", 19, 1, 19, 1},
		{"portrait-padding", 37, 23, 42, 23},
		{"landscape-padding", 37, 23, 37, 28},
		{"downscale-padding", 17, 11, 22, 16},
	} {
		for name, src := range preparationSources() {
			t.Run(shape.name+"/"+name, func(t *testing.T) {
				want := referencePreparePixels(src, shape.width, shape.height, shape.paddedW, shape.paddedH)
				got := preparePixels(src, shape.width, shape.height, shape.paddedW, shape.paddedH)
				assertSamePixels(t, got, want)
				var wantJPEG, gotJPEG bytes.Buffer
				if err := jpeg.Encode(&wantJPEG, want, nil); err != nil {
					t.Fatal(err)
				}
				if err := jpeg.Encode(&gotJPEG, got, nil); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotJPEG.Bytes(), wantJPEG.Bytes()) {
					t.Fatal("JPEG encoding differs from original pixels")
				}
			})
		}
	}
}

func TestPreparePixelsFlattensEveryAlphaAndChannelValue(t *testing.T) {
	for _, straight := range []bool{false, true} {
		t.Run(fmt.Sprintf("straight-alpha=%v", straight), func(t *testing.T) {
			var src stddraw.Image = image.NewRGBA(image.Rect(0, 0, 256, 256))
			if straight {
				src = image.NewNRGBA(src.Bounds())
			}
			for a := range 256 {
				for c := range 256 {
					if straight {
						src.Set(c, a, color.NRGBA{R: uint8(c), G: uint8(255 - c), B: uint8(c * 7), A: uint8(a)})
					} else {
						src.Set(c, a, color.RGBA{R: uint8(c), G: uint8(255 - c), B: uint8(c * 7), A: uint8(a)})
					}
				}
			}
			want := referencePreparePixels(src, 256, 256, 256, 256)
			got := preparePixels(src, 256, 256, 256, 256)
			assertSamePixels(t, got, want)
		})
	}
}

func TestPreparePixelsDoesNotChangeSubimageNeighbors(t *testing.T) {
	for _, straight := range []bool{false, true} {
		t.Run(fmt.Sprintf("straight-alpha=%v", straight), func(t *testing.T) {
			r := image.Rect(-5, -3, 11, 13)
			sub := image.Rect(-2, 1, 7, 9)
			parent := image.NewNRGBA(r)
			for i := range parent.Pix {
				parent.Pix[i] = uint8(i * 19)
			}
			before := bytes.Clone(parent.Pix)
			var src image.Image = parent.SubImage(sub)
			if !straight {
				rgba := &image.RGBA{Pix: parent.Pix, Stride: parent.Stride, Rect: r}
				src = rgba.SubImage(sub)
			}
			preparePixels(src, sub.Dx(), sub.Dy(), sub.Dx(), sub.Dy())
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					if !image.Pt(x, y).In(sub) {
						i := parent.PixOffset(x, y)
						if !bytes.Equal(parent.Pix[i:i+4], before[i:i+4]) {
							t.Fatalf("changed pixel outside subimage: %d,%d", x, y)
						}
					}
				}
			}
		})
	}
}

func TestPreparePNGMatchesOriginalJPEGAndPreservesSource(t *testing.T) {
	for _, test := range []struct {
		name  string
		mode  mode
		w, h  int
		alpha bool
	}{
		{"opaque-document", documentMode, 41, 29, false},
		{"transparent-document", documentMode, 41, 29, true},
		{"transparent-thumbnail", thumbnailMode, 401, 299, true},
		{"opaque-thumbnail", thumbnailMode, 401, 299, false},
		{"transparent-wide-photo", photoMode, 401, 5, true},
		{"opaque-tall-photo", photoMode, 5, 401, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := image.NewNRGBA(image.Rect(0, 0, test.w, test.h))
			for y := range test.h {
				for x := range test.w {
					a := uint8(255)
					if test.alpha {
						a = uint8(x*37 + y*19)
					}
					src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 13), G: uint8(y * 29), B: uint8(x*7 + y*17), A: a})
				}
			}
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, src); err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(encoded.Bytes())
			decoded, err := png.Decode(bytes.NewReader(original))
			if err != nil {
				t.Fatal(err)
			}
			w, h, pw, ph := outputSize(test.w, test.h, test.mode)
			pixels := referencePreparePixels(decoded, w, h, pw, ph)
			var want bytes.Buffer
			quality := 85
			if test.mode == thumbnailMode {
				quality = 80
			}
			if err := jpeg.Encode(&want, pixels, &jpeg.Options{Quality: quality}); err != nil {
				t.Fatal(err)
			}
			source := spool.Bytes(encoded.Bytes())
			defer source.Close()
			p := NewProcessor(1, 64<<20)
			var got Prepared
			if test.mode == documentMode {
				got, err = p.PrepareDocument(context.Background(), source, "source.png")
			} else {
				got, err = p.Prepare(context.Background(), source, "source.png", test.mode == thumbnailMode)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer got.Cleanup()
			if !got.Owned || got.Data == source || got.Filename != "source.jpg" || got.ContentType != "image/jpeg" {
				t.Fatalf("unexpected prepared metadata: %+v", got)
			}
			if !bytes.Equal(got.Data.Bytes(), want.Bytes()) {
				t.Fatal("JPEG encoding differs from original conversion")
			}
			if !bytes.Equal(source.Bytes(), original) {
				t.Fatal("preparation changed encoded source")
			}
			assertOwnsOnlyJPEG(t, got, pw, ph)
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("conversion leaked image memory reservation")
			}
			p.memory.Release(p.budget)
		})
	}
}

func TestPrepareRejectsCorruptPixelStreamsInEveryMode(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 80, 40))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 31)
	}
	var pngData, jpegData bytes.Buffer
	if err := png.Encode(&pngData, src); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, src, nil); err != nil {
		t.Fatal(err)
	}
	webpData, err := os.ReadFile("testdata/static.webp")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"png": pngData.Bytes(), "jpeg": jpegData.Bytes(), "webp": webpData} {
		broken := data[:len(data)/2]
		if _, _, err := image.DecodeConfig(bytes.NewReader(broken)); err != nil {
			t.Fatalf("%s fixture lacks valid headers: %v", name, err)
		}
		if _, _, err := image.Decode(bytes.NewReader(broken)); err == nil {
			t.Fatalf("%s fixture has valid pixels", name)
		}
		for _, m := range []mode{photoMode, thumbnailMode, documentMode} {
			t.Run(fmt.Sprintf("%s/mode=%d", name, m), func(t *testing.T) {
				p := NewProcessor(1, 64<<20)
				source := spool.Bytes(broken)
				defer source.Close()
				var prepared Prepared
				var err error
				if m == documentMode {
					prepared, err = p.PrepareDocument(context.Background(), source, "bad."+name)
				} else {
					prepared, err = p.Prepare(context.Background(), source, "bad."+name, m == thumbnailMode)
				}
				if err == nil || prepared.Data != nil {
					prepared.Cleanup()
					t.Fatal("accepted corrupt pixels", err)
				}
				if len(p.slots) != 0 || !p.memory.TryAcquire(p.budget) {
					t.Fatal("corrupt image leaked processing reservation")
				}
				p.memory.Release(p.budget)
			})
		}
	}
}
