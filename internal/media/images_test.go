package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/spool"
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
func load(t *testing.T, path string) *spool.Buffer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return spool.Bytes(data)
}
func configOf(t *testing.T, prepared Prepared) image.Config {
	t.Helper()
	c, _, err := image.DecodeConfig(bytes.NewReader(prepared.Data.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPhotoPreservesNativeJPEGBytes(t *testing.T) {
	path := writeImage(t, 600, 400, "jpg")
	input, _ := os.ReadFile(path)
	prepared, err := NewProcessor(2, 1<<30).Prepare(context.Background(), load(t, path), "photo.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	output := prepared.Data.Bytes()
	if prepared.Owned || prepared.ContentType != "image/jpeg" || prepared.Filename != "photo.jpg" || !bytes.Equal(input, output) {
		t.Fatal("native photo changed")
	}
}

func TestThumbnailPNGBecomesBoundedJPEG(t *testing.T) {
	path := writeImage(t, 800, 600, "png")
	prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, path), "cover.png", true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared)
	if prepared.ContentType != "image/jpeg" || prepared.Filename != "cover.jpg" || config.Width != 320 || config.Height != 240 || prepared.Size >= 200000 || !prepared.Owned {
		t.Fatalf("%#v %#v", prepared, config)
	}
	data := prepared.Data.Bytes()
	if !bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatal("not JPEG")
	}
	assertOwnsOnlyJPEG(t, prepared, 320, 240)
}

func TestPhotoPadsExtremeAspectRatio(t *testing.T) {
	path := writeImage(t, 1000, 10, "png")
	prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, path), "wide.png", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared)
	if config.Width != 1000 || config.Height != 50 || prepared.ContentType != "image/jpeg" {
		t.Fatalf("%#v %#v", prepared, config)
	}
	assertOwnsOnlyJPEG(t, prepared, 1000, 50)
}

// assertOwnsOnlyJPEG checks that a converted image keeps no encoder capacity
// beyond its final bytes, since prepared albums hold every result until upload.
func assertOwnsOnlyJPEG(t *testing.T, prepared Prepared, width, height int) {
	t.Helper()
	data := prepared.Data.Bytes()
	if int64(len(data)) != prepared.Size || cap(data) != len(data) {
		t.Fatalf("converted JPEG len %d cap %d size %d", len(data), cap(data), prepared.Size)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("decoded %v", img.Bounds())
	}
}

func TestHEICUsesNativeDecoder(t *testing.T) {
	prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, "testdata/sample.heic"), "sample.heic", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared)
	if prepared.ContentType != "image/jpeg" || config.Width <= 0 || config.Height <= 0 {
		t.Fatalf("%#v %#v", prepared, config)
	}
}

