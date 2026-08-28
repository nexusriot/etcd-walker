# Etcd-walker — Roadmap

Prioritized backlog, last revised 2026-08-28. Bug ids (`B-*`) and limitation
ids (`L-*`) refer to [DESIGN.md §12](DESIGN.md); feature ids (`F-*`, `N-*`)
are referenced back from there and from §13. Nothing here is scheduled — it
is an ordered menu.

The current version is **0.10.0**, which adds time-travel browsing (F-19),
undo snapshots for recursive deletes (N-25) and the two-pane layout (F-20),
described in [DESIGN.md §14](DESIGN.md). 0.9.0 carried the safety work
(F-4, F-5, N-6, N-11, N-14, N-21) and every fix from the B-1..B-18 rounds.
Note that the last tag is 0.6.0.

## 0. Open findings — all cleared

The 2026-07-26 review catalogued six findings in DESIGN.md §12. B-4 was
fixed the same day; **B-1, B-2, B-3, B-5 and B-6 were fixed 2026-08-08**,
each with a regression test verified to fail against the pre-fix code.

1. ~~**B-1 — shared-lease revoke can delete other keys**~~ — `setTTL` now
   calls `revokeIfOrphaned`, which resolves the lease with
   `TimeToLive(..., WithAttachedKeys())` and revokes only when nothing else
   is attached. Lease RPCs moved behind an injectable `lessor` interface so
   a `fakeLessor` can pin the behaviour.
2. ~~**B-2 — `Auth: ?` in auto/v2 modes**~~ — new `authLabelOf` helper is
   applied in every arm of `model.NewModel` (revives the tested-but-dead
   `v2Backend.authStatus`).
3. ~~**B-3 — leaked v3 client on probe failure**~~ — new `v3Backend.close()`
   is called before the `auto` fallback to v2 and on explicit-probe failure.
4. ~~**B-4 — dead file branch of `controller.edit()`**~~ — `edit()` removed
   entirely; `Ctrl+E` on directories delegates to the `rename()` dialog.
5. ~~**B-5 — `.dir` reserved-name guard in `Model.Import`**~~ — marker keys
   are counted as skipped, matching `export`, which omits them.
6. ~~**B-6 — legend styling typo**~~ — `[::]` → `[::b]` on the Search entry.

The **2026-08-11 deep review** added B-7..B-12, all fixed the same day and
catalogued in DESIGN §12. B-7 was the serious one: `Ctrl+E`/`Ctrl+T` wrote the
*cached* value, which on v3 is empty until the details pane fetches it — so
one swallowed refresh error turned an edit into a silent wipe.

That review also logged three findings as behaviour changes rather than
defects (O-1 export overwrite confirmation, O-2 capped revision values with an
`[f]` expand, O-3 diffing against a live re-read instead of the picker's
snapshot). **All three landed 2026-08-11**, and implementing O-2 turned up
**B-13**: tview's colour-tag parser was eating every `[d]`/`[r]`/`[Esc]` hint
on the history screens, in titles as much as in body text, so those hotkeys
had been invisible since the feature shipped.

A **2026-08-16 follow-up review** of the 0.9.0 safety work found three more,
all fixed same day (DESIGN §12): B-14 a declined protected-save confirmation
left a modal that resurfaced behind the value editor; B-15 dry-run still
mutated the injected cache, so a rehearsal could leave a phantom row; B-16
dry-run's success modals claimed changes had been made.

A **2026-08-19 review** of the test/CI round found two more in the startup
path, both fixed (DESIGN §12): B-17 the flag bundle could lose a field and
nil-panic on startup while every test passed; B-18 no boolean flag accepted
its bare form, so the README's own `-read-only` example did not run.

**2026-08-14** shipped the safety work — F-4 (read-only sessions), N-11
(visible stale state), N-14 (protected prefixes), then F-5 (type-to-confirm
recursive deletes), N-6 (session journal) and N-21 (dry-run), all described
in DESIGN §13. **2026-08-28** shipped F-19, N-25 and F-20 (DESIGN §14),
which also turned up two defects of their own: undo snapshots corrupted
non-UTF-8 values through `encoding/json` (fixed with a base64 side map), and
`NewView` rooted an empty layout so the running app started with no focus and
every list binding dead (fixed and pinned by a view test). Next up: F-3, then
N-1 (CAS-safe writes).

## 1. Quick wins

