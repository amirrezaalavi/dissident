package xraycfg

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"dissident/internal/contract"
	"dissident/internal/uri"
)

const testUUID = "109d47e4-4efe-45f8-9f63-52af26e1a5e2"

func realityKey() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func vmessRaw(payload string) string {
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
}

func ssUser(method, password string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
}

func mustLink(t *testing.T, raw string) contract.Link {
	t.Helper()
	link, err := uri.Parse(raw)
	if err != nil {
		t.Fatalf("uri.Parse(%q) error: %v", raw, err)
	}
	return link
}

func renderDoc(t *testing.T, link contract.Link, port int) map[string]any {
	t.Helper()
	data, err := Render(link, port)
	if err != nil {
		t.Fatalf("Render(%s) error: %v", link.Protocol, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("rendered JSON is invalid: %v\n%s", err, data)
	}
	return doc
}

func pathValue(t *testing.T, doc any, path ...any) any {
	t.Helper()
	current := doc
	for i, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("path %v: step %d: have %T, want object", path, i, current)
			}
			value, ok := object[key]
			if !ok {
				t.Fatalf("path %v: missing key %q (keys: %v)", path, key, mapKeys(object))
			}
			current = value
		case int:
			array, ok := current.([]any)
			if !ok {
				t.Fatalf("path %v: step %d: have %T, want array", path, i, current)
			}
			if key < 0 || key >= len(array) {
				t.Fatalf("path %v: index %d out of range (len %d)", path, key, len(array))
			}
			current = array[key]
		default:
			t.Fatalf("path %v: unsupported step type %T", path, step)
		}
	}
	return current
}

func mapKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

func wantString(t *testing.T, doc any, want string, path ...any) {
	t.Helper()
	got, ok := pathValue(t, doc, path...).(string)
	if !ok || got != want {
		t.Fatalf("path %v = %v, want string %q", path, pathValue(t, doc, path...), want)
	}
}

func wantNumber(t *testing.T, doc any, want float64, path ...any) {
	t.Helper()
	got, ok := pathValue(t, doc, path...).(float64)
	if !ok || got != want {
		t.Fatalf("path %v = %v, want number %v", path, pathValue(t, doc, path...), want)
	}
}

func wantBool(t *testing.T, doc any, want bool, path ...any) {
	t.Helper()
	got, ok := pathValue(t, doc, path...).(bool)
	if !ok || got != want {
		t.Fatalf("path %v = %v, want bool %v", path, pathValue(t, doc, path...), want)
	}
}

func wantMissing(t *testing.T, doc any, key string, path ...any) {
	t.Helper()
	object, ok := pathValue(t, doc, path...).(map[string]any)
	if !ok {
		t.Fatalf("path %v: have %T, want object", path, pathValue(t, doc, path...))
	}
	if value, exists := object[key]; exists {
		t.Fatalf("path %v: key %q must be absent, got %v", path, key, value)
	}
}

func outboundOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	outbounds, ok := doc["outbounds"].([]any)
	if !ok {
		t.Fatalf("outbounds = %v, want array", doc["outbounds"])
	}
	if len(outbounds) != 1 {
		t.Fatalf("outbounds = %d, want exactly 1", len(outbounds))
	}
	outbound, ok := outbounds[0].(map[string]any)
	if !ok {
		t.Fatalf("outbound = %T, want object", outbounds[0])
	}
	if _, exists := outbound["mux"]; exists {
		t.Fatalf("mux must be omitted, got %v", outbound["mux"])
	}
	protocol, _ := outbound["protocol"].(string)
	if protocol == "freedom" || protocol == "direct" {
		t.Fatalf("outbound protocol %q would leak traffic directly", protocol)
	}
	return outbound
}

func TestRenderStructure(t *testing.T) {
	link := mustLink(t, "vless://"+testUUID+"@one.example:443?encryption=none&security=tls&sni=one.example#One")
	doc := renderDoc(t, link, 10800)

	wantString(t, doc, "warning", "log", "loglevel")

	inbounds, ok := doc["inbounds"].([]any)
	if !ok || len(inbounds) != 1 {
		t.Fatalf("inbounds = %v, want exactly one", doc["inbounds"])
	}
	wantString(t, doc, "127.0.0.1", "inbounds", 0, "listen")
	wantNumber(t, doc, 10800, "inbounds", 0, "port")
	wantString(t, doc, "socks", "inbounds", 0, "protocol")
	wantBool(t, doc, false, "inbounds", 0, "settings", "udp")

	outbound := outboundOf(t, doc)
	if protocol, _ := outbound["protocol"].(string); protocol != "vless" {
		t.Fatalf("outbound protocol = %q, want vless", protocol)
	}
}