func TestCorruptImagesFailAndCancelledQueueStops(t *testing.T) {
	processor := NewProcessor(1, 1<<30)
	path := filepath.Join(t.TempDir(), "bad.jpg")
	if err := os.WriteFile(path, []byte{0xff, 0xd8, 0xff, 0, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.Prepare(context.Background(), load(t, path), "bad.jpg", false); err == nil {
		t.Fatal("accepted corrupt JPEG")
	}
	processor.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := processor.Prepare(ctx, load(t, path), "x.jpg", false); err != context.Canceled {
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
	prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, path), "rotated.jpg", true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	config := configOf(t, prepared)
	if config.Width != 40 || config.Height != 80 {
		t.Fatalf("orientation not applied: %#v", config)
	}
}

func TestAnimatedImagesBecomeSingleJPEGFrames(t *testing.T) {
	for _, name := range []string{"animated.webp", "animated.png"} {
		t.Run(name, func(t *testing.T) {
			prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, filepath.Join("testdata", name)), name, false)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Cleanup()
			if !prepared.Owned || prepared.ContentType != "image/jpeg" {
				t.Fatalf("animation forwarded: %#v", prepared)
			}
			img, err := jpeg.Decode(bytes.NewReader(prepared.Data.Bytes()))
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
	if result, err := NewProcessor(1, 1<<30).Prepare(context.Background(), load(t, path), "cover.jpg", true); err == nil {
		result.Cleanup()
		t.Fatal("truncated JPEG accepted as a thumbnail")
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	source := load(t, path)
	result, err := NewProcessor(1, 1<<30).Prepare(context.Background(), source, "cover.jpg", true)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Cleanup()
	if result.Owned || result.Data != source {
		t.Fatal("valid thumbnail was unnecessarily re-encoded")
	}
}

func TestDiskBackedHEICKeepsHeader(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	for _, thumbnail := range []bool{false, true} {
		source, err := spool.NewStore(t.TempDir(), 0, 0).New(int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = source.Writer(0, -1).Write(data); err != nil || source.Complete() != nil || source.InMemory() {
			t.Fatal("HEIC fixture not on disk", err)
		}
		prepared, err := NewProcessor(1, 1<<30).Prepare(context.Background(), source, "sample.heic", thumbnail)
		source.Close()
		if err != nil {
			t.Fatal(thumbnail, err)
		}
		config := configOf(t, prepared)
		prepared.Cleanup()
		if prepared.ContentType != "image/jpeg" || prepared.Filename != "sample.jpg" || config.Width <= 0 || config.Height <= 0 {
			t.Fatalf("%v %#v %#v", thumbnail, prepared, config)
		}
		if thumbnail && max(config.Width, config.Height) > 320 {
			t.Fatalf("thumbnail %dx%d", config.Width, config.Height)
		}
	}
}

func TestDecodingWaitsForImageMemory(t *testing.T) {
	read := func(path string) []byte {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	photo := read(writeImage(t, 600, 400, "jpg"))
	scan := bytes.Index(photo, []byte{0xff, 0xda})
	brokenPixels := photo[:scan+2+int(binary.BigEndian.Uint16(photo[scan+2:scan+4]))]
	for _, input := range []struct {
		name      string
		data      []byte
		thumbnail bool
		corrupt   bool
	}{
		{"validated JPEG", photo, false, false},
		{"JPEG with corrupt pixels", brokenPixels, false, true},
		{"converted PNG", read(writeImage(t, 800, 600, "png")), true, false},
		{"animated WebP", read("testdata/animated.webp"), false, false},
		{"animated PNG", read("testdata/animated.png"), false, false},
		{"HEIC", read("testdata/sample.heic"), false, false},
	} {
		t.Run(input.name, func(t *testing.T) {
			p := NewProcessor(32, 64<<20)
			// Other decodes hold the whole budget.
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("budget unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			_, err := p.Prepare(ctx, spool.Bytes(input.data), input.name, input.thumbnail)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("decoded without image memory: %v", err)
			}
			p.memory.Release(p.budget)
			prepared, err := p.Prepare(context.Background(), spool.Bytes(input.data), input.name, input.thumbnail)
			if input.corrupt != (err != nil) {
				t.Fatalf("corrupt=%v: %v", input.corrupt, err)
			}
			prepared.Cleanup()
			// Cancellation, failure, and success each return their reservation.
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("image memory leaked")
			}
		})
	}
}

func TestImageMemoryAdmitsSmallImagesTogetherAndOversizedAlone(t *testing.T) {
	small := load(t, writeImage(t, 16, 16, "png")).Bytes()
	large := load(t, writeImage(t, 1000, 1000, "png")).Bytes()
	p := NewProcessor(32, 2<<20)
	if !p.memory.TryAcquire(1 << 20) {
		t.Fatal("budget unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		group.Go(func() {
			prepared, err := p.Prepare(ctx, spool.Bytes(small), "small.png", true)
			prepared.Cleanup()
			errs <- err
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("small image did not fit beside other work:", err)
		}
	}
	waiting, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := p.Prepare(waiting, spool.Bytes(large), "large.png", false)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("large image decoded beside other work: %v", err)
	}
	p.memory.Release(1 << 20)
	// The large image needs more than the whole budget, so it runs alone.
	prepared, err := p.Prepare(ctx, spool.Bytes(large), "large.png", false)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Cleanup()
	if !p.memory.TryAcquire(p.budget) {
		t.Fatal("image memory leaked")
	}
}

// heifSource returns a RAM or disk buffer holding the first n bytes of data,
// and the writer for the rest.
func heifSource(t *testing.T, data []byte, n int, disk bool) (*spool.Buffer, *spool.Writer) {
	t.Helper()
	limit := int64(1 << 20)
	if disk {
		limit = 0
	}
	source, err := spool.NewStore(t.TempDir(), limit, limit).New(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.Close)
	writer := source.Writer(0, int64(len(data)))
	if _, err = writer.Write(data[:n]); err != nil || source.InMemory() == disk {
		t.Fatal("HEIC fixture not stored as requested", err)
	}
	return source, writer
}

func TestHEIFReservesMemoryBeforeReadingPayload(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Index(data, []byte("mdat")) - 4
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "RAM", true: "disk"}[disk], func(t *testing.T) {
			// Only the metadata has arrived, so reading the encoded image fails.
			source, writer := heifSource(t, data, payload, disk)
			p := NewProcessor(32, 64<<20)
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("budget unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			_, err := p.Prepare(ctx, source, "sample.heic", false)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("read encoded image before reserving memory: %v", err)
			}
			p.memory.Release(p.budget)
			if _, err = p.Prepare(context.Background(), source, "sample.heic", false); !errors.Is(err, spool.ErrIncomplete) {
				t.Fatalf("missing payload: %v", err)
			}
			if _, err = writer.Write(data[payload:]); err != nil || source.Complete() != nil {
				t.Fatal(err)
			}
			prepared, err := p.Prepare(context.Background(), source, "sample.heic", true)
			if err != nil {
				t.Fatal(err)
			}
			prepared.Cleanup()
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("image memory leaked")
			}
		})
	}
}

