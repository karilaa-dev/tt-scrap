package core

import "testing"

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
