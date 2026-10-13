package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const maxPixels = 100_000_000
const photoMaxBytes = 10 * 1024 * 1024

// Prepared refers to a file, so concurrent Telegram uploads need not buffer media.
type Prepared struct {
	Path        string
	Filename    string
	ContentType string
	Size        int64
	Owned       bool
}

func (p Prepared) Cleanup() {
	if p.Owned {
		_ = os.Remove(p.Path)
	}
}

// Processor bounds decoded image memory independently of HTTP download concurrency.
type Processor struct {
	slots     chan struct{}
	directory string
}

func NewProcessor(concurrency int, directory string) *Processor {
	if concurrency < 1 {
		concurrency = min(runtime.GOMAXPROCS(0), 4)
	}
	return &Processor{slots: make(chan struct{}, concurrency), directory: directory}
}

func FilenameForType(name, contentType string) string {
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif", "image/heic": ".heic", "image/heif": ".heif", "image/avif": ".avif", "image/bmp": ".bmp", "image/tiff": ".tiff", "video/mp4": ".mp4", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/ogg": ".ogg"}[strings.Split(contentType, ";")[0]]
	if ext == "" {
		return name
	}
	return strings.TrimSuffix(name, filepath.Ext(name)) + ext
}

func DetectImageType(prefix []byte) string {
	switch {
	case bytes.HasPrefix(prefix, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(prefix, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(prefix, []byte("GIF87a")) || bytes.HasPrefix(prefix, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(prefix, []byte("II*\x00")) || bytes.HasPrefix(prefix, []byte("MM\x00*")):
		return "image/tiff"
	case bytes.HasPrefix(prefix, []byte("BM")):
		return "image/bmp"
	case len(prefix) >= 12 && string(prefix[4:8]) == "ftyp":
		switch string(prefix[8:12]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "hevc", "hevx", "heim", "heis":
			return "image/heic"
		case "mif1", "msf1":
			return "image/heif"
		}
	}
	return ""
}

func (p *Processor) Prepare(ctx context.Context, path, filename string, thumbnail bool) (Prepared, error) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return Prepared{}, ctx.Err()
	}
	f, err := os.Open(path)
	if err != nil {
		return Prepared{}, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return Prepared{}, err
	}
	var prefix [32]byte
	n, _ := io.ReadFull(f, prefix[:])
	kind := DetectImageType(prefix[:n])
	if kind == "" {
		return Prepared{}, fmt.Errorf("unsupported image format")
	}
	var img image.Image
	var width, height int
	native := kind == "image/jpeg" || kind == "image/png" || kind == "image/webp"
	animatedWebP := kind == "image/webp" && n > 20 && string(prefix[12:16]) == "VP8X" && prefix[20]&2 != 0
	if animatedWebP || kind == "image/png" && animatedPNG(f) {
		native = false
	}
	if animatedWebP {
		img, err = firstWebPFrame(f, prefix[:n])
		if err != nil {
			return Prepared{}, err
		}
		width, height = img.Bounds().Dx(), img.Bounds().Dy()
	} else if kind == "image/heic" || kind == "image/heif" || kind == "image/avif" {
		img, err = decodeHEIF(path)
		if err != nil {
			return Prepared{}, err
		}
		width, height = img.Bounds().Dx(), img.Bounds().Dy()
	} else {
		if _, err = f.Seek(0, 0); err != nil {
			return Prepared{}, err
		}
		config, _, configErr := image.DecodeConfig(f)
		if configErr != nil {
			return Prepared{}, configErr
		}
		width, height = config.Width, config.Height
	}
	if width <= 0 || height <= 0 || int64(width)*int64(height) > maxPixels {
		return Prepared{}, fmt.Errorf("image dimensions exceed limits")
	}
	if thumbnail && kind == "image/jpeg" && width <= 320 && height <= 320 && stat.Size() < 200000 {
		return Prepared{Path: path, Filename: FilenameForType(filename, kind), ContentType: kind, Size: stat.Size()}, nil
	}
	if !thumbnail && native && stat.Size() <= photoMaxBytes && width+height <= 10000 && max(float64(width)/float64(height), float64(height)/float64(width)) <= 20 {
		// Validate the pixel stream before forwarding a nominally valid image.
		if _, err = f.Seek(0, 0); err != nil {
			return Prepared{}, err
		}
		if _, _, err = image.Decode(f); err != nil {
			return Prepared{}, err
		}
		return Prepared{Path: path, Filename: FilenameForType(filename, kind), ContentType: kind, Size: stat.Size()}, nil
	}
	if img == nil {
		if _, err = f.Seek(0, 0); err != nil {
			return Prepared{}, err
		}
		img, _, err = image.Decode(f)
		if err != nil {
			return Prepared{}, err
		}
		img = orient(img, readOrientation(f, kind))
	}
	if err = ctx.Err(); err != nil {
		return Prepared{}, err
	}
	width, height = img.Bounds().Dx(), img.Bounds().Dy()
	if thumbnail {
		scale := min(1.0, 320.0/float64(max(width, height)))
		width = max(1, int(float64(width)*scale))
		height = max(1, int(float64(height)*scale))
	} else if width+height > 10000 {
		scale := 10000.0 / float64(width+height)
		width = max(1, int(float64(width)*scale))
		height = max(1, int(float64(height)*scale))
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	if !thumbnail && (width > height*20 || height > width*20) {
		paddedW, paddedH := width, height
		if width > height*20 {
			paddedH = (width + 19) / 20
		} else {
			paddedW = (height + 19) / 20
		}
		padded := image.NewRGBA(image.Rect(0, 0, paddedW, paddedH))
		stddraw.Draw(padded, padded.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
		offset := image.Pt((paddedW-width)/2, (paddedH-height)/2)
		stddraw.Draw(padded, dst.Bounds().Add(offset), dst, image.Point{}, stddraw.Src)
		dst = padded
		width, height = paddedW, paddedH
	}
	limit, quality := int64(photoMaxBytes), 85
	if thumbnail {
		limit = 199999
		quality = 80
	}
	output, err := os.CreateTemp(p.directory, "tt-scrap-image-*.jpg")
	if err != nil {
		return Prepared{}, err
	}
	name := output.Name()
	succeeded := false
	defer func() {
		_ = output.Close()
		if !succeeded {
			_ = os.Remove(name)
		}
	}()
	var size int64
	for attempt := 0; attempt < 4; attempt++ {
		if err = ctx.Err(); err != nil {
			return Prepared{}, err
		}
		if err = output.Truncate(0); err != nil {
			return Prepared{}, err
		}
		if _, err = output.Seek(0, 0); err != nil {
			return Prepared{}, err
		}
		writer := bufio.NewWriterSize(output, 64*1024)
		if err = jpeg.Encode(writer, dst, &jpeg.Options{Quality: quality}); err != nil {
			return Prepared{}, err
		}
		if err = writer.Flush(); err != nil {
			return Prepared{}, err
		}
		size, err = output.Seek(0, io.SeekCurrent)
		if err != nil {
			return Prepared{}, err
		}
		if size <= limit {
			break
		}
		quality = 75
		if attempt >= 1 {
			scale := math.Sqrt(float64(limit)/float64(size)) * 0.95
			width = max(1, int(float64(width)*scale))
			height = max(1, int(float64(height)*scale))
			resized := image.NewRGBA(image.Rect(0, 0, width, height))
			draw.ApproxBiLinear.Scale(resized, resized.Bounds(), dst, dst.Bounds(), draw.Src, nil)
			dst = resized
		}
	}
	if size > limit {
		return Prepared{}, fmt.Errorf("converted image exceeds size limit")
	}
	if err = output.Close(); err != nil {
		return Prepared{}, err
	}
	succeeded = true
	return Prepared{Path: name, Filename: FilenameForType(filename, "image/jpeg"), ContentType: "image/jpeg", Size: size, Owned: true}, nil
}

func readOrientation(f *os.File, kind string) int {
	if kind != "image/jpeg" {
		return 1
	}
	if _, err := f.Seek(2, 0); err != nil {
		return 1
	}
	var marker [4]byte
	for count := 0; count < 128; count++ {
		if _, err := io.ReadFull(f, marker[:]); err != nil || marker[0] != 0xff {
			return 1
		}
		if marker[1] == 0xda || marker[1] == 0xd9 {
			return 1
		}
		length := int(binary.BigEndian.Uint16(marker[2:])) - 2
		if length < 0 {
			return 1
		}
		if marker[1] != 0xe1 {
			if _, err := f.Seek(int64(length), io.SeekCurrent); err != nil {
				return 1
			}
			continue
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(f, data); err != nil {
			return 1
		}
		if len(data) < 14 || string(data[:6]) != "Exif\x00\x00" {
			continue
		}
		data = data[6:]
		var order binary.ByteOrder = binary.LittleEndian
		if string(data[:2]) == "MM" {
			order = binary.BigEndian
		} else if string(data[:2]) != "II" {
			return 1
		}
		offset := int(order.Uint32(data[4:8]))
		if offset < 0 || offset+2 > len(data) {
			return 1
		}
		entries := int(order.Uint16(data[offset : offset+2]))
		for i := 0; i < entries; i++ {
			start := offset + 2 + i*12
			if start+12 > len(data) {
				break
			}
			if order.Uint16(data[start:]) == 0x112 && order.Uint16(data[start+2:]) == 3 {
				return int(order.Uint16(data[start+8:]))
			}
		}
	}
	return 1
}

func orient(img image.Image, orientation int) image.Image {
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
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
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
