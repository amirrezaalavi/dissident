package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"dissident/internal/contract"
	"dissident/internal/uri"
	"dissident/internal/xraycfg"
)

func TestMain(m *testing.M) {
	if os.Getenv("DISSIDENT_TEST_XRAY_HELPER") == "1" {
		runFakeXray()
		return
	}
	os.Exit(m.Run())
}

// runFakeXray is a stand-in xray binary: it opens the first inbound port from
// its -c config and accepts connections. It is re-executed from this test
// binary by fakeXrayScript.
func runFakeXray() {
	var configPath string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake xray:", err)
		os.Exit(1)
	}
	var document struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Inbounds) == 0 {
		fmt.Fprintln(os.Stderr, "fake xray: bad config")
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(document.Inbounds[0].Port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake xray:", err)
		os.Exit(1)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func fakeXrayScript(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake xray uses a POSIX shell wrapper")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	script := "#!/bin/sh\nDISSIDENT_TEST_XRAY_HELPER=1 exec " + shellQuote(executable) + " \"$@\"\n"
	path := filepath.Join(t.TempDir(), "fake-xray.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray script: %v", err)
	}
	return path
}

func TestFreePort(t *testing.T) {
	for i := 0; i < 5; i++ {
		port, err := FreePort()
		if err != nil {
			t.Fatalf("FreePort: %v", err)
		}
		if port < 1 || port > 65535 {
			t.Fatalf("FreePort returned %d", port)
		}
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("port %d is not bindable: %v", port, err)
		}
		listener.Close()
	}
}

// startEchoServer returns a TCP echo server address.
func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
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
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// startSOCKSServer runs a minimal RFC 1928 no-auth server that relays TCP.
func startSOCKSServer(t *testing.T) string {
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
				_ = serveTestSOCKS(conn)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func serveTestSOCKS(conn net.Conn) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if greeting[0] != 0x05 {
		return fmt.Errorf("bad version %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 0x05 || header[1] != 0x01 {
		return fmt.Errorf("unsupported request")
	}
	var host string
	switch header[3] {
	case 0x01:
		var raw [4]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return err
		}
		host = net.IP(raw[:]).String()
	case 0x04:
		var raw [16]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return err
		}
		host = net.IP(raw[:]).String()
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return err
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return err
		}
		host = string(raw)
	default:
		_, _ = conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("bad address type")
	}
	var portRaw [2]byte
	if _, err := io.ReadFull(conn, portRaw[:]); err != nil {
		return err
	}
	port := int(portRaw[0])<<8 | int(portRaw[1])
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer target.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
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
	return nil
}

