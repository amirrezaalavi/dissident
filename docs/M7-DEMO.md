# M7 — working demo evidence

Date: 2026-10-25 (Asia/Tehran)

Binary: `bin/dissident` (Go 1.26.6, xray 26.3.27)

Config used for the smoke: a private file under
`/home/netcon/dissident/.cache/demo/dissident.conf` (not committed); it points at
one real VMess share link plus two aliases of the same link with different
fragments. The aliases give the ready list something to promote during Replace
without needing a second provider.

## Automated checks

```text
$ gofmt -l cmd internal
(empty)

$ go build ./...
BUILD_OK

$ go vet ./...
VET_OK

$ go test ./... -count=1
ok  dissident/cmd/dissident
ok  dissident/internal/app
ok  dissident/internal/catalog
ok  dissident/internal/config
ok  dissident/internal/contract      [no test files]
ok  dissident/internal/control
ok  dissident/internal/logging
ok  dissident/internal/platform
ok  dissident/internal/proxy
ok  dissident/internal/qualify
ok  dissident/internal/uri
ok  dissident/internal/wan
ok  dissident/internal/xraycfg

$ go test -race ./... -count=1
all packages ok
```

## Live smoke

Start:

```sh
/home/netcon/dissident/.cache/demo/dissident \
  -config /home/netcon/dissident/.cache/demo/dissident.conf
```

After the first qualification pass (final run: `/readyz` 200 after ~20s):

```text
GET /readyz                              -> 200 "ready\n"
GET /                                     -> Active WAN, Ready standbys 2/3,
                                            Catalog entries 3, generation 1
GET /wan                                  -> active + ready table; Replace enabled
GET /tests                                -> active / ready / ready rows
GET /metrics                              -> dissident_active_wan 1,
                                            dissident_ready_entries 2,
                                            dissident_catalog_entries 3
GET /sub, /api/wan, /api/status           -> 404
```

The demo config uses the requested 4,000,000-byte speed test with
`http_timeout = 10s` (the speed stage doubles this), `min_speed_mbps = 2.5`,
and `content_quorum = 3`. The working link measured 4.86 Mbps with a 1.68s TTFB.

Both frontends on port 10909:

```text
$ curl -x http://127.0.0.1:10909 https://www.gstatic.com/generate_204
204 0

$ curl --socks5-hostname 127.0.0.1:10909 \
       https://www.cloudflare.com/cdn-cgi/trace
200 245
```

Access log line (fields match the requested format plus optional detail):

```text
2026-10-25T09:57:06.867Z  127.0.0.1:36076  OK  www.gstatic.com:443
www.gstatic.com:443  connect  0ff13bfa1145  1959  4713  1054  -
```

Replace:

```text
POST /wan/replace -> 303 /wan?msg=active+WAN+replaced
active hash switched, ready count 2 -> 1
system log: wan: replaced active hash=e69e... port=42403
proxy request after replace -> 204
```

## What this proves

- One process serves the panel, HTTP CONNECT, and SOCKS5.
- A real remote WAN was qualified through the full staged pipeline and served
  traffic through a local xray client.
- Ready standbys were persisted and used for a make-before-break Replace while
  the proxy stayed available.
- Access logging, panel pages, readiness, and state persistence work with live
  data, not only fakes.
- Removed surfaces really are gone: `/sub` and the raw API return 404.

## Not covered by this smoke

- A second independent provider (the ready aliases share one endpoint).
- Killing the active xray process from the host (the test process runs in a
  separate PID namespace). Failure windows and replacement are covered by
  `internal/wan` tests with fake processes.
- Auth/TLS, multi-WAN, and code hot reload are explicitly out of scope for this
  demo.
