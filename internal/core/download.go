package core

import (
	"context"
	"log/slog"
)

// DownloadAttemptTimeout identifies a local upstream stall, distinct from the
// caller's cancellation and the total transfer budget. It contains no URL.
type DownloadAttemptTimeout struct{ Phase string }

func (e *DownloadAttemptTimeout) Error() string   { return "Upstream media request stalled" }
func (e *DownloadAttemptTimeout) Timeout() bool   { return true }
func (e *DownloadAttemptTimeout) Temporary() bool { return true }
func (e *DownloadAttemptTimeout) Unwrap() error   { return NewError("upstream_timeout", e.Error(), 504) }

type diagnosticKey struct{}

func WithDiagnosticLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, diagnosticKey{}, logger)
}
func DiagnosticLogger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(diagnosticKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// MediaRecoveryError returns control to the identity owner before source retries
// consume the shared operation allowance on a session that needs repair.
type MediaRecoveryError struct{ Err error }

func (e *MediaRecoveryError) Error() string { return e.Err.Error() }
func (e *MediaRecoveryError) Unwrap() error { return e.Err }
