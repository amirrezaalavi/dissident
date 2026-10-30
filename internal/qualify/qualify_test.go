package qualify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

const validUUID = "11111111-1111-1111-1111-111111111111"

func TestNeedsTLS(t *testing.T) {
	cases := []struct {
		name string
		link contract.Link
		want bool
	}{
		{"vless tls", contract.Link{Protocol: contract.ProtocolVLESS, Params: map[string]string{"security": "tls"}}, true},
		{"vless none", contract.Link{Protocol: contract.ProtocolVLESS, Params: map[string]string{"security": "none"}}, false},
		{"vless default", contract.Link{Protocol: contract.ProtocolVLESS, Params: map[string]string{}}, false},
		{"vless reality", contract.Link{Protocol: contract.ProtocolVLESS, Params: map[string]string{"security": "reality"}}, false},
		{"trojan default", contract.Link{Protocol: contract.ProtocolTrojan, Params: map[string]string{}}, true},
		{"trojan none", contract.Link{Protocol: contract.ProtocolTrojan, Params: map[string]string{"security": "none"}}, false},
		{"vmess tls", contract.Link{Protocol: contract.ProtocolVMess, Params: map[string]string{"tls": "tls"}}, true},
		{"vmess plain", contract.Link{Protocol: contract.ProtocolVMess, Params: map[string]string{"tls": "none"}}, false},
		{"ss", contract.Link{Protocol: contract.ProtocolShadowsocks}, false},
		{"socks", contract.Link{Protocol: contract.ProtocolSOCKS5}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := needsTLS(testCase.link); got != testCase.want {
				t.Fatalf("needsTLS = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestValidateProtocol(t *testing.T) {
	realityKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	base := func(protocol contract.Protocol, params map[string]string) contract.Link {
		return contract.Link{Protocol: protocol, Server: "example.com", Port: 443, Params: params}
	}
	cases := []struct {
		name    string
		link    contract.Link
		wantErr string
	}{
		{"valid vless", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID}), ""},
		{"vless missing uuid", base(contract.ProtocolVLESS, map[string]string{}), "missing uuid"},
		{"vless bad uuid", base(contract.ProtocolVLESS, map[string]string{"uuid": "nope"}), "invalid uuid"},
		{"vless grpc no service", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "network": "grpc"}), "service name"},
		{"vless grpc service", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "network": "grpc", "servicename": "svc"}), ""},
		{"vless xhttp no path", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "network": "xhttp", "host": "example.com"}), "requires path"},
		{"vless reality valid", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "security": "reality", "pbk": realityKey, "sni": "example.com", "sid": "ab12"}), ""},
		{"vless reality bad key", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "security": "reality", "pbk": "short", "sni": "example.com"}), "32-byte"},
		{"vless reality bad sid", base(contract.ProtocolVLESS, map[string]string{"uuid": validUUID, "security": "reality", "pbk": realityKey, "sni": "example.com", "sid": "zz"}), "not hex"},
		{"valid vmess", base(contract.ProtocolVMess, map[string]string{"id": validUUID, "aid": "0", "scy": "auto", "network": "ws"}), ""},
		{"vmess bad aid", base(contract.ProtocolVMess, map[string]string{"id": validUUID, "aid": "70000"}), "alterId"},
		{"vmess bad cipher", base(contract.ProtocolVMess, map[string]string{"id": validUUID, "scy": "rot13"}), "cipher"},
		{"valid trojan", base(contract.ProtocolTrojan, map[string]string{"password": "secret", "network": "tcp"}), ""},
		{"trojan missing password", base(contract.ProtocolTrojan, map[string]string{}), "missing password"},
		{"valid ss", base(contract.ProtocolShadowsocks, map[string]string{"method": "aes-256-gcm", "password": "secret"}), ""},
		{"ss plugin", base(contract.ProtocolShadowsocks, map[string]string{"method": "aes-256-gcm", "password": "secret", "plugin": "obfs"}), "plugins"},
		{"valid socks", base(contract.ProtocolSOCKS5, map[string]string{}), ""},
		{"socks password only", base(contract.ProtocolSOCKS5, map[string]string{"password": "secret"}), "without username"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateProtocol(testCase.link)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, testCase.wantErr)
			}
		})
	}
}

