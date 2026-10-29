package core

import (
	"context"
	"net/http"
	"sync"

	"github.com/karilaa-dev/tt-scrap/internal/spool"
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
	Position        int              `json:"position"`
	MediaType       string           `json:"media_type"`
	Quality         *string          `json:"quality"`
	Width           *int             `json:"width"`
	Height          *int             `json:"height"`
	DurationSeconds *int             `json:"duration_seconds"`
	Asset           AssetDescriptor  `json:"asset"`
	Thumbnail       *AssetDescriptor `json:"thumbnail"`
}
type InstagramExtraction struct {
	ExtractionID               string           `json:"extraction_id"`
	Platform                   string           `json:"platform"`
	Method                     string           `json:"method"`
	SourceID                   string           `json:"source_id"`
	SourceURL                  string           `json:"source_url"`
	ResolvedURL                string           `json:"resolved_url"`
	CreatorUsername            *string          `json:"creator_username"`
	ContentType                string           `json:"content_type"`
	Cover                      *AssetDescriptor `json:"cover"`
	Width                      *int             `json:"width"`
	Height                     *int             `json:"height"`
	DurationSeconds            *int             `json:"duration_seconds"`
	Likes                      *int64           `json:"likes"`
	Views                      *int64           `json:"views"`
	Comments                   *int64           `json:"comments"`
	Music                      *MusicMetadata   `json:"music"`
	Caption                    *string          `json:"caption"`
	CreatedAt                  *string          `json:"created_at"`
	ThirdPartyDownloadEligible *bool            `json:"third_party_download_eligible"`
	Media                      []InstagramMedia `json:"media"`
	ExpiresAt                  string           `json:"expires_at"`
}
type AssetContext struct {
	Platform        string
	Kind            string
	Filename        string
	UpstreamURL     string
	AlternateURLs   []string
	ContentType     string
	Referer         string
	Headers         http.Header
	IdentityID      string
	DurationSeconds int
	ExtractionID    string
}

// Downloaded media stays in RAM unless it is large or the RAM budget is spent.
type Downloaded struct {
	Data        *spool.Buffer
	Filename    string
	ContentType string
	Size        int64
	SHA256      string
	Width       int
	Height      int
	once        sync.Once
}

func (d *Downloaded) Cleanup() {
	if d != nil {
		d.once.Do(func() { d.Data.Close() })
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
	// WithIdentity shares one lease across concurrent downloads. The callback may
	// be retried after identity recovery; it must join its workers before returning.
	WithIdentity(context.Context, string, func(DownloadFunc) error) error
}

// DeliveryDownloader optionally prepares media without computing SHA256.
// It preserves WithIdentity's lease, retry, and complete validation semantics.
// Callers needing a checksum must use Downloader instead.
type DeliveryDownloader interface {
	WithDeliveryIdentity(context.Context, string, func(DownloadFunc) error) error
}

// DownloadFunc fetches a complete, validated asset. When started is set, it
// receives each transfer attempt once the length and media type are known,
// while bytes are still arriving; a failed attempt fails its buffer.
type DownloadFunc func(ctx context.Context, asset AssetContext, maxBytes int64, started func(*Downloaded)) (*Downloaded, error)
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

// InstagramBackend uses a stable account route for metadata and subsequent media.
type InstagramBackend interface {
	Detail(context.Context, string) (AppDetail, error)
	WithMedia(context.Context, string, func(OpenAppMedia) error) error
}
type OpenAppMedia func(context.Context, string, http.Header) (*http.Response, error)
type AppDetail struct {
	Response   map[string]any
	IdentityID string
	UserAgent  string
}

func Ptr[T any](value T) *T { return &value }
