package platform

import (
	"context"
	"testing"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type observedContext struct {
	context.Context
	waiting chan struct{}
}

func (c observedContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestMetadataFollowerRetriesAfterLeaderCancellation(t *testing.T) {
	m := newMemo[int](time.Minute, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, leaderDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = m.do(ctx, "same", false, func() (int, error) {
			close(started)
			<-ctx.Done()
			return 0, core.Upstream("wrapped upstream cancellation")
		})
	}()
	<-started
	followerCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	waiting := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := m.do(observedContext{followerCtx, waiting}, "same", false, func() (int, error) { return 42, nil })
		if err != nil || v != 42 {
			t.Errorf("live follower inherited canceled leader: %d %v", v, err)
		}
	}()
	<-waiting
	cancel()
	<-leaderDone
	<-done
}