// startTestSOCKS runs a no-auth SOCKS5 server that relays TCP.
func startTestSOCKS(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				serveTestSOCKS(conn)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func serveTestSOCKS(conn net.Conn) {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting[0] != 0x05 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	if header[0] != 0x05 || header[1] != 0x01 {
		return
	}
	var host string
	switch header[3] {
	case 0x01:
		var raw [4]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return
		}
		host = net.IP(raw[:]).String()
	case 0x04:
		var raw [16]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return
		}
		host = net.IP(raw[:]).String()
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = string(raw)
	default:
		return
	}
	var portRaw [2]byte
	if _, err := io.ReadFull(conn, portRaw[:]); err != nil {
		return
	}
	port := int(portRaw[0])<<8 | int(portRaw[1])
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(target, conn)
		if tcp, ok := target.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(conn, target)
	<-done
}

func startTestHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = 'x'
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/generate_204", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/ok", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	})
	mux.HandleFunc("/content", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	})
	mux.HandleFunc("/small", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("small"))
	})
	mux.HandleFunc("/redirect", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/content", http.StatusFound)
	})
	mux.HandleFunc("/speed", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	})
	mux.HandleFunc("/fail", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func testOptions(urls config.URLsConfig) Options {
	return Options{
		Test: config.TestConfig{
			TCPTimeout:      time.Second,
			TLSTimeout:      time.Second,
			ProtocolTimeout: time.Second,
			XrayTimeout:     10 * time.Second,
			HTTPTimeout:     2 * time.Second,
			SpeedSize:       64 << 10,
			StabilityPasses: 1,
			ContentQuorum:   1,
			ContentMinBytes: 1024,
		},
		URLs:    urls,
		XrayBin: "/nonexistent/xray",
	}
}

func TestSpeedWithSOCKS(t *testing.T) {
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)

	t.Run("primary", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{Speed: server.URL + "/speed"}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mbps, ttfb, err := tester.Speed(ctx, socksAddr)
		if err != nil {
			t.Fatalf("Speed: %v", err)
		}
		if mbps <= 0 {
			t.Fatalf("mbps = %f", mbps)
		}
		if ttfb < 0 {
			t.Fatalf("ttfb = %s", ttfb)
		}
	})

	t.Run("fallback on non-2xx", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{
			Speed:         server.URL + "/fail",
			SpeedFallback: server.URL + "/speed",
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := tester.Speed(ctx, socksAddr); err != nil {
			t.Fatalf("Speed fallback: %v", err)
		}
	})

	t.Run("all fail", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{Speed: server.URL + "/fail"}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := tester.Speed(ctx, socksAddr); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestVerifyAndProbeWithSOCKS(t *testing.T) {
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tester := New(testOptions(config.URLsConfig{
		Verify: []string{server.URL + "/generate_204"},
		Probe:  []string{server.URL + "/fail", server.URL + "/redirect"},
	}))
	if err := tester.Verify(ctx, socksAddr); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := tester.Probe(ctx, socksAddr); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	failing := New(testOptions(config.URLsConfig{
		Verify: []string{server.URL + "/fail"},
		Probe:  []string{server.URL + "/fail"},
	}))
	if err := failing.Verify(ctx, socksAddr); err == nil {
		t.Fatal("expected Verify failure")
	}
	if err := failing.Probe(ctx, socksAddr); err == nil {
		t.Fatal("expected Probe failure")
	}
}

func TestUploadWithSOCKS(t *testing.T) {
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)

	t.Run("accepted response", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{Upload: server.URL + "/generate_204"}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tester.Upload(ctx, socksAddr); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{Upload: server.URL + "/fail"}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tester.Upload(ctx, socksAddr); err == nil {
			t.Fatal("expected Upload failure")
		}
	})

	t.Run("unset URL is a no-op", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{}))
		if err := tester.Upload(context.Background(), socksAddr); err != nil {
			t.Fatalf("Upload without URL: %v", err)
		}
	})
}

// acceptCloser runs a listener that accepts and closes connections so TCP
// stages pass without a real service behind them.
func acceptCloser(t *testing.T) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	addr, _ := listener.Addr().(*net.TCPAddr)
	return listener.Addr().String(), addr.Port
}

