package platform

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxProxyHeader bounds the HTTP response head accepted from an egress proxy.
const maxProxyHeader = 64 << 10

// proxyEndpoint is one parsed http:// or https:// egress proxy URL.
type proxyEndpoint struct {
	scheme  string
	host    string
	port    int
	user    string
	pass    string
	hasAuth bool
}

func (p proxyEndpoint) addr() string {
	return net.JoinHostPort(p.host, strconv.Itoa(p.port))
}

// parseEgressProxy validates an http(s) proxy URL. SOCKS schemes are rejected
// because xray's dialerProxy expects an HTTP proxy outbound.
func parseEgressProxy(raw string) (proxyEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return proxyEndpoint{}, errors.New("platform: empty egress proxy URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return proxyEndpoint{}, fmt.Errorf("platform: invalid egress proxy URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
	case "socks", "socks5", "socks5h", "socks4", "socks4a":
		return proxyEndpoint{}, fmt.Errorf("platform: egress proxy scheme %q is not supported (only http and https)", parsed.Scheme)
	default:
		return proxyEndpoint{}, fmt.Errorf("platform: egress proxy scheme %q is not supported (only http and https)", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return proxyEndpoint{}, fmt.Errorf("platform: egress proxy URL %q has no host", raw)
	}
	port := 0
	if portString := parsed.Port(); portString != "" {
		value, err := strconv.Atoi(portString)
		if err != nil || value < 1 || value > 65535 {
			return proxyEndpoint{}, fmt.Errorf("platform: egress proxy port %q is invalid", portString)
		}
		port = value
	}
	if port == 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	endpoint := proxyEndpoint{scheme: scheme, host: host, port: port}
	if parsed.User != nil {
		name := parsed.User.Username()
		if name != "" {
			password, _ := parsed.User.Password()
			endpoint.user = name
			endpoint.pass = password
			endpoint.hasAuth = true
		}
	}
	return endpoint, nil
}

// NewEgressDialer returns a dial function that connects to an HTTP(S) proxy,
// issues CONNECT for each target, requires a 2xx response, and returns the
// tunnel. Proxy userinfo becomes Proxy-Authorization basic auth. The returned
// function is safe for concurrent use.
func NewEgressDialer(proxyURL string, timeout time.Duration) (func(context.Context, string, string) (net.Conn, error), error) {
	proxy, err := parseEgressProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialEgress(ctx, proxy, network, address, timeout)
	}, nil
}

func dialEgress(ctx context.Context, proxy proxyEndpoint, network, address string, timeout time.Duration) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkTCPNetwork(network); err != nil {
		return nil, err
	}
	if _, _, err := splitTarget(address); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", proxy.addr())
	if err != nil {
		return nil, fmt.Errorf("platform: egress: dial proxy %s: %w", proxy.addr(), err)
	}
	clearDeadline := armContext(ctx, conn, timeout)
	fail := func(err error) (net.Conn, error) {
		clearDeadline()
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("platform: egress: %s: %w", proxy.addr(), ctxErr)
		}
		return nil, fmt.Errorf("platform: egress: %s: %w", proxy.addr(), err)
	}

	if proxy.scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: proxy.host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("proxy TLS handshake: %w", err))
		}
		conn = tlsConn
	}

	if _, err := io.WriteString(conn, connectRequest(address, proxy)); err != nil {
		return fail(fmt.Errorf("write CONNECT request: %w", err))
	}
	status, err := readConnectResponse(conn)
	if err != nil {
		return fail(err)
	}
	if status < 200 || status > 299 {
		return fail(fmt.Errorf("proxy rejected CONNECT with status %d", status))
	}
	clearDeadline()
	return conn, nil
}

// connectRequest builds an RFC 7231 CONNECT request. Credentials never appear
// in error messages.
func connectRequest(address string, proxy proxyEndpoint) string {
	var builder strings.Builder
	builder.WriteString("CONNECT ")
	builder.WriteString(address)
	builder.WriteString(" HTTP/1.1\r\nHost: ")
	builder.WriteString(address)
	builder.WriteString("\r\n")
	if proxy.hasAuth {
		token := base64.StdEncoding.EncodeToString([]byte(proxy.user + ":" + proxy.pass))
		builder.WriteString("Proxy-Authorization: Basic ")
		builder.WriteString(token)
		builder.WriteString("\r\n")
	}
	builder.WriteString("Proxy-Connection: keep-alive\r\n\r\n")
	return builder.String()
}

// readConnectResponse reads the proxy's response head without reading past
// it, so no tunneled bytes can be lost.
func readConnectResponse(conn net.Conn) (int, error) {
	head := make([]byte, 0, 512)
	var one [1]byte
	for len(head) < maxProxyHeader {
		n, err := conn.Read(one[:])
		if n > 0 {
			head = append(head, one[0])
			if len(head) >= 4 && string(head[len(head)-4:]) == "\r\n\r\n" {
				break
			}
		}
		if err != nil {
			return 0, fmt.Errorf("read proxy response: %w", err)
		}
	}
	if len(head) < 4 || string(head[len(head)-4:]) != "\r\n\r\n" {
		return 0, errors.New("proxy response head is missing its terminator")
	}
	lines := strings.Split(string(head), "\r\n")
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, fmt.Errorf("malformed proxy status line %q", lines[0])
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil || status < 100 || status > 599 {
		return 0, fmt.Errorf("malformed proxy status code %q", fields[1])
	}
	return status, nil
}
