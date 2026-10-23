package spool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

func TestCopyPreservesRangeAndReadOffset(t *testing.T) {
	for _, storage := range []string{"ram", "disk", "growing"} {
		t.Run(storage, func(t *testing.T) {
			limit, size := int64(1<<20), int64(10)
			if storage == "disk" {
				limit = 0
			}
			if storage == "growing" {
				size = -1
			}
			b, err := NewStore(t.TempDir(), limit, limit).New(size)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			r := b.Reader(context.Background(), 2, 8)
			if _, err := b.Writer(0, size).Write([]byte("0123456789")); err != nil {
				t.Fatal(err)
			}
			if err := b.Complete(); err != nil {
				t.Fatal(err)
			}
			head := make([]byte, 2)
			if _, err := io.ReadFull(r, head); err != nil || string(head) != "23" {
				t.Fatal(string(head), err)
			}
			var got bytes.Buffer
			if n, err := io.Copy(&got, r); n != 4 || err != nil || got.String() != "4567" {
				t.Fatal(n, got.String(), err)
			}
			if n, err := io.Copy(&got, r); n != 0 || err != nil {
				t.Fatal("exhausted reader copied bytes", n, err)
			}
		})
	}
}

func TestCopyShortAndFailedWritesAdvanceOnlyAcceptedBytes(t *testing.T) {
	failure := errors.New("output failed")
	for _, err := range []error{nil, failure} {
		b := Bytes([]byte("0123456789"))
		r := b.Reader(context.Background(), 2, 8)
		n, gotErr := io.Copy(writeFunc(func(p []byte) (int, error) {
			if string(p) != "234567" {
				t.Fatalf("unexpected output bytes: %q", p)
			}
			return 2, err
		}), r)
		wantErr := err
		if wantErr == nil {
			wantErr = io.ErrShortWrite
		}
		if n != 2 || gotErr != wantErr {
			t.Fatal("incorrect write result", n, gotErr)
		}
		remaining, gotErr := io.ReadAll(r)
		if gotErr != nil || string(remaining) != "4567" {
			t.Fatal("incorrect remaining bytes", string(remaining), gotErr)
		}
		b.Close()
	}
}

func TestCopyBlockedOutputKeepsRAMReservedWithoutBlockingBuffer(t *testing.T) {
	store := NewStore(t.TempDir(), 10, 10)
	b, err := store.New(10)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	w := b.Writer(0, 10)
	if _, err := w.Write([]byte("01234")); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	defer func() {
		for _, release := range releases {
			select {
			case <-release:
			default:
				close(release)
			}
		}
	}()
	results := []chan error{make(chan error, 1), make(chan error, 1)}
	for i := range releases {
		release, done := releases[i], results[i]
		go func() {
			n, err := io.Copy(writeFunc(func(p []byte) (int, error) {
				entered <- struct{}{}
				<-release
				if string(p) != "01234" {
					return 0, errors.New("in-flight bytes changed")
				}
				return len(p), nil
			}), b.Reader(context.Background(), 0, 10))
			if n != 5 || err != ErrClosed {
				done <- errors.New("copy did not preserve its prefix and report closure")
				return
			}
			done <- nil
		}()
	}
	for range releases {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("copy did not reach output")
		}
	}
	closed := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("56789"))
		b.Close()
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked output held the buffer lock")
	}
	for i, release := range releases {
		if store.Used() != 10 {
			t.Fatal("in-flight RAM reservation released early", store.Used())
		}
		next, err := store.New(10)
		if err != nil {
			t.Fatal(err)
		}
		if next.InMemory() {
			t.Error("in-flight RAM budget was reused")
		}
		next.Close()
		close(release)
		select {
		case err := <-results[i]:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("closed reader did not finish")
		}
	}
	if store.Used() != 0 {
		t.Fatal("finished writes kept RAM reserved", store.Used())
	}
}

func TestCopyFailureAndCancellation(t *testing.T) {
	failure := errors.New("download failed")
	for _, stop := range []string{"fail", "cancel", "close"} {
		t.Run(stop, func(t *testing.T) {
			b, err := NewStore("", 10, 10).New(10)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if _, err := b.Writer(0, 10).Write([]byte("01234")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			written := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				n, err := io.Copy(writeFunc(func(p []byte) (int, error) {
					written <- struct{}{}
					return len(p), nil
				}), b.Reader(ctx, 0, -1))
				if n != 5 {
					done <- errors.New("ready prefix was lost")
					return
				}
				done <- err
			}()
			select {
			case <-written:
			case <-time.After(2 * time.Second):
				t.Fatal("copy did not write the ready prefix")
			}
			want := failure
			switch stop {
			case "fail":
				b.Fail(failure)
			case "cancel":
				cancel()
				want = context.Canceled
			case "close":
				b.Close()
				want = ErrClosed
			}
			select {
			case err := <-done:
				if err != want {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("copy remained blocked")
			}
		})
	}
}

func TestCopyObservesCancellationBetweenReadyChunks(t *testing.T) {
	b := Bytes(pattern(1 << 20))
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := io.Copy(writeFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	}), b.Reader(ctx, 0, -1))
	if err != context.Canceled || n == 0 || n >= b.Size() {
		t.Fatal("copy ignored cancellation while bytes were ready", n, err)
	}
}

func TestCopyPanickingWriterReleasesRAM(t *testing.T) {
	for _, closeDuringWrite := range []bool{false, true} {
		store := NewStore("", 10, 10)
		b, err := store.New(10)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Writer(0, 10).Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != "output panic" {
					t.Errorf("unexpected panic: %v", recovered)
				}
			}()
			_, _ = io.Copy(writeFunc(func([]byte) (int, error) {
				if closeDuringWrite {
					b.Close()
					if store.Used() != 10 {
						t.Error("in-flight RAM released early")
					}
				}
				panic("output panic")
			}), b.Reader(context.Background(), 0, 10))
		}()
		b.Close()
		if store.Used() != 0 {
			t.Fatal("panicking output kept RAM pinned", store.Used())
		}
	}
}

func TestCopyGrowingBufferCanSpillWhileOutputBlocks(t *testing.T) {
	store := NewStore(t.TempDir(), minGrowth, minGrowth)
	b, err := store.New(-1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	w := b.Writer(0, -1)
	data := pattern(2 * minGrowth)
	if _, err := w.Write(data[:100]); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	first := true
	n, err := io.Copy(writeFunc(func(p []byte) (int, error) {
		if first {
			first = false
			if _, err := w.Write(data[100:]); err != nil {
				return 0, err
			}
			if b.InMemory() || store.Used() != 0 {
				t.Error("growing buffer did not release spilled RAM")
			}
			if err := b.Complete(); err != nil {
				return 0, err
			}
		}
		return got.Write(p)
	}), b.Reader(context.Background(), 0, -1))
	if err != nil || n != int64(len(data)) || !bytes.Equal(got.Bytes(), data) {
		t.Fatal("spill changed streamed bytes", n, err)
	}
}
