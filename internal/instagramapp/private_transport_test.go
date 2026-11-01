package instagramapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestAPITransportRoutesOnlyPrivateOriginsToAndroidTransport(t *testing.T) {
	var ordinary atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ordinary.Add(1)
		fmt.Fprint(w, "ordinary")
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	base := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: endpoint.Hostname()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, endpoint.Host)
		},
	}
	transport := newAPITransport(base)
	defer transport.CloseIdleConnections()
	// A closed mobile side provides an unambiguous routing result without
	// sending credentials or opening any connection to Instagram.
	transport.mobile.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, target := range []string{"https://i.instagram.com/api/v1/media/123/info/", "https://b.i.instagram.com/", "https://I.INSTAGRAM.COM:443/"} {
		if _, err := client.Get(target); err == nil || !strings.Contains(err.Error(), "login transport closed") {
			t.Fatalf("private origin did not use Android transport: %s", target)
		}
	}
	for _, target := range []string{server.URL, "https://scontent.cdninstagram.com/media", "https://www.instagram.com/", "https://i.instagram.com.other.invalid/", "https://i.instagram.com:444/"} {
		res, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	if ordinary.Load() != 5 {
		t.Fatalf("unexpected ordinary transport request count: %d", ordinary.Load())
	}
}

func TestLoginTransportReplacesClosedConnectionBeforeNextRequest(t *testing.T) {
	var requests, connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request := func() {
		t.Helper()
		res, err := client.Post(server.URL, "text/plain", strings.NewReader("fixture"))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	request()
	transport.mu.Lock()
	for _, conn := range transport.connections {
		conn.Close()
	}
	transport.mu.Unlock()
	request()
	if requests.Load() != 2 || connections.Load() != 2 {
		t.Fatalf("closed connection was not replaced once: requests=%d connections=%d", requests.Load(), connections.Load())
	}
}

func TestLoginTransportFailedPOSTIsNotReplayedAndNextRequestReconnects(t *testing.T) {
	for _, mode := range []string{"connection_lost", "goaway"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer certServer.Close()
			config := certServer.TLS.Clone()
			config.NextProtos = []string{"h2"}
			var requests, connections atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				for n := range 2 {
					raw, err := listener.Accept()
					if err != nil {
						return
					}
					connections.Add(1)
					raw.SetDeadline(time.Now().Add(5 * time.Second))
					conn := tls.Server(raw, config)
					if err := conn.Handshake(); err != nil {
						raw.Close()
						return
					}
					if n == 1 {
						//lint:ignore SA1019 Test the explicit HTTP/2 connection API without retries.
						(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							requests.Add(1)
							fmt.Fprint(w, "ok")
						})})
						conn.Close()
						return
					}
					preface := make([]byte, len(http2.ClientPreface))
					if _, err := io.ReadFull(conn, preface); err != nil {
						conn.Close()
						return
					}
					framer := http2.NewFramer(conn, conn)
					framer.WriteSettings()
					for {
						frame, err := framer.ReadFrame()
						if err != nil {
							break
						}
						if _, ok := frame.(*http2.HeadersFrame); ok {
							requests.Add(1)
							if mode == "goaway" {
								framer.WriteGoAway(0, http2.ErrCodeNo, nil)
							}
							break
						}
					}
					conn.Close()
				}
			}()
			roots := x509.NewCertPool()
			roots.AddCert(certServer.Certificate())
			transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
			t.Cleanup(func() {
				transport.CloseIdleConnections()
				listener.Close()
				<-done
			})
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			target := "https://" + listener.Addr().String()
			if _, err := client.Post(target, "text/plain", strings.NewReader("first-password")); err == nil || requests.Load() != 1 || connections.Load() != 1 {
				t.Fatalf("failed credential POST replayed: requests=%d connections=%d", requests.Load(), connections.Load())
			}
			res, err := client.Post(target, "text/plain", strings.NewReader("next-explicit-request"))
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if requests.Load() != 2 || connections.Load() != 2 {
				t.Fatalf("next explicit request did not reconnect once: requests=%d connections=%d", requests.Load(), connections.Load())
			}
		})
	}
}
