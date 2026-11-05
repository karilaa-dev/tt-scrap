package nettransport

import (
	"context"
	"net/http"
)

type mediaExchangeKey struct{}
type MediaExchange func(func() (*http.Response, error)) (*http.Response, error)

// WithMediaExchange lets a media downloader guard individual HTTP exchanges
// before a backend reads response bytes for redirects or content inspection.
func WithMediaExchange(ctx context.Context, exchange MediaExchange) context.Context {
	return context.WithValue(ctx, mediaExchangeKey{}, exchange)
}
func ExchangeMedia(ctx context.Context, next func() (*http.Response, error)) (*http.Response, error) {
	if exchange, ok := ctx.Value(mediaExchangeKey{}).(MediaExchange); ok {
		return exchange(next)
	}
	return next()
}
