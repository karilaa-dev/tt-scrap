package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
)

const maxResponseBytes = 8 << 20

type preparedRequest struct {
	SourceID string      `json:"source_id"`
	URL      string      `json:"url"`
	Headers  [][2]string `json:"headers"`
	Method   string      `json:"method"`
}
type requestMetric struct {
	SourceID        string   `json:"source_id"`
	Success         bool     `json:"success"`
	Status          int      `json:"status"`
	LatencyMS       float64  `json:"latency_ms"`
	HTTPLatencyMS   float64  `json:"http_latency_ms"`
	ValidationMS    float64  `json:"validation_ms"`
	Bytes           int64    `json:"bytes"`
	QueueMS         float64  `json:"queue_ms"`
	SignatureAgeMS  *float64 `json:"signature_age_ms"`
	ByteMeasurement string   `json:"byte_measurement"`
	Error           string   `json:"error,omitempty"`
}
type capturedResponse struct {
	Status  int         `json:"status"`
	Headers [][2]string `json:"headers"`
	Body    []byte      `json:"body"`
}
type batchResult struct {
	Requests    []requestMetric     `json:"requests"`
	Responses   []*capturedResponse `json:"responses"`
	WallSeconds float64             `json:"wallSeconds"`
	CPUSeconds  float64             `json:"cpuSeconds"`
}
type activeBatch struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type httpExecutor struct {
	mu        sync.Mutex
	capacity  int
	timeout   time.Duration
	transport *http.Transport
	client    *http.Client
	active    *activeBatch
	closed    bool
}

