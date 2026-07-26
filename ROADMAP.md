# Etcd-walker — Roadmap

Prioritized backlog as of the 2026-07-26 review (v0.7.0). Bug ids (`B-*`)
and limitation ids (`L-*`) refer to [DESIGN.md §12](DESIGN.md); feature ids
(`F-*`) are referenced back from there. Nothing here is scheduled — it is
an ordered menu.

## 0. Fix the open findings first

The 2026-07-26 review catalogued six findings in DESIGN.md §12; B-4 has
since been fixed, the rest are open. In severity order:

1. **B-1 — shared-lease revoke can delete other keys** (`setTTL`): resolve
   the old lease with `Lease.TimeToLive(ctx, id, clientv3.WithAttachedKeys())`
   and revoke only when the edited key was its sole user. One function, plus
   a fake-lessor test.
2. **B-2 — `Auth: ?` in auto/v2 modes**: call `authStatus()` in the `auto`
   and `v2` arms of `model.NewModel` (revives the tested-but-dead
   `v2Backend.authStatus`).
3. **B-3 — leaked v3 client on probe failure**: `Close()` the v3 client
   before falling back to v2 in `auto` (and on explicit-probe failure).
4. ~~**B-4 — delete the dead file branch of `controller.edit()`**~~ —
   **fixed 2026-07-26**: `edit()` removed entirely; `Ctrl+E` on directories
   delegates to the `rename()` dialog.
5. **B-5 — apply the `.dir` reserved-name guard in `Model.Import`**.
6. **B-6 — legend styling typo** (`[::]` → `[::b]` for the Search entry).

## 1. Quick wins

| Id | Feature | Sketch |
|----|---------|--------|
| F-1 | **Watch mode / live refresh** | Toggleable (hotkey, e.g. `Ctrl+G`) v3 `Watch` on the current prefix (v2: polling fallback) that re-runs `updateList` via `App.QueueUpdateDraw` on events. Indicator in the header. The single most useful addition for anyone staring at a changing cluster. |
| F-2 | **Auth label everywhere** | Same as B-2 above — the feature half is showing `ON/OFF` in `auto`/`v2` modes. |
| F-3 | **`-version` flag + single-sourced version** | `var version = "dev"` + `-ldflags "-X main.version=$(VERSION)"` in the Makefile; header reads it, `-version` prints it. Kills the three-place bump documented in DESIGN §9.3. |
| F-4 | **Read-only mode** | `-read-only` flag / `"read_only": true` config: mutating hotkeys show a "read-only session" modal instead of acting; header shows `[RO]`. Cheap insurance for browsing production. |
| F-5 | **Type-to-confirm for recursive deletes** | For directory deletes, require typing the basename (à la GitHub repo deletion) instead of a bare ok/cancel — a recursive `DeleteRange` on a fat prefix is the most destructive thing the tool can do. |

## 2. Power features

| Id | Feature | Sketch |
|----|---------|--------|
| F-6 | **Multi-select + batch ops** | `Space` marks/unmarks rows (`currentNodes` already keys them stably); batch delete first, then batch copy/move to a target dir. |
| F-8 | **Lease browser (v3)** | New screen listing `Leases()` with TTL + attached keys (`TimeToLive` + `WithAttachedKeys`); actions: revoke, keepalive-once, jump to key. Also the natural home for B-1's shared-lease warning. |
| F-9 | **Paginated range ops** | Replace the single `Range` in `ls`/`search`/`export` with a `WithLimit(N)`+`WithFromKey` loop (L-1); progress modal with cancel for search/export. Unlocks million-key clusters. |
| F-10 | **$EDITOR integration** | `App.Suspend()` + `$EDITOR` on a temp file for values too big for the built-in editor; re-read on exit, save via `SetKeepTTL`. |
| F-11 | **Connection profiles / multiple endpoints** | Config grows a `"profiles": {name: {host, port, …}}` map + `-profile` flag; `Options.Host/Port` becomes an endpoint list passed to `clientv3.Config.Endpoints`. Startup picker when several profiles exist. |
| F-12 | **Namespace scoping** | `-prefix /team-a` wraps the v3 KV/Lease clients in `clientv3/namespace` so the walker is confined to a subtree — safe delegation of a shared cluster. |

## 3. Bigger bets

| Id | Feature | Sketch |
|----|---------|--------|
| F-13 | **Drop the v2 backend** (major version) | Removes `github.com/coreos/etcd`, the `grpc => v1.29.1` pin and `dgrijalva/jwt-go` (L-4), halves the model, and unlocks current gRPC/etcd clients. Ship a final v2-capable 0.x release first. |
| F-14 | **Cluster status pane** | `Maintenance` API: member list + leader, per-member db size, alarms, endpoint health/RTT; either a modal or a third pane. |
| F-15 | **Directory diff** | Compare two prefixes (same or different cluster once F-11 lands): added/removed/changed keys, drill into per-key value diff. |
| F-16 | **Auth management (v3)** | Users/roles CRUD + permission grants via the `Auth` client — turns the walker into a small etcd admin console. |
| F-17 | **Bookmarks + navigation history** | Persist favourite paths per profile (config or XDG state file); `Ctrl+B` to bookmark, picker to jump; back/forward stack alongside `position`. |
| F-18 | **Snapshot save** | `Maintenance.Snapshot` streamed to a local file with progress — one-keystroke cluster backup before risky edits. |

## Shipped

| Id | Feature | Version |
|----|---------|---------|
| F-7 | **Revision history viewer (v3)** — `Ctrl+V`: revision picker (`Get`+`WithRev` walk, compaction-aware), detail view, LCS line-diff vs current, restore via `SetKeepTTL` (lease/TTL preserved). DESIGN §5.6/§7.5. | 0.8.0 |

## Non-goals (for now)

- Full etcdctl parity (compaction, defrag, move-leader) — keep the tool a
  *browser* with guardrails, not a cluster surgeon.
- Windows clipboard/terminal quirks beyond what `atotto/clipboard` + OSC52
  already cover.
- A mouse-driven UI: keyboard-first is the point.
