package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"dissident/internal/contract"
)

// maxFirstByte is the read buffer used for sniffing. It is deliberately modest:
// the request parser (net/http.ReadRequest or the SOCKS5 reader) reads through
// the same buffered reader, so bytes it over-reads past the handshake remain
// available to the relay.
const firstByteBufferSize = 4096

// acceptLoop accepts connections until shutdown, applying backpressure through
// a bounded semaphore instead of spawning unbounded goroutines.
func (s *Service) acceptLoop() {
	defer s.wg.Done()

	var backoff time.Duration
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.isStopped() || errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept errors must not kill the listener.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
				if backoff > time.Second {
					backoff = time.Second
				}
			}
			s.logger.Warn("proxy: accept failed", "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-s.ctx.Done():
				return
			}
			continue
		}
		backoff = 0

		select {
		case s.sem <- struct{}{}:
		case <-s.ctx.Done():
			_ = conn.Close()
			return
		}
		if !s.track(conn) {
			_ = conn.Close()
			<-s.sem
			return
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// serveConn handles exactly one client connection and guarantees an access
// record even for panics. The listener is never affected by a bad connection.
func (s *Service) serveConn(client net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.sem }()

	st := &connState{
		svc:    s,
		ctx:    s.ctx,
		client: client,
		start:  time.Now(),
	}
	if addr := client.RemoteAddr(); addr != nil {
		st.src = addr.String()
	}

	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("proxy: connection panic",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			st.finish(stateFail, fmt.Sprintf("panic: %v", r), fmt.Errorf("panic: %v", r))
		}
		st.finalize()
		_ = client.Close()
		s.untrack(client)
	}()

	s.serve(st, client)
}

// serve sniffs the first byte and dispatches to the matching front-end.
func (s *Service) serve(st *connState, client net.Conn) {
	if err := client.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		st.finish(s.stateForError(err), "set handshake deadline: "+err.Error(), err)
		return
	}

	br := bufio.NewReaderSize(client, firstByteBufferSize)
	first, err := br.Peek(1)
	if err != nil {
		st.finish(s.stateForError(err), "read first byte: "+err.Error(), nil)
		return
	}

	switch first[0] {
	case socksVersion:
		st.proto = protoSOCKS5
		s.serveSOCKS5(st, client, br)
	case 'C':
		st.proto = protoConnect
		s.serveHTTPConnect(st, client, br)
	default:
		if isHTTPInitial(first[0]) {
			writeHTTPError(client, 400)
			st.finish(stateFail, fmt.Sprintf("unsupported HTTP request start byte %q", first[0]), nil)
			return
		}
		st.finish(stateFail, fmt.Sprintf("unsupported first byte 0x%02x", first[0]), nil)
	}
}

// isHTTPInitial reports whether b can start an HTTP request line. Only these
// bytes get a short 400; arbitrary binary input is closed without a reply.
func isHTTPInitial(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

// connState accumulates the outcome of one connection. It is owned by that
// connection's handler goroutine; finish/finalize are once-guarded so a panic
// defer cannot overwrite an outcome or emit a second record.
type connState struct {
	svc    *Service
	ctx    context.Context
	client net.Conn

	start time.Time
	src   string

	proto  string
	dst    string
	domain string
	port   int

	route     Route
	haveRoute bool

	up   int64
	down int64

	state      string
	reason     string
	outcomeErr error

	finishOnce   sync.Once
	finalizeOnce sync.Once
}

// setRoute records the selected route so the outcome is reported exactly once
// even on later failures.
func (st *connState) setRoute(route Route) {
	st.route = route
	st.haveRoute = true
}

// finish records the first final outcome.
func (st *connState) finish(state, reason string, err error) {
	if state == "" {
		state = stateFail
	}
	st.finishOnce.Do(func() {
		st.state = state
		st.reason = reason
		st.outcomeErr = err
	})
}

// finalize reports the route outcome (when a route was obtained) and emits the
// single access record for this connection.
func (st *connState) finalize() {
	st.finalizeOnce.Do(func() {
		if st.state == "" {
			st.finish(stateFail, "connection ended without an outcome", nil)
		}
		if st.haveRoute {
			st.report()
		}
		if st.svc.opts.Access != nil {
			rec := contract.AccessRecord{
				Time:     st.start,
				Src:      st.src,
				State:    st.state,
				Dst:      st.dst,
				Domain:   st.domain,
				Port:     st.port,
				Proto:    st.proto,
				WAN:      st.route.ID,
				Up:       st.up,
				Down:     st.down,
				Duration: time.Since(st.start),
				Reason:   st.reason,
			}
			st.svc.deliverAccess(rec)
		}
	})
}

// report calls Provider.Report exactly once for a connection that obtained a
// route. The detached, bounded context means an outcome is still recorded
// while the service is shutting down.
func (st *connState) report() {
	provider := st.svc.opts.Provider
	if provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(st.ctx), reportTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			st.svc.logger.Error("proxy: route report panic",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	provider.Report(ctx, st.route, st.outcomeErr)
}

// deliverAccess invokes the access callback defensively: a misbehaving
// callback must not take down the process.
func (s *Service) deliverAccess(rec contract.AccessRecord) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("proxy: access callback panic",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	s.opts.Access(rec)
}

// stateForError maps a request/dial/relay failure to the access state.
func (s *Service) stateForError(err error) string {
	if s.ctx != nil && s.ctx.Err() != nil {
		return stateCancel
	}
	if errors.Is(err, context.Canceled) {
		return stateCancel
	}
	if errors.Is(err, errRelayIdle) || isTimeoutError(err) {
		return stateTimeout
	}
	return stateFail
}

// clearHandshakeDeadline drops the client read/write deadline before dialing
// and relaying. Route selection and dialing may legitimately take longer than
// the handshake bound.
func (st *connState) clearHandshakeDeadline() {
	_ = st.client.SetDeadline(time.Time{})
}
