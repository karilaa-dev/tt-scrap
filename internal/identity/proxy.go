package identity

import (
	"bufio"
	"errors"
	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/nettransport"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func Endpoint(raw string) (key, address string, err error) {
	if raw == "" {
		return "direct", "", nil
	}
	if !strings.Contains(raw, "://") {
		parts := strings.Split(raw, ":")
		switch len(parts) {
		case 2:
			raw = "http://" + raw
		case 4:
			raw = "http://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1]
		default:
			return "", "", errors.New("invalid proxy address")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("invalid proxy address")
	}
	port := u.Port()
	switch u.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	case "socks5", "socks5h":
		if port == "" {
			port = "1080"
		}
	default:
		return "", "", errors.New("unsupported proxy scheme")
	}
	key = strings.ToLower(u.Hostname()) + ":" + port
	return key, u.String(), nil
}

func ReadProxies(cfg core.Config) ([]string, error) {
	var result []string
	seen := map[string]bool{}
	if cfg.ProxyFile != "" {
		f, err := os.Open(cfg.ProxyFile)
		if err != nil {
			return nil, errors.New("cannot open PROXY_FILE")
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			raw := strings.TrimSpace(scanner.Text())
			if raw == "" || strings.HasPrefix(raw, "#") {
				continue
			}
			key, address, err := Endpoint(raw)
			if err != nil {
				return nil, err
			}
			if !seen[key] {
				seen[key] = true
				result = append(result, address)
			}
		}
		if scanner.Err() != nil {
			return nil, errors.New("cannot read PROXY_FILE")
		}
	}
	if cfg.ProxyIncludeHost {
		result = append(result, "")
	}
	return result, nil
}

func newTransport(raw string, connections int) (*http.Transport, error) {
	return nettransport.New(raw, connections)
}
