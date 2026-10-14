package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func box(kind string, body ...[]byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(bytes.Join(body, nil))))
	return append(append(b, kind...), bytes.Join(body, nil)...)
}

func fullBox(kind string, version byte, flags uint32, body ...[]byte) []byte {
	header := binary.BigEndian.AppendUint32(nil, uint32(version)<<24|flags)
	return box(kind, append([][]byte{header}, body...)...)
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func ispe(w, h uint32) []byte { return fullBox("ispe", 0, 0, u32(w), u32(h)) }

// payloadReader serves the given byte ranges of a file and fails the test on
// a read anywhere else, such as the encoded image.
type payloadReader struct {
	t        *testing.T
	segments map[int64][]byte
}

func (r payloadReader) ReadAt(p []byte, off int64) (int, error) {
	for start, data := range r.segments {
		if off >= start && off+int64(len(p)) <= start+int64(len(data)) {
			return copy(p, data[off-start:]), nil
		}
	}
	r.t.Errorf("read %d bytes at %d outside the metadata", len(p), off)
	return 0, io.ErrUnexpectedEOF
}

// heifFile places meta after a 1 GB mdat whose body is never served.
func heifFile(t *testing.T, meta []byte) (io.ReaderAt, int64) {
	ftyp := box("ftyp", []byte("heic"), u32(0), []byte("mif1heic"))
	// An mdat with a 64-bit size.
	mdat := append(append(u32(1), "mdat"...), binary.BigEndian.AppendUint64(nil, 16+1<<30)...)
	head := append(ftyp, mdat...)
	metaAt := int64(len(ftyp)) + 16 + 1<<30
	return payloadReader{t, map[int64][]byte{0: head, metaAt: meta}}, metaAt + int64(len(meta))
}

func TestHEIFSizeParsesPrimaryItemProperties(t *testing.T) {
	// Index 200 needs the wide form of ipma's 15-bit property indexes.
	var padding [][]byte
	for range 197 {
		padding = append(padding, box("free"))
	}
	wideIpco := box("ipco", append([][]byte{ispe(10, 20), ispe(30, 40)}, append(padding, ispe(4000, 3000))...)...)
	for _, test := range []struct {
		name          string
		meta          []byte
		width, height int
	}{
		{"16-bit items", fullBox("meta", 0, 0,
			fullBox("pitm", 0, 0, u16(2)),
			box("iprp",
				box("ipco", ispe(10, 20), box("free"), ispe(300, 200)),
				// Item 1 is a tile; item 2, the primary, takes property 3.
				fullBox("ipma", 0, 0, u32(2), u16(1), []byte{1, 0x81}, u16(2), []byte{2, 0x82, 3}))), 300, 200},
		{"32-bit items and wide indexes", fullBox("meta", 0, 0,
			box("iprp", wideIpco,
				// Item 2 shares the low 16 bits of the primary's ID.
				fullBox("ipma", 1, 1, u32(2), u32(2), []byte{1}, u16(0x8002), u32(0x10002), []byte{1}, u16(0x8000|200))),
			// pitm after iprp, with a 32-bit item ID.
			fullBox("pitm", 1, 0, u32(0x10002))), 4000, 3000},
		{"associations split across ipma boxes", fullBox("meta", 0, 0,
			fullBox("pitm", 0, 0, u16(7)),
			box("iprp",
				fullBox("ipma", 0, 0, u32(1), u16(1), []byte{1, 1}),
				box("ipco", ispe(10, 20), ispe(640, 480)),
				fullBox("ipma", 1, 0, u32(1), u32(7), []byte{1, 0x82}))), 640, 480},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, size := heifFile(t, test.meta)
			if w, h, err := heifSize(r, size); err != nil || w != test.width || h != test.height {
				t.Fatal(w, h, err)
			}
		})
	}
}

