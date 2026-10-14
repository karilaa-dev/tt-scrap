package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/media"
)

type RequestEvent struct {
	RequestID, Method, Path string
	Status                  int
	Duration                time.Duration
	DurationMS              float64
	Bytes                   int64
	Delivery                *DeliveryEvent
}
type Dependencies struct {
	Config     core.Config
	Assets     core.AssetReader
	TikTok     core.TikTokService
	Instagram  core.InstagramService
	Downloader core.Downloader
	IsReady    func() bool
	OpenAPI    []byte
	HTTPClient core.HTTPDoer
	OnRequest  func(RequestEvent)
	Identities core.IdentityPreparer
}
type handler struct {
	dependencies Dependencies
	telegram     *telegramService
}
type response struct {
	status   int
	headers  http.Header
	data     []byte
	file     *os.File
	cleanup  func()
	delivery *DeliveryEvent
}

func NewHandler(dependencies Dependencies) http.Handler {
	service := newTelegramService(dependencies.Config, dependencies)
	return &handler{dependencies: dependencies, telegram: service}
}

func requestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" {
		return string([]rune(value)[:min(len([]rune(value)), 128)])
	}
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	data[6] = (data[6] & 15) | 64
	data[8] = (data[8] & 63) | 128
	value := hex.EncodeToString(data[:])
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

func jsonResponse(status int, value any) (response, error) {
	data, err := json.Marshal(value)
	return response{status: status, headers: http.Header{"Content-Type": []string{"application/json"}}, data: data}, err
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
		result = errorResponse(err, id)
		result.delivery = delivery
	}
	if result.cleanup != nil {
		defer result.cleanup()
	}
	if result.file != nil {
		defer result.file.Close()
	}
	elapsed := time.Since(started)
	for key, values := range result.headers {
		w.Header()[key] = values
	}
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.3f", float64(elapsed)/float64(time.Millisecond)))
	if result.file == nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(result.data)))
	}
	written = true
	w.WriteHeader(result.status)
	var size int64
	if r.Method != "HEAD" {
		if result.file != nil {
			size, _ = io.Copy(w, result.file)
		} else {
			n, _ := w.Write(result.data)
			size = int64(n)
		}
	}
	if h.dependencies.OnRequest != nil {
		total := time.Since(started)
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/assets/") {
			path = "/v1/assets/{token}"
		}
		h.dependencies.OnRequest(RequestEvent{RequestID: id, Method: r.Method, Path: path, Status: result.status, Duration: total, DurationMS: float64(total) / float64(time.Millisecond), Bytes: size, Delivery: result.delivery})
	}
}

const preparationPath = "/v1/tiktok/identities/preparations"

var routes = map[string]string{"/health/live": "GET", "/health/ready": "GET", "/v1/tiktok/resolutions": "POST", "/v1/tiktok/extractions": "POST", "/v1/tiktok/music": "POST", "/v1/tiktok/telegram-deliveries": "POST", "/v1/instagram/extractions": "POST", "/v1/instagram/telegram-deliveries": "POST", preparationPath: "GET, POST"}

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
		if d.Config.RapidAPIKey == "" {
			return response{}, core.NewError("service_not_configured", "RAPIDAPI_KEY is not configured", 503)
		}
		if d.IsReady != nil && !d.IsReady() {
			return response{}, core.NewError("service_not_configured", "TikTok identities are not ready", 503)
		}
		return jsonResponse(200, fields{"status": "ready"})
	}
	var body fields
	var err error
	if r.Method == "POST" && (path != preparationPath || r.ContentLength != 0) {
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
		file, err := os.Open(asset.Path)
		if err != nil {
			asset.Cleanup()
			return response{}, err
		}
		filename := media.FilenameForType(assetContext.Filename, asset.ContentType)
		filename = strings.NewReplacer("\r", "_", "\n", "_", "\"", "_", "\\", "_").Replace(filename)
		return response{status: 200, file: file, cleanup: asset.Cleanup, headers: http.Header{"Content-Type": []string{asset.ContentType}, "Content-Length": []string{strconv.FormatInt(asset.Size, 10)}, "Content-Disposition": []string{`attachment; filename="` + filename + `"`}, "Etag": []string{`"` + asset.SHA256 + `"`}, "X-Content-Sha256": []string{asset.SHA256}, "Cache-Control": []string{"private, no-store"}}}, nil
	}
	switch path {
	case preparationPath:
		if len(body) != 0 {
			return response{}, invalidRequest()
		}
		if d.Identities == nil {
			return response{}, core.NewError("service_not_configured", "Identity preparation requires TikTok app mode", 503)
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
		return response{status: output.Status, headers: http.Header{"Content-Type": []string{output.ContentType}}, data: output.Body, delivery: output.Event}, err
	}
	return jsonResponse(404, fields{"detail": "Not Found"})
}

func attachmentName(prefix string, batch, item int) string {
	return prefix + "_" + strconv.Itoa(batch) + "_" + strconv.Itoa(item)
}

const docsPage = `<!doctype html><html><head><title>tt-scrap - Swagger UI</title><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({url:"/openapi.json",dom_id:"#swagger-ui",deepLinking:true,presets:[SwaggerUIBundle.presets.apis,SwaggerUIBundle.SwaggerUIStandalonePreset],layout:"BaseLayout",oauth2RedirectUrl:window.location.origin+"/docs/oauth2-redirect"})</script></body></html>`
const redocPage = `<!doctype html><html><head><title>tt-scrap - ReDoc</title></head><body><redoc spec-url="/openapi.json"></redoc><script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script></body></html>`
const oauthPage = `<!doctype html><html><body><script>window.close()</script></body></html>`