func TestRenderVLESSTLSWS(t *testing.T) {
	raw := "vless://" + testUUID + "@ws.example:8443?encryption=none&security=tls&type=ws" +
		"&path=%2Fws&host=cdn.example.com&sni=sni.example.com&fp=firefox&alpn=h2%2Chttp%2F1.1&allowInsecure=1&packetEncoding=xudp#WS"
	doc := renderDoc(t, mustLink(t, raw), 10801)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "vless", "protocol")
	wantString(t, outbound, "ws.example", "settings", "vnext", 0, "address")
	wantNumber(t, outbound, 8443, "settings", "vnext", 0, "port")
	wantString(t, outbound, testUUID, "settings", "vnext", 0, "users", 0, "id")
	wantString(t, outbound, "none", "settings", "vnext", 0, "users", 0, "encryption")
	wantString(t, outbound, "xudp", "settings", "vnext", 0, "users", 0, "packetEncoding")
	wantMissing(t, outbound, "flow", "settings", "vnext", 0, "users", 0)

	wantString(t, outbound, "ws", "streamSettings", "network")
	wantString(t, outbound, "tls", "streamSettings", "security")
	wantString(t, outbound, "/ws", "streamSettings", "wsSettings", "path")
	wantString(t, outbound, "cdn.example.com", "streamSettings", "wsSettings", "headers", "Host")
	wantString(t, outbound, "sni.example.com", "streamSettings", "tlsSettings", "serverName")
	wantString(t, outbound, "firefox", "streamSettings", "tlsSettings", "fingerprint")
	wantBool(t, outbound, true, "streamSettings", "tlsSettings", "allowInsecure")
	wantString(t, outbound, "h2", "streamSettings", "tlsSettings", "alpn", 0)
	wantString(t, outbound, "http/1.1", "streamSettings", "tlsSettings", "alpn", 1)
	wantMissing(t, outbound, "realitySettings", "streamSettings")
}

func TestRenderVLESSFlowAndNoMux(t *testing.T) {
	raw := "vless://" + testUUID + "@flow.example:443?encryption=none&security=tls&type=tcp" +
		"&sni=flow.example&flow=xtls-rprx-vision#Flow"
	doc := renderDoc(t, mustLink(t, raw), 10802)
	outbound := outboundOf(t, doc) // also asserts mux is absent
	wantString(t, outbound, "xtls-rprx-vision", "settings", "vnext", 0, "users", 0, "flow")
	wantString(t, outbound, "tcp", "streamSettings", "network")
	wantMissing(t, outbound, "tcpSettings", "streamSettings")
}

func TestRenderVLESSTCPHTTPHeader(t *testing.T) {
	raw := "vless://" + testUUID + "@front.example:443?encryption=none&security=none&type=tcp" +
		"&headerType=http&path=%2Fapi&host=front.example.com#TCPHTTP"
	doc := renderDoc(t, mustLink(t, raw), 10803)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "tcp", "streamSettings", "network")
	wantString(t, outbound, "none", "streamSettings", "security")
	wantString(t, outbound, "http", "streamSettings", "tcpSettings", "header", "type")
	wantString(t, outbound, "1.1", "streamSettings", "tcpSettings", "header", "request", "version")
	wantString(t, outbound, "GET", "streamSettings", "tcpSettings", "header", "request", "method")
	wantString(t, outbound, "/api", "streamSettings", "tcpSettings", "header", "request", "path", 0)
	wantString(t, outbound, "front.example.com", "streamSettings", "tcpSettings", "header", "request", "headers", "Host", 0)
	wantMissing(t, outbound, "tlsSettings", "streamSettings")
}

func TestRenderVLESSTCPHTTPHeaderDefaultsPath(t *testing.T) {
	raw := "vless://" + testUUID + "@front.example:443?encryption=none&security=none&type=tcp" +
		"&headerType=http&host=front.example.com#DefaultPath"
	doc := renderDoc(t, mustLink(t, raw), 10804)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "/", "streamSettings", "tcpSettings", "header", "request", "path", 0)
	wantString(t, outbound, "front.example.com", "streamSettings", "tcpSettings", "header", "request", "headers", "Host", 0)
}

