// Package xraycfg renders contract.Link values into complete xray JSON
// configuration documents and validates them with a real xray binary.
package xraycfg

import (
	"encoding/json"
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

// Supports reports whether the link falls inside the demo's rendered matrix:
// VLESS tcp/ws/grpc/xhttp (plus reality security), VMess and Trojan
// tcp/ws/grpc, Shadowsocks without plugins, and SOCKS5.
func Supports(link contract.Link) error {
	switch link.Protocol {
	case contract.ProtocolVLESS, contract.ProtocolVMess, contract.ProtocolTrojan,
		contract.ProtocolShadowsocks, contract.ProtocolSOCKS5:
	default:
		if link.Protocol == "" {
			return errors.New("xraycfg: link has no protocol")
		}
		return fmt.Errorf("xraycfg: unsupported protocol %q: supported protocols are vless, vmess, trojan, ss and socks5", link.Protocol)
	}
	if strings.TrimSpace(link.Server) == "" {
		return fmt.Errorf("xraycfg: %s link has an empty server address", link.Protocol)
	}
	if link.Port < 1 || link.Port > 65535 {
		return fmt.Errorf("xraycfg: %s link port %d out of range 1-65535", link.Protocol, link.Port)
	}

	switch link.Protocol {
	case contract.ProtocolVLESS:
		return supportsVLESS(link)
	case contract.ProtocolVMess:
		return supportsVMess(link)
	case contract.ProtocolTrojan:
		return supportsTrojan(link)
	case contract.ProtocolShadowsocks:
		return supportsSS(link)
	default:
		return nil
	}
}

func supportsVLESS(link contract.Link) error {
	uuid := param(link.Params, "uuid")
	if uuid == "" {
		return errors.New("xraycfg: vless link is missing uuid")
	}
	if err := validateUUID(uuid); err != nil {
		return fmt.Errorf("xraycfg: vless: %w", err)
	}
	network, err := networkOf(link)
	if err != nil {
		return fmt.Errorf("xraycfg: vless: %w", err)
	}
	switch network {
	case "tcp", "ws", "grpc", "xhttp":
	default:
		return fmt.Errorf("xraycfg: vless: unsupported transport %q: only tcp, ws, grpc and xhttp are supported", network)
	}
	security, err := securityOf(link)
	if err != nil {
		return fmt.Errorf("xraycfg: vless: %w", err)
	}
	if security == "reality" {
		if param(link.Params, "pbk") == "" {
			return errors.New("xraycfg: vless: reality security requires pbk")
		}
		if param(link.Params, "sni") == "" {
			return errors.New("xraycfg: vless: reality security requires sni")
		}
		switch network {
		case "tcp", "grpc", "xhttp":
		default:
			return fmt.Errorf("xraycfg: vless: reality security is not supported with %s transport", network)
		}
	}
	if network == "xhttp" {
		if param(link.Params, "path") == "" {
			return errors.New("xraycfg: vless: xhttp transport requires path")
		}
		if param(link.Params, "host") == "" {
			return errors.New("xraycfg: vless: xhttp transport requires host")
		}
	}
	return nil
}

func supportsVMess(link contract.Link) error {
	id := param(link.Params, "id")
	if id == "" {
		return errors.New("xraycfg: vmess link is missing user id")
	}
	if err := validateUUID(id); err != nil {
		return fmt.Errorf("xraycfg: vmess: %w", err)
	}
	if s := param(link.Params, "aid"); s != "" {
		aid, err := strconv.Atoi(s)
		if err != nil || aid < 0 || aid > 65535 {
			return fmt.Errorf("xraycfg: vmess: invalid alterId %q", s)
		}
	}
	if scy := strings.ToLower(param(link.Params, "scy")); scy != "" && !vmessCiphers[scy] {
		return fmt.Errorf("xraycfg: vmess: unsupported cipher %q", scy)
	}
	network, err := networkOf(link)
	if err != nil {
		return fmt.Errorf("xraycfg: vmess: %w", err)
	}
	switch network {
	case "tcp", "ws", "grpc":
	default:
		return fmt.Errorf("xraycfg: vmess: unsupported transport %q: only tcp, ws and grpc are supported", network)
	}
	_, err = securityOf(link)
	if err != nil {
		return fmt.Errorf("xraycfg: vmess: %w", err)
	}
	return nil
}

func supportsTrojan(link contract.Link) error {
	if param(link.Params, "password") == "" {
		return errors.New("xraycfg: trojan link is missing password")
	}
	network, err := networkOf(link)
	if err != nil {
		return fmt.Errorf("xraycfg: trojan: %w", err)
	}
	switch network {
	case "tcp", "ws", "grpc":
	default:
		return fmt.Errorf("xraycfg: trojan: unsupported transport %q: only tcp, ws and grpc are supported", network)
	}
	if _, err := securityOf(link); err != nil {
		return fmt.Errorf("xraycfg: trojan: %w", err)
	}
	return nil
}

func supportsSS(link contract.Link) error {
	if param(link.Params, "plugin") != "" || param(link.Params, "plugin-opts") != "" {
		return errors.New("xraycfg: ss plugins are not supported in this demo")
	}
	if param(link.Params, "method") == "" {
		return errors.New("xraycfg: ss link is missing method")
	}
	if param(link.Params, "password") == "" {
		return errors.New("xraycfg: ss link is missing password")
	}
	return nil
}

// Render renders one complete xray config with a SOCKS inbound on
// 127.0.0.1:localPort and exactly one outbound for link. Mux is never
// emitted and there is no fallback outbound.
func Render(link contract.Link, localPort int) ([]byte, error) {
	if localPort < 1 || localPort > 65535 {
		return nil, fmt.Errorf("xraycfg: local port %d out of range 1-65535", localPort)
	}
	if err := Supports(link); err != nil {
		return nil, err
	}
	outbound, err := renderOutbound(link)
	if err != nil {
		return nil, err
	}
	cfg := Config{
		Log: LogConfig{LogLevel: "warning"},
		Inbounds: []Inbound{{
			Listen:   "127.0.0.1",
			Port:     localPort,
			Protocol: "socks",
			Settings: json.RawMessage(`{"udp":false}`),
		}},
		Outbounds: []Outbound{outbound},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("xraycfg: marshal config: %w", err)
	}
	return data, nil
}

func renderOutbound(link contract.Link) (Outbound, error) {
	out := Outbound{}
	switch link.Protocol {
	case contract.ProtocolVLESS:
		out.Protocol = "vless"
		user := map[string]any{
			"id":         param(link.Params, "uuid"),
			"encryption": firstNonEmpty(param(link.Params, "encryption"), "none"),
		}
		if flow := param(link.Params, "flow"); flow != "" {
			user["flow"] = flow
		}
		if pe := param(link.Params, "packetencoding"); pe != "" {
			user["packetEncoding"] = pe
		}
		settings, err := marshalSettings(vnextSettings(link, user))
		if err != nil {
			return Outbound{}, err
		}
		out.Settings = settings

	case contract.ProtocolVMess:
		out.Protocol = "vmess"
		aid := 0
		if s := param(link.Params, "aid"); s != "" {
			aid, _ = strconv.Atoi(s) // validated by Supports
		}
		scy := firstNonEmpty(param(link.Params, "scy"), "auto")
		user := map[string]any{
			"id":       param(link.Params, "id"),
			"alterId":  aid,
			"security": scy,
		}
		settings, err := marshalSettings(vnextSettings(link, user))
		if err != nil {
			return Outbound{}, err
		}
		out.Settings = settings

	case contract.ProtocolTrojan:
		out.Protocol = "trojan"
		server := map[string]any{
			"address":  link.Server,
			"port":     link.Port,
			"password": param(link.Params, "password"),
		}
		if flow := param(link.Params, "flow"); flow != "" {
			server["flow"] = flow
		}
		settings, err := marshalSettings(map[string]any{"servers": []any{server}})
		if err != nil {
			return Outbound{}, err
		}
		out.Settings = settings

	case contract.ProtocolShadowsocks:
		out.Protocol = "shadowsocks"
		settings, err := marshalSettings(map[string]any{
			"servers": []any{map[string]any{
				"address":  link.Server,
				"port":     link.Port,
				"method":   param(link.Params, "method"),
				"password": param(link.Params, "password"),
			}},
		})
		if err != nil {
			return Outbound{}, err
		}
		out.Settings = settings

	case contract.ProtocolSOCKS5:
		out.Protocol = "socks"
		server := map[string]any{
			"address": link.Server,
			"port":    link.Port,
		}
		if username := param(link.Params, "username"); username != "" {
			server["users"] = []any{map[string]any{
				"user": username,
				"pass": param(link.Params, "password"),
			}}
		}
		settings, err := marshalSettings(map[string]any{"servers": []any{server}})
		if err != nil {
			return Outbound{}, err
		}
		out.Settings = settings

	default:
		return Outbound{}, fmt.Errorf("xraycfg: unsupported protocol %q", link.Protocol)
	}

	switch link.Protocol {
	case contract.ProtocolVLESS, contract.ProtocolVMess, contract.ProtocolTrojan:
		stream, err := renderStream(link)
		if err != nil {
			return Outbound{}, err
		}
		out.StreamSettings = stream
	}
	return out, nil
}

func vnextSettings(link contract.Link, user map[string]any) map[string]any {
	return map[string]any{
		"vnext": []any{map[string]any{
			"address": link.Server,
			"port":    link.Port,
			"users":   []any{user},
		}},
	}
}

func renderStream(link contract.Link) (*StreamSettings, error) {
	network, err := networkOf(link)
	if err != nil {
		return nil, err
	}
	security, err := securityOf(link)
	if err != nil {
		return nil, err
	}
	params := link.Params
	stream := &StreamSettings{Network: network, Security: security}

	switch network {
	case "tcp":
		switch headerType := headerTypeOf(link); headerType {
		case "", "none":
			// Plain TCP: xray's default header, nothing to emit.
		case "http":
			request := &HTTPRequest{
				Version: "1.1",
				Method:  "GET",
				Path:    []string{firstNonEmpty(param(params, "path"), "/")},
			}
			if host := param(params, "host"); host != "" {
				request.Headers = map[string][]string{"Host": {host}}
			}
			stream.TCPSettings = &TCPSettings{Header: &TCPHeader{Type: "http", Request: request}}
		default:
			stream.TCPSettings = &TCPSettings{Header: &TCPHeader{Type: headerType}}
		}

	case "ws":
		ws := &WSSettings{Path: firstNonEmpty(param(params, "path"), "/")}
		if host := param(params, "host"); host != "" {
			ws.Headers = map[string]string{"Host": host}
		}
		stream.WSSettings = ws

	case "grpc":
		stream.GRPCSettings = &GRPCSettings{
			ServiceName: firstNonEmpty(param(params, "servicename"), param(params, "path")),
		}

	case "xhttp":
		path := param(params, "path")
		host := param(params, "host")
		if path == "" {
			return nil, errors.New("xraycfg: xhttp transport requires path")
		}
		if host == "" {
			return nil, errors.New("xraycfg: xhttp transport requires host")
		}
		xhttp := &XHTTPSettings{
			Path: path,
			Host: host,
			Mode: firstNonEmpty(param(params, "mode"), "auto"),
		}
		if extra := param(params, "extra"); extra != "" {
			trimmed := strings.TrimSpace(extra)
			if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
				return nil, fmt.Errorf("xraycfg: xhttp extra must be a JSON object, got %q", extra)
			}
			xhttp.Extra = json.RawMessage(trimmed)
		}
		stream.XHTTPSettings = xhttp

	default:
		return nil, fmt.Errorf("xraycfg: unsupported transport %q", network)
	}

	switch security {
	case "tls":
		stream.TLSSettings = &TLSSettings{
			ServerName:    firstNonEmpty(param(params, "sni"), param(params, "host")),
			Fingerprint:   param(params, "fp"),
			ALPN:          splitALPN(param(params, "alpn")),
			AllowInsecure: truthy(param(params, "allowinsecure")) || truthy(param(params, "insecure")),
		}
	case "reality":
		stream.RealitySettings = &RealitySettings{
			ServerName:  firstNonEmpty(param(params, "sni"), param(params, "host")),
			Fingerprint: param(params, "fp"),
			PublicKey:   param(params, "pbk"),
			ShortID:     param(params, "sid"),
			SpiderX:     param(params, "spx"),
		}
	}
	return stream, nil
}

