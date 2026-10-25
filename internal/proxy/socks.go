package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
)

// SOCKS5 protocol constants (RFC 1928).
const (
	socksVersion          = 0x05
	socksMethodNoAuth     = 0x00
	socksMethodNoAccept   = 0xFF
	socksCmdConnect       = 0x01
	socksCmdBind          = 0x02
	socksCmdUDPAssociate  = 0x03
	socksAtypIPv4         = 0x01
	socksAtypDomain       = 0x03
	socksAtypIPv6         = 0x04
	socksRepSucceeded     = 0x00
	socksRepGeneralFail   = 0x01
	socksRepHostUnreach   = 0x04
	socksRepNetUnreach    = 0x03
	socksRepConnRefused   = 0x05
	socksRepTTLExpired    = 0x06
	socksRepCmdNotSupport = 0x07
	socksRepAtypNotSupp   = 0x08
)

var (
	errSOCKSAddrType = errors.New("socks5: address type not supported")
	errSOCKSTarget   = errors.New("socks5: invalid destination target")
)

// socksSuccessReply is the fixed BND.ADDR/BND.PORT form required by the spec:
// 0.0.0.0:0.
var socksSuccessReply = []byte{
	socksVersion, socksRepSucceeded, 0x00, socksAtypIPv4,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// serveSOCKS5 performs the no-auth greeting, accepts only CONNECT, dials the
// target through the selected route, and relays.
func (s *Service) serveSOCKS5(st *connState, client net.Conn, br *bufio.Reader) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(br, header); err != nil {
		st.finish(s.stateForError(err), "socks5 greeting: "+err.Error(), nil)
		return
	}
	if header[0] != socksVersion {
		st.finish(stateFail, fmt.Sprintf("socks5 version 0x%02x", header[0]), nil)
		return
	}
	methods := make([]byte, int(header[1]))
	if len(methods) > 0 {
		if _, err := io.ReadFull(br, methods); err != nil {
			st.finish(s.stateForError(err), "socks5 methods: "+err.Error(), nil)
			return
		}
	}
	if !containsByte(methods, socksMethodNoAuth) {
		_, _ = client.Write([]byte{socksVersion, socksMethodNoAccept})
		st.finish(stateDeny, "socks5: no acceptable authentication method", nil)
		return
	}
	if _, err := client.Write([]byte{socksVersion, socksMethodNoAuth}); err != nil {
		st.finish(stateFail, "socks5 method reply: "+err.Error(), nil)
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		st.finish(s.stateForError(err), "socks5 request: "+err.Error(), nil)
		return
	}
	if req[0] != socksVersion {
		st.finish(stateFail, fmt.Sprintf("socks5 request version 0x%02x", req[0]), nil)
		return
	}
	if req[1] != socksCmdConnect {
		writeSocksReply(client, socksRepCmdNotSupport)
		st.finish(stateDeny, "socks5: "+socksCommandName(req[1])+" is not supported", nil)
		return
	}

	host, port, err := readSOCKSTarget(br, req[3])
	if err != nil {
		if !isTimeoutError(err) {
			rep := byte(socksRepGeneralFail)
			if errors.Is(err, errSOCKSAddrType) {
				rep = socksRepAtypNotSupp
			}
			writeSocksReply(client, rep)
		}
		st.finish(s.stateForError(err), "socks5 target: "+err.Error(), nil)
		return
	}
	st.dst = net.JoinHostPort(host, strconv.Itoa(port))
	st.port = port
	if net.ParseIP(host) == nil {
		st.domain = host
	}
	st.clearHandshakeDeadline()

	route, err := s.opts.Provider.Route(st.ctx)
	if err != nil {
		writeSocksReply(client, socksRepGeneralFail)
		st.finish(s.stateForError(err), "route: "+err.Error(), nil)
		return
	}
	st.setRoute(route)

	upstream, err := dialRoute(st, route)
	if err != nil {
		writeSocksReply(client, socksReplyForError(err))
		st.finish(s.stateForError(err), "dial: "+err.Error(), err)
		return
	}
	defer upstream.Close()

	if _, err := client.Write(socksSuccessReply); err != nil {
		st.finish(stateFail, "socks5 success reply: "+err.Error(), err)
		return
	}

	s.relay(st, client, upstream, io.MultiReader(br, client))
}

// readSOCKSTarget reads DST.ADDR and DST.PORT for the request ATYP.
func readSOCKSTarget(br *bufio.Reader, atyp byte) (string, int, error) {
	var host string
	switch atyp {
	case socksAtypIPv4:
		buf := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case socksAtypIPv6:
		buf := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case socksAtypDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(br, lenBuf); err != nil {
			return "", 0, err
		}
		if lenBuf[0] == 0 {
			return "", 0, fmt.Errorf("%w: empty domain", errSOCKSTarget)
		}
		buf := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", 0, err
		}
		host = string(buf)
	default:
		return "", 0, fmt.Errorf("%w: 0x%02x", errSOCKSAddrType, atyp)
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(br, portBuf); err != nil {
		return "", 0, err
	}
	port := int(portBuf[0])<<8 | int(portBuf[1])
	if port == 0 {
		return "", 0, fmt.Errorf("%w: port 0", errSOCKSTarget)
	}
	return host, port, nil
}

// writeSocksReply writes a 10-byte reply with the fixed 0.0.0.0:0 bind address
// (the spec uses that form for both success and failure).
func writeSocksReply(conn net.Conn, rep byte) {
	_, _ = conn.Write([]byte{
		socksVersion, rep, 0x00, socksAtypIPv4,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	})
}

// socksReplyForError maps a target dial error to the closest SOCKS5 reply
// code.
func socksReplyForError(err error) byte {
	switch {
	case err == nil:
		return socksRepSucceeded
	case errors.Is(err, syscall.ECONNREFUSED):
		return socksRepConnRefused
	case errors.Is(err, syscall.EHOSTUNREACH):
		return socksRepHostUnreach
	case errors.Is(err, syscall.ENETUNREACH):
		return socksRepNetUnreach
	case errors.Is(err, syscall.ETIMEDOUT):
		return socksRepTTLExpired
	case errors.Is(err, context.DeadlineExceeded):
		return socksRepTTLExpired
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return socksRepTTLExpired
		}
		return socksRepHostUnreach
	}
	if isTimeoutError(err) {
		return socksRepTTLExpired
	}
	return socksRepGeneralFail
}

func socksCommandName(cmd byte) string {
	switch cmd {
	case socksCmdBind:
		return "BIND (0x02)"
	case socksCmdUDPAssociate:
		return "UDP ASSOCIATE (0x03)"
	default:
		return fmt.Sprintf("command 0x%02x", cmd)
	}
}

func containsByte(haystack []byte, needle byte) bool {
	for _, b := range haystack {
		if b == needle {
			return true
		}
	}
	return false
}
