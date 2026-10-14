package httpapi

import (
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	"github.com/karilaa-dev/tt-scrap/internal/core"
)

type fields map[string]any
type telegramRequest struct {
	URL, ExtractionID, VideoID, Delivery, Selector string
	Refresh                                        bool
	Telegram                                       fields
}

func invalidRequest() error {
	return core.NewError("validation_error", "Request validation failed", 422)
}

func readBody(w http.ResponseWriter, r *http.Request, allowEmpty bool) (fields, error) {
	if header := r.Header.Get("Content-Type"); header != "" {
		kind, _, err := mime.ParseMediaType(header)
		if err != nil || !(kind == "application/json" || strings.HasPrefix(kind, "application/") && strings.HasSuffix(kind, "+json")) {
			return nil, invalidRequest()
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var value fields
	err := decoder.Decode(&value)
	if err == io.EOF && allowEmpty {
		return nil, nil
	}
	if err != nil || value == nil {
		return nil, invalidRequest()
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, invalidRequest()
	}
	return value, nil
}

func httpURL(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 2083 {
		return "", invalidRequest()
	}
	text = strings.TrimSpace(text)
	u, err := url.Parse(text)
	if err != nil {
		return "", invalidRequest()
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(u.Host, " \r\n\t") {
		return "", invalidRequest()
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) == nil {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", invalidRequest()
		}
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 0 || number > 65535 {
			return "", invalidRequest()
		}
		if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
			port = ""
		}
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func boolean(value any) (bool, error) {
	switch x := value.(type) {
	case bool:
		return x, nil
	case json.Number:
		if x == "0" {
			return false, nil
		}
		if x == "1" {
			return true, nil
		}
	case string:
		switch strings.ToLower(x) {
		case "true", "1", "yes", "on", "t", "y":
			return true, nil
		case "false", "0", "no", "off", "f", "n":
			return false, nil
		}
	}
	return false, invalidRequest()
}

func refresh(body fields) (bool, error) {
	if value, ok := body["refresh"]; ok {
		return boolean(value)
	}
	return false, nil
}

func integer(value any, positive bool) (string, error) {
	var text string
	switch x := value.(type) {
	case string:
		text = strings.TrimSpace(x)
		if !integerString.MatchString(text) {
			return "", invalidRequest()
		}
	case json.Number:
		text = string(x)
		if at := strings.IndexAny(text, "eE"); at >= 0 {
			exponent, err := strconv.Atoi(text[at+1:])
			if err != nil || exponent > 1024 || exponent < -1024 {
				return "", invalidRequest()
			}
		}
	case bool:
		if x {
			text = "1"
		} else {
			text = "0"
		}
	default:
		return "", invalidRequest()
	}
	if len(text) > 1024 {
		return "", invalidRequest()
	}
	number, ok := new(big.Rat).SetString(text)
	if !ok || !number.IsInt() || (positive && number.Sign() <= 0) {
		return "", invalidRequest()
	}
	return number.Num().String(), nil
}

var integerString = regexp.MustCompile(`^[+-]?[0-9]+(?:\.0+)?$`)

var telegramStringFields = strings.Fields("business_connection_id callback_query_id caption parse_mode performer title message_effect_id")
var telegramIntegerFields = strings.Fields("message_thread_id direct_messages_topic_id receiver_user_id duration width height start_timestamp")
var telegramBoolFields = strings.Fields("show_caption_above_media has_spoiler supports_streaming disable_content_type_detection disable_notification protect_content allow_paid_broadcast")
var telegramObjectFields = strings.Fields("suggested_post_parameters reply_parameters reply_markup")
var telegramReservedFields = strings.Fields("audio cover document media photo thumbnail video")

func parseTelegram(body fields, instagram bool) (telegramRequest, error) {
	request := telegramRequest{Delivery: "media"}
	var err error
	if request.Refresh, err = refresh(body); err != nil {
		return request, err
	}
	if value, ok := body["delivery"]; ok {
		request.Delivery, ok = value.(string)
		if !ok || !(request.Delivery == "media" || request.Delivery == "document" || (!instagram && request.Delivery == "audio")) {
			return request, invalidRequest()
		}
	}
	source, ok := body["source"].(map[string]any)
	if !ok {
		return request, invalidRequest()
	}
	count := 0
	if value := source["url"]; value != nil {
		count++
		request.Selector = "url"
		request.URL, err = httpURL(value)
		if err != nil {
			return request, err
		}
	}
	if value := source["extraction_id"]; value != nil {
		count++
		request.Selector = "extraction_id"
		text, ok := value.(string)
		if !ok {
			return request, invalidRequest()
		}
		request.ExtractionID = strings.TrimSpace(text)
	}
	if value := source["video_id"]; value != nil && !instagram {
		count++
		request.Selector = "video_id"
		request.VideoID, err = integer(value, true)
		if err != nil {
			return request, err
		}
	}
	if count != 1 || (source["extraction_id"] != nil && request.Refresh) || (request.VideoID != "" && request.Delivery != "audio") {
		return request, invalidRequest()
	}
	parameters, ok := body["telegram"].(map[string]any)
	if !ok {
		return request, invalidRequest()
	}
	request.Telegram = fields(parameters)
	chat, ok := parameters["chat_id"]
	if !ok || chat == nil {
		return request, invalidRequest()
	}
	if _, ok = chat.(string); !ok {
		number, err := integer(chat, false)
		if err != nil {
			return request, err
		}
		parameters["chat_id"] = json.Number(number)
	}
	for _, key := range telegramReservedFields {
		if _, ok := parameters[key]; ok {
			return request, invalidRequest()
		}
	}
	for _, key := range telegramStringFields {
		if value := parameters[key]; value != nil {
			if _, ok := value.(string); !ok {
				return request, invalidRequest()
			}
		}
	}
	for _, key := range telegramIntegerFields {
		if value := parameters[key]; value != nil {
			number, err := integer(value, false)
			if err != nil {
				return request, err
			}
			parameters[key] = json.Number(number)
		}
	}
	for _, key := range telegramBoolFields {
		if value := parameters[key]; value != nil {
			converted, err := boolean(value)
			if err != nil {
				return request, err
			}
			parameters[key] = converted
		}
	}
	for _, key := range telegramObjectFields {
		if value := parameters[key]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				return request, invalidRequest()
			}
		}
	}
	if value := parameters["caption_entities"]; value != nil {
		list, ok := value.([]any)
		if !ok {
			return request, invalidRequest()
		}
		for _, item := range list {
			if _, ok := item.(map[string]any); !ok {
				return request, invalidRequest()
			}
		}
	}
	return request, nil
}