func TestSOCKS5Dialer(t *testing.T) {
	echoAddr := startEchoServer(t)
	socksAddr := startSOCKSServer(t)
	dialer := &SOCKS5Dialer{Addr: socksAddr, Timeout: 3 * time.Second}

	roundTrip := func(t *testing.T, target string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("hello")); err != nil {
			t.Fatalf("write: %v", err)
		}
		buffer := make([]byte, 5)
		if _, err := io.ReadFull(conn, buffer); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buffer) != "hello" {
			t.Fatalf("echo = %q", buffer)
		}
	}

	t.Run("ip target", func(t *testing.T) { roundTrip(t, echoAddr) })

	host, port, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("domain target", func(t *testing.T) {
		if host == "127.0.0.1" {
			roundTrip(t, net.JoinHostPort("localhost", port))
		}
	})

	t.Run("refused", func(t *testing.T) {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedAddr := probe.Addr().String()
		probe.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := dialer.DialContext(ctx, "tcp", closedAddr); err == nil {
			t.Fatal("expected a dial error")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		blackhole, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blackhole.Close()
		go func() {
			for {
				conn, err := blackhole.Accept()
				if err != nil {
					return
				}
				go func(conn net.Conn) {
					defer conn.Close()
					_, _ = io.Copy(io.Discard, conn)
				}(conn)
			}
		}()
		slow := &SOCKS5Dialer{Addr: blackhole.Addr().String(), Timeout: 5 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := slow.DialContext(ctx, "tcp", "127.0.0.1:80"); err == nil {
			t.Fatal("expected a handshake timeout")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("handshake timeout took %s", elapsed)
		}
	})
}

// startConnectProxy runs a minimal HTTP CONNECT proxy. expectedAuth is the
// full Proxy-Authorization value required, or empty for no auth.
func startConnectProxy(t *testing.T, expectedAuth string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
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
				serveTestConnect(conn, expectedAuth, "")
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func serveTestConnect(conn net.Conn, expectedAuth, rejectWith string) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || fields[0] != "CONNECT" {
		fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	target := fields[1]
	auth := ""
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(header, "\r\n")
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "proxy-authorization:") {
			auth = strings.TrimSpace(trimmed[len("proxy-authorization:"):])
		}
	}
	if expectedAuth != "" && auth != expectedAuth {
		fmt.Fprint(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if rejectWith != "" {
		fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: 0\r\n\r\n", rejectWith)
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		fmt.Fprint(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(conn, upstream)
	<-done
}

func TestEgressDialer(t *testing.T) {
	echoAddr := startEchoServer(t)

	token := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	authProxy := startConnectProxy(t, "Basic "+token)
	openProxy := startConnectProxy(t, "")

	t.Run("http connect", func(t *testing.T) {
		dial, err := NewEgressDialer("http://"+openProxy, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := dial(ctx, "tcp", echoAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(conn, buffer); err != nil {
			t.Fatal(err)
		}
		if string(buffer) != "ping" {
			t.Fatalf("echo = %q", buffer)
		}
	})

	t.Run("basic auth", func(t *testing.T) {
		dial, err := NewEgressDialer("http://user:secret@"+authProxy, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := dial(ctx, "tcp", echoAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.Close()
	})

	t.Run("bad auth", func(t *testing.T) {
		dial, err := NewEgressDialer("http://user:wrong@"+authProxy, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := dial(ctx, "tcp", echoAddr); err == nil || !strings.Contains(err.Error(), "407") {
			t.Fatalf("expected 407 error, got %v", err)
		}
	})

	t.Run("missing auth", func(t *testing.T) {
		dial, err := NewEgressDialer("http://"+authProxy, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := dial(ctx, "tcp", echoAddr); err == nil {
			t.Fatal("expected auth failure")
		}
	})

	t.Run("socks rejected", func(t *testing.T) {
		if _, err := NewEgressDialer("socks5://127.0.0.1:1080", time.Second); err == nil {
			t.Fatal("expected scheme error")
		}
	})

	t.Run("unsupported network", func(t *testing.T) {
		dial, err := NewEgressDialer("http://"+openProxy, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dial(context.Background(), "udp", echoAddr); err == nil {
			t.Fatal("expected network error")
		}
	})
}

func TestRenderWithEgress(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVLESS,
		Server:   "example.com",
		Port:     443,
		Hash:     "test",
		Params: map[string]string{
			"uuid":     "11111111-1111-1111-1111-111111111111",
			"network":  "ws",
			"security": "tls",
			"host":     "example.com",
			"sni":      "example.com",
			"path":     "/ws",
		},
	}
	const port = 23456

	plain, err := RenderWithEgress(link, port, "")
	if err != nil {
		t.Fatalf("render plain: %v", err)
	}
	want, err := xraycfg.Render(link, port)
	if err != nil {
		t.Fatalf("xraycfg.Render: %v", err)
	}
	if !bytes.Equal(plain, want) {
		t.Fatal("empty egress changed the rendered config")
	}

	rendered, err := RenderWithEgress(link, port, "http://user:secret@proxy.example:3128")
	if err != nil {
		t.Fatalf("render with egress: %v", err)
	}
	var document struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Tag      string `json:"tag"`
			Settings struct {
				Servers []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
					Users   []struct {
						User string `json:"user"`
						Pass string `json:"pass"`
					} `json:"users"`
				} `json:"servers"`
			} `json:"settings"`
			StreamSettings struct {
				Network  string `json:"network"`
				Security string `json:"security"`
				Sockopt  struct {
					DialerProxy string `json:"dialerProxy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatalf("rendered config does not parse: %v", err)
	}
	if len(document.Inbounds) != 1 || document.Inbounds[0].Port != port {
		t.Fatalf("inbound not preserved: %+v", document.Inbounds)
	}
	if len(document.Outbounds) != 2 {
		t.Fatalf("outbounds = %d, want 2", len(document.Outbounds))
	}
	first, egress := document.Outbounds[0], document.Outbounds[1]
	if first.Protocol != "vless" || first.StreamSettings.Network != "ws" || first.StreamSettings.Security != "tls" {
		t.Fatalf("first outbound changed: %+v", first)
	}
	if first.StreamSettings.Sockopt.DialerProxy != "egress" {
		t.Fatalf("dialerProxy = %q", first.StreamSettings.Sockopt.DialerProxy)
	}
	if egress.Protocol != "http" || egress.Tag != "egress" {
		t.Fatalf("egress outbound = %+v", egress)
	}
	if len(egress.Settings.Servers) != 1 {
		t.Fatalf("egress servers = %+v", egress.Settings.Servers)
	}
	server := egress.Settings.Servers[0]
	if server.Address != "proxy.example" || server.Port != 3128 {
		t.Fatalf("egress server = %+v", server)
	}
	if len(server.Users) != 1 || server.Users[0].User != "user" || server.Users[0].Pass != "secret" {
		t.Fatalf("egress users = %+v", server.Users)
	}

	noAuth, err := RenderWithEgress(link, port, "https://proxy.example")
	if err != nil {
		t.Fatalf("render https egress: %v", err)
	}
	var withoutAuth struct {
		Outbounds []struct {
			Settings struct {
				Servers []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
					Users   []any  `json:"users"`
				} `json:"servers"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(noAuth, &withoutAuth); err != nil {
		t.Fatalf("https egress config does not parse: %v", err)
	}
	if len(withoutAuth.Outbounds) != 2 {
		t.Fatalf("https egress outbounds = %d", len(withoutAuth.Outbounds))
	}
	httpsServer := withoutAuth.Outbounds[1].Settings.Servers[0]
	if httpsServer.Address != "proxy.example" || httpsServer.Port != 443 {
		t.Fatalf("https egress server = %+v", httpsServer)
	}
	if len(httpsServer.Users) != 0 {
		t.Fatalf("unexpected users without userinfo: %+v", httpsServer.Users)
	}

	if _, err := RenderWithEgress(link, port, "socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("expected socks egress to be rejected")
	}
}

func TestStartXrayUsesConfigPort(t *testing.T) {
	chosen, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	link := contract.Link{
		Protocol: contract.ProtocolSOCKS5,
		Server:   "127.0.0.1",
		Port:     1080,
		Params:   map[string]string{},
	}
	cfg, err := xraycfg.Render(link, chosen)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	xray, err := StartXray(ctx, fakeXrayScript(t), cfg, dir)
	if err != nil {
		t.Fatalf("StartXray: %v", err)
	}
	if xray.Port != chosen {
		t.Fatalf("xray.Port = %d, want %d", xray.Port, chosen)
	}
	if !xray.Alive() || xray.PID() <= 0 {
		t.Fatal("xray should be alive with a pid")
	}
	info, err := os.Stat(xray.ConfigPath)
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(chosen)), time.Second)
	if err != nil {
		t.Fatalf("dial config port: %v", err)
	}
	conn.Close()

	raw, err := os.ReadFile(xray.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Inbounds) != 1 || parsed.Inbounds[0].Port != chosen {
		t.Fatalf("config port not preserved: %v %+v", err, parsed.Inbounds)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopCancel()
	if err := xray.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if xray.Alive() || xray.PID() != 0 {
		t.Fatal("xray still alive after Stop")
	}
	select {
	case <-xray.Exited:
	default:
		t.Fatal("Exited not closed after Stop")
	}
	if _, err := os.Stat(xray.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("config file still exists: %v", err)
	}
	if err := xray.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	var nilXray *Xray
	if nilXray.Alive() || nilXray.PID() != 0 {
		t.Fatal("nil xray should be inert")
	}
	if err := nilXray.Stop(context.Background()); err != nil {
		t.Fatalf("nil Stop: %v", err)
	}
}

func TestStartXrayCleanupOnExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fails.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"inbounds":[{"listen":"127.0.0.1","port":1,"protocol":"socks"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := StartXray(ctx, script, config, dir); err == nil {
		t.Fatal("expected startup failure")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("exit detection took %s", elapsed)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "fails.sh" {
		t.Fatalf("temp config leaked: %+v", entries)
	}
}

func TestStartXrayInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := StartXray(context.Background(), fakeXrayScript(t), []byte(`{}`), dir); err == nil {
		t.Fatal("expected missing inbound port error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temp config leaked: %+v", entries)
	}
}

func TestStartXrayProcessDeath(t *testing.T) {
	chosen, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	link := contract.Link{
		Protocol: contract.ProtocolSOCKS5,
		Server:   "127.0.0.1",
		Port:     1080,
		Params:   map[string]string{},
	}
	cfg, err := xraycfg.Render(link, chosen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	xray, err := StartXray(ctx, fakeXrayScript(t), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pid := xray.PID()
	if pid <= 0 {
		t.Fatal("missing pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-xray.Exited:
	case <-time.After(3 * time.Second):
		t.Fatal("Exited did not close after process death")
	}
	if xray.Alive() || xray.PID() != 0 {
		t.Fatal("dead process reported alive")
	}
	if _, err := os.Stat(xray.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("config file not removed after death: %v", err)
	}
	if err := xray.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after death: %v", err)
	}
}

func TestStartXrayContextExpiryKillsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"inbounds":[{"listen":"127.0.0.1","port":1,"protocol":"socks"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := StartXray(ctx, script, cfg, dir); err == nil {
		t.Fatal("expected context deadline error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "hang.sh" {
		t.Fatalf("temp config leaked: %+v", entries)
	}
}

func TestStartXrayRealFreedom(t *testing.T) {
	const xrayBin = "/usr/local/bin/xray"
	if _, err := os.Stat(xrayBin); err != nil {
		t.Skip("xray binary not installed")
	}
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{
			"listen":   "127.0.0.1",
			"port":     port,
			"protocol": "socks",
			"settings": map[string]any{"udp": false},
		}},
		"outbounds": []any{map[string]any{"protocol": "freedom"}},
	}
	cfg, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, "through-xray")
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	xray, err := StartXray(ctx, xrayBin, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("StartXray: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		if err := xray.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	dialer := &SOCKS5Dialer{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port)), Timeout: 3 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext},
		Timeout:   5 * time.Second,
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET through xray: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "through-xray" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
}

// TestEgressSmoke is opt-in and performs a real request through the demo
// HTTP egress proxy:
//
//	DISSIDENT_EGRESS_PROXY=http://proxy:port \
//	DISSIDENT_EGRESS_LINK='vmess://...' \
//	go test ./internal/platform -run TestEgressSmoke -v
//
// It renders the link on a free port, starts xray with the egress outbound,
// and fetches the liveness URL through the resulting SOCKS inbound.
func TestEgressSmoke(t *testing.T) {
	const xrayBin = "/usr/local/bin/xray"
	proxy := strings.TrimSpace(os.Getenv("DISSIDENT_EGRESS_PROXY"))
	rawLink := strings.TrimSpace(os.Getenv("DISSIDENT_EGRESS_LINK"))
	if proxy == "" || rawLink == "" {
		t.Skip("set DISSIDENT_EGRESS_PROXY and DISSIDENT_EGRESS_LINK to run")
	}
	if _, err := os.Stat(xrayBin); err != nil {
		t.Skip("xray binary not installed")
	}
	link, err := uri.Parse(rawLink)
	if err != nil {
		t.Fatalf("parse smoke link: %v", err)
	}
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := RenderWithEgress(link, port, proxy)
	if err != nil {
		t.Fatalf("RenderWithEgress: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	xray, err := StartXray(ctx, xrayBin, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("StartXray: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		if err := xray.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	dialer := &SOCKS5Dialer{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port)), Timeout: 5 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext},
		Timeout:   10 * time.Second,
	}
	response, err := client.Get("https://www.gstatic.com/generate_204")
	if err != nil {
		t.Fatalf("GET generate_204 via egress: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}
