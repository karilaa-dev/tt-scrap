package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"os"
)

func animatedPNG(file *os.File) bool {
	if _, err := file.Seek(8, 0); err != nil {
		return false
	}
	var header [8]byte
	for count := 0; count < 1024; count++ {
		if _, err := io.ReadFull(file, header[:]); err != nil {
			return false
		}
		switch string(header[4:]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		if _, err := file.Seek(int64(binary.BigEndian.Uint32(header[:4]))+4, io.SeekCurrent); err != nil {
			return false
		}
	}
	return false
}

func little24(data []byte) int { return int(data[0]) | int(data[1])<<8 | int(data[2])<<16 }
func putLittle24(data []byte, value int) {
	data[0] = byte(value)
	data[1] = byte(value >> 8)
	data[2] = byte(value >> 16)
}

// A WebP animation embeds ordinary WebP chunks inside each ANMF frame.
// Decode only the first frame and composite it onto the declared canvas.
func firstWebPFrame(file *os.File, prefix []byte) (image.Image, error) {
	if len(prefix) < 30 {
		return nil, fmt.Errorf("truncated WebP canvas")
	}
	w, h := little24(prefix[24:27])+1, little24(prefix[27:30])+1
	if w <= 0 || h <= 0 || int64(w)*int64(h) > maxPixels {
		return nil, fmt.Errorf("WebP dimensions exceed limits")
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if _, err = file.Seek(12, 0); err != nil {
		return nil, err
	}
	var background color.NRGBA
	for count := 0; count < 1024; count++ {
		var chunk [8]byte
		if _, err = io.ReadFull(file, chunk[:]); err != nil {
			return nil, err
		}
		length := int64(binary.LittleEndian.Uint32(chunk[4:]))
		start, _ := file.Seek(0, io.SeekCurrent)
		if start+length > stat.Size() {
			return nil, fmt.Errorf("truncated WebP chunk")
		}
		switch string(chunk[:4]) {
		case "ANIM":
			var data [6]byte
			if length < 6 {
				return nil, fmt.Errorf("truncated WebP animation")
			}
			if _, err = io.ReadFull(file, data[:]); err != nil {
				return nil, err
			}
			background = color.NRGBA{R: data[2], G: data[1], B: data[0], A: data[3]}
		case "ANMF":
			if length < 24 {
				return nil, fmt.Errorf("truncated WebP frame")
			}
			var frame [20]byte
			if _, err = io.ReadFull(file, frame[:]); err != nil {
				return nil, err
			}
			x, y := 2*little24(frame[:3]), 2*little24(frame[3:6])
			fw, fh := little24(frame[6:9])+1, little24(frame[9:12])+1
			if x+fw > w || y+fh > h {
				return nil, fmt.Errorf("WebP frame exceeds canvas")
			}
			header := make([]byte, 30)
			copy(header, "RIFF")
			binary.LittleEndian.PutUint32(header[4:], uint32(22+length-16))
			copy(header[8:], "WEBPVP8X")
			binary.LittleEndian.PutUint32(header[16:], 10)
			if string(frame[16:20]) == "ALPH" {
				header[20] = 0x10
			}
			putLittle24(header[24:], fw-1)
			putLittle24(header[27:], fh-1)
			reader := func() io.Reader {
				return io.MultiReader(bytes.NewReader(header), io.NewSectionReader(file, start+16, length-16))
			}
			config, _, err := image.DecodeConfig(reader())
			if err != nil {
				return nil, err
			}
			if config.Width != fw || config.Height != fh {
				return nil, fmt.Errorf("WebP frame dimensions mismatch")
			}
			decoded, _, err := image.Decode(reader())
			if err != nil {
				return nil, err
			}
			canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
			draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
			operation := draw.Over
			if frame[15]&2 != 0 {
				operation = draw.Src
			}
			draw.Draw(canvas, image.Rect(x, y, x+fw, y+fh), decoded, decoded.Bounds().Min, operation)
			return canvas, nil
		}
		if _, err = file.Seek(start+length+(length&1), 0); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("WebP has no decodable frame")
}
