# Provenance

Imported on 2026-10-25.

| Subtree | Upstream | Imported revision |
|---|---|---|
| `viberoxy/` | https://github.com/amirrezaalavi/Viberoxy | `c01a7a8` (`origin/develop`, merge `93f2221`; quality-gating line) |
| `viberayd/` | https://github.com/amirrezaalavi/Viberayd | `9b83a59` (`main`, merge of `fix/test-quality-verification`) |
| `viber-console/` | https://github.com/yolka-wiz/viber-console | `a1888b8` (`main`) |

All imports used `git subtree add`, preserving full history under the
respective prefixes.

## Why Viberoxy `develop`, not `main`

At import time `origin/main` (`1ae5974`) and `origin/develop` (`c01a7a8` /
merge `93f2221`) had diverged: `develop` had **80 commits not in `main`**
(health and leak guards, TTFB gating, repeat-pass promotion, degraded
fallback, stability probes, and their tests), while `main` had 2 commits not
in `develop` (WAN drop/replacement control API and docs).

The first import used `main`, but that tree failed its own test suite
consistently on this machine:

- `TestTestSpeed`: `main` defaults `BuildXrayConfig` to mux-on, while the test's
  mock SOCKS server relays raw bytes and cannot speak Mux.Cool.
- `TestStartup_DegradedBoot`: proxy never became reachable with 1 of 2 WANs
  within 15 s.

The same tests pass on `develop`, and the full `develop` suite is green
(15 packages). The `main` import is kept for reference:

```sh
git tag -l 'archive/*'          # archive/viberoxy-main-1ae5974
git show archive/viberoxy-main-1ae5974 --stat
```

**Open item:** `main`'s WAN drop/replacement API is not in this baseline. If it
is wanted, port it onto `develop` (the layouts differ substantially, so a
straight cherry-pick will conflict) and re-run the suite.

## Pulling upstream updates

```sh
git remote add viberoxy https://github.com/amirrezaalavi/Viberoxy.git
git remote add viberayd https://github.com/amirrezaalavi/Viberayd.git
git remote add viber-console https://github.com/yolka-wiz/viber-console.git

git fetch viberoxy develop main
git fetch viberayd main
git fetch viber-console main

git subtree pull --prefix=viberoxy viberoxy develop
git subtree pull --prefix=viberayd viberayd main
git subtree pull --prefix=viber-console viber-console main
```

Local pre-existing checkouts used during the feasibility study were
`/home/netcon/playground/ai/Viberoxy-fix` (at `c01a7a8`) and
`/home/netcon/playground/ai/Viberayd-fix` (at `753a3d7`). They are not part
of this repository.