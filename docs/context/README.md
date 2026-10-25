# Local context

Fast-reference notes for the three upstream codebases and the merge design.
These are summaries with exact file/function pointers, not replacements for the
source or the upstream `AGENTS.md` files.

| File | What it answers |
|---|---|
| [VIBEROXY.md](VIBEROXY.md) | How the proxy/data plane is wired, where the globals and listeners are |
| [VIBERAYD.md](VIBERAYD.md) | How the catalog/test pipeline/state/HTTP work |
| [VIBER-CONSOLE.md](VIBER-CONSOLE.md) | How the UI polls, supervises, and edits config |
| [COUPLING-MAP.md](COUPLING-MAP.md) | Every API/file boundary, duplication, drift, and collision |
| [MODULE-DESIGN.md](MODULE-DESIGN.md) | Target one-process module layout, hot-reload semantics, migration phases |

Related: [../MERGE-FEASIBILITY.md](../MERGE-FEASIBILITY.md) (full assessment) and
[../PROVENANCE.md](../PROVENANCE.md) (imported revisions).
