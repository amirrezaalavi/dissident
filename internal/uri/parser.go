// Package uri parses proxy share links and subscription payloads into
// contract.Link values.
//
// Canonicalisation contract: Link.Raw is the share link exactly as supplied
// after trimming surrounding whitespace and a leading UTF-8 BOM. Link.Hash is
// the lowercase hex SHA-256 of Link.Raw, so identical links that differ only
// in surrounding whitespace or a BOM dedupe to the same hash.
//
// Link.Params holds every meaningful query/JSON field under a lowercase key,
// plus these canonical derived keys:
//
//	network       tcp | ws | grpc | xhttp (always set)
//	headertype    TCP header type, when one applies (e.g. "http", "none")
//	uuid          VLESS user id
//	id,aid,scy    VMess user id, alterId, cipher
//	password      Trojan / Shadowsocks / SOCKS5 password
//	method        Shadowsocks cipher
//	username      SOCKS5 username
//	servicename   gRPC service name (falls back to path during parsing)
//	allowinsecure "true" when any of allowinsecure/insecure is truthy
//
// Query keys are kept even when they are not interpreted, so rendering never
// has to re-parse the raw URI.
package uri

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
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

// Parse parses exactly one share link. Supported schemes are vless://,
// vmess://, trojan://, ss:// and socks5:// (socks:// is accepted as an alias).
// Everything else is rejected with a reason.
func Parse(raw string) (contract.Link, error) {
	clean := cleanLink(raw)
	if clean == "" {
		return contract.Link{}, errors.New("empty link")
	}
	scheme, err := schemeOf(clean)
	if err != nil {
		return contract.Link{}, err
	}

	var (
		link contract.Link
		perr error
	)
	switch scheme {
	case "vless":
		link, perr = parseVLESS(clean)
	case "vmess":
		link, perr = parseVMess(clean)
	case "trojan":
		link, perr = parseTrojan(clean)
	case "ss":
		link, perr = parseSS(clean)
	case "socks5", "socks":
		link, perr = parseSOCKS5(clean)
	case "hysteria2", "hy2", "tuic", "wireguard", "wg":
		return contract.Link{}, fmt.Errorf("unsupported protocol %q: only vless, vmess, trojan, ss and socks5 are supported in this demo", scheme)
	case "socks4", "socks4a":
		return contract.Link{}, fmt.Errorf("unsupported protocol %q: SOCKS4 is not supported, use socks5://", scheme)
	case "http", "https":
		return contract.Link{}, fmt.Errorf("unsupported protocol %q: only proxy share links are accepted", scheme)
	default:
		return contract.Link{}, fmt.Errorf("unsupported protocol %q: supported schemes are vless, vmess, trojan, ss and socks5", scheme)
	}
	if perr != nil {
		return contract.Link{}, perr
	}

	link.Raw = clean
	sum := sha256.Sum256([]byte(clean))
	link.Hash = hex.EncodeToString(sum[:])
	if link.Params == nil {
		link.Params = map[string]string{}
	}
	return link, nil
}

// ParseMany parses a subscription payload. It accepts a whole-payload base64
// subscription (standard or URL-safe, padded or raw, with line wrapping), a
// newline-separated list, comments starting with "#", a UTF-8 BOM and
// surrounding whitespace. Bad lines are reported in errs together with their
// 1-based line number; good lines are still returned.
func ParseMany(data []byte) ([]contract.Link, []error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, []error{errors.New("empty subscription payload")}
	}

	if looksLikeBase64Payload(text) {
		if decoded, err := decodeBase64Loose(text); err == nil && strings.Contains(string(decoded), "://") {
			text = strings.TrimPrefix(string(decoded), "\ufeff")
		}
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	links := make([]contract.Link, 0, len(lines))
	var errs []error
	for i, line := range lines {
		line = cleanLink(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		link, err := Parse(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
			continue
		}
		links = append(links, link)
	}
	if len(links) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("subscription contains no share links"))
	}
	return links, errs
}

// ---------- per-protocol parsers ----------

