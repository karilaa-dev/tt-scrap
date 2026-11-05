package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

func operationPath(path string) bool {
	switch path {
	case "/v1/tiktok/resolutions", "/v1/tiktok/extractions", "/v1/tiktok/music", "/v1/tiktok/telegram-deliveries":
		return true
	}
	return false
}
func operationRequest(r *http.Request) (*http.Request, context.CancelFunc, error) {
	noop := func() {}
	budget, attempt := r.Header.Get("X-TT-Operation-Budget-Ms"), r.Header.Get("X-TT-Operation-Attempt")
	if budget == "" {
		if attempt != "" || (operationPath(r.URL.Path) && r.Header.Get("Accept") == "application/x-ndjson") {
			return r, noop, invalidRequest()
		}
		return r, noop, nil
	}
	millis, err := strconv.Atoi(budget)
	if err != nil || millis < 1 || millis > 15000 || !operationPath(r.URL.Path) {
		return r, noop, invalidRequest()
	}
	n := 1
	if attempt != "" {
		n, err = strconv.Atoi(attempt)
		if err != nil || n < 1 || n > 3 {
			return r, noop, invalidRequest()
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(millis)*time.Millisecond)
	return r.WithContext(core.WithOperation(ctx, core.NewOperation(n))), cancel, nil
}

type terminalEvent struct {
	Type          string          `json:"type"`
	Attempt       int             `json:"attempt"`
	Maximum       int             `json:"max_attempts"`
	Status        int             `json:"status"`
	Outcome       string          `json:"outcome"`
	ErrorCode     string          `json:"error_code"`
	DeliveryState string          `json:"delivery_state"`
	SafeToRetry   bool            `json:"safe_to_retry"`
	Result        json.RawMessage `json:"result"`
}

func operationTerminal(op *core.Operation, result response, err error) terminalEvent {
	state := op.Finish()
	terminal := terminalEvent{Type: "terminal", Attempt: state.Attempt, Maximum: 3, Status: result.status, Outcome: "succeeded", ErrorCode: result.errorCode, DeliveryState: "not_started", SafeToRetry: true, Result: result.data}
	if !json.Valid(terminal.Result) {
		terminal.Result = json.RawMessage("null")
	}
	if err != nil {
		terminal.Outcome = "failed"
	}
	if result.delivery != nil {
		terminal.Outcome = result.delivery.Outcome
	}
	if errors.Is(err, context.Canceled) {
		terminal.Outcome = "canceled"
	}
	if state.Started {
		terminal.DeliveryState = "uncommitted"
	}
	if state.Possible {
		terminal.DeliveryState = "unknown"
		terminal.SafeToRetry = false
	}
	if state.Confirmed {
		terminal.DeliveryState = "confirmed"
		terminal.SafeToRetry = false
	}
	if result.delivery != nil {
		if result.delivery.Outcome == "partial" {
			terminal.DeliveryState = "partial"
			terminal.SafeToRetry = false
		}
		if result.delivery.Outcome == "rejected" && state.Rejected && !state.Confirmed {
			terminal.DeliveryState = "rejected"
			terminal.SafeToRetry = true
		}
	}
	if terminal.Outcome == "rejected" && !state.Rejected {
		terminal.Outcome = "failed"
	}
	// Successful non-delivery calls are complete; retrying them is unnecessary.
	if terminal.Outcome == "succeeded" {
		terminal.SafeToRetry = false
	}
	return terminal
}

func (h *handler) operationStream(r *http.Request, body []byte) response {
	return response{status: 200, headers: http.Header{"Content-Type": {"application/x-ndjson"}, "Cache-Control": {"no-store"}, "X-Accel-Buffering": {"no"}}, stream: func(w http.ResponseWriter) (int, *DeliveryEvent, int64, string) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		op := core.OperationFrom(ctx)
		var written int64
		controller := http.NewResponseController(w)
		emit := func(value any) {
			// Slow readers must not hold an identity or transfer beyond the budget.
			deadline := time.Now().Add(time.Second)
			if end, ok := ctx.Deadline(); ok && end.Before(deadline) && ctx.Err() == nil {
				deadline = end
			}
			_ = controller.SetWriteDeadline(deadline)
			data, err := json.Marshal(value)
			if err == nil {
				var n int
				n, err = w.Write(append(data, '\n'))
				written += int64(n)
			}
			if err == nil {
				err = controller.Flush()
			}
			if err == nil {
				// Bound writes, not the quiet preparation interval (HTTP/2 resets
				// streams when an armed write deadline expires even without I/O).
				_ = controller.SetWriteDeadline(time.Time{})
			}
			if err != nil {
				cancel()
			}
		}
		op.Observe(ctx, func(progress core.AttemptProgress) { emit(progress) })
		next := r.Clone(ctx)
		next.Header.Set("Accept", "application/json")
		next.Body = io.NopCloser(bytes.NewReader(body))
		next.ContentLength = int64(len(body))
		result, err := h.route(w, next)
		if result.cleanup != nil {
			defer result.cleanup()
		}
		if err != nil {
			delivery := result.delivery
			reported := err
			if errors.Is(err, context.Canceled) {
				reported = cancellation(ctx)
			}
			result = errorResponse(reported, w.Header().Get("X-Request-ID"))
			result.delivery = delivery
		}
		terminal := operationTerminal(op, result, err)
		emit(terminal)
		_ = controller.SetWriteDeadline(time.Time{})
		return terminal.Status, result.delivery, written, terminal.ErrorCode
	}}
}