func TestRenderVLESSReality(t *testing.T) {
	key := realityKey()
	raw := "vless://" + testUUID + "@reality.example:443?encryption=none&security=reality" +
		"&sni=www.example.com&fp=chrome&pbk=" + key + "&sid=ab12&spx=%2Fprobe&flow=xtls-rprx-vision&type=tcp#Reality"
	doc := renderDoc(t, mustLink(t, raw), 10805)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "reality", "streamSettings", "security")
	wantString(t, outbound, "www.example.com", "streamSettings", "realitySettings", "serverName")
	wantString(t, outbound, "chrome", "streamSettings", "realitySettings", "fingerprint")
	wantString(t, outbound, key, "streamSettings", "realitySettings", "publicKey")
	wantString(t, outbound, "ab12", "streamSettings", "realitySettings", "shortId")
	wantString(t, outbound, "/probe", "streamSettings", "realitySettings", "spiderX")
	wantMissing(t, outbound, "tlsSettings", "streamSettings")
	wantString(t, outbound, "xtls-rprx-vision", "settings", "vnext", 0, "users", 0, "flow")
}

func TestRenderVMessWS(t *testing.T) {
	payload := "{\"v\":\"2\",\"ps\":\"VMess WS\",\"add\":\"vm.example\",\"port\":443," +
		"\"id\":\"" + testUUID + "\",\"aid\":\"0\",\"scy\":\"aes-128-gcm\",\"net\":\"ws\"," +
		"\"type\":\"none\",\"host\":\"h.example\",\"path\":\"/vm\",\"tls\":\"tls\",\"sni\":\"sni.example\",\"fp\":\"chrome\"}"
	doc := renderDoc(t, mustLink(t, vmessRaw(payload)), 10806)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "vmess", "protocol")
	wantString(t, outbound, "vm.example", "settings", "vnext", 0, "address")
	wantString(t, outbound, testUUID, "settings", "vnext", 0, "users", 0, "id")
	wantNumber(t, outbound, 0, "settings", "vnext", 0, "users", 0, "alterId")
	wantString(t, outbound, "aes-128-gcm", "settings", "vnext", 0, "users", 0, "security")
	wantString(t, outbound, "ws", "streamSettings", "network")
	wantString(t, outbound, "tls", "streamSettings", "security")
	wantString(t, outbound, "/vm", "streamSettings", "wsSettings", "path")
	wantString(t, outbound, "h.example", "streamSettings", "wsSettings", "headers", "Host")
	wantString(t, outbound, "sni.example", "streamSettings", "tlsSettings", "serverName")
	wantMissing(t, outbound, "tcpSettings", "streamSettings")
}

func TestRenderVMessTCPHeaderTypeFromParamsType(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVMess,
		Server:   "vm.example",
		Port:     443,
		Params: map[string]string{
			"id": testUUID, "aid": "2", "net": "tcp", "type": "http",
			"path": "/x", "host": "h.example", "tls": "none",
		},
	}
	if err := Supports(link); err != nil {
		t.Fatalf("Supports: %v", err)
	}
	doc := renderDoc(t, link, 10807)
	outbound := outboundOf(t, doc)

	wantNumber(t, outbound, 2, "settings", "vnext", 0, "users", 0, "alterId")
	wantString(t, outbound, "auto", "settings", "vnext", 0, "users", 0, "security")
	wantString(t, outbound, "tcp", "streamSettings", "network")
	wantString(t, outbound, "http", "streamSettings", "tcpSettings", "header", "type")
	wantString(t, outbound, "/x", "streamSettings", "tcpSettings", "header", "request", "path", 0)
	wantString(t, outbound, "h.example", "streamSettings", "tcpSettings", "header", "request", "headers", "Host", 0)
}

func TestRenderVMessHeaderTypeCaseInsensitive(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVMess,
		Server:   "vm.example",
		Port:     443,
		Params: map[string]string{
			"id": testUUID, "net": "tcp", "type": "none", "headerType": "http",
			"path": "/y", "host": "h.example",
		},
	}
	doc := renderDoc(t, link, 10808)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "http", "streamSettings", "tcpSettings", "header", "type")
	wantString(t, outbound, "/y", "streamSettings", "tcpSettings", "header", "request", "path", 0)
}

func TestRenderVMessPlainTCPHasNoTCPSettings(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVMess,
		Server:   "vm.example",
		Port:     443,
		Params:   map[string]string{"id": testUUID, "net": "tcp", "type": "none"},
	}
	doc := renderDoc(t, link, 10809)
	outbound := outboundOf(t, doc)
	wantMissing(t, outbound, "tcpSettings", "streamSettings")
}

