package identity

import (
	"context"
	"net/http"
	"net/url"

	"github.com/karilaa-dev/tt-scrap/internal/tiktokapp"
)

func (p *Pool) mediaRequest(ctx context.Context, client *tiktokapp.Client, address string, headers http.Header) (*http.Response, error) {
	response, err := client.MediaRequest(ctx, address, headers)
	if tiktokapp.IsContentRestricted(err) && p.account.Valid() {
		if u, parseErr := url.Parse(address); parseErr == nil && tiktokapp.IsAppHost(u) {
			return client.MediaRequestWithAccount(ctx, address, headers, p.account)
		}
	}
	return response, err
}
