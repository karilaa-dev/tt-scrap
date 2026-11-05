package tiktokapp

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestMediaRouteObserverFollowsAppToDirectCDNRedirect(t *testing.T) {
	client := NewClient(registeredTestSession(t), doerFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(302, nil, http.Header{"Location": {"https://cdn.example/video"}}), nil
	}))
	client.SetMediaTransport(doerFunc(func(r *http.Request) (*http.Response, error) { return testResponse(200, []byte("media"), nil), nil }))
	var routes []bool
	ctx := WithMediaRouteObserver(context.Background(), func(direct bool) { routes = append(routes, direct) })
	res, err := client.MediaRequest(ctx, "https://api19-va.tiktokv.com/aweme/v1/play/", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if !reflect.DeepEqual(routes, []bool{false, true}) {
		t.Fatal("redirect attributed to wrong route", routes)
	}
}
