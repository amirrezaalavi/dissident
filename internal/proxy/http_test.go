package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHTTPConnectSuccessAndByteAccounting(t *testing.T) {
	echo := newEchoServer(t)
	captured := make(chan string, 1)
	prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), captured))
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	writeConnect(t, c, echo.Addr())

	if got := string(readExact(t, c, len(connectOK))); got != connectOK {
		t.Fatalf("CONNECT reply = %q, want %q", got, connectOK)
	}

	payload := bytes.Repeat([]byte("dissident-"), 700) // 6300 bytes
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if got := readExact(t, c, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("echoed payload differs (%d bytes)", len(got))
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
	if rec.State != stateOK {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateOK, rec.Reason)
	}
	if rec.Proto != protoConnect {
		t.Errorf("proto = %q, want %q", rec.Proto, protoConnect)
	}
	if rec.WAN != "wan-test" {
		t.Errorf("WAN = %q, want wan-test", rec.WAN)
	}
	if rec.Dst != echo.Addr() {
		t.Errorf("Dst = %q, want %q", rec.Dst, echo.Addr())
	}
	if rec.Domain != "" {
		t.Errorf("Domain = %q, want empty for an IP target", rec.Domain)
	}
	if rec.Port != targetPort(t, echo.Addr()) {
		t.Errorf("Port = %d, want %d", rec.Port, targetPort(t, echo.Addr()))
	}
	if rec.Up != int64(len(payload)) {
		t.Errorf("Up = %d, want %d", rec.Up, len(payload))
	}
	if rec.Down != int64(len(payload)) {
		t.Errorf("Down = %d, want %d", rec.Down, len(payload))
	}
	if rec.Src == "" {
		t.Error("Src is empty")
	}
	if rec.Time.IsZero() {
		t.Error("Time is zero")
	}
	if rec.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", rec.Duration)
	}
	if rec.Reason != "" {
		t.Errorf("Reason = %q, want empty", rec.Reason)
	}

	rep := waitReport(t, prov.reports)
	if rep.err != nil {
		t.Fatalf("report error = %v, want nil", rep.err)
	}
	if rep.route.ID != "wan-test" {
		t.Fatalf("reported route = %q, want wan-test", rep.route.ID)
	}
	assertNoReport(t, prov.reports)
}

func TestHTTPConnectTargetForms(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		wantDial   string
		wantDst    string
		wantDomain string
		wantPort   int
	}{
		{
			name:       "default port",
			target:     "example.test",
			wantDial:   "example.test:443",
			wantDst:    "example.test:443",
			wantDomain: "example.test",
			wantPort:   443,
		},
		{
			name:       "ipv6 bracket with port",
			target:     "[::1]:8443",
			wantDial:   "[::1]:8443",
			wantDst:    "[::1]:8443",
			wantDomain: "",
			wantPort:   8443,
		},
		{
			name:       "ipv6 bracket without port",
			target:     "[2001:db8::1]",
			wantDial:   "[2001:db8::1]:443",
			wantDst:    "[2001:db8::1]:443",
			wantDomain: "",
			wantPort:   443,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			echo := newEchoServer(t)
			captured := make(chan string, 1)
			prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), captured))
			s, access := newRunningService(t, prov)

			c := dialProxy(t, s)
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			writeConnect(t, c, tc.target)
			if got := string(readExact(t, c, len(connectOK))); got != connectOK {
				t.Fatalf("CONNECT reply = %q", got)
			}
			if _, err := c.Write([]byte("ping")); err != nil {
				t.Fatalf("write payload: %v", err)
			}
			if got := string(readExact(t, c, 4)); got != "ping" {
				t.Fatalf("echo = %q", got)
			}
			closeWriteClient(t, c)

			select {
			case got := <-captured:
				if got != tc.wantDial {
					t.Fatalf("dialed %q, want %q", got, tc.wantDial)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("dialer was not called")
			}

			rec := waitAccess(t, access)
			if rec.State != stateOK {
				t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateOK, rec.Reason)
			}
			if rec.Dst != tc.wantDst || rec.Domain != tc.wantDomain || rec.Port != tc.wantPort {
				t.Fatalf("record dst/domain/port = %q/%q/%d, want %q/%q/%d",
					rec.Dst, rec.Domain, rec.Port, tc.wantDst, tc.wantDomain, tc.wantPort)
			}
			rep := waitReport(t, prov.reports)
			if rep.err != nil {
				t.Fatalf("report error = %v, want nil", rep.err)
			}
		})
	}
}

func TestHTTPMalformedRequestGets400(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	if _, err := io.WriteString(c, "CONNECT\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if status := readHTTPStatus(t, c); status != "HTTP/1.1 400 Bad Request" {
		t.Fatalf("status = %q, want 400", status)
	}

	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateFail, rec.Reason)
	}
	if rec.Proto != protoConnect {
		t.Fatalf("proto = %q, want %q", rec.Proto, protoConnect)
	}
	if prov.calls() != 0 {
		t.Fatalf("route provider called %d times for a malformed request", prov.calls())
	}
	assertNoReport(t, prov.reports)
}

func TestHTTPProviderErrorGets502(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	prov.routeErr = errors.New("no active WAN")
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	writeConnect(t, c, "example.test:443")
	if status := readHTTPStatus(t, c); status != "HTTP/1.1 502 Bad Gateway" {
		t.Fatalf("status = %q, want 502", status)
	}

	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q", rec.State, stateFail)
	}
	if !strings.Contains(rec.Reason, "no active WAN") {
		t.Fatalf("reason = %q, want provider error", rec.Reason)
	}
	if rec.WAN != "" {
		t.Fatalf("WAN = %q, want empty without a route", rec.WAN)
	}
	if prov.calls() != 1 {
		t.Fatalf("route provider called %d times, want 1", prov.calls())
	}
	assertNoReport(t, prov.reports)
}

func TestHTTPDialErrorGets502AndReports(t *testing.T) {
	dialErr := errors.New("upstream exploded")
	prov := newFakeProvider("wan-test", dialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, dialErr
	}))
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	writeConnect(t, c, "example.test:443")
	if status := readHTTPStatus(t, c); status != "HTTP/1.1 502 Bad Gateway" {
		t.Fatalf("status = %q, want 502", status)
	}

	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q", rec.State, stateFail)
	}
	if rec.Up != 0 || rec.Down != 0 {
		t.Fatalf("Up/Down = %d/%d, want 0/0", rec.Up, rec.Down)
	}

	rep := waitReport(t, prov.reports)
	if !errors.Is(rep.err, dialErr) {
		t.Fatalf("reported error = %v, want %v", rep.err, dialErr)
	}
	if rep.route.ID != "wan-test" {
		t.Fatalf("reported route = %q", rep.route.ID)
	}
	assertNoReport(t, prov.reports)
}

func TestHTTPInvalidTargetGets400(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	writeConnect(t, c, "example.test:notaport")
	if status := readHTTPStatus(t, c); status != "HTTP/1.1 400 Bad Request" {
		t.Fatalf("status = %q, want 400", status)
	}
	rec := waitAccess(t, access)
	if rec.State != stateFail {
		t.Fatalf("state = %q, want %q", rec.State, stateFail)
	}
	if prov.calls() != 0 {
		t.Fatalf("route provider called %d times for an invalid target", prov.calls())
	}
	assertNoReport(t, prov.reports)
}
