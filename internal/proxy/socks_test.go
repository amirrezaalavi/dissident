package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestSOCKS5DomainConnectAndByteAccounting(t *testing.T) {
	echo := newEchoServer(t)
	captured := make(chan string, 1)
	prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), captured))
	s, access := newRunningService(t, prov)

	c := dialThroughSOCKS(t, s)
	port := targetPort(t, echo.Addr())
	socksRequestDomain(t, c, "example.test", port)
	if rep := readSOCKSReply(t, c); rep != socksRepSucceeded {
		t.Fatalf("reply = 0x%02x, want success", rep)
	}

	payload := bytes.Repeat([]byte("socks5-"), 500)
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if got := readExact(t, c, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("echoed payload differs (%d bytes)", len(got))
	}
	closeWriteClient(t, c)

	select {
	case got := <-captured:
		want := net.JoinHostPort("example.test", strconv.Itoa(port))
		if got != want {
			t.Fatalf("dialed %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialer was not called")
	}

	rec := waitAccess(t, access)
	if rec.State != stateOK {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateOK, rec.Reason)
	}
	if rec.Proto != protoSOCKS5 {
		t.Errorf("proto = %q, want %q", rec.Proto, protoSOCKS5)
	}
	if rec.WAN != "wan-test" {
		t.Errorf("WAN = %q, want wan-test", rec.WAN)
	}
	if rec.Domain != "example.test" {
		t.Errorf("Domain = %q, want example.test", rec.Domain)
	}
	if rec.Dst != net.JoinHostPort("example.test", strconv.Itoa(port)) {
		t.Errorf("Dst = %q", rec.Dst)
	}
	if rec.Port != port {
		t.Errorf("Port = %d, want %d", rec.Port, port)
	}
	if rec.Up != int64(len(payload)) || rec.Down != int64(len(payload)) {
		t.Errorf("Up/Down = %d/%d, want %d/%d", rec.Up, rec.Down, len(payload), len(payload))
	}
	if rec.Reason != "" {
		t.Errorf("Reason = %q, want empty", rec.Reason)
	}

	rep := waitReport(t, prov.reports)
	if rep.err != nil {
		t.Fatalf("report error = %v, want nil", rep.err)
	}
	assertNoReport(t, prov.reports)
}

func TestSOCKS5IPv4Connect(t *testing.T) {
	echo := newEchoServer(t)
	captured := make(chan string, 1)
	prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), captured))
	s, access := newRunningService(t, prov)

	c := dialThroughSOCKS(t, s)
	host, _, _ := net.SplitHostPort(echo.Addr())
	socksRequestIPv4(t, c, net.ParseIP(host), targetPort(t, echo.Addr()))
	if rep := readSOCKSReply(t, c); rep != socksRepSucceeded {
		t.Fatalf("reply = 0x%02x, want success", rep)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if got := string(readExact(t, c, 5)); got != "hello" {
		t.Fatalf("echo = %q", got)
	}
	closeWriteClient(t, c)

	select {
	case got := <-captured:
		if got != echo.Addr() {
			t.Fatalf("dialed %q, want %q", got, echo.Addr())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialer was not called")
	}

	rec := waitAccess(t, access)
	if rec.State != stateOK || rec.Proto != protoSOCKS5 {
		t.Fatalf("record state/proto = %q/%q", rec.State, rec.Proto)
	}
	if rec.Dst != echo.Addr() || rec.Domain != "" || rec.Port != targetPort(t, echo.Addr()) {
		t.Fatalf("record dst/domain/port = %q/%q/%d", rec.Dst, rec.Domain, rec.Port)
	}
	if rep := waitReport(t, prov.reports); rep.err != nil {
		t.Fatalf("report error = %v", rep.err)
	}
}

func TestSOCKS5IPv6Connect(t *testing.T) {
	echo := newEchoServer(t)
	captured := make(chan string, 1)
	prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), captured))
	s, access := newRunningService(t, prov)

	c := dialThroughSOCKS(t, s)
	socksRequestIPv6(t, c, net.ParseIP("::1"), 8080)
	if rep := readSOCKSReply(t, c); rep != socksRepSucceeded {
		t.Fatalf("reply = 0x%02x, want success", rep)
	}
	if _, err := c.Write([]byte("v6")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if got := string(readExact(t, c, 2)); got != "v6" {
		t.Fatalf("echo = %q", got)
	}
	closeWriteClient(t, c)

	select {
	case got := <-captured:
		if got != "[::1]:8080" {
			t.Fatalf("dialed %q, want [::1]:8080", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialer was not called")
	}

	rec := waitAccess(t, access)
	if rec.State != stateOK {
		t.Fatalf("state = %q (reason %q)", rec.State, rec.Reason)
	}
	if rec.Dst != "[::1]:8080" || rec.Port != 8080 || rec.Domain != "" {
		t.Fatalf("record dst/domain/port = %q/%q/%d", rec.Dst, rec.Domain, rec.Port)
	}
}

func TestSOCKS5RejectsBindAndUDP(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  byte
	}{
		{name: "bind", cmd: socksCmdBind},
		{name: "udp associate", cmd: socksCmdUDPAssociate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := newFakeProvider("wan-test", nil)
			s, access := newRunningService(t, prov)
			c := dialThroughSOCKS(t, s)
			socksRequestRaw(t, c, tc.cmd, socksAtypIPv4, []byte{127, 0, 0, 1}, 80)
			if rep := readSOCKSReply(t, c); rep != socksRepCmdNotSupport {
				t.Fatalf("reply = 0x%02x, want 0x07", rep)
			}

			rec := waitAccess(t, access)
			if rec.State != stateDeny {
				t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateDeny, rec.Reason)
			}
			if rec.Proto != protoSOCKS5 {
				t.Fatalf("proto = %q", rec.Proto)
			}
			if prov.calls() != 0 {
				t.Fatalf("route provider called %d times for %s", prov.calls(), tc.name)
			}
			assertNoReport(t, prov.reports)
		})
	}
}

