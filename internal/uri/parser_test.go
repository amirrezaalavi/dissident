package uri

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dissident/internal/contract"
)

const testUUID = "109d47e4-4efe-45f8-9f63-52af26e1a5e2"

func testRealityKey() string {
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

func mustParse(t *testing.T, raw string) contract.Link {
	t.Helper()
	link, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", raw, err)
	}
	return link
}

func checkParams(t *testing.T, link contract.Link, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got := link.Params[key]; got != value {
			t.Errorf("Params[%q] = %q, want %q (all params: %v)", key, got, value, link.Params)
		}
	}
}

func TestParseSupportedMatrix(t *testing.T) {
	key := testRealityKey()

	vmessWSJSON := "{\"v\":\"2\",\"ps\":\"VMess WS\",\"add\":\"vm.example\",\"port\":443," +
		"\"id\":\"" + testUUID + "\",\"aid\":0,\"scy\":\"auto\",\"net\":\"ws\",\"type\":\"none\"," +
		"\"host\":\"h.example\",\"path\":\"/vm\",\"tls\":\"tls\",\"sni\":\"sni.example\",\"fp\":\"chrome\"}"
	vmessTCPJSON := "{\"add\":\"vm2.example\",\"port\":\"8443\",\"id\":\"" + testUUID + "\"," +
		"\"aid\":\"0\",\"net\":\"tcp\",\"type\":\"http\",\"host\":\"front.example\",\"path\":\"/vmx\"," +
		"\"tls\":\"\"}"

	legacySS := base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:legacy-pass@legacy.example:8389"))

	cases := []struct {
		name     string
		raw      string
		protocol contract.Protocol
		server   string
		port     int
		nodeName string
		params   map[string]string
	}{
		{
			name:     "vless-reality",
			raw:      "vless://" + testUUID + "@reality.example:443?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=" + key + "&sid=ab12&spx=%2Fprobe&flow=xtls-rprx-vision&type=tcp#Reality%20Node",
			protocol: contract.ProtocolVLESS,
			server:   "reality.example",
			port:     443,
			nodeName: "Reality Node",
			params: map[string]string{
				"uuid": testUUID, "encryption": "none", "security": "reality", "network": "tcp",
				"flow": "xtls-rprx-vision", "sni": "www.example.com", "fp": "chrome", "pbk": key,
				"sid": "ab12", "spx": "/probe",
			},
		},
		{
			name:     "vless-ws-tls",
			raw:      "vless://" + testUUID + "@ws.example:8443?encryption=none&security=tls&type=ws&path=%2Fws%3Fed%3D2048&host=cdn.example.com&sni=cdn.example.com&fp=firefox&alpn=h2%2Chttp%2F1.1&allowInsecure=1#WS",
			protocol: contract.ProtocolVLESS,
			server:   "ws.example",
			port:     8443,
			nodeName: "WS",
			params: map[string]string{
				"security": "tls", "network": "ws", "path": "/ws?ed=2048", "host": "cdn.example.com",
				"alpn": "h2,http/1.1", "allowinsecure": "true", "fp": "firefox",
			},
		},
		{
			name:     "vless-tcp-http-header",
			raw:      "vless://" + testUUID + "@front.example:443?encryption=none&security=none&type=tcp&headerType=http&path=%2Fapi&host=front.example.com#TCPHTTP",
			protocol: contract.ProtocolVLESS,
			server:   "front.example",
			port:     443,
			nodeName: "TCPHTTP",
			params: map[string]string{
				"security": "none", "network": "tcp", "headertype": "http", "path": "/api",
				"host": "front.example.com",
			},
		},
		{
			name:     "vless-grpc",
			raw:      "vless://" + testUUID + "@grpc.example:443?encryption=none&security=tls&type=grpc&serviceName=mygrpc&sni=grpc.example&fp=chrome#GRPC",
			protocol: contract.ProtocolVLESS,
			server:   "grpc.example",
			port:     443,
			nodeName: "GRPC",
			params: map[string]string{
				"security": "tls", "network": "grpc", "servicename": "mygrpc", "sni": "grpc.example",
			},
		},
		{
			name:     "vless-xhttp",
			raw:      "vless://" + testUUID + "@x.example:443?encryption=none&security=tls&type=xhttp&path=%2Fx&host=x.example&sni=x.example&extra=%7B%22xPaddingBytes%22%3A%22100-1000%22%7D#XHTTP",
			protocol: contract.ProtocolVLESS,
			server:   "x.example",
			port:     443,
			nodeName: "XHTTP",
			params: map[string]string{
				"security": "tls", "network": "xhttp", "path": "/x", "host": "x.example",
				"mode": "auto", "extra": "{\"xPaddingBytes\":\"100-1000\"}",
			},
		},
		{
			name:     "vless-plain-tcp",
			raw:      "vless://" + testUUID + "@plain.example:80?encryption=none&packetEncoding=xudp#Plain",
			protocol: contract.ProtocolVLESS,
			server:   "plain.example",
			port:     80,
			nodeName: "Plain",
			params: map[string]string{
				"security": "none", "network": "tcp", "encryption": "none", "packetencoding": "xudp",
			},
		},
		{
			name:     "vmess-ws-type-none",
			raw:      vmessRaw(vmessWSJSON),
			protocol: contract.ProtocolVMess,
			server:   "vm.example",
			port:     443,
			nodeName: "VMess WS",
			params: map[string]string{
				"id": testUUID, "aid": "0", "scy": "auto", "tls": "tls", "network": "ws",
				"headertype": "none", "path": "/vm", "host": "h.example", "sni": "sni.example",
				"type": "none",
			},
		},
		{
			name:     "vmess-tcp-http-header",
			raw:      vmessRaw(vmessTCPJSON),
			protocol: contract.ProtocolVMess,
			server:   "vm2.example",
			port:     8443,
			nodeName: "",
			params: map[string]string{
				"id": testUUID, "aid": "0", "scy": "auto", "tls": "none", "network": "tcp",
				"headertype": "http", "path": "/vmx", "host": "front.example", "type": "http",
			},
		},
		{
			name:     "trojan-grpc",
			raw:      "trojan://p%40ss@trojan.example:443?security=tls&type=grpc&serviceName=svc&sni=t.example&fp=chrome&alpn=h2#T%20Grpc",
			protocol: contract.ProtocolTrojan,
			server:   "trojan.example",
			port:     443,
			nodeName: "T Grpc",
			params: map[string]string{
				"password": "p@ss", "security": "tls", "network": "grpc", "servicename": "svc",
				"sni": "t.example", "fp": "chrome", "alpn": "h2",
			},
		},
		{
			name:     "trojan-ws-default-tls",
			raw:      "trojan://secret@t.example:443?type=ws&path=%2Ft&host=t.example#TW",
			protocol: contract.ProtocolTrojan,
			server:   "t.example",
			port:     443,
			nodeName: "TW",
			params: map[string]string{
				"password": "secret", "security": "tls", "network": "ws", "path": "/t", "host": "t.example",
			},
		},
		{
			name:     "ss-sip002-base64",
			raw:      "ss://" + ssUser("aes-256-gcm", "p@ss w0rd") + "@ss.example:8388#SS%20SIP",
			protocol: contract.ProtocolShadowsocks,
			server:   "ss.example",
			port:     8388,
			nodeName: "SS SIP",
			params:   map[string]string{"method": "aes-256-gcm", "password": "p@ss w0rd"},
		},
		{
			name:     "ss-sip002-plain",
			raw:      "ss://aes-128-gcm:plain%20pass@ss2.example:8388#SS%20Plain",
			protocol: contract.ProtocolShadowsocks,
			server:   "ss2.example",
			port:     8388,
			nodeName: "SS Plain",
			params:   map[string]string{"method": "aes-128-gcm", "password": "plain pass"},
		},
		{
			name:     "ss-legacy-base64",
			raw:      "ss://" + legacySS + "#Legacy",
			protocol: contract.ProtocolShadowsocks,
			server:   "legacy.example",
			port:     8389,
			nodeName: "Legacy",
			params:   map[string]string{"method": "aes-128-gcm", "password": "legacy-pass"},
		},
		{
			name:     "ss-ipv6",
			raw:      "ss://" + ssUser("aes-128-gcm", "v6pass") + "@[2001:db8::1]:8388#v6",
			protocol: contract.ProtocolShadowsocks,
			server:   "2001:db8::1",
			port:     8388,
			nodeName: "v6",
			params:   map[string]string{"method": "aes-128-gcm", "password": "v6pass"},
		},
		{
			name:     "socks5-auth",
			raw:      "socks5://user:p%40ss@socks.example:1080#Socks",
			protocol: contract.ProtocolSOCKS5,
			server:   "socks.example",
			port:     1080,
			nodeName: "Socks",
			params:   map[string]string{"username": "user", "password": "p@ss"},
		},
		{
			name:     "socks5-no-auth",
			raw:      "socks5://1.2.3.4:1080",
			protocol: contract.ProtocolSOCKS5,
			server:   "1.2.3.4",
			port:     1080,
			params:   map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := mustParse(t, tc.raw)
			if link.Protocol != tc.protocol {
				t.Errorf("protocol = %q, want %q", link.Protocol, tc.protocol)
			}
			if link.Server != tc.server {
				t.Errorf("server = %q, want %q", link.Server, tc.server)
			}
			if link.Port != tc.port {
				t.Errorf("port = %d, want %d", link.Port, tc.port)
			}
			if tc.nodeName != "" && link.Name != tc.nodeName {
				t.Errorf("name = %q, want %q", link.Name, tc.nodeName)
			}
			if link.Raw != tc.raw {
				t.Errorf("raw = %q, want %q", link.Raw, tc.raw)
			}
			checkParams(t, link, tc.params)

			sum := sha256.Sum256([]byte(tc.raw))
			if want := hex.EncodeToString(sum[:]); link.Hash != want {
				t.Errorf("hash = %q, want sha256(raw) = %q", link.Hash, want)
			}
		})
	}
}