func parseVLESS(raw string) (contract.Link, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return contract.Link{}, fmt.Errorf("vless: invalid URL: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return contract.Link{}, errors.New("vless: missing uuid")
	}
	uuid := u.User.Username()
	if err := validateUUID(uuid, "vless"); err != nil {
		return contract.Link{}, err
	}
	server, port, err := serverPort(u, "vless")
	if err != nil {
		return contract.Link{}, err
	}
	params, err := queryParams(u)
	if err != nil {
		return contract.Link{}, fmt.Errorf("vless: invalid query: %w", err)
	}
	params["uuid"] = uuid
	params["encryption"] = firstNonEmpty(strings.TrimSpace(params["encryption"]), "none")

	rawNetwork := firstNonEmpty(params["type"], params["network"])
	network, err := normalizeNetwork(rawNetwork)
	if err != nil {
		return contract.Link{}, fmt.Errorf("vless: %w", err)
	}
	params["network"] = network

	if headerType := headerTypeFor("vless", params, rawNetwork); headerType != "" {
		params["headertype"] = headerType
	}

	security := strings.ToLower(strings.TrimSpace(params["security"]))
	if security == "" {
		security = "none"
	}
	switch security {
	case "none", "tls":
	case "reality":
		if params["pbk"] == "" {
			return contract.Link{}, errors.New("vless: reality security requires pbk (public key)")
		}
		if params["sni"] == "" {
			return contract.Link{}, errors.New("vless: reality security requires sni")
		}
		switch network {
		case "tcp", "grpc", "xhttp":
		default:
			return contract.Link{}, fmt.Errorf("vless: reality security is not supported with %s transport", network)
		}
	default:
		return contract.Link{}, fmt.Errorf("vless: unsupported security %q: supported values are none, tls and reality", security)
	}
	params["security"] = security

	if network == "grpc" {
		if svc := firstNonEmpty(params["servicename"], params["path"]); svc != "" {
			params["servicename"] = svc
		}
	}
	if network == "xhttp" {
		if params["path"] == "" {
			return contract.Link{}, errors.New("vless: xhttp transport requires path")
		}
		if params["host"] == "" {
			return contract.Link{}, errors.New("vless: xhttp transport requires host")
		}
		if params["mode"] == "" {
			params["mode"] = "auto"
		}
	}
	normalizeAllowInsecure(params)
	return contract.Link{
		Protocol: contract.ProtocolVLESS,
		Name:     u.Fragment,
		Server:   server,
		Port:     port,
		Params:   params,
	}, nil
}

