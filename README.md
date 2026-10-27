# dissident

One Go binary, one process: subscription catalog + qualification pipeline +
one active WAN with ready standbys + HTTP CONNECT/SOCKS5 proxy + a small
server-rendered panel. No HTTP between modules, no supervisor, no `/sub`.

This repository is the working demo built from the Viberoxy/Viberayd/Viber
Console merge. The original trees remain under `viberoxy/`, `viberayd/`, and
`viber-console/` for provenance; the new service lives at the repository root.

## Quick start

```sh
make dissident

cp configs/dissident.demo.conf configs/dissident.conf
# Put one or more share links or subscription URLs in configs/urls.txt.
$EDITOR configs/urls.txt

./bin/dissident -config configs/dissident.conf
```

- Panel: <http://127.0.0.1:8090/>
- Proxy: `127.0.0.1:10909` — both HTTP CONNECT and SOCKS5 on the same port.

```sh
curl -x http://127.0.0.1:10909 https://www.gstatic.com/generate_204
curl --socks5-hostname 127.0.0.1:10909 https://www.cloudflare.com/cdn-cgi/trace
```

## Releases

Tags matching `v*` build linux/amd64 and linux/arm64 assets for `dissident`,
`viberayd`, `viberoxy`, and `viber-console` through
`.github/workflows/release.yml`. Build the same set locally with:

```sh
make release VERSION=v0.1.0
```

The assets and `SHA256SUMS` are written to `dist/`.

## Windows

The service cross-compiles for Windows and runs as a console process:

```powershell
go build -trimpath -o dissident.exe ./cmd/dissident
.\dissident.exe -version
```

- Put `xray.exe` on `PATH`, or set `paths.xray_bin = xray.exe` in the
  config. The Windows default is `xray.exe`; Unix keeps
  `/usr/local/bin/xray`.
- The default state, log, and subscription paths are relative
  (`./var`, `./configs`), so run the service from a writable directory.
- SIGHUP reload is Unix-only. On Windows, restart the process to apply the
  same configuration changes.
- There is no built-in Windows service wrapper yet. Run it under Task
  Scheduler, NSSM, or WinSW if it must survive logoff.
- Panel and proxy listen on the same `0.0.0.0:8090` and `0.0.0.0:10909`;
  add firewall rules for the interfaces you expose.

CI builds every module on `windows-latest`, vets and tests the dissident
package there, and smoke-tests the resulting `dissident.exe`.

Stop with Ctrl-C or SIGTERM. On Unix, SIGHUP reloads the configuration. The accepted live
fields are `log.level`, `cycle.*`, and `swap.*`; listener, qualification, URL,
path, and log-file changes are rejected with a restart-required error so a
half-applied config can never run.

## What works

- **Catalog**: reads `paths.urls_file` (one share link or http(s) subscription
  URL per line), fetches at most 8 MiB per source, parses base64 payloads, and
  deduplicates by SHA-256 of the raw link.
- **Qualification**: one strict pipeline for every config:
  support → TCP → TLS (TLS transports only) → protocol → render → `xray -test`
  → real HTTP → content quorum → speed/TTFB → stability. A config that fails a
  stage never reaches the next one. The real xray is started once per candidate
  and stopped on every path. Cheap stages run in `test.cheap_workers` parallel
  workers; deep tests are bounded by `test.real_workers`; the 1 MB download,
  32 KiB upload, and 128 KiB HTTP/2 gates are serialized.
- **WAN runtime**: exactly one active xray client, a persisted ready list, 15s
  liveness (3 probes / 8s), passive-failure burst, and make-before-break
  replacement with a drain window. The only manual WAN actions are **Replace**
  and **Remove ready**.
- **Proxy**: one listener, first-byte sniff, HTTP CONNECT, SOCKS5 no-auth
  CONNECT, byte accounting, and a TSV access log.
- **Panel** (server-rendered, no SPA/JSON API): Overview, WAN, Tests,
  Subscriptions, Settings, Logs, plus `/healthz`, `/readyz`, and `/metrics`.
- **State**: active and ready links persist atomically (0600), so a restart can
  re-promote the same active path without waiting for a full cycle.
