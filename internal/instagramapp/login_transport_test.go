package instagramapp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type helloRecorder struct {
	net.Conn
	mu   sync.Mutex
	data []byte
}

func (c *helloRecorder) Read(b []byte) (int, error) {
	n, e := c.Conn.Read(b)
	c.mu.Lock()
	if len(c.data) < 10000 {
		c.data = append(c.data, b[:n]...)
	}
	c.mu.Unlock()
	return n, e
}

type helloListener struct {
	net.Listener
	mu          sync.Mutex
	connections []*helloRecorder
}

func (l *helloListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	r := &helloRecorder{Conn: c}
	l.mu.Lock()
	l.connections = append(l.connections, r)
	l.mu.Unlock()
	return r, nil
}
func helloExtensions(t *testing.T, wire []byte) []uint16 {
	t.Helper()
	if len(wire) < 44 || wire[0] != 22 || wire[5] != 1 {
		t.Fatal("missing wire ClientHello")
	}
	pos := 43
	pos += 1 + int(wire[pos])
	size := int(binary.BigEndian.Uint16(wire[pos:]))
	pos += 2 + size
	pos += 1 + int(wire[pos])
	size = int(binary.BigEndian.Uint16(wire[pos:]))
	pos += 2
	end := pos + size
	var extensions []uint16
	for pos < end {
		extensions = append(extensions, binary.BigEndian.Uint16(wire[pos:]))
		pos += 4 + int(binary.BigEndian.Uint16(wire[pos+2:]))
	}
	return extensions
}
func TestLoginTransportWireAndReuse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.ProtoMajor != 2 || r.TLS.Version != tls.VersionTLS13 || string(body) != "password=fixture%2Bvalue" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("wire request changed")
		}
		fmt.Fprint(w, "ok")
	}))
	listener := &helloListener{Listener: server.Listener}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		if !reflect.DeepEqual(h.CipherSuites, []uint16{tls.TLS_AES_128_GCM_SHA256}) || !reflect.DeepEqual(h.SupportedVersions, []uint16{tls.VersionTLS13}) || !reflect.DeepEqual(h.SupportedProtos, []string{"h2"}) || !reflect.DeepEqual(h.SupportedCurves, []tls.CurveID{tls.X25519MLKEM768, tls.X25519, tls.CurveP256}) {
			t.Error("ClientHello differs from Pixel capture")
		}
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for i := 0; i < 2; i++ {
		res, e := client.Post(server.URL, "application/x-www-form-urlencoded", strings.NewReader("password=fixture%2Bvalue"))
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if requests.Load() != 2 || len(listener.connections) != 1 {
		t.Fatal("login connection was not reused")
	}
	listener.connections[0].mu.Lock()
	defer listener.connections[0].mu.Unlock()
	if got := helloExtensions(t, listener.connections[0].data); !reflect.DeepEqual(got, []uint16{0, 43, 10, 51, 13, 16, 45}) {
		// IP literals correctly omit SNI; all other extensions retain their order.
		if !reflect.DeepEqual(got, []uint16{43, 10, 51, 13, 16, 45}) {
			t.Fatalf("extensions %v", got)
		}
	}
}