func newExecutor(proxy *string, capacity, timeoutMS int) (*httpExecutor, error) {
	if capacity < 1 || capacity > 32 || timeoutMS < 1 || timeoutMS > 120000 {
		return nil, errors.New("invalid executor configuration")
	}
	raw := ""
	if proxy != nil {
		raw = *proxy
	}
	transport, e := nettransport.New(raw, capacity)
	if e != nil {
		return nil, e
	}
	return &httpExecutor{capacity: capacity, timeout: time.Duration(timeoutMS) * time.Millisecond, transport: transport, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func processCPUSeconds() float64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}
func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func (e *httpExecutor) execute(ctx context.Context, requests []preparedRequest, concurrency int) (batchResult, error) {
	if len(requests) < 1 || len(requests) > 10 || concurrency < 1 || concurrency > e.capacity {
		return batchResult{}, errors.New("invalid HTTP batch")
	}
	for _, entry := range requests {
		u, err := url.Parse(entry.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (entry.Method != "" && entry.Method != "GET") || entry.SourceID == "" {
			return batchResult{}, errors.New("invalid prepared request")
		}
	}
	e.mu.Lock()
	if e.closed || e.active != nil {
		e.mu.Unlock()
		return batchResult{}, errors.New("executor unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	batch := &activeBatch{cancel: cancel, done: make(chan struct{})}
	e.active = batch
	e.mu.Unlock()
	defer func() { cancel(); e.mu.Lock(); e.active = nil; close(batch.done); e.mu.Unlock() }()
	result := batchResult{Requests: make([]requestMetric, len(requests)), Responses: make([]*capturedResponse, len(requests))}
	started := time.Now()
	cpuStarted := processCPUSeconds()
	indices := make(chan int, len(requests))
	for i := range requests {
		indices <- i
	}
	close(indices)
	var workers sync.WaitGroup
	for range min(concurrency, len(requests)) {
		workers.Go(func() {
			for i := range indices {
				result.Requests[i], result.Responses[i] = e.request(ctx, requests[i], started)
			}
		})
	}
	workers.Wait()
	result.WallSeconds = time.Since(started).Seconds()
	result.CPUSeconds = processCPUSeconds() - cpuStarted
	return result, nil
}
func (e *httpExecutor) request(batch context.Context, entry preparedRequest, queuedAt time.Time) (metric requestMetric, captured *capturedResponse) {
	started := time.Now()
	metric = requestMetric{SourceID: entry.SourceID, QueueMS: milliseconds(started.Sub(queuedAt)), ByteMeasurement: "upstream_http_body"}
	ctx, cancel := context.WithTimeout(batch, e.timeout)
	defer cancel()
	defer func() {
		if metric.LatencyMS == 0 {
			metric.LatencyMS = milliseconds(time.Since(started))
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "GET", entry.URL, nil)
	if err != nil {
		metric.Error = "network_error"
		return
	}
	for _, header := range entry.Headers {
		req.Header.Add(header[0], header[1])
	}
	if timestamp, err := strconv.ParseInt(req.Header.Get("X-Khronos"), 10, 64); err == nil && timestamp > 0 {
		age := float64(time.Now().UnixMilli() - timestamp*1000)
		metric.SignatureAgeMS = &age
	}
	res, err := e.client.Do(req)
	if err != nil {
		metric.Error = "network_error"
		if ctx.Err() != nil {
			metric.Error = "cancelled_or_timed_out"
		}
		return
	}
	defer res.Body.Close()
	metric.Status = res.StatusCode
	switch res.StatusCode {
	case 301, 302, 303, 307, 308:
		metric.Error = "unsupported_redirect"
		return
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	metric.Bytes = int64(len(body))
	if err != nil {
		metric.Error = "network_error"
		if ctx.Err() != nil {
			metric.Error = "cancelled_or_timed_out"
		}
		return
	}
	if len(body) > maxResponseBytes {
		metric.Error = "response_too_large"
		return
	}
	received := time.Now()
	metric.HTTPLatencyMS = milliseconds(received.Sub(started))
	metric.Error = validateResponse(res.StatusCode, body, entry.SourceID)
	validated := time.Now()
	metric.ValidationMS = milliseconds(validated.Sub(received))
	metric.LatencyMS = milliseconds(validated.Sub(started))
	metric.Success = metric.Error == ""
	headers := make([][2]string, 0, len(res.Header))
	for name, values := range res.Header {
		for _, value := range values {
			headers = append(headers, [2]string{name, value})
		}
	}
	captured = &capturedResponse{Status: res.StatusCode, Headers: headers, Body: body}
	return
}
func validateResponse(status int, body []byte, sourceID string) string {
	if status != 200 {
		return "http_status"
	}
	if len(body) == 0 {
		return "empty_body"
	}
	var envelope struct {
		StatusCode *int              `json:"status_code"`
		Detail     json.RawMessage   `json:"aweme_detail"`
		Details    []json.RawMessage `json:"aweme_details"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&envelope) != nil {
		return "invalid_json"
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return "invalid_json"
	}
	if envelope.StatusCode == nil || *envelope.StatusCode != 0 {
		return "upstream_status"
	}
	var detail struct {
		ID    string                     `json:"aweme_id"`
		Video map[string]json.RawMessage `json:"video"`
	}
	if len(envelope.Detail) > 0 && string(envelope.Detail) != "null" {
		if json.Unmarshal(envelope.Detail, &detail) != nil {
			return "wrong_source_id"
		}
	} else {
		for _, raw := range envelope.Details {
			var item struct {
				ID    string                     `json:"aweme_id"`
				Video map[string]json.RawMessage `json:"video"`
			}
			if json.Unmarshal(raw, &item) == nil && item.ID == sourceID {
				detail = item
				break
			}
		}
	}
	if detail.ID != sourceID {
		return "wrong_source_id"
	}
	if detail.Video == nil {
		return "missing_video"
	}
	return ""
}
func (e *httpExecutor) cancel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != nil {
		e.active.cancel()
	}
}
func (e *httpExecutor) close() {
	e.mu.Lock()
	e.closed = true
	batch := e.active
	if batch != nil {
		batch.cancel()
	}
	e.mu.Unlock()
	if batch != nil {
		<-batch.done
	}
	e.transport.CloseIdleConnections()
}