func TestStageOrdering(t *testing.T) {
	address, portNumber := acceptCloser(t)
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("support first", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{}))
		result := tester.Test(context.Background(), contract.Link{
			Protocol: "hysteria2", Server: host, Port: portNumber, Hash: "bad",
		})
		if result.OK || result.Stage != contract.StageParse {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("tcp before render", func(t *testing.T) {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := probe.Addr().(*net.TCPAddr).Port
		probe.Close()

		tester := New(testOptions(config.URLsConfig{}))
		result := tester.Test(context.Background(), contract.Link{
			Protocol: contract.ProtocolSOCKS5,
			Server:   "127.0.0.1",
			Port:     closedPort,
			Hash:     "tcp",
			Params:   map[string]string{},
		})
		if result.OK || result.Stage != contract.StageTCP {
			t.Fatalf("result = %+v", result)
		}
		if !strings.HasPrefix(result.Reason, "tcp:") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})

	t.Run("protocol before render", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{}))
		result := tester.Test(context.Background(), contract.Link{
			Protocol: contract.ProtocolVLESS,
			Server:   host,
			Port:     portNumber,
			Hash:     "protocol",
			Params: map[string]string{
				"uuid":    validUUID,
				"network": "grpc",
			},
		})
		if result.OK || result.Stage != contract.StageProtocol {
			t.Fatalf("result = %+v", result)
		}
		if !strings.HasPrefix(result.Reason, "protocol:") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})

	t.Run("tls before protocol", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{}))
		result := tester.Test(context.Background(), contract.Link{
			Protocol: contract.ProtocolVLESS,
			Server:   host,
			Port:     portNumber,
			Hash:     "tls",
			Params: map[string]string{
				"uuid":     validUUID,
				"security": "tls",
				"sni":      "example.com",
			},
		})
		if result.OK || result.Stage != contract.StageTLS {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("preflight before real", func(t *testing.T) {
		tester := New(testOptions(config.URLsConfig{}))
		result := tester.Test(context.Background(), contract.Link{
			Protocol: contract.ProtocolSOCKS5,
			Server:   host,
			Port:     portNumber,
			Hash:     "render",
			Params:   map[string]string{},
		})
		if result.OK || result.Stage != contract.StageRender {
			t.Fatalf("result = %+v", result)
		}
	})
}

func realXrayAvailable() bool {
	_, err := os.Stat("/usr/local/bin/xray")
	return err == nil
}

func fullPipelineOptions(server *httptest.Server) Options {
	return Options{
		Test: config.TestConfig{
			TCPTimeout:      2 * time.Second,
			TLSTimeout:      2 * time.Second,
			ProtocolTimeout: 2 * time.Second,
			XrayTimeout:     10 * time.Second,
			HTTPTimeout:     3 * time.Second,
			SpeedSize:       64 << 10,
			MinSpeedMbps:    0,
			MaxTTFBMs:       0,
			StabilityPasses: 2,
			ContentQuorum:   2,
			ContentMinBytes: 1024,
		},
		URLs: config.URLsConfig{
			Probe:         []string{server.URL + "/generate_204"},
			Verify:        []string{server.URL + "/generate_204"},
			Content:       []string{server.URL + "/content", server.URL + "/small", server.URL + "/redirect"},
			Speed:         server.URL + "/speed",
			SpeedFallback: server.URL + "/speed",
		},
		XrayBin: "/usr/local/bin/xray",
	}
}

func socksLink(t *testing.T, socksAddr string) contract.Link {
	t.Helper()
	host, portString, err := net.SplitHostPort(socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portString)
	return contract.Link{
		Protocol: contract.ProtocolSOCKS5,
		Server:   host,
		Port:     port,
		Hash:     "socks-upstream",
		Params:   map[string]string{},
	}
}