func TestRenderTrojanGRPC(t *testing.T) {
	raw := "trojan://p%40ss@trojan.example:443?security=tls&type=grpc&serviceName=svc&sni=t.example#T"
	doc := renderDoc(t, mustLink(t, raw), 10810)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "trojan", "protocol")
	wantString(t, outbound, "p@ss", "settings", "servers", 0, "password")
	wantString(t, outbound, "trojan.example", "settings", "servers", 0, "address")
	wantNumber(t, outbound, 443, "settings", "servers", 0, "port")
	wantMissing(t, outbound, "flow", "settings", "servers", 0)
	wantString(t, outbound, "grpc", "streamSettings", "network")
	wantString(t, outbound, "svc", "streamSettings", "grpcSettings", "serviceName")
	wantString(t, outbound, "t.example", "streamSettings", "tlsSettings", "serverName")
	wantMissing(t, outbound, "wsSettings", "streamSettings")
}

func TestRenderTrojanGRPCServiceNameFromPath(t *testing.T) {
	raw := "trojan://secret@trojan.example:443?type=grpc&path=fallback-svc&sni=t.example#T"
	doc := renderDoc(t, mustLink(t, raw), 10811)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "fallback-svc", "streamSettings", "grpcSettings", "serviceName")
}

func TestRenderTrojanFlow(t *testing.T) {
	raw := "trojan://secret@trojan.example:443?type=tcp&flow=xtls-rprx-vision&sni=t.example#T"
	doc := renderDoc(t, mustLink(t, raw), 10812)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "xtls-rprx-vision", "settings", "servers", 0, "flow")
	wantString(t, outbound, "tls", "streamSettings", "security")
}

func TestRenderShadowsocks(t *testing.T) {
	raw := "ss://" + ssUser("chacha20-ietf-poly1305", "p@ss w0rd") + "@ss.example:8388#SS"
	doc := renderDoc(t, mustLink(t, raw), 10813)
	outbound := outboundOf(t, doc)

	wantString(t, outbound, "shadowsocks", "protocol")
	wantString(t, outbound, "ss.example", "settings", "servers", 0, "address")
	wantNumber(t, outbound, 8388, "settings", "servers", 0, "port")
	wantString(t, outbound, "chacha20-ietf-poly1305", "settings", "servers", 0, "method")
	wantString(t, outbound, "p@ss w0rd", "settings", "servers", 0, "password")
	wantMissing(t, outbound, "streamSettings")
}

func TestRenderSOCKS5(t *testing.T) {
	doc := renderDoc(t, mustLink(t, "socks5://user:p%40ss@socks.example:1080#S"), 10814)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "socks", "protocol")
	wantString(t, outbound, "socks.example", "settings", "servers", 0, "address")
	wantNumber(t, outbound, 1080, "settings", "servers", 0, "port")
	wantString(t, outbound, "user", "settings", "servers", 0, "users", 0, "user")
	wantString(t, outbound, "p@ss", "settings", "servers", 0, "users", 0, "pass")
	wantMissing(t, outbound, "streamSettings")

	noAuth := renderDoc(t, mustLink(t, "socks5://1.2.3.4:1080"), 10815)
	wantMissing(t, outboundOf(t, noAuth), "users", "settings", "servers", 0)
}

func TestRenderXHTTP(t *testing.T) {
	raw := "vless://" + testUUID + "@x.example:443?encryption=none&security=tls&type=xhttp" +
		"&path=%2Fx&host=x.example&sni=x.example&mode=packet-up&extra=%7B%22xPaddingBytes%22%3A%22100-1000%22%7D#X"
	doc := renderDoc(t, mustLink(t, raw), 10816)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "xhttp", "streamSettings", "network")
	wantString(t, outbound, "/x", "streamSettings", "xhttpSettings", "path")
	wantString(t, outbound, "x.example", "streamSettings", "xhttpSettings", "host")
	wantString(t, outbound, "packet-up", "streamSettings", "xhttpSettings", "mode")
	wantString(t, outbound, "100-1000", "streamSettings", "xhttpSettings", "extra", "xPaddingBytes")
	wantString(t, outbound, "x.example", "streamSettings", "tlsSettings", "serverName")
}

