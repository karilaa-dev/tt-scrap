package spool

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
)

// The read_only cases hide WriterTo to measure the previous scratch-copy path
// against the same bytes and hash, without changing the fixture or RAM budget.
func BenchmarkSpoolHash(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		for _, disk := range []bool{false, true} {
			storage := "ram"
			limit := int64(size)
			if disk {
				storage, limit = "disk", 0
			}
			b.Run(fmt.Sprintf("%s/%dMiB", storage, size>>20), func(b *testing.B) {
				data := pattern(size)
				source, err := NewStore(b.TempDir(), limit, limit).New(int64(size))
				if err != nil {
					b.Fatal(err)
				}
				defer source.Close()
				if _, err := source.Writer(0, int64(size)).Write(data); err != nil {
					b.Fatal(err)
				}
				if err := source.Complete(); err != nil {
					b.Fatal(err)
				}
				want := sha256.Sum256(data)
				for _, direct := range []bool{false, true} {
					name := "read_only"
					if direct {
						name = "io_copy"
					}
					b.Run(name, func(b *testing.B) {
						scratch := make([]byte, 128<<10)
						digest := sha256.New()
						b.ReportAllocs()
						b.SetBytes(int64(size))
						b.ResetTimer()
						for range b.N {
							digest.Reset()
							r := source.Reader(context.Background(), 0, -1)
							if !direct {
								r = struct{ io.Reader }{r}
							}
							if n, err := io.CopyBuffer(digest, r, scratch); err != nil || n != int64(size) {
								b.Fatal(n, err)
							}
						}
						b.StopTimer()
						if got := digest.Sum(nil); string(got) != string(want[:]) {
							b.Fatal("hash differs")
						}
					})
				}
			})
		}
	}
}
