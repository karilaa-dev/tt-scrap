package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

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
