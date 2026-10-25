package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// serveHTTPConnect parses one HTTP request from the sniffed reader. Only
// CONNECT is supported; the tunnel is established only after the target has
// been dialed, so a route/dial failure can still be answered with 502.
func (s *Service) serveHTTPConnect(st *connState, client net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		writeHTTPError(client, http.StatusBadRequest)
		st.finish(s.stateForError(err), "http request: "+err.Error(), nil)
		return
	}
	if req.Method != http.MethodConnect {
		writeHTTPError(client, http.StatusMethodNotAllowed)
		st.finish(stateDeny, fmt.Sprintf("http method %s not allowed", req.Method), nil)
		return
	}

	target := req.Host
	if target == "" {
		target = req.URL.Host
	}
	host, port, err := normalizeTarget(target)
	if err != nil {
		writeHTTPError(client, http.StatusBadRequest)
		st.finish(stateFail, "http target: "+err.Error(), nil)
		return
	}
	st.dst = net.JoinHostPort(host, port)
	if p, convErr := strconv.Atoi(port); convErr == nil {
		st.port = p
	}
	if net.ParseIP(host) == nil {
		st.domain = host
	}
	st.clearHandshakeDeadline()

	route, err := s.opts.Provider.Route(st.ctx)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway)
		st.finish(s.stateForError(err), "route: "+err.Error(), nil)
		return
	}
	st.setRoute(route)

	upstream, err := dialRoute(st, route)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway)
		st.finish(s.stateForError(err), "dial: "+err.Error(), err)
		return
	}
	defer upstream.Close()

	// Reply exactly, with no extra headers, as required for CONNECT.
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		st.finish(stateFail, "write CONNECT response: "+err.Error(), err)
		return
	}

	s.relay(st, client, upstream, io.MultiReader(br, client))
}

// dialRoute dials the target through the selected route.
func dialRoute(st *connState, route Route) (net.Conn, error) {
	if route.Dial == nil {
		return nil, errors.New("proxy: route has no dialer")
	}
	return route.Dial.DialContext(st.ctx, "tcp", st.dst)
}

// writeHTTPError writes a minimal, connection-closing HTTP error response.
func writeHTTPError(w io.Writer, status int) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		status, http.StatusText(status))
}

// normalizeTarget turns a CONNECT authority-form target into host and port.
// A missing port defaults to 443. Bracketed IPv6 ("[2001:db8::1]"), IPv6 with
// a port ("[2001:db8::1]:8443"), and bare IPv6 literals ("2001:db8::1") are
// accepted; bare IPv6 gets the default port because it cannot carry one.
func normalizeTarget(raw string) (host, port string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("empty target")
	}

	if h, p, splitErr := net.SplitHostPort(raw); splitErr == nil {
		if err := validateHost(h); err != nil {
			return "", "", err
		}
		pn, err := parsePort(p)
		if err != nil {
			return "", "", err
		}
		return h, strconv.Itoa(pn), nil
	}

	switch {
	case strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]"):
		h := raw[1 : len(raw)-1]
		if err := validateHost(h); err != nil {
			return "", "", err
		}
		return h, "443", nil
	case strings.Count(raw, ":") >= 2:
		// A bare IPv6 literal. Anything else with multiple colons is
		// malformed (for example "[::1]:bad").
		if net.ParseIP(raw) == nil {
			return "", "", fmt.Errorf("invalid target %q", raw)
		}
		return raw, "443", nil
	case strings.Contains(raw, ":"):
		return "", "", fmt.Errorf("invalid target %q", raw)
	default:
		if err := validateHost(raw); err != nil {
			return "", "", err
		}
		return raw, "443", nil
	}
}

func parsePort(p string) (int, error) {
	if p == "" {
		return 0, errors.New("empty port")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", p)
	}
	return n, nil
}

func validateHost(h string) error {
	if h == "" {
		return errors.New("empty host")
	}
	if strings.ContainsAny(h, " \t\r\n/\\") {
		return fmt.Errorf("invalid host %q", h)
	}
	for _, r := range h {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid host %q", h)
		}
	}
	return nil
}