func TestFullPipelineRealXray(t *testing.T) {
	if !realXrayAvailable() {
		t.Skip("xray binary not installed")
	}
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)
	tester := New(fullPipelineOptions(server))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result := tester.Test(ctx, socksLink(t, socksAddr))
	if !result.OK {
		t.Fatalf("qualification failed: stage=%s reason=%s", result.Stage, result.Reason)
	}
	if result.Stage != contract.StageStability || result.Reason != "ok" {
		t.Fatalf("result = %+v", result)
	}
	if result.TCPLatency <= 0 || result.TTFB <= 0 || result.SpeedMbps <= 0 {
		t.Fatalf("missing measurements: %+v", result)
	}
	if result.HTTPStatus < 200 || result.HTTPStatus > 299 {
		t.Fatalf("status = %d", result.HTTPStatus)
	}
	if result.At.Location() != time.UTC {
		t.Fatalf("At is not UTC: %s", result.At)
	}

	// Content quorum failure: only one URL reaches the minimum.
	strict := fullPipelineOptions(server)
	strict.Test.ContentQuorum = 3
	strict.Test.ContentMinBytes = 4096
	failing := New(strict)
	failed := failing.Test(ctx, socksLink(t, socksAddr))
	if failed.OK || failed.Stage != contract.StageContent {
		t.Fatalf("expected content failure, got %+v", failed)
	}
	if failed.Reason != "content: 2/3" {
		t.Fatalf("content reason = %q", failed.Reason)
	}
}

func TestPromoteRealXray(t *testing.T) {
	if !realXrayAvailable() {
		t.Skip("xray binary not installed")
	}
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)
	tester := New(fullPipelineOptions(server))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, xray, err := tester.Promote(ctx, socksLink(t, socksAddr))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if xray == nil || !xray.Alive() {
		t.Fatal("Promote returned no live process")
	}
	if !result.OK || result.Reason != "ok" {
		t.Fatalf("result = %+v", result)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopCancel()
	defer xray.Stop(stopCtx)

	// The promoted process must serve the verify URL.
	verifyTester := New(Options{
		Test:    config.TestConfig{HTTPTimeout: 3 * time.Second, TLSTimeout: time.Second},
		URLs:    config.URLsConfig{Verify: []string{server.URL + "/generate_204"}},
		XrayBin: "/usr/local/bin/xray",
	})
	if err := verifyTester.Verify(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port))); err != nil {
		t.Fatalf("Verify promoted process: %v", err)
	}

	// A failed promotion must stop its process and return nil.
	failing := New(fullPipelineOptions(server))
	failing.urls.Verify = []string{server.URL + "/fail"}
	_, process, err := failing.Promote(ctx, socksLink(t, socksAddr))
	if err == nil || process != nil {
		t.Fatalf("expected failed promotion, got process=%v err=%v", process, err)
	}
}

func TestPromoteTrustedRealXray(t *testing.T) {
	if !realXrayAvailable() {
		t.Skip("xray binary not installed")
	}
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Poison every stage the trusted path must skip. If PromoteTrusted calls
	// upload, HTTP/2, or speed it will fail instead of returning a process.
	poisoned := fullPipelineOptions(server)
	poisoned.URLs.Upload = server.URL + "/fail"
	poisoned.URLs.HTTP2 = server.URL + "/fail"
	poisoned.URLs.Speed = server.URL + "/fail"
	poisoned.URLs.SpeedFallback = server.URL + "/fail"

	result, xray, err := New(poisoned).PromoteTrusted(ctx, socksLink(t, socksAddr))
	if err != nil {
		t.Fatalf("PromoteTrusted: %v", err)
	}
	if xray == nil || !xray.Alive() {
		t.Fatal("PromoteTrusted returned no live process")
	}
	if !result.OK || result.Reason != "ok" || result.Stage != contract.StageReal {
		t.Fatalf("result = %+v", result)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopCancel()
	defer xray.Stop(stopCtx)

	// The same poisoned configuration must fail the full path at upload,
	// proving the trusted path really skipped those stages.
	if _, process, err := New(poisoned).Promote(ctx, socksLink(t, socksAddr)); err == nil || process != nil {
		t.Fatalf("full Promote with poisoned upload/speed = (%v, %v), want failure", process, err)
	}

	// A failed trusted verify must stop its process and return nil.
	failing := New(fullPipelineOptions(server))
	failing.urls.Verify = []string{server.URL + "/fail"}
	_, process, err := failing.PromoteTrusted(ctx, socksLink(t, socksAddr))
	if err == nil || process != nil {
		t.Fatalf("expected failed trusted promotion, got process=%v err=%v", process, err)
	}
}

func TestVerifyTrustedRetriesTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"i/o timeout", errors.New("read tcp 127.0.0.1:1: i/o timeout"), true},
		{"unexpected EOF", errors.New("unexpected EOF"), true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped reset", fmt.Errorf("verify: %w", syscall.ECONNRESET), true},
		{"context canceled", context.Canceled, false},
		{"http 500", errors.New("https://verify.example: HTTP 500"), false},
	} {
		if got := isTransientVerifyError(tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", tc.name, got, tc.want)
		}
	}

	t.Run("recovers", func(t *testing.T) {
		tester := New(Options{})
		calls := 0
		tester.trustedVerify = func(context.Context, string) error {
			calls++
			if calls < trustedVerifyAttempts {
				return errors.New("read tcp 127.0.0.1:1: i/o timeout")
			}
			return nil
		}
		start := time.Now()
		if _, err := tester.verifyTrusted(context.Background(), "hash", "127.0.0.1:1"); err != nil {
			t.Fatalf("verifyTrusted: %v", err)
		}
		if calls != trustedVerifyAttempts {
			t.Fatalf("attempts = %d, want %d", calls, trustedVerifyAttempts)
		}
		if elapsed := time.Since(start); elapsed < 2*trustedVerifyBackoff {
			t.Fatalf("elapsed = %v, want at least two backoffs", elapsed)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		tester := New(Options{})
		calls := 0
		tester.trustedVerify = func(context.Context, string) error {
			calls++
			return errors.New("connection reset by peer")
		}
		if _, err := tester.verifyTrusted(context.Background(), "hash", "127.0.0.1:1"); err == nil {
			t.Fatal("verifyTrusted should fail after the retry budget")
		}
		if calls != trustedVerifyAttempts {
			t.Fatalf("attempts = %d, want %d", calls, trustedVerifyAttempts)
		}
	})

	t.Run("permanent", func(t *testing.T) {
		tester := New(Options{})
		calls := 0
		tester.trustedVerify = func(context.Context, string) error {
			calls++
			return errors.New("https://verify.example: HTTP 403")
		}
		if _, err := tester.verifyTrusted(context.Background(), "hash", "127.0.0.1:1"); err == nil {
			t.Fatal("verifyTrusted should fail on a non-2xx response")
		}
		if calls != 1 {
			t.Fatalf("attempts = %d, want 1 (permanent failure must not retry)", calls)
		}
	})
}

