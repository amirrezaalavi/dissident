package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dissident/internal/contract"
)

// ---------- fake route provider / dialer ----------

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type reportCall struct {
	route Route
	err   error
}

type fakeProvider struct {
	mu         sync.Mutex
	route      Route
	routeErr   error
	routeCalls int
	reports    chan reportCall
}

func newFakeProvider(id string, dial Dialer) *fakeProvider {
	return &fakeProvider{
		route:   Route{ID: id, Dial: dial},
		reports: make(chan reportCall, 32),
	}
}

func (p *fakeProvider) Route(ctx context.Context) (Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routeCalls++
	if p.routeErr != nil {
		return Route{}, p.routeErr
	}
	return p.route, nil
}

func (p *fakeProvider) Report(ctx context.Context, route Route, err error) {
	select {
	case p.reports <- reportCall{route: route, err: err}:
	default:
	}
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routeCalls
}

// echoDialer dials the local echo server regardless of the requested target,
// optionally capturing the requested address. It keeps tests fully local.
func echoDialer(echoAddr string, captured chan<- string) Dialer {
	return dialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		if captured != nil {
			select {
			case captured <- address:
			default:
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, echoAddr)
	})
}

// ---------- local echo server ----------

type echoServer struct {
	ln    net.Listener
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	once  sync.Once
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	e := &echoServer{ln: ln, conns: make(map[net.Conn]struct{})}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.mu.Lock()
			e.conns[c] = struct{}{}
			e.mu.Unlock()
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				defer c.Close()
				defer func() {
					e.mu.Lock()
					delete(e.conns, c)
					e.mu.Unlock()
				}()
				buf := make([]byte, 32*1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(e.Close)
	return e
}

func (e *echoServer) Addr() string { return e.ln.Addr().String() }

func (e *echoServer) Close() {
	e.once.Do(func() {
		_ = e.ln.Close()
		e.mu.Lock()
		for c := range e.conns {
			_ = c.Close()
		}
		e.mu.Unlock()
		e.wg.Wait()
	})
}

// ---------- service helpers ----------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestService(t *testing.T, provider RouteProvider) (*Service, <-chan contract.AccessRecord) {
	t.Helper()
	access := make(chan contract.AccessRecord, 64)
	s := New(Options{
		Listen:   "127.0.0.1:0",
		Provider: provider,
		Access: func(rec contract.AccessRecord) {
			select {
			case access <- rec:
			default:
			}
		},
		Logger: discardLogger(),
	})
	t.Cleanup(func() { stopTestService(t, s) })
	return s, access
}

func startTestService(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func newRunningService(t *testing.T, provider RouteProvider) (*Service, <-chan contract.AccessRecord) {
	t.Helper()
	s, access := newTestService(t, provider)
	startTestService(t, s)
	return s, access
}

func stopTestService(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func waitAccess(t *testing.T, ch <-chan contract.AccessRecord) contract.AccessRecord {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for access record")
		return contract.AccessRecord{}
	}
}

func waitReport(t *testing.T, ch <-chan reportCall) reportCall {
	t.Helper()
	select {
	case rep := <-ch:
		return rep
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route report")
		return reportCall{}
	}
}

func assertNoReport(t *testing.T, ch <-chan reportCall) {
	t.Helper()
	select {
	case rep := <-ch:
		t.Fatalf("unexpected route report: route=%q err=%v", rep.route.ID, rep.err)
	default:
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- client helpers ----------

func dialProxy(t *testing.T, s *Service) net.Conn {
	t.Helper()
	addr := s.Addr()
	if addr == nil {
		t.Fatal("service has no listener address")
	}
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

const connectOK = "HTTP/1.1 200 Connection Established\r\n\r\n"

func writeConnect(t *testing.T, c net.Conn, target string) {
	t.Helper()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
}

func readExact(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func readHTTPStatus(t *testing.T, c net.Conn) string {
	t.Helper()
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read HTTP status: %v", err)
	}
	return strings.TrimSpace(line)
}

func closeWriteClient(t *testing.T, c net.Conn) {
	t.Helper()
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("client connection is %T, want *net.TCPConn", c)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
}

// ---------- SOCKS5 client helpers ----------

func socksGreeting(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write([]byte{socksVersion, 1, socksMethodNoAuth}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := readExact(t, c, 2)
	if reply[0] != socksVersion || reply[1] != socksMethodNoAuth {
		t.Fatalf("greeting reply = %v, want [5 0]", reply)
	}
}

func socksRequestIPv4(t *testing.T, c net.Conn, ip net.IP, port int) {
	t.Helper()
	v4 := ip.To4()
	if v4 == nil {
		t.Fatalf("%s is not IPv4", ip)
	}
	req := append([]byte{socksVersion, socksCmdConnect, 0x00, socksAtypIPv4}, v4...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write SOCKS request: %v", err)
	}
}

func socksRequestIPv6(t *testing.T, c net.Conn, ip net.IP, port int) {
	t.Helper()
	v6 := ip.To16()
	if v6 == nil {
		t.Fatalf("%s is not IPv6", ip)
	}
	req := append([]byte{socksVersion, socksCmdConnect, 0x00, socksAtypIPv6}, v6...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write SOCKS request: %v", err)
	}
}

func socksRequestDomain(t *testing.T, c net.Conn, host string, port int) {
	t.Helper()
	if len(host) == 0 || len(host) > 255 {
		t.Fatalf("invalid domain length %d", len(host))
	}
	req := []byte{socksVersion, socksCmdConnect, 0x00, socksAtypDomain, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write SOCKS request: %v", err)
	}
}

func socksRequestRaw(t *testing.T, c net.Conn, cmd, atyp byte, addr []byte, port int) {
	t.Helper()
	req := []byte{socksVersion, cmd, 0x00, atyp}
	req = append(req, addr...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write SOCKS request: %v", err)
	}
}

// readSOCKSReply consumes the fixed 10-byte reply and returns REP.
func readSOCKSReply(t *testing.T, c net.Conn) byte {
	t.Helper()
	reply := readExact(t, c, 10)
	if reply[0] != socksVersion {
		t.Fatalf("reply version = %d, want 5", reply[0])
	}
	if reply[2] != 0x00 {
		t.Fatalf("reply RSV = %d, want 0", reply[2])
	}
	wantBnd := []byte{socksAtypIPv4, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(reply[3:], wantBnd) {
		t.Fatalf("reply BND = %v, want %v", reply[3:], wantBnd)
	}
	return reply[1]
}

func targetPort(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port %q: %v", p, err)
	}
	return n
}

// dialThroughSOCKS performs the no-auth greeting and a CONNECT request.
func dialThroughSOCKS(t *testing.T, s *Service) net.Conn {
	t.Helper()
	c := dialProxy(t, s)
	socksGreeting(t, c)
	return c
}

var errFakeDial = errors.New("fake dial failure")