| Id | Feature | Sketch |
|----|---------|--------|
| F-1 | **Watch mode / live refresh** | Toggleable (hotkey — `Ctrl+G` and `Ctrl+B` are taken as of 0.10.0) v3 `Watch` on the current prefix (v2: polling fallback) that re-runs `updateList` via `App.QueueUpdateDraw` on events. Indicator in the header. The single most useful addition for anyone staring at a changing cluster. |
| F-2 | **Auth label everywhere** | Same as B-2 above — the feature half is showing `ON/OFF` in `auto`/`v2` modes. |
| F-3 | **`-version` flag + single-sourced version** | `var version = "dev"` + `-ldflags "-X main.version=$(VERSION)"` in the Makefile; header reads it, `-version` prints it. Kills the three-place bump documented in DESIGN §9.3. |
| ~~F-4~~ | **Read-only mode** — shipped in 0.9.0 | `-read-only` flag / `"read_only": true` config: mutating hotkeys show a "read-only session" modal instead of acting; header shows `[RO]`. Cheap insurance for browsing production. |
| ~~F-5~~ | **Type-to-confirm for recursive deletes** — shipped in 0.9.0 | For directory deletes, require typing the basename (à la GitHub repo deletion) instead of a bare ok/cancel — a recursive `DeleteRange` on a fat prefix is the most destructive thing the tool can do. |

## 2. Power features

| Id | Feature | Sketch |
|----|---------|--------|
| F-21 | **Live pane, live diff** | With two panes and per-pane revisions in place, F-15 (directory diff) is now "diff pane A against pane B" — the two sides are already chosen, `diffLines`/`renderDiff` already exist (N-13), and the interesting case (same prefix, two revisions) is one keystroke away from what 0.10.0 ships. |
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
| F-17 | **Bookmarks + navigation history** | Persist favourite paths per profile (config or XDG state file); a free key to bookmark (`Ctrl+B` is the pane toggle as of 0.10.0), picker to jump; back/forward stack alongside `position`. |
| F-18 | **Snapshot save** | `Maintenance.Snapshot` streamed to a local file with progress — one-keystroke cluster backup before risky edits. |

## 4. Newly identified (2026-08-08 analysis)