func TestHEIFDimensionsComeFromMetadata(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	// The coded image is 64x64; its clean aperture crops it to 32x24.
	if w, h, err := heifSize(bytes.NewReader(data), int64(len(data))); err != nil || w != 64 || h != 64 {
		t.Fatal(w, h, err)
	}
	payload := bytes.Index(data, []byte("mdat")) - 4
	for name, input := range map[string][]byte{
		"truncated metadata": data[:payload-40],
		"corrupt payload":    append(bytes.Clone(data[:payload+8]), make([]byte, len(data)-payload-8)...),
	} {
		t.Run(name, func(t *testing.T) {
			p := NewProcessor(32, 64<<20)
			if _, err := p.Prepare(context.Background(), spool.Bytes(input), "bad.heic", false); err == nil {
				t.Fatal("accepted invalid HEIF")
			}
			if !p.memory.TryAcquire(p.budget) {
				t.Fatal("image memory leaked")
			}
		})
	}
}

func TestConcurrentHEIFAdmissionCompletes(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	// About two images fit in the first budget; each image needs more than
	// the second, so those run one at a time.
	for _, budget := range []int64{300_000, 100_000} {
		p := NewProcessor(32, budget)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var group sync.WaitGroup
		errs := make(chan error, 64)
		for i := range 64 {
			source, _ := heifSource(t, data, len(data), i%2 == 0)
			if err := source.Complete(); err != nil {
				t.Fatal(err)
			}
			group.Go(func() {
				prepared, err := p.Prepare(ctx, source, "sample.heic", i%4 < 2)
				prepared.Cleanup()
				errs <- err
			})
		}
		group.Wait()
		cancel()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(budget, err)
			}
		}
		if !p.memory.TryAcquire(p.budget) {
			t.Fatal("image memory leaked")
		}
	}
}