func TestHEIFSizeRejectsInvalidMetadata(t *testing.T) {
	iprp := box("iprp", box("ipco", ispe(64, 48)), fullBox("ipma", 0, 0, u32(1), u16(1), []byte{1, 1}))
	missing := "HEIF dimensions missing"
	for _, test := range []struct {
		name, err string
		meta      []byte
	}{
		{"no pitm", "HEIF primary image properties missing", fullBox("meta", 0, 0, iprp)},
		{"primary without properties", "HEIF primary image properties missing", fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(2)), iprp)},
		{"primary without ispe", missing, fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(1)),
			box("iprp", box("ipco", box("free"), ispe(64, 48)), fullBox("ipma", 0, 0, u32(1), u16(1), []byte{1, 1})))},
		{"no ipma", "HEIF properties missing", fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(1)), box("iprp", box("ipco", ispe(64, 48))))},
		{"truncated pitm", missing, fullBox("meta", 0, 0, fullBox("pitm", 0, 0, []byte{0}), iprp)},
		{"truncated 32-bit pitm", missing, fullBox("meta", 0, 0, fullBox("pitm", 1, 0, u16(0)), iprp)},
		{"truncated ispe", missing, fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(1)),
			box("iprp", box("ipco", fullBox("ispe", 0, 0, u32(64))), fullBox("ipma", 0, 0, u32(1), u16(1), []byte{1, 1})))},
		{"truncated ipma", io.ErrUnexpectedEOF.Error(), fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(2)),
			box("iprp", box("ipco", ispe(64, 48)), fullBox("ipma", 0, 0, u32(1), u16(2), []byte{2, 1})))},
		{"child longer than meta", missing, func() []byte {
			meta := fullBox("meta", 0, 0, fullBox("pitm", 0, 0, u16(1)), iprp)
			binary.BigEndian.PutUint32(meta[12:], 100)
			return meta
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, size := heifFile(t, test.meta)
			if w, h, err := heifSize(r, size); err == nil || err.Error() != test.err {
				t.Fatal(w, h, err)
			}
		})
	}
}

func TestAVIFDimensionsMatchDecoder(t *testing.T) {
	// sample.avif is a single 48x40 image from ffmpeg; grid.avif, a 64x48
	// grid of four 32x24 tiles from libheif, whose first ispe is the tiles'.
	for _, test := range []struct {
		file          string
		width, height int
	}{{"sample.avif", 48, 40}, {"grid.avif", 64, 48}} {
		t.Run(test.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + test.file)
			if err != nil {
				t.Fatal(err)
			}
			if w, h, err := heifSize(bytes.NewReader(data), int64(len(data))); err != nil || w != test.width || h != test.height {
				t.Fatal(w, h, err)
			}
			var decodedW, decodedH int
			img, err := decodeHEIF(bytes.NewReader(data), int64(len(data)), func(w, h int) error {
				decodedW, decodedH = w, h
				return nil
			})
			if err != nil || decodedW != test.width || decodedH != test.height || img.Bounds().Dx() != test.width || img.Bounds().Dy() != test.height {
				t.Fatal(decodedW, decodedH, err)
			}
			for _, thumbnail := range []bool{false, true} {
				p := NewProcessor(32, 64<<20)
				prepared, err := p.Prepare(context.Background(), load(t, "testdata/"+test.file), test.file, thumbnail)
				if err != nil {
					t.Fatal(err)
				}
				config := configOf(t, prepared)
				prepared.Cleanup()
				if prepared.ContentType != "image/jpeg" || config.Width != test.width || config.Height != test.height {
					t.Fatalf("%#v %#v", prepared, config)
				}
				if !p.memory.TryAcquire(p.budget) {
					t.Fatal("image memory leaked")
				}
			}
		})
	}
}

type unreadable struct{}

func (unreadable) ReadAt([]byte, int64) (int, error) { return 0, errors.New("read before allocation") }

func TestHEIFAllocationFailureReturnsError(t *testing.T) {
	// No allocator can provide 4 EiB, so the C copy of the file fails.
	_, err := decodeHEIF(unreadable{}, 1<<62, func(int, int) error { return nil })
	if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("cannot allocate %d bytes", int64(1<<62))) && !strings.Contains(err.Error(), "CGO") {
		t.Fatal(err)
	}
}
