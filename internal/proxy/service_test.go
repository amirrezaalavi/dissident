package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

func TestStopWhileIdleIsIdempotentAndLeakFree(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	s, access := newTestService(t, prov)
	startTestService(t, s)

	baseline := runtime.NumGoroutine()

	const n = 8
	clients := make([]net.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatalf("dial client %d: %v", i, err)
		}
		clients = append(clients, c)
	}
	// Wait until every accepted connection owns a handler slot.
	waitFor(t, 3*time.Second, func() bool { return len(s.sem) == n })

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, errServiceStopped) {
		t.Fatalf("start after stop = %v, want errServiceStopped", err)
	}

	for i, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Errorf("client %d still readable after stop", i)
		}
		_ = c.Close()
	}

	for i := 0; i < n; i++ {
		rec := waitAccess(t, access)
		if rec.State != stateCancel {
			t.Errorf("idle handshake record state = %q, want %q (reason %q)", rec.State, stateCancel, rec.Reason)
		}
		if rec.Proto != "" {
			t.Errorf("idle record proto = %q, want empty", rec.Proto)
		}
	}
	select {
	case rec := <-access:
		t.Fatalf("extra access record: %+v", rec)
	case <-time.After(100 * time.Millisecond):
	}

	waitFor(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= baseline+2 })
}

func TestHandshakeTimeoutRecordsTimeout(t *testing.T) {
	prov := newFakeProvider("wan-test", nil)
	s, access := newTestService(t, prov)
	s.handshakeTimeout = 100 * time.Millisecond
	startTestService(t, s)

	c := dialProxy(t, s)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle client was not closed by the handshake deadline")
	}

	rec := waitAccess(t, access)
	if rec.State != stateTimeout {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateTimeout, rec.Reason)
	}
	if rec.Proto != "" || rec.Dst != "" || rec.WAN != "" {
		t.Fatalf("unexpected fields on idle handshake: %+v", rec)
	}
	if prov.calls() != 0 {
		t.Fatalf("route provider called %d times for a handshake that never arrived", prov.calls())
	}
	assertNoReport(t, prov.reports)
}

func TestFirstByteHandling(t *testing.T) {
	t.Run("binary is closed", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialProxy(t, s)
		if _, err := c.Write([]byte{0x00, 0x01, 0x02}); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("binary first byte was not closed")
		}
		rec := waitAccess(t, access)
		if rec.State != stateFail {
			t.Fatalf("state = %q, want %q", rec.State, stateFail)
		}
		if rec.Proto != "" {
			t.Fatalf("proto = %q, want empty", rec.Proto)
		}
		assertNoReport(t, prov.reports)
	})

	t.Run("http-looking gets 400", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialProxy(t, s)
		if _, err := io.WriteString(c, "GARBAGE / HTTP/1.1\r\nHost: example.test\r\n\r\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if status := readHTTPStatus(t, c); status != "HTTP/1.1 400 Bad Request" {
			t.Fatalf("status = %q, want 400", status)
		}
		rec := waitAccess(t, access)
		if rec.State != stateFail {
			t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateFail, rec.Reason)
		}
		assertNoReport(t, prov.reports)
	})

	t.Run("non-CONNECT method gets 405", func(t *testing.T) {
		prov := newFakeProvider("wan-test", nil)
		s, access := newRunningService(t, prov)
		c := dialProxy(t, s)
		if _, err := io.WriteString(c, "COPY / HTTP/1.1\r\nHost: example.test\r\n\r\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if status := readHTTPStatus(t, c); status != "HTTP/1.1 405 Method Not Allowed" {
			t.Fatalf("status = %q, want 405", status)
		}
		rec := waitAccess(t, access)
		if rec.State != stateDeny {
			t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateDeny, rec.Reason)
		}
		if rec.Proto != protoConnect {
			t.Fatalf("proto = %q, want %q", rec.Proto, protoConnect)
		}
		assertNoReport(t, prov.reports)
	})
}

func TestStartRequiresProvider(t *testing.T) {
	s := New(Options{Listen: "127.0.0.1:0"})
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start with a nil provider succeeded")
	}
	if s.Addr() != nil {
		t.Fatal("Addr is non-nil after a failed Start")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}
