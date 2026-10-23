// Package spool holds media bytes in RAM, or in an unlinked temporary file when
// a file is large or the shared memory budget is spent. Range writers fill one
// buffer concurrently while readers stream the contiguous prefix.
package spool

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
)

var (
	ErrClosed     = errors.New("media buffer closed")
	ErrIncomplete = errors.New("media bytes have not arrived")
	errOverflow   = errors.New("media write exceeds its range")
)

const minGrowth = 64 << 10

// Store decides where buffers live. A buffer uses RAM when its size fits the
// per-file limit and the shared budget; otherwise it uses a temporary file.
type Store struct {
	dir       string
	fileLimit int64
	budget    int64
	mu        sync.Mutex
	used      int64
}

func NewStore(dir string, fileLimit, budget int64) *Store {
	return &Store{dir: dir, fileLimit: fileLimit, budget: budget}
}

// Used reports the RAM reserved by buffers and their in-flight writes.
func (s *Store) Used() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

func (s *Store) reserve(total, delta int64) bool {
	if s == nil || total > s.fileLimit {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+delta > s.budget {
		return false
	}
	s.used += delta
	return true
}

func (s *Store) release(n int64) {
	if s != nil && n != 0 {
		s.mu.Lock()
		s.used -= n
		s.mu.Unlock()
	}
}

func (s *Store) tempFile() (*os.File, error) {
	file, err := os.CreateTemp(s.dir, "tt-scrap-media-*")
	if err != nil {
		return nil, err
	}
	// Unlinked files disappear with the last descriptor, even after a crash.
	_ = os.Remove(file.Name())
	return file, nil
}

// New allocates a buffer for size bytes. A negative size grows as one
// sequential writer appends and spills to disk past the RAM limits.
func (s *Store) New(size int64) (*Buffer, error) {
	b := &Buffer{store: s, size: size}
	switch {
	case size < 0:
		b.mem = []byte{}
	case s.reserve(size, size):
		b.mem = make([]byte, size)
		b.reserved = size
	default:
		file, err := s.tempFile()
		if err != nil {
			return nil, err
		}
		b.file = file
	}
	return b, nil
}

// Bytes wraps complete data that is already in RAM.
func Bytes(data []byte) *Buffer {
	return &Buffer{size: int64(len(data)), mem: data, ready: int64(len(data)), complete: true}
}

type region struct{ start, end, written int64 }

type Buffer struct {
	store    *Store
	mu       sync.Mutex
	waiters  chan struct{}
	size     int64
	mem      []byte
	file     *os.File
	reserved int64
	pinned   int
	regions  []*region
	ready    int64
	err      error
	complete bool
	closed   bool
}

// Size is the declared length, or -1 while a growing buffer is incomplete.
func (b *Buffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// InMemory reports whether the bytes live in RAM.
func (b *Buffer) InMemory() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.file == nil
}

// Bytes returns the contents of a complete RAM buffer, or nil.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.complete || b.closed || b.file != nil {
		return nil
	}
	return b.mem[:b.size]
}

// Err reports a failure or closure; nil while healthy.
func (b *Buffer) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Writer returns a sequential writer for [start, end). Writers for disjoint
// ranges may run concurrently. A growing buffer accepts one writer from zero
// with a negative end.
func (b *Buffer) Writer(start, end int64) *Writer {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := &region{start: start, end: end}
	index := sort.Search(len(b.regions), func(i int) bool { return b.regions[i].start >= start })
	b.regions = append(b.regions, nil)
	copy(b.regions[index+1:], b.regions[index:])
	b.regions[index] = r
	return &Writer{b: b, r: r}
}

type Writer struct {
	b *Buffer
	r *region
}

// Offset is the absolute position of the next byte this writer expects.
func (w *Writer) Offset() int64 {
	w.b.mu.Lock()
	defer w.b.mu.Unlock()
	return w.r.start + w.r.written
}

