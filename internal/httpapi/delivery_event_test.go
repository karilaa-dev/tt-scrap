package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type deliveryDoer func(*http.Request) (*http.Response, error)

func (f deliveryDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliveryRequestEventsPreserveOutcomesWithoutPayloads(t *testing.T) {
	cases := []struct {
		name          string
		items, status int
		want          DeliveryEvent
	}{
		{"success", 1, 200, DeliveryEvent{Outcome: "succeeded", Batches: 1, Attempts: 1, Succeeded: 1}},
		{"rejected", 1, 200, DeliveryEvent{Outcome: "rejected", FailedStage: "upload", Batches: 1, Attempts: 1, TelegramStatus: 200, TelegramErrorCode: 400}},
		{"partial", 11, 207, DeliveryEvent{Outcome: "partial", FailedStage: "upload", Batches: 2, Attempts: 2, Succeeded: 1, TelegramStatus: 400, TelegramErrorCode: 400}},
		{"network", 11, 502, DeliveryEvent{Outcome: "partial", FailedStage: "upload", Batches: 2, Attempts: 2, Succeeded: 1, ErrorCode: "telegram_network_error"}},
		{"preparation", 1, 502, DeliveryEvent{Outcome: "failed", FailedStage: "preparation", Batches: 1, ErrorCode: "upstream_network_error"}},
		{"extraction", 1, 404, DeliveryEvent{Outcome: "failed", FailedStage: "extraction", ErrorCode: "extraction_not_found_or_expired"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dependencies, tt, downloader, assets := fixture(t)
			downloader.data, downloader.mime = jpegFixture(t), "image/jpeg"
			tt.extraction.ContentType = "slideshow"
			for i := range tc.items {
				tt.extraction.Media = append(tt.extraction.Media, assets.Register(core.AssetContext{Kind: "image", Filename: "image.jpg", UpstreamURL: "fixture"}, i))
			}
			const secret = "sensitive-fixture-payload"
			dependencies.Config.TelegramBotToken = secret
			if tc.name == "preparation" {
				downloader.fail = "fixture"
			}
			calls := 0
			dependencies.HTTPClient = deliveryDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
				}
				if tc.name == "network" && calls == 2 {
					return nil, errors.New(secret)
				}
				status, body := 200, `{"ok":true,"result":{"text":"`+secret+`"}}`
				if tc.name == "rejected" || (tc.name == "partial" && calls == 2) {
					body = `{"ok":false,"error_code":400,"description":"` + secret + `"}`
					if tc.name == "partial" {
						status = 400
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			var event RequestEvent
			dependencies.OnRequest = func(value RequestEvent) { event = value }
			extraction := "saved"
			if tc.name == "extraction" {
				extraction = "missing"
			}
			body := `{"source":{"extraction_id":"` + extraction + `"},"telegram":{"chat_id":1,"future_fixture_field":"` + secret + `"}}`
			response := request(NewHandler(dependencies), "POST", "/v1/tiktok/telegram-deliveries", body, true)
			if response.Code != tc.status || event.Delivery == nil || !reflect.DeepEqual(*event.Delivery, tc.want) {
				t.Fatalf("response=%d, delivery=%+v; want %d, %+v", response.Code, event.Delivery, tc.status, tc.want)
			}
			encoded, err := json.Marshal(event)
			if err != nil || strings.Contains(string(encoded), secret) {
				t.Fatalf("unsafe event: %s %v", encoded, err)
			}
			if strings.Contains(response.Body.String(), "failed_stage") {
				t.Fatal("logging changed public response", response.Body.String())
			}
		})
	}
}