// networkOf resolves the transport. Links parsed by package uri carry a
// canonical "network" key; externally built links may only have the raw
// protocol-specific key (vmess "net", vless/trojan "type").
func networkOf(link contract.Link) (string, error) {
	raw := param(link.Params, "network")
	if raw == "" {
		if link.Protocol == contract.ProtocolVMess {
			raw = param(link.Params, "net")
		} else {
			raw = param(link.Params, "type")
		}
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "tcp":
		return "tcp", nil
	case "ws", "websocket":
		return "ws", nil
	case "grpc", "gun":
		return "grpc", nil
	case "xhttp":
		return "xhttp", nil
	case "http":
		// Legacy spelling for tcp with an HTTP header, not an h2 transport.
		return "tcp", nil
	case "kcp", "mkcp":
		return "", errors.New("unsupported transport kcp")
	case "quic":
		return "", errors.New("unsupported transport quic")
	case "httpupgrade":
		return "", errors.New("unsupported transport httpupgrade")
	case "h2", "h2c":
		return "", errors.New("unsupported transport h2 (HTTP/2)")
	default:
		return "", fmt.Errorf("unsupported transport %q: only tcp, ws, grpc and xhttp are supported", raw)
	}
}

func securityOf(link contract.Link) (string, error) {
	params := link.Params
	switch link.Protocol {
	case contract.ProtocolVLESS:
		security := strings.ToLower(param(params, "security"))
		if security == "" {
			security = "none"
		}
		switch security {
		case "none", "tls", "reality":
			return security, nil
		default:
			return "", fmt.Errorf("unsupported security %q: supported values are none, tls and reality", security)
		}
	case contract.ProtocolTrojan:
		security := strings.ToLower(param(params, "security"))
		if security == "" {
			security = "tls"
		}
		switch security {
		case "none", "tls":
			return security, nil
		default:
			return "", fmt.Errorf("unsupported security %q: Trojan supports tls and none", security)
		}
	case contract.ProtocolVMess:
		tlsValue := strings.ToLower(param(params, "tls"))
		secValue := strings.ToLower(param(params, "security"))
		switch {
		case tlsValue == "tls" || tlsValue == "xtls" || secValue == "tls" || secValue == "xtls":
			return "tls", nil
		case secValue == "reality":
			return "", errors.New("reality security is not supported for vmess")
		default:
			return "none", nil
		}
	default:
		return "none", nil
	}
}

