package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// relayBufferSize bounds each copy direction's buffer.
const relayBufferSize = 32 * 1024

// relay splices client and upstream until both directions finish, a fatal
// error occurs, or the idle bound expires. One direction reaching EOF
// half-closes the peer's write side, so the reverse direction can still
// deliver its final bytes.
//
// The idle bound is shared by both directions and reset on every byte, so a
// long but active transfer is never cut; only genuine inactivity (default 5
// minutes) ends the tunnel.
func (s *Service) relay(st *connState, client, upstream net.Conn, clientSrc io.Reader) {
	_ = client.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})

	idle := s.idleTimeout
	var activity atomic.Int64
	activity.Store(time.Now().UnixNano())

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	var errOnce sync.Once
	var firstErr error
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
		closeBoth()
	}

	var up, down int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, err := copyStream(upstream, clientSrc, client, idle, &activity)
		up = n
		if err != nil {
			fail(err)
			return
		}
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		n, err := copyStream(client, upstream, upstream, idle, &activity)
		down = n
		if err != nil {
			fail(err)
			return
		}
		closeWrite(client)
	}()
	wg.Wait()
	closeBoth()

	st.up = up
	st.down = down
	if firstErr == nil {
		st.finish(stateOK, "", nil)
		return
	}
	st.finish(s.stateForError(firstErr), firstErr.Error(), firstErr)
}

// copyStream copies src into dst, reading from readConn so the shared idle
// deadline can be armed on the socket. It returns the number of bytes written
// (not read) and the first fatal error, or nil on EOF.
func copyStream(dst net.Conn, src io.Reader, readConn net.Conn, idle time.Duration, activity *atomic.Int64) (int64, error) {
	buf := make([]byte, relayBufferSize)
	var count int64
	for {
		if idle > 0 {
			_ = readConn.SetReadDeadline(time.Unix(0, activity.Load()).Add(idle))
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			activity.Store(time.Now().UnixNano())
			if idle > 0 {
				_ = dst.SetWriteDeadline(time.Unix(0, activity.Load()).Add(idle))
			}
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				count += int64(nw)
				activity.Store(time.Now().UnixNano())
			}
			if werr != nil {
				if isTimeoutError(werr) {
					return count, errRelayIdle
				}
				return count, werr
			}
			if nw != nr {
				return count, io.ErrShortWrite
			}
		}
		switch {
		case rerr == nil:
			continue
		case errors.Is(rerr, io.EOF):
			return count, nil
		case isTimeoutError(rerr):
			if idle > 0 && time.Now().Before(time.Unix(0, activity.Load()).Add(idle)) {
				continue // activity happened while we were blocked; re-arm
			}
			return count, errRelayIdle
		default:
			return count, rerr
		}
	}
}

// closeWrite half-closes c's write side when supported, leaving reads intact
// so the peer's final bytes still arrive.
func closeWrite(c net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// isTimeoutError reports whether err is a network or context deadline.
func isTimeoutError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
