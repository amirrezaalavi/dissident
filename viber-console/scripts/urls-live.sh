#!/usr/bin/env bash
# Live check: subscription URL replace + effective-config values.
set -u
export PATH="$HOME/.local/go/bin:$PATH"
cd "$(dirname "$0")/.."

go build -o /tmp/viber-console-ui ./cmd/console

# Mock viberayd WITH mutable urls (POST/PUT/DELETE support)
python3 - <<'EOF' &
import http.server, json
URLS = ["https://example.com/sub"]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global URLS
        if self.path.startswith("/api/urls"):
            body = json.dumps(URLS).encode()
        elif self.path.startswith("/api/stats"):
            body = json.dumps({"total":120,"working":8,"failed":100,"unreachable":12,"updated_at":"2026-08-13T09:00:00Z"}).encode()
        else:
            body = b'{}'
        self.send_response(200); self.send_header("Content-Type","application/json"); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        global URLS
        n = int(self.headers.get("Content-Length", 0))
        u = json.loads(self.rfile.read(n)).get("url")
        URLS.append(u)
        body = json.dumps({"status":"added"}).encode()
        self.send_response(201); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_DELETE(self):
        global URLS
        i = int(self.path.rsplit("/",1)[1]) - 1
        URLS.pop(i)
        body = json.dumps({"status":"removed"}).encode()
        self.send_response(200); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 18081), H).serve_forever()
EOF
VD_PID=$!

python3 - <<'EOF' &
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"ok" if self.path in ("/healthz","/readyz") else b"# TYPE viberoxy_wans_active gauge\nviberoxy_wans_active 2\n"
        self.send_response(200); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 19090), H).serve_forever()
EOF
VX_PID=$!

cleanup() { kill $VD_PID $VX_PID $CONSOLE_PID 2>/dev/null || true; }
trap cleanup EXIT
sleep 0.5

rm -rf /tmp/viber-cfg-live2
VIBER_CONFIG_DIR=/tmp/viber-cfg-live2 VIBERAYD_API_URL=http://127.0.0.1:18081 VIBEROXY_METRICS_URL=http://127.0.0.1:19090 VIBEROXY_API_URL=http://127.0.0.1:19090 /tmp/viber-console-ui > /tmp/console-live.log 2>&1 &
CONSOLE_PID=$!
sleep 1.2

echo "=== 1. GET urls (initial) ==="
curl -sf http://127.0.0.1:8090/api/viberayd/urls
echo
echo "=== 2. PUT urls (replace list: add one, keep one) ==="
curl -sf -X PUT http://127.0.0.1:8090/api/viberayd/urls -H "Content-Type: application/json" -d '{"urls":["https://example.com/sub","https://second.example/sub"]}'
echo
echo "=== 3. PUT urls with invalid line -> expect 400 ==="
curl -s -o /dev/null -w "%{http_code}\n" -X PUT http://127.0.0.1:8090/api/viberayd/urls -H "Content-Type: application/json" -d '{"urls":["https://good.example/sub","not-a-url"]}'
echo "=== 4. effective config values (defaults filled, no env file) ==="
curl -sf http://127.0.0.1:8090/api/config/values | python3 -c "import json,sys; d=json.load(sys.stdin); vx=d['viberoxy']; print('WAN_COUNT:', vx.get('WAN_COUNT'), '| XRAY_MUX:', vx.get('XRAY_MUX'), '| ROUTE_MODE:', vx.get('ROUTE_MODE'))"
echo "=== 5. URLs card in HTML ==="
curl -sf http://127.0.0.1:8090/ | grep -c 'urls-card\|urls-textarea'
echo "URLS LIVE OK"