func TestRenderXHTTPDefaultMode(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVLESS,
		Server:   "x.example",
		Port:     443,
		Params: map[string]string{
			"uuid": testUUID, "network": "xhttp", "security": "none",
			"path": "/x", "host": "x.example",
		},
	}
	doc := renderDoc(t, link, 10817)
	outbound := outboundOf(t, doc)
	wantString(t, outbound, "auto", "streamSettings", "xhttpSettings", "mode")
}

func TestRenderXHTTPExtraMustBeObject(t *testing.T) {
	link := contract.Link{
		Protocol: contract.ProtocolVLESS,
		Server:   "x.example",
		Port:     443,
		Params: map[string]string{
			"uuid": testUUID, "network": "xhttp", "security": "none",
			"path": "/x", "host": "x.example", "extra": "not-json",
		},
	}
	if _, err := Render(link, 10818); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("Render error = %v, want an xhttp extra error", err)
	}
}

func TestRenderRejectsInvalidLocalPort(t *testing.T) {
	link := mustLink(t, "socks5://1.2.3.4:1080")
	for _, port := range []int{0, -1, 65536, 70000} {
		if _, err := Render(link, port); err == nil {
			t.Errorf("Render(port=%d) succeeded, want error", port)
		}
	}
}

func TestRenderRejectsUnsupportedLink(t *testing.T) {
	cases := []struct {
		name string
		link contract.Link
		want string
	}{
		{
			name: "protocol",
			link: contract.Link{Protocol: "hysteria2", Server: "h.example", Port: 443},
			want: "unsupported protocol",
		},
		{
			name: "xhttp missing host",
			link: contract.Link{Protocol: contract.ProtocolVLESS, Server: "x.example", Port: 443,
				Params: map[string]string{"uuid": testUUID, "network": "xhttp", "path": "/x"}},
			want: "requires host",
		},
		{
			name: "ss plugin",
			link: contract.Link{Protocol: contract.ProtocolShadowsocks, Server: "ss.example", Port: 8388,
				Params: map[string]string{"method": "aes-128-gcm", "password": "p", "plugin": "v2ray-plugin"}},
			want: "plugins",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render(tc.link, 10819); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Render error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func supportedRawCases(t *testing.T) []struct{ name, raw string } {
	t.Helper()
	key := realityKey()
	vmessWS := "{\"add\":\"vm.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"net\":\"ws\"," +
		"\"type\":\"none\",\"host\":\"h.example\",\"path\":\"/vm\",\"tls\":\"tls\",\"sni\":\"sni.example\"}"
	vmessTCP := "{\"add\":\"vm.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"net\":\"tcp\"," +
		"\"type\":\"http\",\"host\":\"h.example\",\"path\":\"/vm\",\"tls\":\"\"}"
	vmessGRPC := "{\"add\":\"vm.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"net\":\"grpc\"," +
		"\"path\":\"vmsvc\",\"tls\":\"tls\",\"sni\":\"sni.example\"}"

	return []struct{ name, raw string }{
		{"vless-tcp-tls", "vless://" + testUUID + "@v.example:443?encryption=none&security=tls&type=tcp&sni=v.example&fp=chrome#v"},
		{"vless-ws-tls", "vless://" + testUUID + "@v.example:443?encryption=none&security=tls&type=ws&path=%2Fws&host=h.example&sni=v.example#v"},
		{"vless-grpc-tls", "vless://" + testUUID + "@v.example:443?encryption=none&security=tls&type=grpc&serviceName=svc&sni=v.example#v"},
		{"vless-tcp-http", "vless://" + testUUID + "@v.example:443?encryption=none&security=none&type=tcp&headerType=http&path=%2Fapi&host=h.example#v"},
		{"vless-xhttp", "vless://" + testUUID + "@v.example:443?encryption=none&security=tls&type=xhttp&path=%2Fx&host=x.example&sni=x.example#v"},
		{"vless-reality", "vless://" + testUUID + "@v.example:443?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=" + key + "&sid=ab12&spx=%2F&flow=xtls-rprx-vision&type=tcp#v"},
		{"vmess-ws", vmessRaw(vmessWS)},
		{"vmess-tcp-http", vmessRaw(vmessTCP)},
		{"vmess-grpc", vmessRaw(vmessGRPC)},
		{"trojan-tcp-tls", "trojan://secret@t.example:443?security=tls&sni=t.example#t"},
		{"trojan-ws", "trojan://secret@t.example:443?type=ws&path=%2Ft&host=t.example&sni=t.example#t"},
		{"trojan-grpc", "trojan://secret@t.example:443?type=grpc&serviceName=svc&sni=t.example#t"},
		{"ss", "ss://" + ssUser("aes-256-gcm", "ss-pass") + "@ss.example:8388#ss"},
		{"socks5", "socks5://user:pass@1.2.3.4:1080#s"},
	}
}

func TestSupportsMatrix(t *testing.T) {
	for _, tc := range supportedRawCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			link := mustLink(t, tc.raw)
			if err := Supports(link); err != nil {
				t.Fatalf("Supports(%s) error: %v", tc.name, err)
			}
			if _, err := Render(link, 10820); err != nil {
				t.Fatalf("Render(%s) error: %v", tc.name, err)
			}
		})
	}
}

