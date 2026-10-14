package core

import (
	"os"
	"strings"
	"testing"
)

func TestTikTokSessionIDConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		invalid     bool
	}{
		{"disabled", "", false},
		{"session", "fixture-session", false},
		{"cookie header", "sessionid=secret; sid_tt=secret", true},
		{"newline", "secret\r\nX-Header: injected", true},
		{"control character", "secret\x00value", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig(map[string]string{
				"TT_SCRAP_API_KEY": "test-key-sixteen-characters",
				"TIKTOK_METHOD":    "web", "TIKTOK_SESSION_ID": tc.value,
			})
			if tc.invalid {
				if err == nil || err.Error() != "invalid configuration: TIKTOK_SESSION_ID" {
					t.Fatal("invalid session must fail without exposing its value")
				}
			} else if err != nil || cfg.TikTokSessionID != tc.value {
				t.Fatal("session configuration was not preserved", err)
			}
		})
	}
	if DefaultConfig().TikTokSessionID != "" {
		t.Fatal("account fallback must be disabled by default")
	}
}

func TestTikTokSessionIDEnvironmentOverridesDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TT_SCRAP_API_KEY", "test-key-sixteen-characters")
	t.Setenv("TIKTOK_METHOD", "web")
	t.Setenv("TIKTOK_SESSION_ID", "environment-session")
	if err := os.WriteFile(".env", []byte("TIKTOK_SESSION_ID=file-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.TikTokSessionID != "environment-session" {
		t.Fatal("environment did not override .env session", err)
	}
	t.Setenv("TIKTOK_SESSION_ID", "")
	cfg, err = LoadConfig()
	if err != nil || cfg.TikTokSessionID != "" {
		t.Fatal("empty environment value did not disable account fallback", err)
	}
}

func TestTelegramBaseURLValidationAndNormalization(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{" https://api.telegram.org/// ", "https://api.telegram.org"},
		{"http://localhost:8081/", "http://localhost:8081"},
		{"http://[::1]:8081/", "http://[::1]:8081"},
		{"https://gateway.example/telegram/", "https://gateway.example/telegram"},
		{"", ""}, {"localhost:8081", ""}, {"ftp://api.telegram.org", ""},
		{"https://", ""}, {"https://api.telegram.org:bad", ""},
		{"https://api.telegram.org?option=1", ""}, {"https://api.telegram.org/#part", ""},
	} {
		env := map[string]string{"TT_SCRAP_API_KEY": "test-key-sixteen-characters", "TIKTOK_METHOD": "web", "TELEGRAM_API_BASE_URL": test.input}
		cfg, err := ParseConfig(env)
		if test.want == "" {
			if err == nil {
				t.Errorf("accepted invalid Telegram base URL %q", test.input)
			}
		} else if err != nil || cfg.TelegramAPIBaseURL != test.want {
			t.Errorf("base URL %q: got %q, error %v; want %q", test.input, cfg.TelegramAPIBaseURL, err, test.want)
		}
	}
}

func TestLogLevelValidationAndNormalization(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"debug", "DEBUG"}, {" INFO ", "INFO"}, {"wArN", "WARN"},
		{" warning ", "WARNING"}, {"error", "ERROR"}, {" critical ", "CRITICAL"},
		{"", ""}, {"  ", ""}, {"ERORR", ""}, {"TRACE", ""}, {"FATAL", ""},
	} {
		env := map[string]string{"TT_SCRAP_API_KEY": "test-key-sixteen-characters", "TIKTOK_METHOD": "web", "LOG_LEVEL": test.input}
		cfg, err := ParseConfig(env)
		if test.want == "" {
			if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
				t.Errorf("invalid log level %q did not fail startup: %v", test.input, err)
			}
		} else if err != nil || cfg.LogLevel != test.want {
			t.Errorf("log level %q: got %q, error %v; want %q", test.input, cfg.LogLevel, err, test.want)
		}
	}
}