func parseVMess(raw string) (contract.Link, error) {
	rest := raw[strings.Index(raw, "://")+3:]
	payload, name := splitFragment(rest)
	data, err := decodeBase64Loose(payload)
	if err != nil {
		return contract.Link{}, fmt.Errorf("vmess: invalid base64 payload: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return contract.Link{}, fmt.Errorf("vmess: malformed JSON payload: %w", err)
	}
	if len(fields) == 0 {
		return contract.Link{}, errors.New("vmess: empty JSON payload")
	}
	params := make(map[string]string, len(fields)+8)
	for k, v := range fields {
		params[strings.ToLower(k)] = jsonScalar(v)
	}
	if name == "" {
		name = params["ps"]
	}

	server := strings.TrimSpace(params["add"])
	if server == "" {
		return contract.Link{}, errors.New("vmess: missing server address (add)")
	}
	port, err := parsePort(params["port"], "vmess")
	if err != nil {
		return contract.Link{}, err
	}
	id := strings.TrimSpace(params["id"])
	if id == "" {
		return contract.Link{}, errors.New("vmess: missing user id")
	}
	if err := validateUUID(id, "vmess"); err != nil {
		return contract.Link{}, err
	}
	aid := 0
	if s := strings.TrimSpace(params["aid"]); s != "" {
		n, aerr := strconv.Atoi(s)
		if aerr != nil || n < 0 || n > 65535 {
			return contract.Link{}, fmt.Errorf("vmess: invalid alterId %q", s)
		}
		aid = n
	}
	params["aid"] = strconv.Itoa(aid)

	scy := strings.ToLower(strings.TrimSpace(params["scy"]))
	if scy == "" {
		scy = "auto"
	}
	if !vmessCiphers[scy] {
		return contract.Link{}, fmt.Errorf("vmess: unsupported cipher %q", scy)
	}
	params["scy"] = scy

	rawNetwork := firstNonEmpty(params["net"], params["network"])
	network, err := normalizeNetwork(rawNetwork)
	if err != nil {
		return contract.Link{}, fmt.Errorf("vmess: %w", err)
	}
	switch network {
	case "tcp", "ws", "grpc":
	default:
		return contract.Link{}, fmt.Errorf("vmess: unsupported transport %q: only tcp, ws and grpc are supported", network)
	}
	params["network"] = network

	// Upstream decodes the JSON "type" field (the TCP header type) but drops
	// it. Keep it: it is what makes tcp+http-header and similar links work.
	if headerType := headerTypeFor("vmess", params, rawNetwork); headerType != "" {
		params["headertype"] = headerType
	}

	tlsValue := strings.ToLower(strings.TrimSpace(params["tls"]))
	secValue := strings.ToLower(strings.TrimSpace(params["security"]))
	switch {
	case tlsValue == "tls" || tlsValue == "xtls" || secValue == "tls" || secValue == "xtls":
		params["tls"] = "tls"
	case secValue == "reality":
		return contract.Link{}, errors.New("vmess: reality security is not supported")
	default:
		params["tls"] = "none"
	}

	if network == "grpc" {
		if svc := firstNonEmpty(params["servicename"], params["path"]); svc != "" {
			params["servicename"] = svc
		}
	}
	normalizeAllowInsecure(params)
	return contract.Link{
		Protocol: contract.ProtocolVMess,
		Name:     name,
		Server:   server,
		Port:     port,
		Params:   params,
	}, nil
}

func parseTrojan(raw string) (contract.Link, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return contract.Link{}, fmt.Errorf("trojan: invalid URL: %w", err)
	}
	if u.User == nil {
		return contract.Link{}, errors.New("trojan: missing password")
	}
	password := u.User.Username()
	if pw, ok := u.User.Password(); ok && pw != "" {
		password = pw
	}
	if password == "" {
		return contract.Link{}, errors.New("trojan: missing password")
	}
	server, port, err := serverPort(u, "trojan")
	if err != nil {
		return contract.Link{}, err
	}
	params, err := queryParams(u)
	if err != nil {
		return contract.Link{}, fmt.Errorf("trojan: invalid query: %w", err)
	}
	params["password"] = password

	rawNetwork := firstNonEmpty(params["type"], params["network"])
	network, err := normalizeNetwork(rawNetwork)
	if err != nil {
		return contract.Link{}, fmt.Errorf("trojan: %w", err)
	}
	switch network {
	case "tcp", "ws", "grpc":
	default:
		return contract.Link{}, fmt.Errorf("trojan: unsupported transport %q: only tcp, ws and grpc are supported", network)
	}
	params["network"] = network
	if headerType := headerTypeFor("trojan", params, rawNetwork); headerType != "" {
		params["headertype"] = headerType
	}

	security := strings.ToLower(strings.TrimSpace(params["security"]))
	if security == "" {
		security = "tls"
	}
	switch security {
	case "tls", "none":
	default:
		return contract.Link{}, fmt.Errorf("trojan: unsupported security %q: supported values are tls and none", security)
	}
	params["security"] = security

	if network == "grpc" {
		if svc := firstNonEmpty(params["servicename"], params["path"]); svc != "" {
			params["servicename"] = svc
		}
	}
	normalizeAllowInsecure(params)
	return contract.Link{
		Protocol: contract.ProtocolTrojan,
		Name:     u.Fragment,
		Server:   server,
		Port:     port,
		Params:   params,
	}, nil
}