func TestParsePreservesUnknownParams(t *testing.T) {
	link := mustParse(t, "vless://"+testUUID+"@example.com:443?type=tcp&security=none&customKey=Value&flag=1&path=%2Fa&mode=auto#n")
	checkParams(t, link, map[string]string{
		"customkey": "Value",
		"flag":      "1",
		"path":      "/a",
		"mode":      "auto",
	})
}

func TestParseAllowInsecureAliases(t *testing.T) {
	for _, query := range []string{"allowInsecure=1", "insecure=true"} {
		link := mustParse(t, "vless://"+testUUID+"@example.com:443?encryption=none&"+query)
		if got := link.Params["allowinsecure"]; got != "true" {
			t.Errorf("%s: allowinsecure = %q, want %q", query, got, "true")
		}
	}
}

func TestParsePercentEncodedName(t *testing.T) {
	link := mustParse(t, "vless://"+testUUID+"@example.com:443?encryption=none#%E2%9C%93%20a+b")
	if link.Name != "✓ a+b" {
		t.Fatalf("name = %q, want %q", link.Name, "✓ a+b")
	}
}

func TestParseRejections(t *testing.T) {
	key := testRealityKey()
	ssCreds := ssUser("aes-128-gcm", "pass")

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"hysteria2", "hysteria2://pass@host:443#h", "unsupported protocol"},
		{"hy2", "hy2://pass@host:443#h", "unsupported protocol"},
		{"tuic", "tuic://uuid@host:443#t", "unsupported protocol"},
		{"wireguard", "wireguard://key@host:51820#w", "unsupported protocol"},
		{"socks4", "socks4://1.2.3.4:1080", "SOCKS4"},
		{"http", "http://example.com", "unsupported protocol"},
		{"unknown scheme", "ftp://example.com/file", "unsupported protocol"},
		{"no scheme", "vless-example", "missing \"://\""},
		{"empty", "   ", "empty link"},
		{"ss plugin", "ss://" + ssCreds + "@1.2.3.4:8388?plugin=obfs-local%3Bobfs%3Dhttp#p", "plugin"},
		{"ss plugin-opts", "ss://" + ssCreds + "@1.2.3.4:8388?plugin-opts=obfs=http#p", "plugin-opts"},
		{"ss bad userinfo", "ss://not-base64!@1.2.3.4:8388#p", "neither base64"},
		{"ss missing password", "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:")) + "@1.2.3.4:8388#p", "missing password"},
		{"ss bad port", "ss://" + ssCreds + "@1.2.3.4:99999#p", "out of range"},
		{"vless missing uuid", "vless://example.com:443", "missing uuid"},
		{"vless invalid uuid", "vless://not-a-uuid@example.com:443", "invalid uuid"},
		{"vless missing port", "vless://" + testUUID + "@example.com", "missing port"},
		{"vless zero port", "vless://" + testUUID + "@example.com:0", "out of range"},
		{"vless large port", "vless://" + testUUID + "@example.com:70000", "out of range"},
		{"vless kcp", "vless://" + testUUID + "@example.com:443?type=kcp", "unsupported transport"},
		{"vless quic", "vless://" + testUUID + "@example.com:443?type=quic", "unsupported transport"},
		{"vless httpupgrade", "vless://" + testUUID + "@example.com:443?type=httpupgrade", "unsupported transport"},
		{"vless h2", "vless://" + testUUID + "@example.com:443?type=h2", "unsupported transport"},
		{"vless xtls", "vless://" + testUUID + "@example.com:443?security=xtls", "unsupported security"},
		{"reality missing pbk", "vless://" + testUUID + "@example.com:443?security=reality&sni=x.example", "requires pbk"},
		{"reality missing sni", "vless://" + testUUID + "@example.com:443?security=reality&pbk=" + key, "requires sni"},
		{"reality with ws", "vless://" + testUUID + "@example.com:443?security=reality&pbk=" + key + "&sni=x.example&type=ws", "not supported with ws"},
		{"xhttp missing path", "vless://" + testUUID + "@example.com:443?type=xhttp&host=h.example", "requires path"},
		{"xhttp missing host", "vless://" + testUUID + "@example.com:443?type=xhttp&path=%2Fx", "requires host"},
		{"vmess bad base64", "vmess://!!!!", "invalid base64"},
		{"vmess malformed json", vmessRaw("this is not json"), "malformed JSON"},
		{"vmess empty json", vmessRaw("{}"), "empty JSON"},
		{"vmess missing id", vmessRaw("{\"add\":\"h.example\",\"port\":443}"), "missing user id"},
		{"vmess missing server", vmessRaw("{\"id\":\"" + testUUID + "\",\"port\":443}"), "missing server address"},
		{"vmess missing port", vmessRaw("{\"add\":\"h.example\",\"id\":\"" + testUUID + "\"}"), "missing port"},
		{"vmess bad cipher", vmessRaw("{\"add\":\"h.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"scy\":\"rc4\"}"), "unsupported cipher"},
		{"vmess xhttp", vmessRaw("{\"add\":\"h.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"net\":\"xhttp\"}"), "unsupported transport"},
		{"vmess reality", vmessRaw("{\"add\":\"h.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"security\":\"reality\"}"), "reality security is not supported"},
		{"trojan missing password", "trojan://example.com:443", "missing password"},
		{"trojan xhttp", "trojan://pass@example.com:443?type=xhttp", "unsupported transport"},
		{"trojan reality", "trojan://pass@example.com:443?security=reality", "unsupported security"},
		{"socks5 bad port", "socks5://1.2.3.4:99999", "out of range"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, err := Parse(tc.raw)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded (%+v), want error containing %q", tc.raw, link, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) error = %q, want it to contain %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestParseManyText(t *testing.T) {
	vlessRaw := "vless://" + testUUID + "@one.example:443?encryption=none&security=tls&sni=one.example#One"
	trojanRaw := "trojan://secret@two.example:443#Two"
	data := "\ufeff# subscription comment\r\n\r\n" + vlessRaw + "\r\n" + trojanRaw + "\r\nbroken line\r\n# tail\r\n"

	links, errs := ParseMany([]byte(data))
	if len(links) != 2 {
		t.Fatalf("links = %d, want 2 (%+v)", len(links), links)
	}
	if links[0].Raw != vlessRaw || links[1].Raw != trojanRaw {
		t.Fatalf("unexpected link order: %q, %q", links[0].Raw, links[1].Raw)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly 1", errs)
	}
	if !strings.Contains(errs[0].Error(), "line 5") {
		t.Fatalf("error = %q, want it to name line 5", errs[0].Error())
	}
}

