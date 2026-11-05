package tiktokapp

import "context"

type routeObserverKey struct{}

// WithMediaRouteObserver reports the selected transport for each request hop,
// including redirects and bootstrap calls. No network address is exposed.
func WithMediaRouteObserver(ctx context.Context, observe func(directCDN bool)) context.Context {
	return context.WithValue(ctx, routeObserverKey{}, observe)
}
func observeRoute(ctx context.Context, directCDN bool) {
	if observe, ok := ctx.Value(routeObserverKey{}).(func(bool)); ok {
		observe(directCDN)
	}
}