func TestPromoteTrustedRetriesTransientVerifyAndKeepsProcess(t *testing.T) {
	if !realXrayAvailable() {
		t.Skip("xray binary not installed")
	}
	server := startTestHTTP(t)
	socksAddr := startTestSOCKS(t)
	tester := New(fullPipelineOptions(server))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	calls := 0
	var addrs []string
	tester.trustedVerify = func(_ context.Context, addr string) error {
		calls++
		addrs = append(addrs, addr)
		if calls < trustedVerifyAttempts {
			return errors.New("read tcp: i/o timeout")
		}
		return nil
	}

	result, xray, err := tester.PromoteTrusted(ctx, socksLink(t, socksAddr))
	if err != nil {
		t.Fatalf("PromoteTrusted: %v", err)
	}
	if calls != trustedVerifyAttempts {
		t.Fatalf("verify attempts = %d, want %d", calls, trustedVerifyAttempts)
	}
	for i := 1; i < len(addrs); i++ {
		if addrs[i] != addrs[0] {
			t.Fatalf("verify used different SOCKS ports after retry: %v", addrs)
		}
	}
	if xray == nil || !xray.Alive() {
		t.Fatal("PromoteTrusted did not keep the started process")
	}
	if !result.OK || result.Reason != "ok" {
		t.Fatalf("result = %+v", result)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopCancel()
	defer xray.Stop(stopCtx)
}

func TestPromoteTrustedDoesNotRetryStaticFailure(t *testing.T) {
	tester := New(testOptions(config.URLsConfig{}))
	calls := 0
	tester.trustedVerify = func(context.Context, string) error {
		calls++
		return nil
	}
	link := contract.Link{
		Protocol: contract.Protocol("not-a-protocol"),
		Server:   "127.0.0.1",
		Port:     1080,
		Hash:     "static-failure",
		Params:   map[string]string{},
	}
	result, xray, err := tester.PromoteTrusted(context.Background(), link)
	if err == nil || xray != nil {
		t.Fatalf("static failure = (%v, %v), want error and no process", xray, err)
	}
	if result.Stage != contract.StageParse {
		t.Fatalf("stage = %q, want parse", result.Stage)
	}
	if calls != 0 {
		t.Fatalf("verify calls = %d, want 0 for a static support failure", calls)
	}
}