func TestParseManyBase64Payload(t *testing.T) {
	vlessRaw := "vless://" + testUUID + "@one.example:443?encryption=none#One"
	trojanRaw := "trojan://secret@two.example:443#Two"
	plain := vlessRaw + "\n# comment\n" + trojanRaw + "\n"

	for _, tc := range []struct {
		name string
		data string
	}{
		{"std", base64.StdEncoding.EncodeToString([]byte(plain))},
		{"url-raw", base64.RawURLEncoding.EncodeToString([]byte(plain))},
		{
			"wrapped",
			func() string {
				encoded := base64.StdEncoding.EncodeToString([]byte(plain))
				return encoded[:16] + "\n" + encoded[16:32] + "\n" + encoded[32:]
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links, errs := ParseMany([]byte(tc.data))
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(links) != 2 {
				t.Fatalf("links = %d, want 2", len(links))
			}
			if links[0].Protocol != contract.ProtocolVLESS || links[1].Protocol != contract.ProtocolTrojan {
				t.Fatalf("unexpected protocols: %q, %q", links[0].Protocol, links[1].Protocol)
			}
		})
	}
}

func TestParseManyTestdata(t *testing.T) {
	for _, tc := range []struct {
		file string
		want int
	}{
		{"subscription.txt", 2},
		{"subscription.b64", 2},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read testdata: %v", err)
			}
			links, errs := ParseMany(data)
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(links) != tc.want {
				t.Fatalf("links = %d, want %d", len(links), tc.want)
			}
		})
	}
}