// Remaining is the number of bytes left in a bounded range.
func (w *Writer) Remaining() int64 {
	w.b.mu.Lock()
	defer w.b.mu.Unlock()
	return w.r.end - w.r.start - w.r.written
}

func (w *Writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, r := w.b, w.r
	b.mu.Lock()
	if b.err != nil {
		defer b.mu.Unlock()
		return 0, b.err
	}
	offset := r.start + r.written
	if r.end >= 0 && int64(len(p)) > r.end-offset || b.size >= 0 && offset+int64(len(p)) > b.size {
		b.mu.Unlock()
		return 0, errOverflow
	}
	if b.size < 0 {
		defer b.mu.Unlock()
		return b.appendLocked(r, p)
	}
	mem, file := b.mem, b.file
	if file == nil {
		b.pinned++
	}
	b.mu.Unlock()
	// Disjoint ranges ahead of the ready prefix are never read concurrently.
	if file == nil {
		copy(mem[offset:], p)
		mem = nil
	} else if _, err := file.WriteAt(p, offset); err != nil {
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if file == nil {
		b.unpinLocked()
	}
	if b.err != nil {
		return 0, b.err
	}
	r.written += int64(len(p))
	b.advanceLocked()
	return len(p), nil
}

func (b *Buffer) appendLocked(r *region, p []byte) (int, error) {
	length := r.written + int64(len(p))
	if b.file == nil && length > int64(cap(b.mem)) {
		capacity := max(length, 2*int64(cap(b.mem)), minGrowth)
		if b.store != nil && b.store.fileLimit > 0 {
			capacity = max(length, min(capacity, b.store.fileLimit))
		}
		if b.store.reserve(capacity, capacity-b.reserved) {
			grown := make([]byte, len(b.mem), capacity)
			copy(grown, b.mem)
			b.mem = grown
			b.reserved = capacity
		} else if err := b.spillLocked(); err != nil {
			b.err = err
			b.notifyLocked()
			return 0, err
		}
	}
	if b.file == nil {
		b.mem = append(b.mem, p...)
	} else if _, err := b.file.WriteAt(p, r.written); err != nil {
		return 0, err
	}
	r.written = length
	b.advanceLocked()
	return len(p), nil
}

func (b *Buffer) spillLocked() error {
	file, err := b.store.tempFile()
	if err != nil {
		return err
	}
	if _, err = file.Write(b.mem); err != nil {
		file.Close()
		return err
	}
	b.store.release(b.reserved)
	b.file, b.mem, b.reserved = file, nil, 0
	return nil
}

func (b *Buffer) advanceLocked() {
	ready := int64(0)
	for _, r := range b.regions {
		if r.start > ready {
			break
		}
		ready = max(ready, r.start+r.written)
		if r.end < 0 || r.start+r.written < r.end {
			break
		}
	}
	if ready > b.ready {
		b.ready = ready
		b.notifyLocked()
	}
}

func (b *Buffer) notifyLocked() {
	if b.waiters != nil {
		close(b.waiters)
		b.waiters = nil
	}
}

// Complete marks every byte as written. A growing buffer fixes its size.
func (b *Buffer) Complete() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if b.size < 0 {
		b.size = b.ready
	}
	if b.ready != b.size {
		return io.ErrUnexpectedEOF
	}
	b.complete = true
	b.notifyLocked()
	return nil
}

// Fail stops readers and writers of an incomplete buffer.
func (b *Buffer) Fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.complete && b.err == nil {
		b.err = err
		b.notifyLocked()
	}
}

// Close releases RAM or the temporary file; later reads fail. RAM passed to an
// in-flight Write remains reserved until that Write returns.
func (b *Buffer) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.err = ErrClosed
	b.notifyLocked()
	if b.pinned == 0 {
		b.store.release(b.reserved)
		b.reserved = 0
	}
	// Retried deliveries keep closed buffers referenced; drop the released RAM.
	b.mem = nil
	if b.file != nil {
		_ = b.file.Close()
	}
}

