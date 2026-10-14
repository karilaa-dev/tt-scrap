package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type upload struct {
	media.Prepared
	Field string
}
type telegramCall struct {
	Method      string
	Status      int
	Body        []byte
	ContentType string
	Value       any
	OK          bool
}
type deliveryResult struct {
	Status      int
	ContentType string
	Body        []byte
	Event       *DeliveryEvent
}
type telegramClient struct {
	settings core.Config
	client   core.HTTPDoer
	uploads  *core.Limiter
}

func newTelegramClient(settings core.Config, client core.HTTPDoer) *telegramClient {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 256
		transport.MaxIdleConnsPerHost = max(20, settings.TelegramUploadConcurrency)
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if settings.TelegramTimeout <= 0 {
		settings.TelegramTimeout = 10 * time.Minute
	}
	if settings.TelegramAPIBaseURL == "" {
		settings.TelegramAPIBaseURL = "https://api.telegram.org"
	}
	return &telegramClient{settings: settings, client: client, uploads: core.NewLimiter(settings.TelegramUploadConcurrency, settings.MaxQueuedRequests)}
}

func (c *telegramClient) call(ctx context.Context, method string, parameters fields, uploads []upload) (telegramCall, error) {
	if c.settings.TelegramBotToken == "" {
		return telegramCall{}, core.NewError("service_not_configured", "Telegram delivery is not configured", 503)
	}
	for _, item := range uploads {
		if c.settings.TelegramMaxBytes > 0 && item.Size > c.settings.TelegramMaxBytes {
			return telegramCall{}, core.NewError("asset_too_large", "Telegram upload exceeds TELEGRAM_UPLOAD_MAX_MB", 413)
		}
	}
	release, err := c.uploads.Acquire(ctx)
	if err != nil {
		return telegramCall{}, err
	}
	defer release()
	body, contentType, length, closeFiles, err := multipartBody(parameters, uploads)
	if err != nil {
		return telegramCall{}, err
	}
	defer closeFiles()
	callContext, cancel := context.WithTimeout(ctx, c.settings.TelegramTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callContext, "POST", strings.TrimRight(c.settings.TelegramAPIBaseURL, "/")+"/bot"+c.settings.TelegramBotToken+"/"+method, body)
	if err != nil {
		return telegramCall{}, core.NewError("telegram_network_error", "Telegram upload failed before a response", 502)
	}
	request.Header.Set("Content-Type", contentType)
	request.ContentLength = length
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return telegramCall{}, ctx.Err()
		}
		if errors.Is(callContext.Err(), context.DeadlineExceeded) {
			return telegramCall{}, core.NewError("telegram_timeout", "Telegram upload timed out", 504)
		}
		return telegramCall{}, core.NewError("telegram_network_error", "Telegram upload failed before a response", 502)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		if errors.Is(callContext.Err(), context.DeadlineExceeded) {
			return telegramCall{}, core.NewError("telegram_timeout", "Telegram upload timed out", 504)
		}
		return telegramCall{}, core.NewError("telegram_network_error", "Telegram response could not be read", 502)
	}
	if len(data) > 8<<20 {
		return telegramCall{}, core.NewError("telegram_network_error", "Telegram response exceeds size limit", 502)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		value = string(data)
	}
	switch value.(type) {
	case json.Number, bool:
		value = string(data)
	}
	ok := false
	if object, isObject := value.(map[string]any); isObject {
		ok, _ = object["ok"].(bool)
	}
	resultType := response.Header.Get("Content-Type")
	if resultType == "" {
		resultType = "application/json"
	}
	return telegramCall{Method: method, Status: response.StatusCode, Body: data, ContentType: resultType, Value: value, OK: response.StatusCode >= 200 && response.StatusCode < 300 && ok}, nil
}

var multipartQuote = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", "_", "\n", "_")

// Multipart headers are tiny; file readers stream directly from disk with exact length.
func multipartBody(parameters fields, uploads []upload) (io.Reader, string, int64, func(), error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	readers := make([]io.Reader, 0, len(uploads)*2+1)
	files := make([]*os.File, 0, len(uploads))
	var size int64
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	flush := func() {
		if buffer.Len() > 0 {
			data := bytes.Clone(buffer.Bytes())
			readers = append(readers, bytes.NewReader(data))
			size += int64(len(data))
			buffer.Reset()
		}
	}
	uploadFields := make(map[string]bool, len(uploads))
	for _, item := range uploads {
		uploadFields[item.Field] = true
	}
	for key, value := range parameters {
		if value == nil || uploadFields[key] {
			continue
		}
		var text string
		switch x := value.(type) {
		case string:
			text = x
		case bool:
			text = strconv.FormatBool(x)
		case json.Number:
			text = string(x)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, "", 0, cleanup, err
			}
			text = string(encoded)
		}
		if err := writer.WriteField(key, text); err != nil {
			return nil, "", 0, cleanup, err
		}
	}
	for _, item := range uploads {
		file, err := os.Open(item.Path)
		if err != nil {
			cleanup()
			return nil, "", 0, func() {}, err
		}
		files = append(files, file)
		stat, err := file.Stat()
		if err != nil {
			cleanup()
			return nil, "", 0, func() {}, err
		}
		if stat.Size() != item.Size {
			cleanup()
			return nil, "", 0, func() {}, fmt.Errorf("prepared upload size changed")
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="`+multipartQuote.Replace(item.Field)+`"; filename="`+multipartQuote.Replace(item.Filename)+`"`)
		header.Set("Content-Type", item.ContentType)
		if _, err = writer.CreatePart(header); err != nil {
			cleanup()
			return nil, "", 0, func() {}, err
		}
		flush()
		readers = append(readers, file)
		size += item.Size
	}
	if err := writer.Close(); err != nil {
		cleanup()
		return nil, "", 0, func() {}, err
	}
	flush()
	return io.MultiReader(readers...), writer.FormDataContentType(), size, cleanup, nil
}

func telegramResponse(calls []telegramCall) (deliveryResult, error) {
	if len(calls) == 0 {
		return deliveryResult{}, core.NewError("internal_error", "Telegram delivery produced no result", 500)
	}
	if len(calls) == 1 {
		return deliveryResult{Status: calls[0].Status, ContentType: calls[0].ContentType, Body: calls[0].Body}, nil
	}
	allOK, someOK := true, false
	deliveries := make([]fields, 0, len(calls))
	for _, call := range calls {
		allOK = allOK && call.OK
		someOK = someOK || call.OK
		deliveries = append(deliveries, fields{"method": call.Method, "status_code": call.Status, "response": call.Value})
	}
	encoded, err := json.Marshal(fields{"ok": allOK, "partial": !allOK && someOK, "deliveries": deliveries})
	if err != nil {
		return deliveryResult{}, err
	}
	status := 200
	if !allOK {
		status = 207
	}
	return deliveryResult{Status: status, ContentType: "application/json", Body: encoded}, nil
}

func albumBatches[T any](items []T) [][]T {
	batches := make([][]T, 0, (len(items)+9)/10)
	for len(items) > 10 {
		count := 10
		if len(items) == 11 {
			count = 9
		}
		batches = append(batches, items[:count])
		items = items[count:]
	}
	if len(items) > 0 {
		batches = append(batches, items)
	}
	return batches
}