| Id | Feature | Sketch |
|----|---------|--------|
| N-1 | **CAS-safe writes** | `Set` is a blind `Put` and the editor saves over whatever is on the server now. `Node.ModRev` is already captured, so wrap saves in `Txn().If(Compare(ModRevision(k), "=", rev))` (v2: `PrevIndex`) and offer reload/overwrite/diff — the F-7 diff renderer is right there — when it fails. The biggest correctness gap left for multi-user clusters. |
| N-2 | **User-level config discovery** | `config.DefaultPath` is only `/etc/etcd-walker/config.json`, so a non-root user needs `-config` every run. Search `$ETCD_WALKER_CONFIG` → `$XDG_CONFIG_HOME/etcd-walker/config.json` → `~/.config/…` → `/etc/…`. Pairs with F-11. |
| N-3 | **Credential hygiene** | `-password` is world-readable in `ps`. Add `-password-file`, an env var, and an interactive prompt when a username is set but no password is. |
| N-4 | **Connection state + reconnect** | The model dials inside `NewController` before any UI exists (unreachable host = blank terminal for the timeout), and there is no reconnect: if etcd goes away every op errors while the header still reads healthy. Prerequisite for F-1. |
| N-5 | **Throttle the details refetch** | `fillDetails` issues a `Get` per cursor move (L-6); holding ↓ through a 500-key dir is 500 round trips. Debounce on cursor rest or cache by `ModRev`. |
| ~~N-6~~ | **Session journal / copy-as-etcdctl** — shipped in 0.9.0 | Record every mutation and export it as an `etcdctl` script. Composes with F-4: browse read-only, emit the script, run it elsewhere. *Shipped as the `Ctrl+A` journal (whole script to file or clipboard); the proposed per-key "copy as etcdctl put" keystroke was not built — the journal covers the use case and the single-key variant is still open if wanted.* |
| N-7 | **Prefix stats ("du" for etcd)** | Key count and total value bytes per child prefix + largest keys. Nothing in the TUI shows where the db size went. Better after F-9. |
| N-8 | **Sort & live filter** | `orderedEntries` is hardcoded dirs-then-files-alpha; add sort by mod-revision/size/TTL and a filter that *hides* non-matching rows (today's search only moves the cursor). |
| N-9 | **Regex/glob in find** | `search` is substring-only; a regex predicate is a one-line change in both backends. |
| N-10 | **Undo the last destructive op** | Stash the prior value/lease on delete, rename and overwrite; `Ctrl+Z` restores. Single keys only — recursive directory deletes are covered by N-25's snapshots as of 0.10.0. |

## 5. From the 2026-08-11 review

Ideas the deep review turned up, roughly by value-per-effort.

| Id | Feature | Sketch |
|----|---------|--------|
| ~~N-11~~ | **Surface refresh failures** — shipped in 0.9.0 | `fillDetails` swallows its own `Get` error and leaves the stale node in place — that silence is what made B-7 a data-loss bug rather than an annoyance. Show a `[stale]` badge in the details pane (and a dimmed row) when the on-focus read fails, so a cached value is never mistaken for a live one. Small, and it retires a whole class of trap. |
| N-12 | **TTL / lease marker in the listing** | Expiry is invisible until you focus a key one at a time. v2 `ls` already returns each child's `TTL`, and v3 `ls` already carries the `lease` id out of the keys-only response and then throws it away (`childInfo.lease` → `Node.LeaseID` → unused by `updateList`). So a "⏳" marker on leased rows costs one line in the row renderer, with the exact remaining time still coming from the on-focus read. |
| N-13 | **Diff any two keys** | `diffLines`/`renderDiff` are pure `(string, string) → ops → text` helpers sitting in `history.go`. Point them at two keys (mark one with `Space`, diff against the focused one) or at a key vs. a local file, and the whole feature is a picker plus a title. Also the engine F-15 (directory diff) would reuse per key. |
| ~~N-14~~ | **Protected prefixes** — shipped in 0.9.0 | Config lists prefixes that are read-only or need a typed confirmation — `/registry/` on a Kubernetes cluster being the obvious one. Complements F-4's blanket read-only mode with something usable day to day: browse freely, but the paths that would take the cluster down need deliberate effort. |
| N-15 | **Kubernetes value decoding** | The single biggest real-world etcd is a Kubernetes cluster, where every value under `/registry/` is a protobuf envelope starting with the magic `k8s\x00`. Detect that prefix and at minimum name the resource from its `TypeMeta` instead of showing a hex dump; full decoding could follow. Turns "unreadable binary" into the most useful screen in the tool for the most common deployment. |
| N-16 | **Bulk find-and-replace under a prefix** | `Search` already walks a subtree with a match predicate; add a replacement plus a dry-run preview listing every key that would change (reusing N-13's diff view), then apply. Pairs with N-6's journal so the change is reviewable afterwards, and with F-9 so it survives large trees. |

## Shipped

| Id | Feature | Version |
|----|---------|---------|
| F-19 | **Time travel** — `Ctrl+G` pins a pane to a past etcd revision; every read (listing, details, find, export) carries it, mutations are refused, and compaction is reported rather than shown as an empty tree. Absolute, relative (`-N`) or empty for live. DESIGN §14.1. | 0.10.0 |
| N-25 | **Undo snapshots** — a recursive directory delete exports the subtree to `$XDG_STATE_HOME/etcd-walker/snapshots` first; `Ctrl+U` restores or discards. A snapshot that cannot be written cancels the delete (`-no-snapshot` opts out); binary values survive byte for byte. DESIGN §14.2. | 0.10.0 |
| F-20 | **Two panes** — `Ctrl+B`/`-dual`, `Tab` to switch, `F5`/`F6` to copy or move into the other pane. Each pane keeps its own directory, cursor and revision. DESIGN §14.3. | 0.10.0 |
| F-4 | **Read-only sessions** — `-read-only` / `"read_only"`. Mutating bindings refused up front, `[READ-ONLY]` in a yellow header, value editor opens disabled so values stay readable. DESIGN §13. | 0.9.0 |
| N-11 | **Visible stale state** — a failed on-focus re-read is reported in the details pane and greys the row with a `(cached)` tag, instead of silently leaving the listing's node in place. DESIGN §13. | 0.9.0 |
| N-14 | **Protected prefixes** — `-protect` / `"protected_prefixes"`. Writes on or under a protected path need the prefix basename typed out; enforced by one `guarded` funnel every mutation passes through, so bulk imports and revision-restores are covered too. DESIGN §13. | 0.9.0 |
| F-5 | **Type-to-confirm recursive deletes** — a directory delete demands its own name typed out; a protected path supersedes the word so one action never asks twice. DESIGN §13.5. | 0.9.0 |
| N-6 | **Session journal** — `Ctrl+A`: every mutation recorded by a decorator around `modelAPI` (exact args, only on success), exportable as an etcdctl script or to the clipboard. Unreproducible ops are comments, never plausible-but-wrong commands. DESIGN §13.4. | 0.9.0 |
| N-21 | **Dry-run** — `-dry-run` / `"dry_run"`. Falls out of the journal decorator: record and skip. DESIGN §13.4. | 0.9.0 |
| F-7 | **Revision history viewer (v3)** — `Ctrl+V`: revision picker (`Get`+`WithRev` walk, compaction-aware), detail view, LCS line-diff vs current, restore via `SetKeepTTL` (lease/TTL preserved). DESIGN §5.6/§7.5. | 0.8.0 |

## Non-goals (for now)

- Full etcdctl parity (compaction, defrag, move-leader) — keep the tool a
  *browser* with guardrails, not a cluster surgeon.
- Windows clipboard/terminal quirks beyond what `atotto/clipboard` + OSC52
  already cover.
- A mouse-driven UI: keyboard-first is the point.

## 6. From the 0.9.0 safety slice (2026-08-14)

Shipping F-4/N-11/N-14 built machinery that makes several existing items
cheaper, and suggested new ones.

**Two items just got much cheaper.** F-5 (type-to-confirm recursive deletes)
is now a call to N-14's existing confirmation form — the dialog, the
typed-word matching and the cancel path all exist. N-6 (session journal /
copy-as-etcdctl) no longer means instrumenting nine handlers: every mutation
already passes through the single `guarded` funnel carrying a structured
`action` and `paths`, which is exactly the hook a journal needs.

| Id | Feature | Sketch |
|----|---------|--------|
| N-17 | **Derive the policy from server RBAC** | On an auth-enabled cluster, `Auth.UserGet` gives the user's roles and `Auth.RoleGet` gives each role's permissions as key/range_end pairs with READ/WRITE/READWRITE. Turn those into a `Policy` automatically: a user with no write permission gets a read-only session without passing `-read-only`, and ranges they cannot write become protected instead of failing opaquely at write time. Makes F-4 automatic and honest — the header would state what the *server* will allow, not what the operator remembered to type. |
| N-18 | **Recently-modified view** | etcd's revision is global and monotonic, so "what changed most recently, anywhere under this prefix" is one range read: `WithSort(SortByModRevision, SortDescend)` + `WithLimit(N)` (the client even ships `WithLastRev()`). Nothing in a filesystem-shaped browser exposes this, and it is the first question during an incident. Cheap and high value. |
| N-19 | **Attribute filters** | Search is substring-only, but `Node` already carries TTL, LeaseID, CreateRev/ModRev/Version and value size. A tiny predicate language — `ttl<60`, `size>1k`, `leased`, `rev>12345`, `empty` — over fields already fetched. Composes with N-8's sorting and N-18. |
| N-20 | **Redact sensitive values** | A `redact_prefixes` list (or reuse of the protected list) masks values in the details pane, previews and clipboard, showing length + SHA-256 only, until explicitly revealed with a keystroke. The natural companion to N-14 for demos, screen-shares and shoulder-surfing, and it fits the direction 0.9.0 took. |
| ~~N-21~~ | **Dry-run mode** — shipped in 0.9.0 | `-dry-run` makes `guarded` log what each mutation *would* do — action plus resolved paths, both of which it already receives — and skip it. With N-6 the session then emits an `etcdctl` script: rehearse against production, review the diff, run it deliberately. Only reachable this cheaply because every write already funnels through one place. |
| N-22 | **Value size guard** | etcd refuses requests over `--max-request-bytes` (1.5 MiB by default) and the failure surfaces as an opaque gRPC error at save time. Warn in the editor and details pane as a value approaches the limit, and refuse before sending. Server limit is discoverable, or configurable locally. |
| N-23 | **Prefix baseline / "what changed since"** | Snapshot a prefix (the `export` walk already exists), then later show added / removed / changed keys against that baseline, drilling into per-key diffs via N-13. A poor-man's audit trail for clusters with no etcd audit log — distinct from F-1 (live refresh) and F-15 (compare two prefixes). |
| N-24 | **Lease expiry warnings** | Once N-12 shows a TTL marker, flag keys whose lease expires within a configurable window. An expiring service-registration key is an incident in progress; the tool already has the lease id in every listing. |
