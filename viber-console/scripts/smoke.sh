#!/usr/bin/env bash
# Smoke test: run the console binary against mock viberayd/viberoxy servers.
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="$HOME/.local/go/bin:$PATH"
go build -o /tmp/viber-console-smoke ./cmd/console

# Mock viberayd on :18081
python3 - <<'EOF' &
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/api/stats"):
            body = json.dumps({"total":120,"working":8,"failed":100,"unreachable":12,"updated_at":"2026-08-13T09:00:00Z"}).encode()
        elif self.path.startswith("/api/configs"):
            body = json.dumps({"page":1,"per_page":50,"total":1,"configs":[{"host":"1.2.3.4","port":443,"protocol":"vless","state":"working","latency_ms":120}]}).encode()
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
viberoxy_proxy_connections_total{wan="0",proto="socks5"} 5
viberoxy_proxy_connections_total{wan="1",proto="connect"} 7
# TYPE viberoxy_proxy_bytes_total counter
viberoxy_proxy_bytes_total{wan="0",direction="up"} 1000
viberoxy_proxy_bytes_total{wan="0",direction="down"} 5000
viberoxy_proxy_bytes_total{wan="1",direction="up"} 2000
viberoxy_proxy_bytes_total{wan="1",direction="down"} 9000
# TYPE viberoxy_proxy_latency_seconds histogram
viberoxy_proxy_latency_seconds_bucket{le="0.05"} 8
viberoxy_proxy_latency_seconds_bucket{le="0.1"} 12
viberoxy_proxy_latency_seconds_bucket{le="0.25"} 18
viberoxy_proxy_latency_seconds_bucket{le="0.5"} 20
viberoxy_proxy_latency_seconds_bucket{le="+Inf"} 20
viberoxy_proxy_latency_seconds_sum 2.5
viberoxy_proxy_latency_seconds_count 20
# TYPE viberoxy_build_info gauge
viberoxy_build_info{version="v0.1.0"} 1
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

# Mock viberoxy control API on a separate listener (:11980), matching the real
# deployment contract where observability and control use different ports.
python3 - <<'EOF' &
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/api/viberoxy/wans":
            body = b'[{"index":0,"state":"active","speed_mbps":42.5}]'
            self.send_response(200)
        else:
            body = b'not found'
            self.send_response(404)
        self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 11980), H).serve_forever()
EOF
VX_API_PID=$!

cleanup() { kill $VD_PID $VX_PID $VX_API_PID $CONSOLE_PID 2>/dev/null || true; }
trap cleanup EXIT
sleep 0.5

CONSOLE_LISTEN=127.0.0.1:18090 VIBERAYD_API_URL=http://127.0.0.1:18081 VIBEROXY_METRICS_URL=http://127.0.0.1:19090 VIBEROXY_API_URL=http://127.0.0.1:11980 /tmp/viber-console-smoke &
CONSOLE_PID=$!
sleep 1

echo "=== /api/health ==="
curl -sf http://127.0.0.1:18090/api/health
echo
echo "=== /api/overview ==="
curl -sf http://127.0.0.1:18090/api/overview
echo
echo "=== /api/viberayd/configs ==="
curl -sf "http://127.0.0.1:18090/api/viberayd/configs?page=1&per_page=50"
echo
echo "=== /api/viberoxy/metrics ==="
curl -sf http://127.0.0.1:18090/api/viberoxy/metrics | head -c 400
echo
echo "=== /api/viberoxy/wans (separate upstream API listener) ==="
curl -sf http://127.0.0.1:18090/api/viberoxy/wans
echo
echo "SMOKE OK"