func TestParseManyEmptyAndComments(t *testing.T) {
	if _, errs := ParseMany(nil); len(errs) != 1 || !strings.Contains(errs[0].Error(), "empty subscription") {
		t.Fatalf("ParseMany(nil) errors = %v, want empty-subscription error", errs)
	}
	if links, errs := ParseMany([]byte("# only a comment\n\n")); len(links) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "no share links") {
		t.Fatalf("comment-only payload = (%v, %v), want no links and one error", links, errs)
	}
}

func TestHashStableAcrossWhitespaceAndDuplicates(t *testing.T) {
	raw := "vless://" + testUUID + "@example.com:443?encryption=none&type=tcp#Hash"
	first := mustParse(t, raw)
	second := mustParse(t, " \t"+raw+"\r\n")

	if second.Raw != first.Raw {
		t.Fatalf("trimmed raw = %q, want %q", second.Raw, first.Raw)
	}
	if second.Hash != first.Hash {
		t.Fatalf("hash changed with surrounding whitespace: %q vs %q", second.Hash, first.Hash)
	}

	links, errs := ParseMany([]byte(raw + "\n" + raw + "\n"))
	if len(errs) != 0 || len(links) != 2 {
		t.Fatalf("ParseMany = (%v, %v), want two links and no errors", links, errs)
	}
	if links[0].Hash != links[1].Hash || links[0].Hash != first.Hash {
		t.Fatalf("duplicate hashes differ: %q, %q, want %q", links[0].Hash, links[1].Hash, first.Hash)
	}
}

func TestParamsContainAllVMessJSONFields(t *testing.T) {
	payload := "{\"add\":\"h.example\",\"port\":443,\"id\":\"" + testUUID + "\",\"net\":\"ws\"," +
		"\"type\":\"none\",\"host\":\"h.example\",\"path\":\"/p\",\"tls\":\"tls\",\"sni\":\"s.example\"," +
		"\"alpn\":\"h2\",\"fp\":\"chrome\",\"extraField\":\"kept\"}"
	link := mustParse(t, vmessRaw(payload))
	rawJSON, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link.Raw, "vmess://"))
	if err != nil {
		t.Fatalf("decode raw payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("decode raw JSON: %v", err)
	}
	for key := range decoded {
		if _, ok := link.Params[strings.ToLower(key)]; !ok {
			t.Errorf("JSON field %q was dropped from Params: %v", key, link.Params)
		}
	}
	checkParams(t, link, map[string]string{"extrafield": "kept", "alpn": "h2"})
}