- **Logs**: `time  src  state  dst  domain:port` plus optional
  `proto wan up down dur reason`; rotated with gzip and retention.

Supported v1 links: VLESS tcp/ws/grpc/reality/xhttp, VMess tcp/ws/grpc, Trojan
tcp/ws/grpc, Shadowsocks without plugins, and SOCKS5. TUIC, Hysteria2,
WireGuard, plugins, kcp, quic, h2, and httpupgrade are rejected explicitly.

## Demo configuration

`configs/dissident.demo.conf` is a safe template: no credentials, no egress
proxy. For a host that reaches the internet through an HTTP proxy, set:

```ini
[test]
egress_proxy = http://proxy.example:8080
```

This makes the qualification TCP/TLS stages use CONNECT and adds an xray HTTP
outbound for the remote server connection. Leave it empty in production; it is
not a routing feature and never falls back to direct egress for proxied traffic.

## Verification

```sh
export GOCACHE=$PWD/.cache/go-build GOTMPDIR=$PWD/.cache/tmp
gofmt -l cmd internal
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

The demo was exercised against a real subscription link through a restricted
host: panel ready, CONNECT 204, SOCKS5 200, access log populated, manual Replace
returned 303 and traffic continued. See `docs/M7-DEMO.md` for the exact checks.

## Production install (systemd)

The `deploy/` directory contains the production unit and configuration used
for the single-process service. It targets a dedicated `dissident` user and
puts mutable state under `/var/lib/dissident`.

```sh
make dissident
sudo useradd --system --no-create-home --home-dir /var/lib/dissident \
  --shell /usr/sbin/nologin --user-group dissident
sudo install -d -o root -g root -m 0755 /opt/dissident
sudo install -o root -g root -m 0755 bin/dissident /opt/dissident/dissident
sudo install -d -o root -g dissident -m 0750 /etc/dissident
sudo install -o root -g dissident -m 0640 deploy/dissident.conf /etc/dissident/dissident.conf
sudo install -d -o dissident -g dissident -m 0750 /var/lib/dissident /var/log/dissident
sudo install -o root -g root -m 0644 deploy/dissident.service /etc/systemd/system/dissident.service
# Put share links or subscription URLs here; the panel may add subscriptions later.
sudo install -o dissident -g dissident -m 0600 /path/to/urls.txt /var/lib/dissident/urls.txt
sudo systemctl daemon-reload
sudo systemctl enable --now dissident
```

The shipped config uses `ready_target = 5` and `ready_min = 5` so the service
actively refills until five standby WANs are qualified. Panel `8090` and proxy
`10909` bind `0.0.0.0`; keep the host on a trusted network until panel auth and
TLS exist. `state.json`, `urls.txt`, and the access/system logs are writable
only by the service user.

## Limitations

- No authentication or TLS on the panel/proxy yet; bind defaults are `0.0.0.0`.
  Run it on a trusted network until auth is added.
- One active WAN by design. Standbys are metadata; only the active one runs xray.
- Code changes require a restart. Config reload is transactional but not every
  field is live.
- The environment-gated egress proxy is a demo escape hatch, not a product
  feature.
- The legacy modules are not wired into `dissident`; their tests are not run by
  the root `go test`.

## Layout

```
cmd/dissident/        CLI entry point
internal/app/         lifecycle, wiring, reload policy
internal/config/      one INI schema, validation, atomic snapshots
internal/catalog/     sources, dedupe, state metadata
internal/uri/         raw-faithful share-link parser
internal/xraycfg/     xray JSON renderer + preflight
internal/qualify/     staged qualification pipeline
internal/platform/    xray process, SOCKS5, egress dialer, ports
internal/proxy/       one-port CONNECT + SOCKS5 data plane
internal/wan/         active WAN, ready list, liveness, replace, persistence
internal/control/     server-rendered panel and action routes
internal/logging/     slog system log + TSV access log
configs/              example and demo configs
docs/                 design, context, and demo evidence
```

## Provenance

The original trees were imported with `git subtree`; histories and licences are
preserved. Viberoxy uses the quality-gating `develop` line (`c01a7a8`) because
upstream `main` fails two of its own tests. See `docs/PROVENANCE.md`.

Licence: GPL-3.0.
