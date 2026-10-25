#!/usr/bin/env bash
# UI smoke test: boot mock daemons + console binary, curl the SPA + API.
set -u
export PATH="$HOME/.local/go/bin:$PATH"
cd "$(dirname "$0")/.."

go build -o /tmp/viber-console-ui ./cmd/console

# Mock viberayd on :18081
python3 - <<'EOF' &
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/api/stats"):
            body = json.dumps({"total":120,"working":8,"failed":100,"unreachable":12,"updated_at":"2026-08-13T09:00:00Z"}).encode()
        elif self.path.startswith("/api/configs"):
            body = json.dumps({"page":1,"per_page":100,"total":2,"configs":[
                {"host":"1.2.3.4","port":443,"protocol":"vless","state":"working","latency_ms":120,"success_count":5,"fail_count":0},
                {"host":"5.6.7.8","port":80,"protocol":"ss","state":"failed","latency_ms":0,"success_count":0,"fail_count":3}]}).encode()
        elif self.path.startswith("/api/urls"):
            body = json.dumps(["https://example.com/sub"]).encode()
        else:
            body = b'{}'
        self.send_response(200); self.send_header("Content-Type","application/json"); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 18081), H).serve_forever()
EOF
VD_PID=$!

# Mock viberoxy on :19090
python3 - <<'EOF' &
import http.server
METRICS = """# TYPE viberoxy_wans_active gauge
viberoxy_wans_active 2
# TYPE viberoxy_wan_speed_mbps gauge
viberoxy_wan_speed_mbps{index="0"} 42.5
viberoxy_wan_speed_mbps{index="1"} 18.2
# TYPE viberoxy_proxy_connections_total counter
viberoxy_proxy_connections_total{wan="0",proto="connect"} 10
viberoxy_proxy_connections_total{wan="1",proto="connect"} 7
# TYPE viberoxy_proxy_bytes_total counter
viberoxy_proxy_bytes_total{wan="0",direction="up"} 1000
viberoxy_proxy_bytes_total{wan="0",direction="down"} 5000
# TYPE viberoxy_proxy_latency_seconds histogram
viberoxy_proxy_latency_seconds_bucket{le="0.05"} 8
viberoxy_proxy_latency_seconds_bucket{le="0.1"} 12
viberoxy_proxy_latency_seconds_bucket{le="0.25"} 18
viberoxy_proxy_latency_seconds_bucket{le="0.5"} 20
viberoxy_proxy_latency_seconds_bucket{le="+Inf"} 20
viberoxy_proxy_latency_seconds_sum 2.5
viberoxy_proxy_latency_seconds_count 20
"""
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz": body = b"ok"
        elif self.path == "/readyz": body = b"ready"
        else: body = METRICS.encode()
        self.send_response(200); self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 19090), H).serve_forever()
EOF
VX_PID=$!

cleanup() { kill $VD_PID $VX_PID $CONSOLE_PID 2>/dev/null || true; }
trap cleanup EXIT
sleep 0.5

rm -rf /tmp/viber-cfg
VIBER_CONFIG_DIR=/tmp/viber-cfg VIBERAYD_API_URL=http://127.0.0.1:18081 VIBEROXY_METRICS_URL=http://127.0.0.1:19090 VIBEROXY_API_URL=http://127.0.0.1:19090 /tmp/viber-console-ui &
CONSOLE_PID=$!
sleep 1

echo "=== HTML serves ==="
curl -sf http://127.0.0.1:8090/ | head -3
echo "=== SPA shell sanity (all sections present) ==="
curl -sf http://127.0.0.1:8090/ | grep -c 'id="\(stats\|wan-grid\|config-list\|settings-form\)"'
echo "=== CSS ==="
curl -sf -o /dev/null -w "%{http_code} %{size_download}B\n" http://127.0.0.1:8090/static/styles.css
echo "=== JS ==="
curl -sf -o /dev/null -w "%{http_code} %{size_download}B\n" http://127.0.0.1:8090/static/app.js
echo "=== schema ==="
curl -sf http://127.0.0.1:8090/api/config/schema | head -c 160
echo
echo "=== values POST ==="
curl -sf -X POST http://127.0.0.1:8090/api/config/values -H "Content-Type: application/json" -d '{"group":"viberoxy","values":{"WAN_COUNT":"3"}}'
echo
echo "=== env file ==="
grep -E "WAN_COUNT|ROUTE_MODE" /tmp/viber-cfg/viberoxy.env
echo "=== overview via API ==="
curl -sf http://127.0.0.1:8090/api/overview | head -c 240
echo
echo "UI SMOKE OK"