func (b *Buffer) readLocked(p []byte, off int64) (int, error) {
	n := int(min(int64(len(p)), b.ready-off))
	if b.file == nil {
		return copy(p, b.mem[off:off+int64(n)]), nil
	}
	file := b.file
	b.mu.Unlock()
	defer b.mu.Lock()
	return file.ReadAt(p[:n], off)
}

// ReadAt reads bytes that have already arrived. Past the end of complete data
// it returns io.EOF; past the ready prefix of incomplete data, ErrIncomplete.
func (b *Buffer) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative media offset")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	if off >= b.ready {
		switch {
		case b.complete:
			return 0, io.EOF
		case b.err != nil:
			return 0, b.err
		}
		return 0, ErrIncomplete
	}
	n, err := b.readLocked(p, off)
	if err == nil && n < len(p) {
		err = ErrIncomplete
		if b.complete {
			err = io.EOF
		}
	}
	return n, err
}

// Reader streams [start, end) in order, waiting for bytes to arrive. A
// negative end reads until the buffer completes.
func (b *Buffer) Reader(ctx context.Context, start, end int64) io.Reader {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil && b.size >= 0 {
		return &memoryReader{reader: reader{b: b, ctx: ctx, offset: start, end: end}}
	}
	return &reader{b: b, ctx: ctx, offset: start, end: end}
}

type reader struct {
	b           *Buffer
	ctx         context.Context
	offset, end int64
}

// Fixed RAM buffers never move or rewrite their ready prefix. Growing and
// disk buffers keep the copying reader so growth and spills cannot retain
// unaccounted backing arrays, and callers can reuse their scratch buffers.
type memoryReader struct{ reader }

func (b *Buffer) unpinLocked() {
	b.pinned--
	if b.closed && b.pinned == 0 {
		b.store.release(b.reserved)
		b.reserved = 0
	}
}

func (b *Buffer) writePinned(w io.Writer, p []byte) (int, error) {
	defer func() {
		b.mu.Lock()
		b.unpinLocked()
		b.mu.Unlock()
	}()
	return w.Write(p)
}

func (r *memoryReader) WriteTo(w io.Writer) (int64, error) {
	var written int64
	b := r.b
	for r.end < 0 || r.offset < r.end {
		b.mu.Lock()
		if err := b.waitReadyLocked(r.ctx, r.offset); err != nil {
			b.mu.Unlock()
			if err == io.EOF {
				err = nil
			}
			return written, err
		}
		if err := r.ctx.Err(); err != nil {
			b.mu.Unlock()
			return written, err
		}
		// Bound each call so cancellation is observed between ready chunks.
		end := r.offset + min(b.ready-r.offset, 128<<10)
		if r.end >= 0 {
			end = min(end, r.end)
		}
		p := b.mem[r.offset:end]
		b.pinned++
		b.mu.Unlock()
		length := len(p)
		n, err := b.writePinned(w, p)
		if n < 0 || n > length {
			return written, errors.New("invalid Write count")
		}
		r.offset += int64(n)
		written += int64(n)
		if n < length && err == nil {
			err = io.ErrShortWrite
		}
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (r *reader) Read(p []byte) (int, error) {
	if r.end >= 0 {
		if r.offset >= r.end {
			return 0, io.EOF
		}
		p = p[:min(int64(len(p)), r.end-r.offset)]
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := r.b.wait(r.ctx, p, r.offset)
	r.offset += int64(n)
	return n, err
}

func (b *Buffer) wait(ctx context.Context, p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.waitReadyLocked(ctx, off); err != nil {
		return 0, err
	}
	return b.readLocked(p, off)
}

func (b *Buffer) waitReadyLocked(ctx context.Context, off int64) error {
	for {
		switch {
		case b.closed:
			return ErrClosed
		case off < b.ready:
			return nil
		case b.complete:
			return io.EOF
		case b.err != nil:
			return b.err
		}
		if b.waiters == nil {
			b.waiters = make(chan struct{})
		}
		changed := b.waiters
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			b.mu.Lock()
			return ctx.Err()
		}
		b.mu.Lock()
	}
}
