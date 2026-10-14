package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
)

func TestRequestLogLevelsReflectDeliveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		status         int
		outcome, level string
	}{
		{200, "succeeded", "INFO"}, {200, "rejected", "WARN"}, {207, "partial", "WARN"}, {502, "partial", "ERROR"},
	} {
		t.Run(tc.outcome+tc.level, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			logRequest(logger, httpapi.RequestEvent{Status: tc.status, Delivery: &httpapi.DeliveryEvent{Outcome: tc.outcome}})
			if !strings.Contains(output.String(), `"level":"`+tc.level+`"`) || !strings.Contains(output.String(), `"outcome":"`+tc.outcome+`"`) {
				t.Fatal("missing delivery outcome or severity", output.String())
			}
		})
	}
}

func TestCriticalLogLevelSuppressesLowerLevels(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: logLevel(" critical ")}))
	logger.Info("info")
	logger.Warn("warning")
	logger.Error("error")
	logger.Log(context.Background(), slog.LevelError+4, "critical")
	if strings.Count(output.String(), "\n") != 1 || !strings.Contains(output.String(), `"msg":"critical"`) {
		t.Fatalf("critical threshold produced unexpected logs: %s", output.String())
	}
}
