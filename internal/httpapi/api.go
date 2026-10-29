package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/instagramapp"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type RequestEvent struct {
	RequestID, Method, Path string
	Status                  int
	Duration                time.Duration
	DurationMS              float64
	Bytes                   int64
	ErrorCode               string
	// WireStatus is the status already sent when a response fails mid-body
	// and Status reports the outcome instead; zero otherwise.
	WireStatus int
	Delivery   *DeliveryEvent
}
type InstagramAccounts interface {
	Import(context.Context, []instagramapp.Credentials) ([]instagramapp.ImportResult, error)
	Status() []instagramapp.AccountStatus
}
type Dependencies struct {
	InstagramAccounts InstagramAccounts
	Config            core.Config
	Assets            core.AssetReader
	TikTok            core.TikTokService
	Instagram         core.InstagramService
	Downloader        core.Downloader
	IsReady           func() bool
	OpenAPI           []byte
	HTTPClient        core.HTTPDoer
	OnRequest         func(RequestEvent)
	Identities        core.IdentityPreparer
}
type handler struct {
	dependencies Dependencies
	telegram     *telegramService
}
type response struct {
	status    int
	headers   http.Header
	data      []byte
	body      io.Reader
	cleanup   func()
	delivery  *DeliveryEvent
	errorCode string
}

func NewHandler(dependencies Dependencies) http.Handler {
	service := newTelegramService(dependencies.Config, dependencies)
	return &handler{dependencies: dependencies, telegram: service}
}

func requestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" {
		runes := []rune(value)
		return string(runes[:min(len(runes), 128)])
	}
	return core.NewID()
}

func jsonResponse(status int, value any) (response, error) {
	data, err := json.Marshal(value)
	return response{status: status, headers: http.Header{"Content-Type": []string{"application/json"}}, data: data}, err
}

// statusClientClosedRequest follows the nginx 499 convention: the caller
// canceled the request before the service finished, so no 5xx is reported.
const statusClientClosedRequest = 499

var errRequestCanceled = core.NewError("request_canceled", "Request canceled by caller", statusClientClosedRequest)

// ErrServiceShutdown is the cancellation cause of the server's base context
// when shutdown stops in-flight requests, so they are not reported as caller
// cancellations.
var ErrServiceShutdown = core.NewError("service_shutting_down", "Service is shutting down", 503)

// cancellation classifies a canceled request context. The first cancellation
// wins: a caller that left before shutdown stays a caller cancellation.
func cancellation(ctx context.Context) *core.Error {
	if errors.Is(context.Cause(ctx), ErrServiceShutdown) {
		return ErrServiceShutdown
	}
	return errRequestCanceled
}

func errorResponse(err error, id string) response {
	code, message, status := "internal_error", "Internal server error", 500
	var failure *core.Error
	if errors.As(err, &failure) {
		code, message, status = failure.Code, failure.Message, failure.Status
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, message, status = "upstream_timeout", "Upstream request timed out", 504
	}
	result, _ := jsonResponse(status, fields{"error": fields{"code": code, "message": message, "request_id": id}})
	result.errorCode = code
	return result
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := requestID(r)
	var result response
	var written bool
	defer func() {
		if recover() != nil && !written {
			result = errorResponse(core.NewError("internal_error", "Internal server error", 500), id)
			w.Header().Set("X-Request-ID", id)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(result.status)
			_, _ = w.Write(result.data)
		}
	}()
	var err error
	result, err = h.route(w, r)
	if err != nil {
		delivery := result.delivery
		// Only the request context's cancellation is a client or shutdown
		// outcome; a context canceled internally while the caller still waits
		// stays a 500.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			failure := cancellation(r.Context())
			// The delivery saw only the raw cancellation; attribute it the same way.
			if delivery != nil && delivery.ErrorCode == errRequestCanceled.Code {
				delivery.ErrorCode = failure.Code
			}
			err = failure
		}
		result = errorResponse(err, id)
		result.delivery = delivery
	}
	if result.cleanup != nil {
		defer result.cleanup()
	}
	elapsed := time.Since(started)
	for key, values := range result.headers {
		w.Header()[key] = values
	}
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.3f", float64(elapsed)/float64(time.Millisecond)))
	if result.body == nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(result.data)))
	}
	written = true
	w.WriteHeader(result.status)
	var size int64
	status, code, wireStatus := result.status, result.errorCode, 0
	if r.Method != "HEAD" {
		var err error
		if result.body != nil {
			size, err = io.Copy(w, result.body)
		} else {
			var n int
			n, err = w.Write(result.data)
			size = int64(n)
		}
		// The status is already sent; report the interrupted body as the outcome.
		if err != nil {
			failure := core.NewError("internal_error", "Internal server error", 500)
			if r.Context().Err() != nil {
				failure = cancellation(r.Context())
			}
			status, code, wireStatus = failure.Status, failure.Code, result.status
		}
	}
	if h.dependencies.OnRequest != nil {
		total := time.Since(started)
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/assets/") {
			path = "/v1/assets/{token}"
		}
		h.dependencies.OnRequest(RequestEvent{RequestID: id, Method: r.Method, Path: path, Status: status, WireStatus: wireStatus, Duration: total, DurationMS: float64(total) / float64(time.Millisecond), Bytes: size, ErrorCode: code, Delivery: result.delivery})
	}
}

