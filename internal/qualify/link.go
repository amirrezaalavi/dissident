package qualify

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dissident/internal/contract"
)

var (
	uuidDashed = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
	uuidPlain  = regexp.MustCompile("^[0-9a-fA-F]{32}$")

	vmessCiphers = map[string]bool{
		"auto": true, "none": true, "zero": true,
		"aes-128-gcm": true, "chacha20-poly1305": true,
	}
)

// needsTLS reports whether the link's own transport performs a TLS handshake.
// Reality is excluded because it hides a different handshake; Shadowsocks and
// SOCKS5 have no TLS layer. Trojan defaults to TLS when security is absent.
func needsTLS(link contract.Link) bool {
	return securityOf(link) == "tls"
}

// sniOf resolves the server name used for the TLS stage.
func sniOf(link contract.Link) string {
	return firstNonEmpty(
		param(link.Params, "sni"),
		param(link.Params, "servername"),
		param(link.Params, "host"),
		link.Server,
	)
}

// validateProtocol checks static credentials only. It never opens a
// connection and never performs a protocol handshake.
func validateProtocol(link contract.Link) error {
	if strings.TrimSpace(link.Server) == "" {
		return errors.New("empty server address")
	}
	if link.Port < 1 || link.Port > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", link.Port)
	}

	switch link.Protocol {
	case contract.ProtocolVLESS:
		if err := validateUUID(param(link.Params, "uuid"), "vless"); err != nil {
			return err
		}
		network := networkOf(link)
		switch network {
		case "tcp", "ws", "grpc", "xhttp":
		default:
			return fmt.Errorf("vless: unsupported transport %q", network)
		}
		if network == "grpc" {
			if err := requireServiceName(link, "vless"); err != nil {
				return err
			}
		}
		if network == "xhttp" {
			if param(link.Params, "path") == "" {
				return errors.New("vless: xhttp transport requires path")
			}
			if param(link.Params, "host") == "" {
				return errors.New("vless: xhttp transport requires host")
			}
		}
		switch security := securityOf(link); security {
		case "none", "tls":
		case "reality":
			if err := validateReality(link); err != nil {
				return err
			}
		default:
			return fmt.Errorf("vless: unsupported security %q", security)
		}

	case contract.ProtocolVMess:
		if err := validateUUID(param(link.Params, "id"), "vmess"); err != nil {
			return err
		}
		if raw := param(link.Params, "aid"); raw != "" {
			aid, err := strconv.Atoi(raw)
			if err != nil || aid < 0 || aid > 65535 {
				return fmt.Errorf("vmess: invalid alterId %q", raw)
			}
		}
		if cipher := strings.ToLower(param(link.Params, "scy")); cipher != "" && !vmessCiphers[cipher] {
			return fmt.Errorf("vmess: unsupported cipher %q", cipher)
		}
		network := networkOf(link)
		switch network {
		case "tcp", "ws", "grpc":
		default:
			return fmt.Errorf("vmess: unsupported transport %q", network)
		}
		if network == "grpc" {
			if err := requireServiceName(link, "vmess"); err != nil {
				return err
			}
		}
		if security := securityOf(link); security == "reality" {
			return errors.New("vmess: reality security is not supported")
		}

	case contract.ProtocolTrojan:
		if param(link.Params, "password") == "" {
			return errors.New("trojan: missing password")
		}
		network := networkOf(link)
		switch network {
		case "tcp", "ws", "grpc":
		default:
			return fmt.Errorf("trojan: unsupported transport %q", network)
		}
		if network == "grpc" {
			if err := requireServiceName(link, "trojan"); err != nil {
				return err
			}
		}
		switch security := securityOf(link); security {
		case "none", "tls":
		default:
			return fmt.Errorf("trojan: unsupported security %q", security)
		}

	case contract.ProtocolShadowsocks:
		if param(link.Params, "plugin") != "" || param(link.Params, "plugin-opts") != "" {
			return errors.New("ss: plugins are not supported")
		}
		if param(link.Params, "method") == "" {
			return errors.New("ss: missing method")
		}
		if param(link.Params, "password") == "" {
			return errors.New("ss: missing password")
		}

	case contract.ProtocolSOCKS5:
		user := param(link.Params, "username")
		password := param(link.Params, "password")
		if user == "" && password != "" {
			return errors.New("socks5: password without username")
		}

	default:
		return fmt.Errorf("unsupported protocol %q", link.Protocol)
	}
	return nil
}

