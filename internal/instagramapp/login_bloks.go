package instagramapp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

func extractAAC(value map[string]any) string {
	layout, _ := value["layout"].(map[string]any)
	bloks, _ := layout["bloks_payload"].(map[string]any)
	data, _ := bloks["data"].([]any)
	for _, node := range data {
		m, _ := node.(map[string]any)
		payload, _ := m["data"].(map[string]any)
		if payload["key"] != "CAA_ACCOUNT_ACCESS_CONTEXT:aac" {
			continue
		}
		if v := stringField(payload, "initial"); v != "" {
			return v
		}
		for _, literal := range bloksStrings.FindAllString(stringField(payload, "initial_lispy"), -1) {
			var v string
			if json.Unmarshal([]byte(literal), &v) == nil && v != "" {
				return v
			}
		}
	}
	return ""
}

// Read static Bloks expressions as data. Never evaluate server-provided code.
func bloksExpression(value string, start int) string {
	depth := 0
	quoted, escaped := false, false
	for i := start; i < len(value); i++ {
		c := value[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '(':
			depth++
			if depth > 64 {
				return ""
			}
		case ')':
			depth--
			if depth == 0 {
				return value[start : i+1]
			}
		}
	}
	return ""
}
func bloksOperands(value string) []string {
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return nil
	}
	var out []string
	for i := 1; i < len(value)-1; {
		if strings.ContainsRune(" \t\r\n", rune(value[i])) {
			i++
			continue
		}
		var item string
		switch value[i] {
		case '(':
			item = bloksExpression(value, i)
		case '"':
			loc := bloksStrings.FindStringIndex(value[i:])
			if loc != nil && loc[0] == 0 {
				item = value[i : i+loc[1]]
			}
		default:
			j := i
			for j < len(value)-1 && !strings.ContainsRune("()\" \t\r\n", rune(value[j])) {
				j++
			}
			item = value[i:j]
		}
		if item == "" {
			return nil
		}
		out = append(out, item)
		i += len(item)
	}
	return out
}
func literalString(value string) string {
	var result string
	if json.Unmarshal([]byte(value), &result) != nil {
		return ""
	}
	return result
}
func contextFromMap(expression, key string) string {
	for depth := 0; depth < 8; depth++ {
		items := bloksOperands(expression)
		if len(items) != 3 || items[0] != "f4i" {
			return ""
		}
		keys, values := bloksOperands(items[1]), bloksOperands(items[2])
		if len(keys) == 0 || len(keys) != len(values) || keys[0] != "dkc" || values[0] != "dkc" {
			return ""
		}
		expression = ""
		for i := 1; i < len(keys); i++ {
			switch literalString(keys[i]) {
			case key:
				return literalString(values[i])
			case "server_params":
				expression = values[i]
			}
		}
		if expression == "" {
			return ""
		}
	}
	return ""
}
func extractTwoStepContext(value map[string]any) string {
	var visit func(any, int) string
	visit = func(v any, depth int) string {
		if depth > 32 {
			return ""
		}
		switch x := v.(type) {
		case map[string]any:
			if s := stringField(x, "two_step_verification_context"); s != "" {
				return s
			}
			for _, child := range x {
				if s := visit(child, depth+1); s != "" {
					return s
				}
			}
		case []any:
			for _, child := range x {
				if s := visit(child, depth+1); s != "" {
					return s
				}
			}
		case string:
			if len(x) < 10000 && strings.Contains(x, "two_step_verification_context") {
				var embedded any
				if json.Unmarshal([]byte(x), &embedded) == nil {
					if _, ok := embedded.(string); !ok {
						if s := visit(embedded, depth+1); s != "" {
							return s
						}
					}
				}
			}
			anchor := `"` + twoStepApp + `"`
			for offset := 0; offset < len(x); {
				index := strings.Index(x[offset:], anchor)
				if index < 0 {
					break
				}
				offset += index + len(anchor)
				tail := x[offset:]
				start := strings.Index(tail, "(f4i")
				next := strings.Index(tail, "com.bloks.")
				if start >= 0 && (next < 0 || next > start) {
					if s := contextFromMap(bloksExpression(tail, start), "two_step_verification_context"); s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	return visit(value, 0)
}
func (s *loginSession) twoStep(ctx context.Context, token string, key []byte) (loginResponse, error) {
	server := func() map[string]any {
		return map[string]any{"should_fallback_to_sms": 0, "device_id": s.device, "two_step_verification_context": token, "flow_source": "two_factor_login"}
	}
	params := server()
	params["family_device_id"] = s.phone
	r, e := s.bloks(ctx, s.origin, "apps", twoStepApp, map[string]any{"client_input_params": map[string]any{"device_id": s.device, "is_whatsapp_installed": 0, "machine_id": s.mid}, "server_params": params}, false)
	if e != nil {
		return r, e
	}
	if e = requireLoginOK(r); e != nil {
		return r, e
	}
	r, e = s.bloks(ctx, s.origin, "apps", "com.bloks.www.two_step_verification.method_picker", map[string]any{"client_input_params": map[string]any{"is_whatsapp_installed": 0}, "server_params": server()}, false)
	if e != nil {
		return r, e
	}
	if e = requireLoginOK(r); e != nil {
		return r, e
	}
	params = server()
	params["spectra_reg_login_data"] = nil
	r, e = s.bloks(ctx, s.origin, "async_action", "com.bloks.www.two_step_verification.method_picker.navigation.async", map[string]any{"client_input_params": map[string]any{"selected_method": "totp", "cloud_trust_token": nil, "network_bssid": nil}, "server_params": params}, false)
	if e != nil {
		return r, e
	}
	if e = requireLoginOK(r); e != nil {
		return r, e
	}
	params = server()
	params["spectra_reg_login_data"] = nil
	params["challenge"] = "totp"
	return s.bloks(ctx, s.origin, "async_action", "com.bloks.www.two_step_verification.verify_code.async", map[string]any{"client_input_params": map[string]any{"auth_secure_device_id": "", "block_store_machine_id": "", "code": totp(key, time.Now()), "should_trust_device": 1, "family_device_id": s.phone, "device_id": s.device, "cloud_trust_token": nil, "network_bssid": nil, "machine_id": s.mid}, "server_params": params}, false)
}