// headerTypeOf resolves the TCP header type. The canonical key is
// "headertype"; param() also matches "headerType" case-insensitively. For
// VMess the raw "type" field is the header type (upstream decoded it but
// dropped it), so it is the documented fallback.
func headerTypeOf(link contract.Link) string {
	if ht := strings.ToLower(strings.TrimSpace(param(link.Params, "headertype"))); ht != "" {
		return ht
	}
	rawType := strings.ToLower(strings.TrimSpace(param(link.Params, "type")))
	rawNetwork := strings.ToLower(strings.TrimSpace(firstNonEmpty(param(link.Params, "network"), param(link.Params, "net"))))
	switch link.Protocol {
	case contract.ProtocolVMess:
		// In VMess JSON, "type" is always the TCP header type, even when it
		// collides with a transport name ("http").
		if rawType == "http" {
			return "http"
		}
		if rawType != "" && !isNetworkWord(rawType) {
			return rawType
		}
		if rawType == "" && rawNetwork == "http" {
			return "http"
		}
	default:
		if rawType == "http" || rawNetwork == "http" {
			return "http"
		}
	}
	return ""
}

// param looks a Params key up case-insensitively: exact, then lowercase, then
// a deterministic scan. The parser only writes lowercase keys, but accepting
// any spelling keeps manually built contract.Link values usable.
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
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			return params[k]
		}
	}
	return ""
}

// sortStrings is a tiny insertion sort to keep this package free of an extra
// import for one call site.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func splitALPN(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isNetworkWord(value string) bool {
	switch value {
	case "tcp", "ws", "websocket", "grpc", "gun", "xhttp", "http", "h2", "h2c", "kcp", "mkcp", "quic", "httpupgrade":
		return true
	}
	return false
}

func validateUUID(value string) error {
	if uuidDashed.MatchString(value) || uuidPlain.MatchString(value) {
		return nil
	}
	return fmt.Errorf("invalid uuid %q (want 8-4-4-4-12 or 32 hex digits)", value)
}

func marshalSettings(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("xraycfg: marshal outbound settings: %w", err)
	}
	return data, nil
}