func TestLoginTLSGeneratesFreshHandshakeMaterial(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	listener := &helloListener{Listener: server.Listener}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for range 2 {
		transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
		res, e := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get(server.URL)
		if e != nil {
			transport.CloseIdleConnections()
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		transport.CloseIdleConnections()
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if len(listener.connections) != 2 {
		t.Fatal("expected two fresh TLS connections")
	}
	var randoms, shares [][]byte
	for _, conn := range listener.connections {
		conn.mu.Lock()
		wire := append([]byte(nil), conn.data...)
		conn.mu.Unlock()
		helloExtensions(t, wire)
		randoms = append(randoms, wire[11:43])
		pos := 43
		pos += 1 + int(wire[pos])
		pos += 2 + int(binary.BigEndian.Uint16(wire[pos:]))
		pos += 1 + int(wire[pos])
		end := pos + 2 + int(binary.BigEndian.Uint16(wire[pos:]))
		pos += 2
		for pos < end {
			kind := binary.BigEndian.Uint16(wire[pos:])
			size := int(binary.BigEndian.Uint16(wire[pos+2:]))
			pos += 4
			if kind == 51 {
				shares = append(shares, wire[pos:pos+size])
			}
			pos += size
		}
	}
	if len(shares) != 2 || bytes.Equal(randoms[0], randoms[1]) || bytes.Equal(shares[0], shares[1]) {
		t.Fatal("TLS random or ephemeral key share reused")
	}
}

func TestLoginTransportCONNECT(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			var upstreamCalls, proxyCalls atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked")
				}
				fmt.Fprint(w, "ok")
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxyCalls.Add(1)
				if r.Method != "CONNECT" || r.Host != u.Host || r.Header.Get("Proxy-Authorization") != "Basic Zml4dHVyZTpwYXNz" {
					t.Error("incorrect proxy tunnel")
					w.WriteHeader(407)
					return
				}
				remote, e := net.Dial("tcp", u.Host)
				if e != nil {
					t.Error(e)
					return
				}
				defer remote.Close()
				local, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
					return
				}
				defer local.Close()
				fmt.Fprint(local, "HTTP/1.1 200 Connection Established\r\n\r\n")
				done := make(chan struct{})
				go func() { io.Copy(remote, local); close(done) }()
				io.Copy(local, remote)
				local.Close()
				<-done
			})
			var gateway *httptest.Server
			if secure {
				gateway = httptest.NewTLSServer(proxyHandler)
			} else {
				gateway = httptest.NewServer(proxyHandler)
			}
			defer gateway.Close()
			route, _ := url.Parse(gateway.URL)
			route.User = url.UserPassword("fixture", "pass")
			roots := x509.NewCertPool()
			roots.AddCert(upstream.Certificate())
			if secure {
				roots.AddCert(gateway.Certificate())
			}
			transport := newLoginTransport(&http.Transport{Proxy: http.ProxyURL(route), TLSClientConfig: &tls.Config{RootCAs: roots}})
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			res, e := client.Post(upstream.URL, "text/plain", strings.NewReader("fixture"))
			if e != nil {
				t.Fatal(e)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if upstreamCalls.Load() != 1 || proxyCalls.Load() != 1 {
				t.Fatal("request did not use selected proxy")
			}
		})
	}
}

func TestLoginTransportRejectsTLSAndALPNBeforePassword(t *testing.T) {
	for _, mode := range []string{"untrusted", "http1"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			server.EnableHTTP2 = mode != "http1"
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			if mode == "http1" {
				roots.AddCert(server.Certificate())
			}
			transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
			defer transport.CloseIdleConnections()
			_, e := (&http.Client{Transport: transport, Timeout: time.Second}).Post(server.URL, "text/plain", strings.NewReader("password"))
			if e == nil || calls.Load() != 0 {
				t.Fatal("password sent without verified TLS and h2")
			}
		})
	}
}

func TestLoginTransportDoesNotReplayRefusedPOST(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certServer.Close()
	config := certServer.TLS.Clone()
	config.NextProtos = []string{"h2"}
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, e := listener.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Server(raw, config)
		if conn.Handshake() != nil {
			return
		}
		preface := make([]byte, len(http2.ClientPreface))
		if _, e = io.ReadFull(conn, preface); e != nil {
			return
		}
		framer := http2.NewFramer(conn, conn)
		framer.WriteSettings()
		for {
			frame, e := framer.ReadFrame()
			if e != nil {
				return
			}
			if h, ok := frame.(*http2.HeadersFrame); ok {
				calls.Add(1)
				framer.WriteRSTStream(h.StreamID, http2.ErrCodeRefusedStream)
			}
		}
	}()
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	transport := newLoginTransport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", "https://"+listener.Addr().String(), bytes.NewBufferString("password=fixture"))
	_, e = transport.RoundTrip(request)
	transport.CloseIdleConnections()
	<-done
	if e == nil || calls.Load() != 1 {
		t.Fatalf("POST replayed: calls=%d err=%v", calls.Load(), e)
	}
}
