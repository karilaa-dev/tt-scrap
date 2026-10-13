package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreparedRequestsRunWithoutSigningAndPreserveResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Argus") != "already-signed" {
			t.Error("prepared signature was changed")
		}
		w.Header().Add("Set-Cookie", "first=one; Path=/")
		w.Header().Add("Set-Cookie", "second=two; Path=/")
		io.WriteString(w, `{"status_code":0,"aweme_detail":{"aweme_id":"7695962883523464478","video":{"play_addr":{"url_list":["https://example.org/file.mp4"]}}}}`)
	}))
	defer server.Close()
	executor, e := newExecutor(nil, 2, 1000)
	if e != nil {
		t.Fatal(e)
	}
	defer executor.close()
	entry := preparedRequest{SourceID: "7695962883523464478", URL: server.URL, Method: "GET", Headers: [][2]string{{"X-Argus", "already-signed"}, {"X-Khronos", strconv.FormatInt(time.Now().Unix(), 10)}}}
	result, e := executor.execute(context.Background(), []preparedRequest{entry, entry, entry}, 2)
	if e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 3 || len(result.Requests) != 3 || len(result.Responses) != 3 || result.WallSeconds <= 0 || result.CPUSeconds < 0 {
		t.Fatalf("bad batch:%+v", result)
	}
	for i, metric := range result.Requests {
		if !metric.Success || metric.Status != 200 || metric.Bytes == 0 || metric.HTTPLatencyMS <= 0 || metric.ValidationMS < 0 || metric.SignatureAgeMS == nil {
			t.Fatalf("bad metric:%+v", metric)
		}
		cookies := 0
		for _, pair := range result.Responses[i].Headers {
			if strings.EqualFold(pair[0], "set-cookie") {
				cookies++
			}
		}
		if cookies != 2 {
			t.Fatal("response collapsed cookie headers")
		}
	}
}
func TestResponseValidationRequiresRequestedVideo(t *testing.T) {
	for _, v := range []struct {
		status      int
		body, error string
	}{{429, `{}`, "http_status"}, {200, "", "empty_body"}, {200, `<html>`, "invalid_json"}, {200, `{"status_code":1}`, "upstream_status"}, {200, `{"status_code":0,"aweme_detail":{"aweme_id":"other","video":{}}}`, "wrong_source_id"}, {200, `{"status_code":0,"aweme_detail":{"aweme_id":"fixture"}}`, "missing_video"}, {200, `{"status_code":0,"aweme_details":[{"aweme_id":"other"},{"aweme_id":"fixture","video":{}}]}`, ""}} {
		if e := validateResponse(v.status, []byte(v.body), "fixture"); e != v.error {
			t.Fatalf("validation got%q expected%q", e, v.error)
		}
	}
}
func TestExecutorKeepsRedirectsManualAndHonorsTimeout(t *testing.T) {
	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/unexpected", 302)
		case "/unexpected":
			redirected.Store(true)
		case "/slow":
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	executor, e := newExecutor(nil, 1, 20)
	if e != nil {
		t.Fatal(e)
	}
	defer executor.close()
	entries := []preparedRequest{{SourceID: "fixture", URL: server.URL + "/redirect"}, {SourceID: "fixture", URL: server.URL + "/slow"}}
	result, e := executor.execute(context.Background(), entries, 1)
	if e != nil {
		t.Fatal(e)
	}
	if result.Requests[0].Error != "unsupported_redirect" || result.Requests[1].Error != "cancelled_or_timed_out" || redirected.Load() {
		t.Fatalf("wrong redirect/timeout behavior:%+v", result.Requests)
	}
	if result.Responses[0] != nil || result.Responses[1] != nil {
		t.Fatal("failed response exposed incomplete body")
	}
}
func TestJSONLInitExecuteAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status_code":0,"aweme_detail":{"aweme_id":"fixture","video":{}}}`)
	}))
	defer server.Close()
	input, writeInput := io.Pipe()
	readOutput, output := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), input, output); output.Close() }()
	encoder := json.NewEncoder(writeInput)
	scanner := bufio.NewScanner(readOutput)
	readReply := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatal("missing JSONL reply")
		}
		var message map[string]any
		if e := json.Unmarshal(scanner.Bytes(), &message); e != nil {
			t.Fatal(e)
		}
		if message["ok"] != true {
			t.Fatalf("JSONL failed:%v", message)
		}
		return message
	}
	encoder.Encode(command{ID: "1", Operation: "init", Concurrency: 1, TimeoutMS: 1000})
	readReply()
	encoder.Encode(command{ID: "2", Operation: "execute", Concurrency: 1, Requests: []preparedRequest{{SourceID: "fixture", URL: server.URL, Method: "GET"}}})
	response := readReply()
	result := response["result"].(map[string]any)
	body := result["responses"].([]any)[0].(map[string]any)["body"]
	if _, ok := body.(string); !ok {
		t.Fatal("response body was not base64 JSON")
	}
	encoder.Encode(command{ID: "3", Operation: "shutdown"})
	readReply()
	writeInput.Close()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	readOutput.Close()
}
func TestJSONLCancelCanInterruptBatch(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	input, writeInput := io.Pipe()
	readOutput, output := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, input, output); output.Close() }()
	encoder := json.NewEncoder(writeInput)
	decoder := json.NewDecoder(readOutput)
	encoder.Encode(command{ID: "1", Operation: "init", Concurrency: 1, TimeoutMS: 10000})
	var response reply
	if e := decoder.Decode(&response); e != nil {
		t.Fatal(e)
	}
	encoder.Encode(command{ID: "2", Operation: "execute", Concurrency: 1, Requests: []preparedRequest{{SourceID: "fixture", URL: server.URL}}})
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("batch did not start")
	}
	encoder.Encode(command{ID: "3", Operation: "cancel"})
	for range 2 {
		if e := decoder.Decode(&response); e != nil || !response.OK {
			t.Fatalf("cancel/execution reply:%v", e)
		}
	}
	encoder.Encode(command{ID: "4", Operation: "shutdown"})
	if e := decoder.Decode(&response); e != nil || !response.OK {
		t.Fatalf("shutdown reply:%v", e)
	}
	writeInput.Close()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	readOutput.Close()
}
func TestExecutorImportsNoRegistrationSigningOrDatabase(t *testing.T) {
	command := exec.Command("go", "list", "-deps", ".")
	raw, e := command.Output()
	if e != nil {
		t.Skip("go tool unavailable for dependency inspection")
	}
	for _, forbidden := range []string{"github.com/karilaa-dev/tt-scrap/internal/tiktokapp", "github.com/karilaa-dev/tt-scrap/internal/identity", "github.com/jackc/pgx"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("HTTP executor imported preparation dependency%s", forbidden)
		}
	}
}
