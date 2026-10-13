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
	"path/filepath"
	"testing"
)

func writeImage(t *testing.T, width, height int, format string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x % 256), B: uint8(y % 256), A: 128})
		}
	}
	path := filepath.Join(t.TempDir(), "source."+format)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if format == "jpg" {
		err = jpeg.Encode(file, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(file, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
func configOf(t *testing.T, path string) image.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPhotoPreservesNativeJPEGBytes(t *testing.T) {
	path := writeImage(t, 600, 400, "jpg")
	input, _ := os.ReadFile(path)
	prepared, err := NewProcessor(2, "").Prepare(context.Background(), path, "photo.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	output, _ := os.ReadFile(prepared.Path)
	if prepared.Owned || prepared.ContentType != "image/jpeg" || prepared.Filename != "photo.jpg" || !bytes.Equal(input, output) {
		t.Fatal("native photo changed")
	}
}

func TestThumbnailPNGBecomesBoundedJPEG(t *testing.T) {
	path := writeImage(t, 800, 600, "png")
	directory := t.TempDir()
	prepared, err := NewProcessor(1, directory).Prepare(context.Background(), path, "cover.png", true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared.Path)
	if prepared.ContentType != "image/jpeg" || prepared.Filename != "cover.jpg" || config.Width != 320 || config.Height != 240 || prepared.Size >= 200000 || !prepared.Owned || filepath.Dir(prepared.Path) != directory {
		t.Fatalf("%#v %#v", prepared, config)
	}
	data, _ := os.ReadFile(prepared.Path)
	if !bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatal("not JPEG")
	}
}

func TestPhotoPadsExtremeAspectRatio(t *testing.T) {
	path := writeImage(t, 1000, 10, "png")
	prepared, err := NewProcessor(1, "").Prepare(context.Background(), path, "wide.png", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared.Path)
	if config.Width != 1000 || config.Height != 50 || prepared.ContentType != "image/jpeg" {
		t.Fatalf("%#v %#v", prepared, config)
	}
}

func TestHEICUsesNativeDecoder(t *testing.T) {
	prepared, err := NewProcessor(1, "").Prepare(context.Background(), "testdata/sample.heic", "sample.heic", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared.Path)
	if prepared.ContentType != "image/jpeg" || config.Width <= 0 || config.Height <= 0 {
		t.Fatalf("%#v %#v", prepared, config)
	}
}

func TestCorruptImagesFailAndCancelledQueueStops(t *testing.T) {
	processor := NewProcessor(1, "")
	path := filepath.Join(t.TempDir(), "bad.jpg")
	if err := os.WriteFile(path, []byte{0xff, 0xd8, 0xff, 0, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.Prepare(context.Background(), path, "bad.jpg", false); err == nil {
		t.Fatal("accepted corrupt JPEG")
	}
	processor.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := processor.Prepare(ctx, path, "x.jpg", false); err != context.Canceled {
		t.Fatal(err)
	}
	<-processor.slots
}

func TestJPEGOrientationAppliedToThumbnail(t *testing.T) {
	path := writeImage(t, 80, 40, "jpg")
	data, _ := os.ReadFile(path)
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	marker := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(marker[2:], uint16(len(exif)+2))
	withExif := append(append(append(bytes.Clone(data[:2]), marker...), exif...), data[2:]...)
	if err := os.WriteFile(path, withExif, 0600); err != nil {
		t.Fatal(err)
	}
	// Force conversion even though this thumbnail already fits Telegram dimensions.
	if err := os.WriteFile(path, append(withExif, make([]byte, 200000)...), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := NewProcessor(1, "").Prepare(context.Background(), path, "rotated.jpg", true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared.Path)
	if config.Width != 40 || config.Height != 80 {
		t.Fatalf("orientation not applied: %#v", config)
	}
}

func TestAnimatedImagesBecomeSingleJPEGFrames(t *testing.T) {
	for _, name := range []string{"animated.webp", "animated.png"} {
		t.Run(name, func(t *testing.T) {
			prepared, err := NewProcessor(1, "").Prepare(context.Background(), filepath.Join("testdata", name), name, false)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Cleanup()
			if !prepared.Owned || prepared.ContentType != "image/jpeg" {
				t.Fatalf("animation forwarded: %#v", prepared)
			}
			f, err := os.Open(prepared.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			img, err := jpeg.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 12 {
				t.Fatal(img.Bounds())
			}
			r, _, b, _ := img.At(8, 6).RGBA()
			if r < 50000 || b > 10000 {
				t.Fatal("did not select first red frame")
			}
		})
	}
}

func TestSmallJPEGThumbnailValidatesPixelStream(t *testing.T) {
	path := writeImage(t, 80, 40, "jpg")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(data, []byte{0xff, 0xda})
	if start < 0 {
		t.Fatal("missing JPEG scan")
	}
	cut := start + 2 + int(binary.BigEndian.Uint16(data[start+2:start+4]))
	broken := data[:cut]
	if _, _, err = image.DecodeConfig(bytes.NewReader(broken)); err != nil {
		t.Fatal("fixture lacks valid headers", err)
	}
	if _, _, err = image.Decode(bytes.NewReader(broken)); err == nil {
		t.Fatal("fixture has valid pixels")
	}
	if err = os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := NewProcessor(1, t.TempDir()).Prepare(context.Background(), path, "cover.jpg", true); err == nil {
		result.Cleanup()
		t.Fatal("truncated JPEG accepted as a thumbnail")
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := NewProcessor(1, t.TempDir()).Prepare(context.Background(), path, "cover.jpg", true)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Cleanup()
	if result.Owned || result.Path != path {
		t.Fatal("valid thumbnail was unnecessarily re-encoded")
	}
}
