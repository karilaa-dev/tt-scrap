package instagramapp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// Pixel 7 / Instagram 452.1.0.53.64, captured on 2026-10-29. These are
// protocol settings, not device secrets. Each handshake generates fresh keys.
// Keep the same private API transport across login and authenticated requests.
// CDN transfers retain the ordinary transport and receive no account context.
func loginClientHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS13, TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{utls.TLS_AES_128_GCM_SHA256}, CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{utls.ECDSAWithP256AndSHA256, utls.ECDSAWithP384AndSHA384, utls.PSSWithSHA256}},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2"}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
		},
	}
}

type loginTransport struct {
	base *http.Transport
	mu   sync.Mutex
	//lint:ignore SA1019 Keep the verified HTTP/2 wire profile and explicit single-attempt connection API.
	connections map[string]*http2.ClientConn
	// Connections draining after GOAWAY remain owned until all streams finish.
	//lint:ignore SA1019 Preserve single-attempt HTTP/2 connection ownership.
	retired map[*http2.ClientConn]struct{}
	closed  bool
}

func newLoginTransport(base *http.Transport) *loginTransport {
	//lint:ignore SA1019 Keep the verified HTTP/2 wire profile and explicit single-attempt connection API.
	return &loginTransport{base: base, connections: make(map[string]*http2.ClientConn), retired: make(map[*http2.ClientConn]struct{})}
}

// apiTransport uses the account's Android transport only for the two fixed
// private API origins. Fixtures, public URLs and CDN transfers use base.
type apiTransport struct {
	base   *http.Transport
	mobile *loginTransport
}

func newAPITransport(base *http.Transport) *apiTransport {
	return &apiTransport{base: base, mobile: newLoginTransport(base)}
}

func (t *apiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && req.URL.Scheme == "https" && req.URL.User == nil && (req.URL.Port() == "" || req.URL.Port() == "443") {
		host := strings.ToLower(req.URL.Hostname())
		if host == "i.instagram.com" || host == "b.i.instagram.com" {
			return t.mobile.RoundTrip(req)
		}
	}
	return t.base.RoundTrip(req)
}

func (t *apiTransport) CloseIdleConnections() {
	t.mobile.CloseIdleConnections()
	t.base.CloseIdleConnections()
}

func (t *loginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Hostname() == "" || req.URL.User != nil {
		return nil, errors.New("login requires HTTPS")
	}
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(req.URL.Hostname(), port)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("login transport closed")
	}
	conn := t.connections[address]
	for old := range t.retired {
		if old.State().Closed {
			delete(t.retired, old)
		}
	}
	if conn != nil {
		state := conn.State()
		if state.Closed || state.Closing {
			t.retireLocked(address, conn)
			conn = nil
		}
	}
	if conn == nil {
		wire, err := t.dial(req.Context(), req.URL, address)
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		//lint:ignore SA1019 Keep the verified HTTP/2 wire profile; ClientConn.RoundTrip never retries credential POSTs.
		conn, err = (&http2.Transport{StrictMaxConcurrentStreams: true}).NewClientConn(wire)
		if err != nil {
			wire.Close()
			t.mu.Unlock()
			return nil, err
		}
		t.connections[address] = conn
	}
	t.mu.Unlock()
	// ClientConn sends once. Transport.RoundTrip's retry loop must not replay a
	// password or TOTP POST after a lost connection / REFUSED_STREAM / GOAWAY.
	response, err := conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		state := conn.State()
		if state.Closed || state.Closing {
			t.retireLocked(address, conn)
		}
		t.mu.Unlock()
	}
	return response, err
}

// Do not close a draining connection while another request still owns a stream.
// Its GOAWAY/close handling will finish it; transport shutdown also owns it.
//
//lint:ignore SA1019 Preserve single-attempt HTTP/2 connection ownership.
func (t *loginTransport) retireLocked(address string, conn *http2.ClientConn) {
	if t.connections[address] == conn {
		delete(t.connections, address)
	}
	state := conn.State()
	if state.StreamsActive == 0 && state.StreamsPending == 0 && state.StreamsReserved == 0 {
		_ = conn.Close()
	} else if !state.Closed {
		t.retired[conn] = struct{}{}
	}
}

func (t *loginTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, conn := range t.connections {
		_ = conn.Close()
	}
	for conn := range t.retired {
		_ = conn.Close()
	}
	clear(t.connections)
	clear(t.retired)
}

type contextDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d contextDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(context.Background(), network, address)
}
func (d contextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

func (t *loginTransport) dial(ctx context.Context, target *url.URL, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dial := t.base.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	var route *url.URL
	if t.base.Proxy != nil {
		var err error
		route, err = t.base.Proxy(&http.Request{URL: target})
		if err != nil {
			return nil, err
		}
	}
	var raw net.Conn
	var err error
	if route == nil {
		raw, err = dial(ctx, "tcp", address)
	} else if route.Scheme == "socks5" || route.Scheme == "socks5h" {
		d, e := proxy.FromURL(route, contextDialer{dial})
		if e != nil {
			return nil, e
		}
		raw, err = d.(proxy.ContextDialer).DialContext(ctx, "tcp", address)
	} else if route.Scheme == "http" || route.Scheme == "https" {
		port := route.Port()
		if port == "" {
			if route.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		raw, err = dial(ctx, "tcp", net.JoinHostPort(route.Hostname(), port))
	} else {
		return nil, errors.New("unsupported login proxy")
	}
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = raw.SetDeadline(deadline)
	wire := raw
	if route != nil && (route.Scheme == "http" || route.Scheme == "https") {
		if route.Scheme == "https" {
			config := &tls.Config{ServerName: route.Hostname(), MinVersion: tls.VersionTLS12}
			if t.base.TLSClientConfig != nil {
				config.RootCAs = t.base.TLSClientConfig.RootCAs
			}
			secure := tls.Client(raw, config)
			if err = secure.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			wire = secure
		}
		request := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
		if route.User != nil {
			password, _ := route.User.Password()
			request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(route.User.Username()+":"+password)))
		}
		if err = request.Write(wire); err != nil {
			raw.Close()
			return nil, err
		}
		reader := bufio.NewReader(io.LimitReader(wire, 64<<10))
		response, e := http.ReadResponse(reader, request)
		if e != nil || response.StatusCode != http.StatusOK || reader.Buffered() != 0 {
			raw.Close()
			return nil, errors.New("login proxy CONNECT failed")
		}
	}
	config := &utls.Config{ServerName: target.Hostname()}
	if t.base.TLSClientConfig != nil {
		config.RootCAs = t.base.TLSClientConfig.RootCAs
	}
	secure := utls.UClient(wire, config, utls.HelloCustom)
	if err = secure.ApplyPreset(loginClientHello()); err == nil {
		err = secure.HandshakeContext(ctx)
	}
	if err == nil && secure.ConnectionState().NegotiatedProtocol != "h2" {
		err = errors.New("login requires HTTP/2")
	}
	if err != nil {
		raw.Close()
		return nil, err
	}
	stop()
	if ctx.Err() != nil {
		raw.Close()
		return nil, ctx.Err()
	}
	_ = raw.SetDeadline(time.Time{})
	return secure, nil
}
