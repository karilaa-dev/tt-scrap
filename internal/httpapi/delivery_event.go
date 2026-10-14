package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

// DeliveryEvent contains operational fields only, never Telegram payloads.
type DeliveryEvent struct {
	Outcome           string `json:"outcome"`
	FailedStage       string `json:"failed_stage,omitempty"`
	Batches           int    `json:"batches"`
	Attempts          int    `json:"attempts"`
	Succeeded         int    `json:"succeeded"`
	ErrorCode         string `json:"error_code,omitempty"`
	TelegramStatus    int    `json:"telegram_status,omitempty"`
	TelegramErrorCode int64  `json:"telegram_error_code,omitempty"`
}

func (e *DeliveryEvent) record(call telegramCall) {
	if call.OK {
		e.Succeeded++
		return
	}
	e.Outcome = "rejected"
	e.TelegramStatus = call.Status
	if object, ok := call.Value.(map[string]any); ok {
		if code, ok := object["error_code"].(json.Number); ok {
			e.TelegramErrorCode, _ = code.Int64()
		}
	}
}

func (e *DeliveryEvent) finish(err error) {
	if err != nil {
		e.Outcome, e.ErrorCode = "failed", "internal_error"
		var failure *core.Error
		switch {
		case errors.As(err, &failure):
			e.ErrorCode = failure.Code
		case errors.Is(err, context.DeadlineExceeded):
			e.ErrorCode = "upstream_timeout"
		case errors.Is(err, context.Canceled):
			e.ErrorCode = "request_canceled"
		}
	}
	if e.Outcome == "" {
		e.Outcome, e.FailedStage = "succeeded", ""
	} else if e.Succeeded > 0 {
		e.Outcome = "partial"
	}
}
