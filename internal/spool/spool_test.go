package spool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func pattern(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

func TestStorePlacesBuffersByFileLimitAndBudget(t *testing.T) {
	directory := t.TempDir()
	store := NewStore(directory, 100, 150)
	small, err := store.New(100)
	if err != nil || !small.InMemory() || store.Used() != 100 {
		t.Fatal("file within limits left RAM", err, store.Used())
	}
	large, err := store.New(101)
	if err != nil || large.InMemory() {
		t.Fatal("file above the per-file limit stayed in RAM", err)
	}
	overBudget, err := store.New(60)
	if err != nil || overBudget.InMemory() || store.Used() != 100 {
		t.Fatal("budget overcommitted", err, store.Used())
	}
	if files, _ := os.ReadDir(directory); len(files) != 0 {
		t.Fatal("disk buffers left directory entries", files)
	}
	small.Close()
	small.Close()
	if store.Used() != 0 {
		t.Fatal("RAM not returned", store.Used())
	}
	if again, _ := store.New(60); !again.InMemory() {
		t.Fatal("released budget not reusable")
	} else {
		again.Close()
	}
	large.Close()
	overBudget.Close()
}

func TestParallelRangesStreamInOrder(t *testing.T) {
	for _, limit := range []int64{1 << 20, 0} {
		data := pattern(300_000)
		store := NewStore(t.TempDir(), limit, limit)
		b, err := store.New(int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		var streamed bytes.Buffer
		read := make(chan error, 1)
		go func() {
			_, err := io.Copy(&streamed, b.Reader(ctx, 0, int64(len(data))))
			read <- err
		}()
		// Later ranges finish first; the reader must still receive bytes in order.
		bounds := []int64{0, 70_000, 150_000, 220_000, 300_000}
		var writers sync.WaitGroup
		for i := len(bounds) - 2; i >= 0; i-- {
			start, end := bounds[i], bounds[i+1]
			w := b.Writer(start, end)
			writers.Go(func() {
				time.Sleep(time.Duration(i) * time.Millisecond)
				for offset := start; offset < end; offset += 4096 {
					if _, err := w.Write(data[offset:min(offset+4096, end)]); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		writers.Wait()
		if err := <-read; err != nil || !bytes.Equal(streamed.Bytes(), data) {
			t.Fatal("stream differs", err)
		}
		if _, err := b.ReadAt(make([]byte, 1), 0); err != nil {
			t.Fatal("arrived bytes are unreadable", err)
		}
		if err := b.Complete(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(io.NewSectionReader(b, 0, b.Size()))
		if err != nil || !bytes.Equal(got, data) || (limit > 0) != (b.Bytes() != nil) {
			t.Fatal("complete buffer differs", err)
		}
		b.Close()
		if _, err := b.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
			t.Fatal("closed buffer readable", err)
		}
	}
}

func TestGrowingBufferSpillsToDisk(t *testing.T) {
	store := NewStore(t.TempDir(), 100_000, 1<<20)
	b, err := store.New(-1)
	if err != nil {
		t.Fatal(err)
	}
	data := pattern(250_000)
	w := b.Writer(0, -1)
	for offset := 0; offset < len(data); offset += 30_000 {
		if _, err := w.Write(data[offset:min(offset+30_000, len(data))]); err != nil {
			t.Fatal(err)
		}
		if offset < 60_000 && !b.InMemory() {
			t.Fatal("spilled before the limit")
		}
	}
	if b.InMemory() || store.Used() != 0 || b.Size() != -1 {
		t.Fatal("large growth stayed in RAM", store.Used())
	}
	if err := b.Complete(); err != nil || b.Size() != int64(len(data)) {
		t.Fatal(err, b.Size())
	}
	got, err := io.ReadAll(b.Reader(context.Background(), 0, -1))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("spilled data differs", err)
	}
	b.Close()
}

func TestFailureAndCancellationReleaseReaders(t *testing.T) {
	store := NewStore("", 1<<20, 1<<20)
	b, _ := store.New(10)
	if _, err := b.Writer(0, 5).Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Writer(5, 10).Write([]byte("too long!")); err == nil {
		t.Fatal("range overflow accepted")
	}
	if err := b.Complete(); err == nil {
		t.Fatal("incomplete buffer completed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := b.Reader(ctx, 0, 10)
	head := make([]byte, 5)
	if _, err := io.ReadFull(reader, head); err != nil || string(head) != "hello" {
		t.Fatal(err, string(head))
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 5))
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled reader kept waiting", err)
	}
	failure := errors.New("transfer failed")
	go func() {
		time.Sleep(10 * time.Millisecond)
		b.Fail(failure)
	}()
	if _, err := b.Reader(context.Background(), 5, 10).Read(make([]byte, 5)); !errors.Is(err, failure) {
		t.Fatal("failure not delivered to readers", err)
	}
	if _, err := b.Writer(5, 10).Write([]byte("x")); !errors.Is(err, failure) {
		t.Fatal("failed buffer accepted writes", err)
	}
	b.Close()
	if store.Used() != 0 {
		t.Fatal("failed buffer kept RAM")
	}
}

func TestCloseDropsRAM(t *testing.T) {
	store := NewStore("", 1<<20, 1<<20)
	fixed, err := store.New(64)
	if err != nil || !fixed.InMemory() {
		t.Fatal("fixed buffer left RAM", err)
	}
	growing, err := store.New(-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = growing.Writer(0, -1).Write(pattern(100)); err != nil || store.Used() == 64 {
		t.Fatal("growing buffer reserved no RAM", err, store.Used())
	}
	for _, b := range []*Buffer{fixed, growing} {
		b.Close()
		b.Close()
		if b.mem != nil {
			t.Fatal("closed buffer kept its RAM", cap(b.mem))
		}
		if _, err = b.ReadAt(make([]byte, 1), 0); err != ErrClosed {
			t.Fatal("closed buffer read", err)
		}
		if _, err = b.Reader(context.Background(), 0, -1).Read(make([]byte, 1)); err != ErrClosed {
			t.Fatal("closed buffer streamed", err)
		}
		if _, err = b.Writer(0, -1).Write([]byte{1}); err != ErrClosed {
			t.Fatal("closed buffer accepted a write", err)
		}
		if b.Bytes() != nil || b.Complete() != ErrClosed {
			t.Fatal("closed buffer exposed or completed")
		}
	}
	if store.Used() != 0 {
		t.Fatal("RAM not returned", store.Used())
	}
	if again, _ := store.New(1 << 20); !again.InMemory() {
		t.Fatal("released budget not reusable")
	} else {
		again.Close()
	}
}
