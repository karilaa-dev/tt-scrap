package core

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	APIKey, Host, DBURL, DBSchema                                                     string
	Port                                                                              int
	ProxyFile                                                                         string
	ProxyIncludeHost, ProxyDataOnly                                                   bool
	TikTokSessionID, TempDir                                                          string
	CacheTTL, MetadataTTL, ResolutionTTL                                              time.Duration
	CacheMaxEntries                                                                   int
	ExtractionConcurrency, DownloadConcurrency, HTTPMaxConnections, MaxQueuedRequests int
	MaxVideoDuration                                                                  int
	MaxAssetBytes                                                                     int64
	MediaMemoryFileBytes, MediaMemoryBudgetBytes                                      int64
	DownloadParts                                                                     int
	UpstreamTimeout, ResolveTimeout, DownloadTimeout                                  time.Duration
	VideoInfoMaxRetries, ResolveRetries, DownloadRetries                              int
	RetryDelay                                                                        time.Duration
	RapidAPIKey, RapidAPIHost, RapidAPIBaseURL                                        string
	InstagramMethod, InstagramAccountsFile, InstagramAccountsDir                      string
	InstagramLegacyFallback                                                           bool
	InstagramActiveAccounts                                                           int
	InstagramAccountConcurrency, InstagramAccountDownloadConcurrency                  int
	InstagramAppTimeout                                                               time.Duration
	InstagramConcurrency, InstagramMaxAttempts                                        int
	InstagramTimeout, InstagramRetryDelay                                             time.Duration
	TelegramBotToken, TelegramAPIBaseURL                                              string
	TelegramPipelineConcurrency, TelegramUploadConcurrency                            int
	TelegramPreparationConcurrency                                                    int
	TelegramTimeout, ThumbnailWait                                                    time.Duration
	TelegramMaxBytes                                                                  int64
	ImageConcurrency                                                                  int
	ImageMemoryBytes                                                                  int64
	LogLevel                                                                          string
}

func DefaultConfig() Config {
	return Config{
		Host: "0.0.0.0", Port: 8000, DBSchema: "public",
		CacheTTL: 600 * time.Second, MetadataTTL: 60 * time.Second, ResolutionTTL: 600 * time.Second, CacheMaxEntries: 10000,
		ExtractionConcurrency: 32, DownloadConcurrency: 64, HTTPMaxConnections: 128, MaxQueuedRequests: 1024,
		MaxVideoDuration: 0, MediaMemoryFileBytes: 250 << 20, MediaMemoryBudgetBytes: 1 << 30, DownloadParts: 4,
		UpstreamTimeout: 180 * time.Second, ResolveTimeout: 12 * time.Second, DownloadTimeout: 60 * time.Second,
		VideoInfoMaxRetries: 3, ResolveRetries: 3, DownloadRetries: 3, RetryDelay: time.Second,
		RapidAPIHost:    "instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com",
		RapidAPIBaseURL: "https://instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com",
		InstagramMethod: "app", InstagramActiveAccounts: 10, InstagramAccountConcurrency: 1, InstagramAccountDownloadConcurrency: 1, InstagramAppTimeout: 15 * time.Second,
		InstagramConcurrency: 4, InstagramMaxAttempts: 3, InstagramTimeout: 3 * time.Second, InstagramRetryDelay: 100 * time.Millisecond,
		TelegramAPIBaseURL: "https://api.telegram.org", TelegramPipelineConcurrency: 32, TelegramUploadConcurrency: 32,
		TelegramPreparationConcurrency: 32,
		TelegramTimeout:                600 * time.Second, ThumbnailWait: 1500 * time.Millisecond, TelegramMaxBytes: 50 * 1024 * 1024,
		ImageConcurrency: 32, ImageMemoryBytes: 1 << 30, LogLevel: "INFO",
	}
}

