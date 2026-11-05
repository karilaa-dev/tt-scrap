package core

import (
	"context"
	"sync"
	"testing"
)

func TestOperationOwnsNestedConcurrentRetriesAndTerminal(t *testing.T) {
	op := NewOperation(1)
	ctx := WithOperation(context.Background(), op)
	var events []AttemptProgress
	op.Observe(ctx, func(event AttemptProgress) { events = append(events, event) })
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { AllowRetry(ctx, "media") })
	}
	wg.Wait()
	state := op.Finish()
	if state.Attempt != 3 || len(events) != 3 {
		t.Fatal(state, events)
	}
	for i, event := range events {
		if event.Attempt != i+1 {
			t.Fatal("unordered attempts", events)
		}
	}
	if AllowRetry(ctx, "extraction") || BeforeDeliveryCommit(ctx) == nil || len(events) != 3 {
		t.Fatal("progress after terminal")
	}
}
func TestOperationCancellationAndOptionalWorkDoNotAdvance(t *testing.T) {
	op := NewOperation(2)
	ctx, cancel := context.WithCancel(WithOperation(context.Background(), op))
	if AllowRetry(WithoutOperationRetries(ctx), "thumbnail") {
		t.Fatal("optional retry consumed operation")
	}
	cancel()
	if AllowRetry(ctx, "media") || op.Attempt() != 2 {
		t.Fatal("cancellation advanced attempt")
	}
}