// requireServiceName enforces a non-empty gRPC service name.
func requireServiceName(link contract.Link, protocol string) error {
	if firstNonEmpty(param(link.Params, "servicename"), param(link.Params, "path")) == "" {
		return fmt.Errorf("%s: grpc transport requires a service name", protocol)
	}
	return nil
}

// validateReality checks the reality public key and optional short id.
func validateReality(link contract.Link) error {
	publicKey := strings.TrimSpace(param(link.Params, "pbk"))
	if publicKey == "" {
		return errors.New("vless: reality requires pbk")
	}
	if !validRealityKey(publicKey) {
		return fmt.Errorf("vless: reality pbk %q is not a 32-byte base64 key", publicKey)
	}
	if firstNonEmpty(param(link.Params, "sni"), param(link.Params, "host")) == "" {
		return errors.New("vless: reality requires sni")
	}
	if shortID := strings.TrimSpace(param(link.Params, "sid")); shortID != "" {
		if len(shortID) > 16 {
			return fmt.Errorf("vless: reality sid %q is longer than 16 hex characters", shortID)
		}
		if len(shortID)%2 != 0 {
			return fmt.Errorf("vless: reality sid %q must have an even number of hex characters", shortID)
		}
		if _, err := hex.DecodeString(shortID); err != nil {
			return fmt.Errorf("vless: reality sid %q is not hex", shortID)
		}
	}
	return nil
}

// validRealityKey accepts the base64 spellings xray accepts for a 32-byte
// reality public key.
func validRealityKey(value string) bool {
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil && len(decoded) == 32 {
			return true
		}
	}
	return false
}

func validateUUID(value, protocol string) error {
	if value == "" {
		return fmt.Errorf("%s: missing uuid", protocol)
	}
	if uuidDashed.MatchString(value) || uuidPlain.MatchString(value) {
		return nil
	}
	return fmt.Errorf("%s: invalid uuid %q (want 8-4-4-4-12 or 32 hex digits)", protocol, value)
}

// networkOf resolves the canonical transport, accepting the raw keys used by
// manually built links as well as the canonical "network" key.
func networkOf(link contract.Link) string {
	raw := param(link.Params, "network")
	if raw == "" {
		if link.Protocol == contract.ProtocolVMess {
			raw = param(link.Params, "net")
		} else {
			raw = param(link.Params, "type")
		}
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "tcp", "http":
		return "tcp"
	case "ws", "websocket":
		return "ws"
	case "grpc", "gun":
		return "grpc"
	case "xhttp":
		return "xhttp"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// securityOf mirrors xraycfg's security resolution, including Trojan's
// default of TLS.
func securityOf(link contract.Link) string {
	switch link.Protocol {
	case contract.ProtocolVLESS:
		security := strings.ToLower(strings.TrimSpace(param(link.Params, "security")))
		if security == "" {
			return "none"
		}
		return security
	case contract.ProtocolTrojan:
		security := strings.ToLower(strings.TrimSpace(param(link.Params, "security")))
		if security == "" {
			return "tls"
		}
		return security
	case contract.ProtocolVMess:
		tlsValue := strings.ToLower(strings.TrimSpace(param(link.Params, "tls")))
		security := strings.ToLower(strings.TrimSpace(param(link.Params, "security")))
		switch {
		case tlsValue == "tls" || tlsValue == "xtls" || security == "tls" || security == "xtls":
			return "tls"
		case security == "reality":
			return "reality"
		default:
			return "none"
		}
	default:
		return "none"
	}
}

// param looks a Params key up case-insensitively, deterministically.
func param(params map[string]string, key string) string {
	if len(params) == 0 {
		return ""
	}
	if value, ok := params[key]; ok {
		return value
	}
	lower := strings.ToLower(key)
	if value, ok := params[lower]; ok {
		return value
	}
	best := ""
	for candidate := range params {
		if strings.EqualFold(candidate, key) && (best == "" || candidate < best) {
			best = candidate
		}
	}
	if best != "" {
		return params[best]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