func LoadConfig() (Config, error) {
	if _, err := os.Stat(".env"); err == nil {
		if err = godotenv.Load(".env"); err != nil {
			return Config{}, fmt.Errorf("load .env: %w", err)
		}
	}
	values := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(k)] = v
		}
	}
	return ParseConfig(values)
}
func ParseConfig(env map[string]string) (Config, error) {
	c := DefaultConfig()
	var invalid string
	stringValue := func(key string, dst *string) {
		if v, ok := env[key]; ok {
			*dst = v
		}
	}
	number := func(key string, dst *int, min, max int) {
		if v, ok := env[key]; ok {
			n, e := strconv.Atoi(v)
			if e != nil || n < min || n > max {
				invalid = key
			} else {
				*dst = n
			}
		}
	}
	duration := func(key string, dst *time.Duration, min, max float64) {
		if v, ok := env[key]; ok {
			n, e := strconv.ParseFloat(v, 64)
			if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min || n > max {
				invalid = key
			} else {
				*dst = time.Duration(n * float64(time.Second))
			}
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := env[key]; ok {
			switch strings.ToLower(v) {
			case "true", "1", "yes", "on":
				*dst = true
			case "false", "0", "no", "off", "":
				*dst = false
			default:
				invalid = key
			}
		}
	}
	for key, dst := range map[string]*string{"TT_SCRAP_API_KEY": &c.APIKey, "HOST": &c.Host, "DB_URL": &c.DBURL, "DB_SCHEMA": &c.DBSchema, "PROXY_FILE": &c.ProxyFile, "TIKTOK_SESSION_ID": &c.TikTokSessionID, "TEMP_DIR": &c.TempDir, "RAPIDAPI_KEY": &c.RapidAPIKey, "RAPIDAPI_HOST": &c.RapidAPIHost, "RAPIDAPI_BASE_URL": &c.RapidAPIBaseURL, "TELEGRAM_BOT_TOKEN": &c.TelegramBotToken, "TELEGRAM_API_BASE_URL": &c.TelegramAPIBaseURL, "LOG_LEVEL": &c.LogLevel, "INSTAGRAM_METHOD": &c.InstagramMethod, "INSTAGRAM_ACCOUNTS_FILE": &c.InstagramAccountsFile, "INSTAGRAM_ACCOUNTS_DIR": &c.InstagramAccountsDir} {
		stringValue(key, dst)
	}
	number("PORT", &c.Port, 1, 65535)
	number("CACHE_MAX_ENTRIES", &c.CacheMaxEntries, 100, 1000000)
	number("EXTRACTION_CONCURRENCY", &c.ExtractionConcurrency, 1, 512)
	number("DOWNLOAD_CONCURRENCY", &c.DownloadConcurrency, 1, 1024)
	number("HTTP_MAX_CONNECTIONS", &c.HTTPMaxConnections, 1, 2048)
	number("MAX_QUEUED_REQUESTS", &c.MaxQueuedRequests, 1, 100000)
	number("MAX_VIDEO_DURATION", &c.MaxVideoDuration, 0, 86400)
	number("VIDEO_INFO_MAX_RETRIES", &c.VideoInfoMaxRetries, 1, 10)
	number("URL_RESOLVE_MAX_RETRIES", &c.ResolveRetries, 1, 10)
	number("DOWNLOAD_MAX_RETRIES", &c.DownloadRetries, 1, 10)
	number("DOWNLOAD_PARALLEL_PARTS", &c.DownloadParts, 1, 16)
	number("INSTAGRAM_ACTIVE_ACCOUNTS", &c.InstagramActiveAccounts, 1, 10000)
	number("INSTAGRAM_ACCOUNT_CONCURRENCY", &c.InstagramAccountConcurrency, 1, 64)
	number("INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY", &c.InstagramAccountDownloadConcurrency, 1, 64)
	number("INSTAGRAM_CONCURRENCY", &c.InstagramConcurrency, 1, 64)
	number("INSTAGRAM_MAX_ATTEMPTS", &c.InstagramMaxAttempts, 1, 10)
	number("TELEGRAM_PIPELINE_CONCURRENCY", &c.TelegramPipelineConcurrency, 1, 1024)
	number("TELEGRAM_UPLOAD_CONCURRENCY", &c.TelegramUploadConcurrency, 1, 256)
	number("TELEGRAM_PREPARATION_CONCURRENCY", &c.TelegramPreparationConcurrency, 1, 64)
	number("IMAGE_CONVERSION_WORKERS", &c.ImageConcurrency, 0, 64)
	if c.ImageConcurrency == 0 {
		c.ImageConcurrency = DefaultConfig().ImageConcurrency
	}
	duration("CACHE_TTL_SECONDS", &c.CacheTTL, 30, 86400)
	duration("TIKTOK_INFO_CACHE_TTL_SECONDS", &c.MetadataTTL, 1, 3600)
	duration("TIKTOK_RESOLUTION_CACHE_TTL_SECONDS", &c.ResolutionTTL, 1, 86400)
	duration("TIKTOK_APP_TIMEOUT_SECONDS", &c.UpstreamTimeout, .001, 600)
	duration("URL_RESOLVE_TIMEOUT_SECONDS", &c.ResolveTimeout, .001, 120)
	duration("UPSTREAM_DOWNLOAD_TIMEOUT_SECONDS", &c.DownloadTimeout, .001, 600)
	duration("DOWNLOAD_RETRY_BASE_DELAY", &c.RetryDelay, 0, 30)
	duration("INSTAGRAM_APP_TIMEOUT_SECONDS", &c.InstagramAppTimeout, .001, 600)
	duration("INSTAGRAM_REQUEST_TIMEOUT_SECONDS", &c.InstagramTimeout, .001, 60)
	duration("INSTAGRAM_RETRY_DELAY_SECONDS", &c.InstagramRetryDelay, 0, 30)
	duration("TELEGRAM_UPLOAD_TIMEOUT_SECONDS", &c.TelegramTimeout, .001, 3600)
	duration("TELEGRAM_THUMBNAIL_WAIT_SECONDS", &c.ThumbnailWait, 0, 30)
	for key, dst := range map[string]*int64{"MAX_ASSET_BYTES": &c.MaxAssetBytes, "TELEGRAM_UPLOAD_MAX_MB": &c.TelegramMaxBytes, "MEDIA_MEMORY_MAX_MB": &c.MediaMemoryFileBytes, "MEDIA_MEMORY_BUDGET_MB": &c.MediaMemoryBudgetBytes, "IMAGE_MEMORY_BUDGET_MB": &c.ImageMemoryBytes} {
		if v, ok := env[key]; ok {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 || n > 1<<40 {
				invalid = key
			} else if strings.HasSuffix(key, "_MB") {
				*dst = n * 1024 * 1024
			} else {
				*dst = n
			}
		}
	}
	if c.ImageMemoryBytes == 0 {
		c.ImageMemoryBytes = DefaultConfig().ImageMemoryBytes
	}
	boolean("INSTAGRAM_LEGACY_FALLBACK", &c.InstagramLegacyFallback)
	boolean("PROXY_INCLUDE_HOST", &c.ProxyIncludeHost)
	boolean("PROXY_DATA_ONLY", &c.ProxyDataOnly)
	if invalid != "" {
		return c, ConfigError(invalid)
	}
	c.InstagramMethod = strings.ToLower(strings.TrimSpace(c.InstagramMethod))
	if c.InstagramMethod != "app" && c.InstagramMethod != "legacy" {
		return c, ConfigError("INSTAGRAM_METHOD")
	}
	if len(c.APIKey) < 16 {
		return c, ConfigError("TT_SCRAP_API_KEY")
	}
	if (&http.Cookie{Name: "sessionid", Value: c.TikTokSessionID}).Valid() != nil {
		return c, ConfigError("TIKTOK_SESSION_ID")
	}
	if strings.TrimSpace(c.DBURL) == "" {
		return c, ConfigError("DB_URL")
	}
	c.LogLevel = strings.ToUpper(strings.TrimSpace(c.LogLevel))
	switch c.LogLevel {
	case "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "CRITICAL":
	default:
		return c, ConfigError("LOG_LEVEL")
	}
	c.TelegramAPIBaseURL = strings.TrimRight(strings.TrimSpace(c.TelegramAPIBaseURL), "/")
	telegramURL, err := url.Parse(c.TelegramAPIBaseURL)
	if err != nil || (telegramURL.Scheme != "https" && telegramURL.Scheme != "http") || telegramURL.Hostname() == "" || telegramURL.RawQuery != "" || telegramURL.Fragment != "" {
		return c, ConfigError("TELEGRAM_API_BASE_URL")
	}
	return c, nil
}
