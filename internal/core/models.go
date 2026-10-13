package core

import (
	"context"
	"net/http"
	"sync"
)

type AssetDescriptor struct {
	AssetID     string  `json:"asset_id"`
	Kind        string  `json:"kind"`
	Position    int     `json:"position"`
	DownloadURL string  `json:"download_url"`
	Filename    string  `json:"filename"`
	ContentType *string `json:"content_type"`
	ExpiresAt   string  `json:"expires_at"`
}
type MusicMetadata struct {
	Title           string           `json:"title"`
	Author          string           `json:"author"`
	DurationSeconds int              `json:"duration_seconds"`
	Cover           *AssetDescriptor `json:"cover"`
	Audio           *AssetDescriptor `json:"audio"`
}
type TikTokExtraction struct {
	ExtractionID    string            `json:"extraction_id"`
	Platform        string            `json:"platform"`
	SourceID        string            `json:"source_id"`
	SourceURL       string            `json:"source_url"`
	ResolvedURL     string            `json:"resolved_url"`
	CreatorUsername *string           `json:"creator_username"`
	ContentType     string            `json:"content_type"`
	Cover           *AssetDescriptor  `json:"cover"`
	Width           *int              `json:"width"`
	Height          *int              `json:"height"`
	DurationSeconds *int              `json:"duration_seconds"`
	Likes           *int64            `json:"likes"`
	Views           *int64            `json:"views"`
	Media           []AssetDescriptor `json:"media"`
	Music           *MusicMetadata    `json:"music"`
	ExpiresAt       string            `json:"expires_at"`
}
type TikTokResolution struct {
	Platform    string `json:"platform"`
	SourceID    string `json:"source_id"`
	SourceURL   string `json:"source_url"`
	ResolvedURL string `json:"resolved_url"`
}
type TikTokMusic struct {
	ExtractionID    string           `json:"extraction_id"`
	Platform        string           `json:"platform"`
	SourceID        string           `json:"source_id"`
	Title           string           `json:"title"`
	Author          string           `json:"author"`
	DurationSeconds int              `json:"duration_seconds"`
	Cover           *AssetDescriptor `json:"cover"`
	Audio           AssetDescriptor  `json:"audio"`
	ExpiresAt       string           `json:"expires_at"`
}
type InstagramMedia struct {
	Position  int              `json:"position"`
	MediaType string           `json:"media_type"`
	Quality   *string          `json:"quality"`
	Asset     AssetDescriptor  `json:"asset"`
	Thumbnail *AssetDescriptor `json:"thumbnail"`
}
type InstagramExtraction struct {
	ExtractionID    string           `json:"extraction_id"`
	Platform        string           `json:"platform"`
	SourceID        string           `json:"source_id"`
	SourceURL       string           `json:"source_url"`
	CreatorUsername *string          `json:"creator_username"`
	ContentType     string           `json:"content_type"`
	Media           []InstagramMedia `json:"media"`
	ExpiresAt       string           `json:"expires_at"`
}
type AuxiliaryAsset struct {
	UpstreamURL   string
	AlternateURLs []string
	ContentType   string
	Cookies       map[string]string
}
type AssetContext struct {
	Platform        string
	Kind            string
	Filename        string
	UpstreamURL     string
	AlternateURLs   []string
	ContentType     string
	Referer         string
	ProxyURL        string
	Cookies         map[string]string
	Headers         http.Header
	IdentityID      string
	ExpectedWidth   int
	ExpectedHeight  int
	DurationSeconds int
	ExtractionID    string
	Audio           *AuxiliaryAsset
}
type Downloaded struct {
	Path        string
	Filename    string
	ContentType string
	Size        int64
	SHA256      string
	Width       int
	Height      int
	Remove      func()
	once        sync.Once
}

func (d *Downloaded) Cleanup() {
	if d != nil {
		d.once.Do(func() {
			if d.Remove != nil {
				d.Remove()
			}
		})
	}
}

type TikTokService interface {
	ResolveURL(context.Context, string) (TikTokResolution, error)
	ExtractURL(context.Context, string, bool) (TikTokExtraction, error)
	ExtractMusic(context.Context, string, bool) (TikTokMusic, error)
	GetExtraction(context.Context, string) (TikTokExtraction, error)
}
type InstagramService interface {
	ExtractURL(context.Context, string, bool) (InstagramExtraction, error)
	GetExtraction(context.Context, string) (InstagramExtraction, error)
}
type AssetReader interface {
	GetAsset(string) (AssetContext, error)
}
type Downloader interface {
	Download(context.Context, AssetContext, int64) (*Downloaded, error)
}
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// AppBackend keeps the platform layer independent of the identity scheduler.
type AppBackend interface {
	Detail(context.Context, string) (AppDetail, error)
	Feed(context.Context) (map[string]any, error)
	OpenMedia(context.Context, string, string, http.Header) (*http.Response, error)
	WithMedia(context.Context, string, func(OpenAppMedia) error) error
}
type OpenAppMedia func(context.Context, string, http.Header) (*http.Response, error)
type AppDetail struct {
	Response   map[string]any
	IdentityID string
	ProxyURL   string
	UserAgent  string
}

func Ptr[T any](value T) *T { return &value }
