package instagramapp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func benchmarkPool(b *testing.B, count int) *Pool {
	b.Helper()
	accounts := make([]map[string]string, count)
	for n := range accounts {
		accounts[n] = map[string]string{"id": fmt.Sprintf("account-%d", n), "authorization": fmt.Sprintf("IGT:2:fixture-%d", n)}
	}
	data, _ := json.Marshal(map[string]any{"accounts": accounts})
	path := filepath.Join(b.TempDir(), "accounts.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		b.Fatal(err)
	}
	p, err := Open(Options{AccountsFile: path, PerAccountConcurrency: 1, MaxQueuedRequests: 1024})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(p.Close)
	return p
}

func BenchmarkInstagramMetadataDuringDownload(b *testing.B) {
	p := benchmarkPool(b, 1)
	var elapsed time.Duration
	b.ResetTimer()
	for range b.N {
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- p.WithMedia(context.Background(), "instagram:account-0", func(core.OpenAppMedia) error {
				close(started)
				time.Sleep(10 * time.Millisecond)
				return nil
			})
		}()
		<-started
		start := time.Now()
		_, release, err := p.acquire(context.Background(), "", nil)
		elapsed += time.Since(start)
		if err != nil {
			b.Fatal(err)
		}
		release()
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "metadata-ns/op")
}

func BenchmarkInstagramPinnedAdmission(b *testing.B) {
	for _, count := range []int{2, 100, 1000} {
		b.Run(fmt.Sprintf("accounts=%d", count), func(b *testing.B) {
			p := benchmarkPool(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := p.WithMedia(context.Background(), "instagram:account-0", func(core.OpenAppMedia) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkInstagramMetadataAdmission(b *testing.B) {
	p := benchmarkPool(b, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, release, err := p.acquire(context.Background(), "", nil)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}