func TestSOCKS5NoAcceptableMethod(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	s, access := newRunningService(t, prov)
	c := dialProxy(t, s)

	if _, err := c.Write([]byte{socksVersion, 1, 0x02}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := readExact(t, c, 2)
	if reply[0] != socksVersion || reply[1] != socksMethodNoAccept {
		t.Fatalf("greeting reply = %v, want [5 255]", reply)
	}

	rec := waitAccess(t, access)
	if rec.State != stateDeny {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateDeny, rec.Reason)
	}
	assertNoReport(t, prov.reports)
}

func TestSOCKS5MalformedGreetingOrRequest(t *testing.T) {
	t.Run("truncated methods", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialProxy(t, s)
		if _, err := c.Write([]byte{socksVersion, 2, socksMethodNoAuth}); err != nil {
			t.Fatalf("write: %v", err)
		}
		closeWriteClient(t, c)
		rec := waitAccess(t, access)
		if rec.State != stateFail {
			t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateFail, rec.Reason)
		}
	})

	t.Run("truncated request", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialThroughSOCKS(t, s)
		if _, err := c.Write([]byte{socksVersion, socksCmdConnect}); err != nil {
			t.Fatalf("write: %v", err)
		}
		closeWriteClient(t, c)
		rec := waitAccess(t, access)
		if rec.State != stateFail {
			t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateFail, rec.Reason)
		}
	})

	t.Run("unsupported address type", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialThroughSOCKS(t, s)
		if _, err := c.Write([]byte{socksVersion, socksCmdConnect, 0x00, 0x09}); err != nil {
			t.Fatalf("write: %v", err)
		}
		if rep := readSOCKSReply(t, c); rep != socksRepAtypNotSupp {
			t.Fatalf("reply = 0x%02x, want 0x08", rep)
		}
		rec := waitAccess(t, access)
		if rec.State != stateFail {
			t.Fatalf("state = %q, want %q", rec.State, stateFail)
		}
	})
}

func TestSOCKS5ProviderError(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	prov.routeErr = errors.New("no route")
	s, access := newRunningService(t, prov)

	c := dialThroughSOCKS(t, s)
	socksRequestIPv4(t, c, net.ParseIP("127.0.0.1"), 80)
	if rep := readSOCKSReply(t, c); rep != socksRepGeneralFail {
		t.Fatalf("reply = 0x%02x, want 0x01", rep)
	}

	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q", rec.State, stateFail)
	}
	if rec.WAN != "" {
		t.Fatalf("WAN = %q, want empty", rec.WAN)
	}
	assertNoReport(t, prov.reports)
}

func TestSOCKS5DialErrorReplyAndReport(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	prov := newFakeProvider("wan-test", dialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, dialErr
	}))
	s, access := newRunningService(t, prov)

	c := dialThroughSOCKS(t, s)
	socksRequestIPv4(t, c, net.ParseIP("127.0.0.1"), 80)
	if rep := readSOCKSReply(t, c); rep != socksRepConnRefused {
		t.Fatalf("reply = 0x%02x, want 0x05", rep)
	}

	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q", rec.State, stateFail)
	}
	rep := waitReport(t, prov.reports)
	if !errors.Is(rep.err, syscall.ECONNREFUSED) {
		t.Fatalf("reported error = %v, want ECONNREFUSED", rep.err)
	}
	assertNoReport(t, prov.reports)
}

func TestSOCKSReplyForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want byte
	}{
		{name: "nil", err: nil, want: socksRepSucceeded},
		{name: "refused", err: syscall.ECONNREFUSED, want: socksRepConnRefused},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: socksRepHostUnreach},
		{name: "network unreachable", err: syscall.ENETUNREACH, want: socksRepNetUnreach},
		{name: "timed out", err: syscall.ETIMEDOUT, want: socksRepTTLExpired},
		{name: "deadline", err: context.DeadlineExceeded, want: socksRepTTLExpired},
		{name: "dns not found", err: &net.DNSError{IsNotFound: true}, want: socksRepHostUnreach},
		{name: "dns timeout", err: &net.DNSError{IsTimeout: true}, want: socksRepTTLExpired},
		{name: "general", err: errors.New("boom"), want: socksRepGeneralFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := socksReplyForError(tc.err); got != tc.want {
				t.Fatalf("reply = 0x%02x, want 0x%02x", got, tc.want)
			}
		})
	}
}