func TestSupportsRejections(t *testing.T) {
	key := realityKey()

	cases := []struct {
		name string
		link contract.Link
		want string
	}{
		{"empty protocol", contract.Link{Server: "example.com", Port: 443}, "no protocol"},
		{"unsupported protocol", contract.Link{Protocol: "tuic", Server: "example.com", Port: 443}, "unsupported protocol"},
		{"empty server", contract.Link{Protocol: contract.ProtocolSOCKS5, Server: "", Port: 1080}, "empty server"},
		{"zero port", contract.Link{Protocol: contract.ProtocolSOCKS5, Server: "h", Port: 0}, "out of range"},
		{"large port", contract.Link{Protocol: contract.ProtocolSOCKS5, Server: "h", Port: 70000}, "out of range"},
		{"vless missing uuid", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443}, "missing uuid"},
		{"vless bad uuid", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": "nope"}}, "invalid uuid"},
		{"vless xtls", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "security": "xtls"}}, "unsupported security"},
		{"vless kcp", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "network": "kcp"}}, "unsupported transport"},
		{"reality missing pbk", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "security": "reality", "sni": "x"}}, "requires pbk"},
		{"reality missing sni", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "security": "reality", "pbk": key}}, "requires sni"},
		{"reality ws", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "security": "reality", "pbk": key, "sni": "x", "network": "ws"}}, "not supported with ws"},
		{"xhttp missing path", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "network": "xhttp", "host": "h"}}, "requires path"},
		{"xhttp missing host", contract.Link{Protocol: contract.ProtocolVLESS, Server: "h", Port: 443,
			Params: map[string]string{"uuid": testUUID, "network": "xhttp", "path": "/x"}}, "requires host"},
		{"vmess missing id", contract.Link{Protocol: contract.ProtocolVMess, Server: "h", Port: 443}, "missing user id"},
		{"vmess bad aid", contract.Link{Protocol: contract.ProtocolVMess, Server: "h", Port: 443,
			Params: map[string]string{"id": testUUID, "aid": "-1"}}, "invalid alterId"},
		{"vmess xhttp", contract.Link{Protocol: contract.ProtocolVMess, Server: "h", Port: 443,
			Params: map[string]string{"id": testUUID, "network": "xhttp"}}, "unsupported transport"},
		{"vmess reality", contract.Link{Protocol: contract.ProtocolVMess, Server: "h", Port: 443,
			Params: map[string]string{"id": testUUID, "security": "reality"}}, "reality security"},
		{"trojan missing password", contract.Link{Protocol: contract.ProtocolTrojan, Server: "h", Port: 443}, "missing password"},
		{"trojan xhttp", contract.Link{Protocol: contract.ProtocolTrojan, Server: "h", Port: 443,
			Params: map[string]string{"password": "p", "network": "xhttp"}}, "unsupported transport"},
		{"ss missing method", contract.Link{Protocol: contract.ProtocolShadowsocks, Server: "h", Port: 8388,
			Params: map[string]string{"password": "p"}}, "missing method"},
		{"ss missing password", contract.Link{Protocol: contract.ProtocolShadowsocks, Server: "h", Port: 8388,
			Params: map[string]string{"method": "aes-128-gcm"}}, "missing password"},
		{"ss plugin", contract.Link{Protocol: contract.ProtocolShadowsocks, Server: "h", Port: 8388,
			Params: map[string]string{"method": "aes-128-gcm", "password": "p", "plugin": "obfs-local"}}, "plugins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Supports(tc.link)
			if err == nil {
				t.Fatalf("Supports(%s) = nil, want error containing %q", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Supports(%s) error = %q, want it to contain %q", tc.name, err, tc.want)
			}
		})
	}

}
