package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayIdleTimeout(t *testing.T) {
	echo := newEchoServer(t)
	prov := newFakeProvider("wan-test", echoDialer(echo.Addr(), nil))
	s, access := newTestService(t, prov)
	s.idleTimeout = 150 * time.Millisecond
	startTestService(t, s)

	c := dialProxy(t, s)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	writeConnect(t, c, echo.Addr())
	if got := string(readExact(t, c, len(connectOK))); got != connectOK {
		t.Fatalf("CONNECT reply = %q", got)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if got := string(readExact(t, c, 5)); got != "hello" {
		t.Fatalf("echo = %q", got)
	}

	rec := waitAccess(t, access)
	if rec.State != stateTimeout {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateTimeout, rec.Reason)
	}
	if rec.Up != 5 || rec.Down != 5 {
		t.Fatalf("Up/Down = %d/%d, want 5/5", rec.Up, rec.Down)
	}

	rep := waitReport(t, prov.reports)
	if rep.err == nil {
		t.Fatal("idle-timeout relay reported a nil error")
	}
	if !errors.Is(rep.err, errRelayIdle) {
		t.Fatalf("reported error = %v, want errRelayIdle", rep.err)
	}
	assertNoReport(t, prov.reports)
}

func TestRelayHalfCloseDeliversLateResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen one-shot server: %v", err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.ReadAll(conn); err != nil {
			return
		}
		_, _ = conn.Write([]byte("late-response"))
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-serverDone
	})

	prov := newFakeProvider("wan-test", dialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}))
	s, access := newRunningService(t, prov)

	c := dialProxy(t, s)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	writeConnect(t, c, ln.Addr().String())
	if got := string(readExact(t, c, len(connectOK))); got != connectOK {
		t.Fatalf("CONNECT reply = %q", got)
	}
	payload := []byte("request-body")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// Half-close the client's write side: the server only responds after it
	// sees EOF, so receiving the response proves CloseWrite semantics.
	closeWriteClient(t, c)
	response := readExact(t, c, len("late-response"))
	if string(response) != "late-response" {
		t.Fatalf("response = %q", response)
	}

	rec := waitAccess(t, access)
	if rec.State != stateOK {
		t.Fatalf("state = %q, want %q (reason %q)", rec.State, stateOK, rec.Reason)
	}
	if rec.Up != int64(len(payload)) {
		t.Errorf("Up = %d, want %d", rec.Up, len(payload))
	}
	if rec.Down != int64(len("late-response")) {
		t.Errorf("Down = %d, want %d", rec.Down, len("late-response"))
	}
	if rep := waitReport(t, prov.reports); rep.err != nil {
		t.Fatalf("report error = %v", rep.err)
	}
}
