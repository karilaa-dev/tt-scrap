package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

type object = map[string]any

func obj(v any) object {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return object{}
}
func arr(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return string(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}
func number(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		if err == nil {
			return n
		}
		f, err := t.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f >= float64(math.MaxInt64) || f < float64(math.MinInt64) {
			return 0
		}
		return int64(f)
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}
func integer(v any) int { return int(number(v)) }
func positive(v any) *int {
	n := integer(v)
	if n <= 0 {
		return nil
	}
	return &n
}
func numeric(v any) *int64 {
	if v == nil {
		return nil
	}
	n := number(v)
	return &n
}
func nonempty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func decode(data []byte) (object, error) {
	var result object
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("upstream JSON must be an object")
	}
	return result, nil
}
func first(vs ...any) any {
	for _, v := range vs {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		return v
	}
	return nil
}
func urls(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case []any:
		result := make([]string, 0, len(t))
		for _, v := range t {
			if s, ok := v.(string); ok && s != "" {
				result = append(result, s)
			}
		}
		return result
	case map[string]any:
		for _, k := range []string{"UrlList", "urlList", "url_list"} {
			if u := urls(t[k]); len(u) > 0 {
				return u
			}
		}
		var result []string
		for _, k := range []string{"MainUrl", "main_url", "BackupUrl", "backup_url", "FallbackUrl", "fallback_url", "src", "url", "download"} {
			result = append(result, urls(t[k])...)
		}
		return result
	}
	return nil
}

func firstURLs(values ...any) []string {
	for _, value := range values {
		if result := urls(value); len(result) > 0 {
			return result
		}
	}
	return nil
}

func decimal(v any) float64 {
	var value float64
	switch x := v.(type) {
	case json.Number:
		value, _ = x.Float64()
	case float64:
		value = x
	default:
		value = float64(number(v))
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