func parseSS(raw string) (contract.Link, error) {
	rest := raw[strings.Index(raw, "://")+3:]
	rest, name := splitFragment(rest)

	query, rest, err := splitQuery(rest, "ss")
	if err != nil {
		return contract.Link{}, err
	}
	for key, values := range query {
		switch strings.ToLower(key) {
		case "plugin":
			return contract.Link{}, fmt.Errorf("ss: plugin %q is not supported in this demo", strings.Join(values, ","))
		case "plugin-opts":
			return contract.Link{}, errors.New("ss: plugin-opts is not supported in this demo")
		}
	}

	var (
		method, password string
		hostport         string
	)
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// SIP002: ss://userinfo@host:port. userinfo is either
		// base64(method:password) (standard or URL-safe) or the plain,
		// percent-encoded pair.
		cred, cerr := decodeSSCredentials(rest[:at])
		if cerr != nil {
			return contract.Link{}, cerr
		}
		method, password, err = splitMethodPassword(cred, "ss")
		if err != nil {
			return contract.Link{}, err
		}
		hostport = rest[at+1:]
	} else {
		// Legacy: ss://base64(method:password@host:port).
		decoded, derr := decodeBase64Loose(rest)
		if derr != nil {
			return contract.Link{}, fmt.Errorf("ss: expected SIP002 userinfo@host:port or a legacy base64 payload: %w", derr)
		}
		plain := string(decoded)
		at := strings.LastIndex(plain, "@")
		if at < 0 {
			return contract.Link{}, errors.New("ss: legacy payload is missing @host:port")
		}
		method, password, err = splitMethodPassword(plain[:at], "ss")
		if err != nil {
			return contract.Link{}, err
		}
		hostport = plain[at+1:]
	}

	server, port, err := splitHostPort(hostport, "ss")
	if err != nil {
		return contract.Link{}, err
	}
	params := valuesParams(query)
	params["method"] = method
	params["password"] = password
	return contract.Link{
		Protocol: contract.ProtocolShadowsocks,
		Name:     name,
		Server:   server,
		Port:     port,
		Params:   params,
	}, nil
}

func parseSOCKS5(raw string) (contract.Link, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return contract.Link{}, fmt.Errorf("socks5: invalid URL: %w", err)
	}
	server, port, err := serverPort(u, "socks5")
	if err != nil {
		return contract.Link{}, err
	}
	params, err := queryParams(u)
	if err != nil {
		return contract.Link{}, fmt.Errorf("socks5: invalid query: %w", err)
	}
	if u.User != nil {
		username := u.User.Username()
		if username != "" {
			params["username"] = username
			if pw, ok := u.User.Password(); ok {
				params["password"] = pw
			}
		}
	}
	return contract.Link{
		Protocol: contract.ProtocolSOCKS5,
		Name:     u.Fragment,
		Server:   server,
		Port:     port,
		Params:   params,
	}, nil
}

// ---------- shared helpers ----------

func cleanLink(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "\ufeff")
	return strings.TrimSpace(s)
}

func schemeOf(s string) (string, error) {
	i := strings.Index(s, "://")
	if i <= 0 {
		return "", fmt.Errorf("not a share link: missing \"://\" in %q", truncate(s, 40))
	}
	return strings.ToLower(s[:i]), nil
}

func serverPort(u *url.URL, proto string) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("%s: missing server address", proto)
	}
	portStr := u.Port()
	if portStr == "" {
		return "", 0, fmt.Errorf("%s: missing port", proto)
	}
	port, err := parsePort(portStr, proto)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func parsePort(s, proto string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%s: missing port", proto)
	}
	port, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid port %q", proto, s)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s: port %d out of range 1-65535", proto, port)
	}
	return port, nil
}

func splitHostPort(s, proto string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, fmt.Errorf("%s: invalid server:port %q: %w", proto, s, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("%s: missing server address", proto)
	}
	port, err := parsePort(portStr, proto)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func validateUUID(s, proto string) error {
	if uuidDashed.MatchString(s) || uuidPlain.MatchString(s) {
		return nil
	}
	return fmt.Errorf("%s: invalid uuid %q (want 8-4-4-4-12 or 32 hex digits)", proto, s)
}

func normalizeNetwork(raw string) (string, error) {
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
		// Legacy spelling for tcp with an HTTP header; not an h2 transport.
		return "tcp", nil
	case "kcp", "mkcp":
		return "", errors.New("unsupported transport \"kcp\"")
	case "quic":
		return "", errors.New("unsupported transport \"quic\"")
	case "httpupgrade":
		return "", errors.New("unsupported transport \"httpupgrade\"")
	case "h2", "h2c":
		return "", errors.New("unsupported transport \"h2\" (HTTP/2)")
	default:
		return "", fmt.Errorf("unsupported transport %q: only tcp, ws, grpc and xhttp are supported", raw)
	}
}

