package core

import (
	"context"
	"sync"
	"time"
)

// Operation is the single retry owner for an opt-in TikTok operation. Nested
// stages share its retry allowance; an attempt resumes the failed stage rather
// than repeating work that already succeeded.
type Operation struct {
	mu                                     sync.Mutex
	attempt                                int
	terminal                               bool
	started, possible, confirmed, rejected bool
	observe                                func(AttemptProgress)
}
type AttemptProgress struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt"`
	Maximum int    `json:"max_attempts"`
	Stage   string `json:"stage"`
}
type operationKey struct{}
type noRetryKey struct{}

func NewOperation(attempt int) *Operation { return &Operation{attempt: attempt} }
func WithOperation(ctx context.Context, op *Operation) context.Context {
	return context.WithValue(ctx, operationKey{}, op)
}
func OperationFrom(ctx context.Context) *Operation {
	op, _ := ctx.Value(operationKey{}).(*Operation)
	return op
}
func WithoutOperationRetries(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey{}, true)
}
func (o *Operation) Observe(ctx context.Context, observe func(AttemptProgress)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observe = observe
	if !o.terminal && ctx.Err() == nil {
		observe(AttemptProgress{"attempt", o.attempt, 3, "admission"})
	}
}
func AllowRetry(ctx context.Context, stage string) bool {
	if ctx.Err() != nil {
		return false
	}
	o := OperationFrom(ctx)
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if disabled, _ := ctx.Value(noRetryKey{}).(bool); disabled || o.terminal || o.possible || o.attempt >= 3 || ctx.Err() != nil {
		return false
	}
	o.attempt++
	if o.observe != nil {
		o.observe(AttemptProgress{"attempt", o.attempt, 3, stage})
	}
	return ctx.Err() == nil
}

type OperationState struct {
	Attempt                                int
	Started, Possible, Confirmed, Rejected bool
}

func (o *Operation) Finish() OperationState {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminal = true
	return OperationState{o.attempt, o.started, o.possible, o.confirmed, o.rejected}
}
func (o *Operation) Attempt() int { o.mu.Lock(); defer o.mu.Unlock(); return o.attempt }
func MarkDeliveryStarted(ctx context.Context) {
	if o := OperationFrom(ctx); o != nil {
		o.mu.Lock()
		o.started = true
		o.mu.Unlock()
	}
}
func BeforeDeliveryCommit(ctx context.Context) error {
	if o := OperationFrom(ctx); o != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.terminal {
			return context.Canceled
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.possible = true
		o.rejected = false
		if o.observe != nil {
			o.observe(AttemptProgress{"delivery_commit", o.attempt, 3, "upload"})
		}
	}
	return ctx.Err()
}
func MarkDeliveryConfirmed(ctx context.Context) {
	if o := OperationFrom(ctx); o != nil {
		o.mu.Lock()
		o.possible = true
		o.confirmed = true
		o.mu.Unlock()
	}
}

// For the 15-second operation contract, leave room for recovery. Progressing
// body reads still have no per-attempt wall-clock cutoff, only the parent budget.
func OperationStallLimit(ctx context.Context, configured time.Duration) time.Duration {
	if OperationFrom(ctx) != nil {
		return min(configured, 3*time.Second)
	}
	return configured
}

func MarkDeliveryRejected(ctx context.Context) {
	if o := OperationFrom(ctx); o != nil {
		o.mu.Lock()
		o.rejected = true
		o.mu.Unlock()
	}
}

// RetryAvailable avoids waiting out a backoff after the shared allowance ends.
// AllowRetry still makes the atomic decision when the real retry starts.
func RetryAvailable(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	o := OperationFrom(ctx)
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	disabled, _ := ctx.Value(noRetryKey{}).(bool)
	return !disabled && !o.terminal && !o.possible && o.attempt < 3
}
