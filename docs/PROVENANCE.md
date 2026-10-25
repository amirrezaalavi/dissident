# Provenance

Imported on 2026-10-25 from the upstream default branches.

| Subtree | Upstream | Imported revision |
|---|---|---|
| `viberoxy/` | https://github.com/amirrezaalavi/Viberoxy | `1ae5974` (`main`, "docs: merge control API documentation") |
| `viberayd/` | https://github.com/amirrezaalavi/Viberayd | `9b83a59` (`main`, merge of `fix/test-quality-verification`) |

Both imports used `git subtree add`, preserving full history under the
respective prefixes. To pull future upstream changes:

```sh
git remote add viberoxy https://github.com/amirrezaalavi/Viberoxy.git
git remote add viberayd https://github.com/amirrezaalavi/Viberayd.git
git fetch viberoxy main
git fetch viberayd main
git subtree pull --prefix=viberoxy viberoxy main
git subtree pull --prefix=viberayd viberayd main
```

## Known divergence: Viberoxy `main` vs `develop`

At import time, Viberoxy's `origin/main` and `origin/develop` had diverged:

- `main` (`1ae5974`): adds the WAN drop/replacement control API and docs.
- `develop` / `fix/quality-gating` (`93f2221` / `c01a7a8`): **80 commits not
  in `main`**, covering health/leak guards, TTFB gating, repeat-pass
  promotion, degraded fallback, and related tests. This is the baseline used
  by `docs/MERGE-FEASIBILITY.md`.
- `main` has 2 commits not in `develop`.

The monorepo currently tracks `main` because that is the upstream default.
If the quality-gating line is the intended base, switch the subtree to
`develop` (or merge the two lines) **before** starting the refactor, so the
merged service does not silently lose those fixes.

Local pre-existing checkouts used during the feasibility study were
`/home/netcon/playground/ai/Viberoxy-fix` (at `c01a7a8`) and
`/home/netcon/playground/ai/Viberayd-fix` (at `753a3d7`). They are not part
of this repository.