// headerTypeFor derives the TCP header type for a link. For VLESS/Trojan the
// share-link "type" is the transport, except for the legacy type=http spelling
// which means tcp+http header; the explicit headerType query key wins. For
// VMess the JSON "type" field *is* the header type and is preserved here.
func headerTypeFor(proto string, params map[string]string, rawNetwork string) string {
	if ht := strings.ToLower(strings.TrimSpace(params["headertype"])); ht != "" {
		return ht
	}
	rawType := strings.ToLower(strings.TrimSpace(params["type"]))
	if proto == "vmess" {
		// In VMess JSON, "type" is always the TCP header type, even when it
		// collides with a transport name ("http"). It must not be dropped.
		if rawType == "http" {
			return "http"
		}
		if rawType != "" && !isNetworkWord(rawType) {
			return rawType
		}
		if rawType == "" && strings.EqualFold(rawNetwork, "http") {
			return "http"
		}
		return ""
	}
	if strings.EqualFold(rawType, "http") || strings.EqualFold(rawNetwork, "http") {
		return "http"
	}
	return ""
}

func isNetworkWord(s string) bool {
	switch strings.ToLower(s) {
	case "tcp", "ws", "websocket", "grpc", "gun", "xhttp", "http", "h2", "h2c", "kcp", "mkcp", "quic", "httpupgrade":
		return true
	}
	return false
}

func normalizeAllowInsecure(params map[string]string) {
	if isTruthy(params["allowinsecure"]) || isTruthy(params["insecure"]) {
		params["allowinsecure"] = "true"
	}
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// splitFragment splits the #fragment (name) off rest and percent-decodes it.
// PathUnescape is used rather than QueryUnescape so that a literal "+" in a
// node name is preserved.
func splitFragment(rest string) (string, string) {
	i := strings.LastIndexByte(rest, '#')
	if i < 0 {
		return rest, ""
	}
	name, err := url.PathUnescape(rest[i+1:])
	if err != nil {
		name = rest[i+1:]
	}
	return rest[:i], name
}

func splitQuery(rest, proto string) (url.Values, string, error) {
	i := strings.IndexByte(rest, '?')
	if i < 0 {
		return url.Values{}, rest, nil
	}
	q, err := url.ParseQuery(rest[i+1:])
	if err != nil {
		return nil, "", fmt.Errorf("%s: invalid query string: %w", proto, err)
	}
	return q, rest[:i], nil
}

func queryParams(u *url.URL) (map[string]string, error) {
	// ParseQuery directly so malformed parameters are an error instead of
	// being silently dropped by url.Values.Get.
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, err
	}
	return valuesParams(q), nil
}

func valuesParams(q url.Values) map[string]string {
	params := make(map[string]string, len(q)+8)
	for key, values := range q {
		if key == "" {
			continue
		}
		params[strings.ToLower(key)] = strings.Join(values, ",")
	}
	return params
}

// decodeSSCredentials resolves SIP002 userinfo to "method:password". The
// plain form wins when it already contains a colon (a base64 string never
// can); otherwise standard and URL-safe base64, padded or raw, are tried.
func decodeSSCredentials(userinfo string) (string, error) {
	candidate := userinfo
	if unescaped, err := url.PathUnescape(userinfo); err == nil {
		candidate = unescaped
	}
	if strings.Contains(candidate, ":") {
		return candidate, nil
	}
	if decoded, err := decodeBase64Loose(candidate); err == nil && strings.Contains(string(decoded), ":") {
		return string(decoded), nil
	}
	return "", fmt.Errorf("ss: userinfo %q is neither base64(method:password) nor method:password", userinfo)
}

func splitMethodPassword(cred, proto string) (string, string, error) {
	method, password, ok := strings.Cut(cred, ":")
	if !ok || method == "" {
		return "", "", fmt.Errorf("%s: userinfo is missing method:password", proto)
	}
	if password == "" {
		return "", "", fmt.Errorf("%s: missing password", proto)
	}
	return method, password, nil
}

func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty base64 payload")
	}
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(compact); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64 payload")
}

func looksLikeBase64Payload(s string) bool {
	if s == "" || strings.Contains(s, "://") {
		return false
	}
	seen := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			seen = true
		case r == '+' || r == '/' || r == '-' || r == '_' || r == '=':
			seen = true
		case r == '\n' || r == '\r' || r == ' ' || r == '\t':
		default:
			return false
		}
	}
	return seen
}

// jsonScalar flattens one JSON value into the string form stored in Params.
// Objects and arrays are kept as compact JSON so no field is lost.
func jsonScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var out string
		if json.Unmarshal(raw, &out) == nil {
			return out
		}
		return strings.Trim(s, "\"")
	}
	return s
}