const preparationPath = "/v1/tiktok/identities/preparations"

var attachmentFilename = strings.NewReplacer("\r", "_", "\n", "_", "\"", "_", "\\", "_")

const instagramImportPath = "/v1/instagram/accounts/imports"
const instagramAccountsPath = "/v1/instagram/accounts"
const instagramDeviceProfilesPath = "/v1/instagram/device-profiles"

var routes = map[string]string{instagramDeviceProfilesPath: "GET", instagramImportPath: "POST", instagramAccountsPath: "GET", "/health/live": "GET", "/health/ready": "GET", "/v1/tiktok/resolutions": "POST", "/v1/tiktok/extractions": "POST", "/v1/tiktok/music": "POST", "/v1/tiktok/telegram-deliveries": "POST", "/v1/instagram/extractions": "POST", "/v1/instagram/telegram-deliveries": "POST", preparationPath: "GET, POST"}

func assetToken(path string) (string, bool) {
	token, ok := strings.CutPrefix(path, "/v1/assets/")
	return token, ok && token != "" && !strings.Contains(token, "/")
}
func routeMethod(path string) string {
	if _, ok := assetToken(path); ok {
		return "GET"
	}
	return routes[path]
}

func (h *handler) route(w http.ResponseWriter, r *http.Request) (response, error) {
	path := r.URL.Path
	d := h.dependencies
	switch path {
	case "/openapi.json", "/docs", "/redoc", "/docs/oauth2-redirect":
		if r.Method != "GET" && r.Method != "HEAD" {
			result, err := jsonResponse(405, fields{"detail": "Method Not Allowed"})
			result.headers.Set("Allow", "GET, HEAD")
			return result, err
		}
		if path == "/openapi.json" {
			return response{status: 200, headers: http.Header{"Content-Type": []string{"application/json"}}, data: d.OpenAPI}, nil
		}
		page := docsPage
		if path == "/redoc" {
			page = redocPage
		} else if path == "/docs/oauth2-redirect" {
			page = oauthPage
		}
		return response{status: 200, headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, data: []byte(page)}, nil
	}
	method := routeMethod(path)
	if method == "" {
		trimmed := strings.TrimSuffix(path, "/")
		if trimmed != path && routeMethod(trimmed) != "" {
			target := url.URL{Path: trimmed, RawQuery: r.URL.RawQuery}
			return response{status: 307, headers: http.Header{"Location": []string{target.String()}}}, nil
		}
		return jsonResponse(404, fields{"detail": "Not Found"})
	}
	if !slices.Contains(strings.Split(method, ", "), r.Method) {
		result, err := jsonResponse(405, fields{"detail": "Method Not Allowed"})
		result.headers.Set("Allow", method)
		return result, err
	}
	if path == "/health/live" {
		return jsonResponse(200, fields{"status": "ok"})
	}
	if path == "/health/ready" {
		instagramReady := d.Instagram != nil
		if service, ok := d.Instagram.(interface{ Ready() bool }); ok {
			instagramReady = service.Ready()
		} else if d.Config.InstagramMethod == "legacy" {
			instagramReady = instagramReady && d.Config.RapidAPIKey != ""
		}
		if !instagramReady {
			return response{}, core.NewError("service_not_configured", "Instagram backend is not ready", 503)
		}
		if d.IsReady != nil && !d.IsReady() {
			return response{}, core.NewError("service_not_configured", "TikTok identities are not ready", 503)
		}
		return jsonResponse(200, fields{"status": "ready"})
	}
	var body fields
	var err error
	if path != instagramImportPath && r.Method == "POST" && (path != preparationPath || r.ContentLength != 0) {
		body, err = readBody(w, r, path == preparationPath)
		if err != nil {
			return response{}, err
		}
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(d.Config.APIKey)) != 1 {
		return response{}, core.NewError("authentication_required", "A valid bearer token is required", 401)
	}
	if token, ok := assetToken(path); ok {
		decoded, err := url.PathUnescape(token)
		if err != nil {
			return response{}, core.MissingAsset()
		}
		assetContext, err := d.Assets.GetAsset(decoded)
		if err != nil {
			return response{}, err
		}
		asset, err := d.Downloader.Download(r.Context(), assetContext, 0)
		if err != nil {
			return response{}, err
		}
		filename := attachmentFilename.Replace(media.FilenameForType(assetContext.Filename, asset.ContentType))
		return response{status: 200, body: asset.Data.Reader(r.Context(), 0, asset.Size), cleanup: asset.Cleanup, headers: http.Header{"Content-Type": []string{asset.ContentType}, "Content-Length": []string{strconv.FormatInt(asset.Size, 10)}, "Content-Disposition": []string{`attachment; filename="` + filename + `"`}, "Etag": []string{`"` + asset.SHA256 + `"`}, "X-Content-Sha256": []string{asset.SHA256}, "Cache-Control": []string{"private, no-store"}}}, nil
	}
	switch path {
	case instagramDeviceProfilesPath:
		return jsonResponse(200, fields{"profiles": instagramapp.DeviceProfiles()})
	case instagramAccountsPath:
		if d.InstagramAccounts == nil {
			return response{}, core.NewError("service_not_configured", "Instagram account management unavailable", 503)
		}
		return jsonResponse(200, fields{"accounts": d.InstagramAccounts.Status(), "active_limit": d.Config.InstagramActiveAccounts})
	case instagramImportPath:
		if d.InstagramAccounts == nil {
			return response{}, core.NewError("service_not_configured", "Instagram account management unavailable", 503)
		}
		accounts, err := readInstagramImport(w, r)
		if err != nil {
			return response{}, err
		}
		result, err := d.InstagramAccounts.Import(r.Context(), accounts)
		if err != nil {
			return response{}, err
		}
		return jsonResponse(200, fields{"results": result})
	case preparationPath:
		if len(body) != 0 {
			return response{}, invalidRequest()
		}
		if d.Identities == nil {
			return response{}, core.NewError("service_not_configured", "TikTok identity pool is unavailable", 503)
		}
		if r.Method == "GET" {
			return jsonResponse(200, d.Identities.PreparationStatus())
		}
		status, err := d.Identities.StartPreparation()
		if err != nil {
			return response{}, err
		}
		code := http.StatusAccepted
		if status.State == "completed" {
			code = http.StatusOK
		}
		return jsonResponse(code, status)
	case "/v1/tiktok/resolutions":
		value, err := httpURL(body["url"])
		if err != nil {
			return response{}, err
		}
		output, err := d.TikTok.ResolveURL(r.Context(), value)
		if err != nil {
			return response{}, err
		}
		return jsonResponse(200, output)
	case "/v1/tiktok/extractions":
		value, err := httpURL(body["url"])
		if err != nil {
			return response{}, err
		}
		bypass, err := refresh(body)
		if err != nil {
			return response{}, err
		}
		output, err := d.TikTok.ExtractURL(r.Context(), value, bypass)
		if err != nil {
			return response{}, err
		}
		return jsonResponse(200, output)
	case "/v1/instagram/extractions":
		value, err := httpURL(body["url"])
		if err != nil {
			return response{}, err
		}
		bypass, err := refresh(body)
		if err != nil {
			return response{}, err
		}
		output, err := d.Instagram.ExtractURL(r.Context(), value, bypass)
		if err != nil {
			return response{}, err
		}
		return jsonResponse(200, output)
	case "/v1/tiktok/music":
		id, err := integer(body["video_id"], true)
		if err != nil {
			return response{}, err
		}
		output, err := d.TikTok.ExtractMusic(r.Context(), id, false)
		if err != nil {
			return response{}, err
		}
		return jsonResponse(200, output)
	case "/v1/tiktok/telegram-deliveries", "/v1/instagram/telegram-deliveries":
		instagram := strings.Contains(path, "/instagram/")
		request, err := parseTelegram(body, instagram)
		if err != nil {
			return response{}, err
		}
		output, err := h.telegram.deliver(r.Context(), request, instagram)
		headers := http.Header{"Content-Type": []string{output.ContentType}}
		if output.Method != "" {
			headers.Set("X-Telegram-Method", output.Method)
		}
		return response{status: output.Status, headers: headers, data: output.Body, delivery: output.Event}, err
	}
	return jsonResponse(404, fields{"detail": "Not Found"})
}

func attachmentName(prefix string, batch, item int) string {
	return prefix + "_" + strconv.Itoa(batch) + "_" + strconv.Itoa(item)
}

const docsPage = `<!doctype html><html><head><title>tt-scrap - Swagger UI</title><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({url:"/openapi.json",dom_id:"#swagger-ui",deepLinking:true,presets:[SwaggerUIBundle.presets.apis,SwaggerUIBundle.SwaggerUIStandalonePreset],layout:"BaseLayout",oauth2RedirectUrl:window.location.origin+"/docs/oauth2-redirect"})</script></body></html>`
const redocPage = `<!doctype html><html><head><title>tt-scrap - ReDoc</title></head><body><redoc spec-url="/openapi.json"></redoc><script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script></body></html>`
const oauthPage = `<!doctype html><html><body><script>window.close()</script></body></html>`
