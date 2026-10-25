package platform

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// defaultDialTimeout applies when SOCKS5Dialer.Timeout or NewEgressDialer's
// timeout is not positive.
const defaultDialTimeout = 10 * time.Second

// SOCKS5Dialer dials TCP targets through a SOCKS5 proxy using RFC 1928
// no-authentication. IPv4, IPv6, and domain-name targets are supported. It is
// safe for concurrent use.
type SOCKS5Dialer struct {
	Addr    string
	Timeout time.Duration
}

// DialContext connects to Addr, performs the SOCKS5 no-auth handshake, and
// returns the tunnel. Any handshake failure closes the connection. The
// context deadline, when earlier, overrides Timeout.
func (d *SOCKS5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || strings.TrimSpace(d.Addr) == "" {
		return nil, errors.New("platform: socks5: empty proxy address")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkTCPNetwork(network); err != nil {
		return nil, err
	}
	host, port, err := splitTarget(address)
	if err != nil {
		return nil, err
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", d.Addr)
	if err != nil {
		return nil, fmt.Errorf("platform: socks5: dial proxy %s: %w", d.Addr, err)
	}
	clearDeadline := armContext(ctx, conn, timeout)
	if err := socks5Handshake(conn, host, port); err != nil {
		clearDeadline()
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("platform: socks5: %s: %w", d.Addr, ctxErr)
		}
		return nil, fmt.Errorf("platform: socks5: %s: %w", d.Addr, err)
	}
	clearDeadline()
	return conn, nil
}

// checkTCPNetwork rejects network values SOCKS5 cannot carry.
func checkTCPNetwork(network string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil
	case "":
		return errors.New("platform: empty network")
	default:
		return fmt.Errorf("platform: network %q is not supported through a SOCKS5 proxy", network)
	}
}

// splitTarget validates and splits a host:port target.
func splitTarget(address string) (string, int, error) {
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("platform: invalid target %q: %w", address, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("platform: invalid target %q: empty host", address)
	}
	port, err := strconv.Atoi(portString)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("platform: invalid target %q: port must be 1-65535", address)
	}
	return host, port, nil
}

// armContext sets the connection deadline from the earlier of Timeout and the
// context deadline and arranges for context cancellation to unblock it. The
// returned function clears those effects.
func armContext(ctx context.Context, conn net.Conn, timeout time.Duration) func() {
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}
}

// socks5Handshake performs the no-auth greeting and CONNECT request.
func socks5Handshake(conn net.Conn, host string, port int) error {
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if greeting[0] != 0x05 {
		return fmt.Errorf("unexpected SOCKS version %d", greeting[0])
	}
	if greeting[1] != 0x00 {
		if greeting[1] == 0xff {
			return errors.New("proxy rejected all authentication methods")
		}
		return fmt.Errorf("proxy selected unsupported authentication method %d", greeting[1])
	}

	request := []byte{0x05, 0x01, 0x00}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return fmt.Errorf("domain target %q is longer than 255 bytes", host)
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	case ip.To4() != nil:
		request = append(request, 0x01)
		request = append(request, ip.To4()...)
	default:
		request = append(request, 0x04)
		request = append(request, ip.To16()...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("write connect request: %w", err)
	}

	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fmt.Errorf("read connect reply: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("unexpected SOCKS version %d in reply", reply[0])
	}
	if err := discardSocks5Address(conn, reply[3]); err != nil {
		return err
	}
	if reply[1] != 0x00 {
		return socks5ReplyError(reply[1])
	}
	return nil
}

// discardSocks5Address consumes the BND.ADDR/BND.PORT portion of a reply.
func discardSocks5Address(conn net.Conn, atyp byte) error {
	var size int64
	switch atyp {
	case 0x01:
		size = 4 + 2
	case 0x04:
		size = 16 + 2
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return fmt.Errorf("read bound domain length: %w", err)
		}
		size = int64(length[0]) + 2
	default:
		return fmt.Errorf("unsupported address type %d in reply", atyp)
	}
	if _, err := io.CopyN(io.Discard, conn, size); err != nil {
		return fmt.Errorf("read bound address: %w", err)
	}
	return nil
}

// socks5ReplyError translates a SOCKS5 REP code into a stable reason.
func socks5ReplyError(code byte) error {
	switch code {
	case 0x01:
		return errors.New("general SOCKS server failure")
	case 0x02:
		return errors.New("connection not allowed by ruleset")
	case 0x03:
		return errors.New("network unreachable")
	case 0x04:
		return errors.New("host unreachable")
	case 0x05:
		return errors.New("connection refused")
	case 0x06:
		return errors.New("TTL expired")
	case 0x07:
		return errors.New("command not supported")
	case 0x08:
		return errors.New("address type not supported")
	default:
		return fmt.Errorf("SOCKS5 connect failed with reply code %d", code)
	}
}
