# viber

One workspace for the Viber family: **Viberoxy** (proxy data plane) and
**Viberayd** (subscription catalog and tester).

This is currently a **Phase 0 monorepo**: both projects keep their own Go
module, entry point, and tests, but they live in one repository with their
full upstream histories preserved. No source code has been changed or moved.

## Layout

| Path | Module | Role | Entry point |
|---|---|---|---|
| `viberoxy/` | `viberoxy` | CONNECT/SOCKS proxy, WAN pool, health, routing, control API | `viberoxy/main.go` |
| `viberayd/` | `github.com/amirrezaalavi/Viberayd` | subscription fetch/test pipeline, state, `/sub`, API | `viberayd/cmd/viberayd` |
| `docs/` | — | merge feasibility, provenance, migration notes | — |

`go.work` ties the two modules together so both can be built from this root.

## Build and test

Requires Go 1.21+ and, for end-to-end tests, an `xray` binary on `PATH`.

```sh
make build   # compile both modules
make test    # run both suites
make race    # run both suites under the race detector
make check   # build + vet + test
```

Equivalent without make:

```sh
go build ./viberoxy/... ./viberayd/...
go test  ./viberoxy/... ./viberayd/...
```

## Status and next step

The two programs still run as **two separate processes** with two config
surfaces and two xray port pools. Unifying them into one binary/process is a
staged refactor, not a copy-paste merge; see
[docs/MERGE-FEASIBILITY.md](docs/MERGE-FEASIBILITY.md) for the architecture,
hot-reload design, risk register, and phased plan.

Immediate milestone: agree on the first behavioural changes, then either

1. keep the monorepo and make those changes in place (lowest risk), or
2. start Phase 1 by moving Viberoxy out of `package main` into
   `internal/proxy` and running both components under one `cmd/viber`.

## Provenance

Both trees were imported with `git subtree`, so `git log` still shows the
original commits. See [docs/PROVENANCE.md](docs/PROVENANCE.md) for upstream
URLs, imported revisions, and the one significant branch divergence.

## Licence

GPL-3.0. Both upstream licences are identical; see [LICENSE](LICENSE).
