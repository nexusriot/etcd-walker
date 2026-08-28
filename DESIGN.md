# Etcd-walker — Design & Architecture

This document describes how `etcd-walker` is put together: what each
package does, how they cooperate at runtime, and the rationale behind the
main design choices. It is aimed at contributors and at users who want to
understand the tool deeply enough to extend it.

For end-user documentation (features, configuration, hotkeys) see
[README.md](README.md).

---

## 1. High-level overview

`etcd-walker` is a single-binary, terminal-UI application written in Go
that lets a human operator browse and edit an etcd key-value store as if
it were a hierarchical filesystem. It supports both etcd v2 (HTTP) and
etcd v3 (gRPC) clusters behind a single uniform UI.

Internally the program follows a classic **MVC** decomposition:

```
                  ┌──────────────┐
   keyboard ──►   │  Controller  │   ◄── tview events
                  └──────┬───────┘
                         │
            ┌────────────┴────────────┐
            ▼                         ▼
      ┌──────────┐               ┌─────────┐
      │  Model   │ ── etcd ─►    │  View   │ ── tview/tcell ─► terminal
      └──────────┘               └─────────┘
```

* **Model** — talks to etcd through a `backend` interface that has two
  implementations (`v2Backend`, `v3Backend`).
* **View** — owns the `tview` application, the list/details panes and the
  modal dialogs. It exposes plain `tview` widgets; it does not know
  anything about etcd.
* **Controller** — holds navigation state (current directory, cursor
  positions, injected nodes), wires keyboard events to model operations
  and refreshes the view.

Cross-cutting concerns live in small support packages: configuration
loading (`pkg/config`) and clipboard / OSC52 handling
(`pkg/util/clip`).

---

## 2. Repository layout

```
etcd-walker/
├── cmd/etcd-walker/
│   ├── main.go              entry point: flags + config → Controller
│   └── main_test.go         flag-wrapper semantics
│
├── pkg/
│   ├── config/              JSON config loader
│   │   ├── config.go
│   │   └── config_test.go
│   ├── model/               etcd abstraction (v2 / v3 behind one iface)
│   │   ├── model.go
│   │   ├── v3backend_test.go   in-memory fakeKV exercises the v3 backend
│   │   ├── v2backend_test.go   fake KeysAPI exercises the v2 backend
│   │   ├── model_test.go       Model→backend delegation wiring
│   │   ├── integration_test.go `integration`-tagged, needs a real etcd
│   │   └── helpers_test.go
│   ├── controller/          UI state + keybindings + business glue
│   │   ├── controller.go
│   │   ├── history.go          revision-history viewer (picker/detail/diff/restore)
│   │   ├── journal.go          session journal + dry-run model decorator
│   │   ├── timetravel.go       revision picker + pinning a pane to the past
│   │   ├── panes.go            two-pane state swap + cross-pane copy/move
│   │   ├── snapshot.go         undo snapshots for recursive deletes
│   │   ├── controller_test.go  fakeModel + headless tview drive the flows
│   │   ├── history_test.go
│   │   ├── journal_test.go
│   │   ├── policy_test.go
│   │   ├── timetravel_test.go
│   │   ├── panes_test.go
│   │   ├── snapshot_test.go
│   │   ├── integration_test.go `integration`-tagged: delete/restore + history
│   │   ├── flows_test.go       dialog flows driven headlessly (create/dup/jump)
│   │   └── helpers_test.go
│   ├── view/                tview-based TUI rendering
│   │   ├── view.go
│   │   └── view_test.go        layout + focus (a lost focus kills every binding)
│   └── util/clip/           clipboard with OSC52 fallback
│       ├── clip.go
│       └── clip_test.go
│
├── .github/workflows/ci.yml  build/vet/gofmt/test/-race, cross-build, etcd
├── resources/               screenshots, icons
├── examples/                import-sample.json for the JSON import feature
├── DEBIAN/                  Debian packaging metadata
├── Makefile                 cross-build matrix + .deb targets (preferred)
├── build-deb.sh             builds an amd64/arm64 .deb (legacy helper)
├── build-deb-arm64.sh       thin wrapper: build-deb.sh arm64
├── dist/                    Makefile binary build output (git-ignored)
├── go.mod / go.sum
├── LICENSE
├── README.md
└── DESIGN.md                this document
```

The codebase is intentionally small and flat — one production file per
package, except `pkg/controller`, where self-contained flows are kept out of
the already-large `controller.go`: `history.go` (the revision-history
screens), `journal.go` (the session journal and its model decorator),
`timetravel.go`, `panes.go` and `snapshot.go` (§14). Every file has a sibling
`_test.go`, so newcomers can read it end-to-end in a single sitting.

---

## 3. Runtime startup flow

The boot sequence ([cmd/etcd-walker/main.go](cmd/etcd-walker/main.go)) is:

1. `registerFlags(flag.CommandLine)` declares every flag and returns them
   bundled as a `cliFlags`. The typed wrappers (`stringFlag`, `boolFlag`)
   record whether each flag was explicitly given, which is what makes "CLI
   overrides config" precise per field; `boolFlag.IsBoolFlag` is what lets
   `-read-only` be written bare. Registration and bundling are one function
   on purpose — see B-17 in §12.
2. Parse `flag.Parse()`.
3. Seed hard-coded defaults (host `127.0.0.1`, port `2379`,
   protocol `auto`, timeout `5s`).
4. Call `config.Load(path)` to read the JSON file. Missing file is **not**
   an error; `Load` returns `(nil, nil)` so the program just keeps the
   defaults.
5. Hand the config and the flag bundle to `resolve()`, which applies the
   config-file values and then overlays any explicitly-set CLI flag,
   returning the `model.Options` (how to reach the cluster) and the
   `controller.Policy` (what this session may do to it — see §13).
6. `controller.NewController(opts, policy)`.
7. `ctrl.Run()` enters the tview main loop and blocks until the user
   quits.

This three-tier precedence (defaults → file → flags) is applied
field-by-field, so a partial config (e.g. only `tls_enabled`) combines
cleanly with a partial set of CLI flags. It lives in `resolve()` rather than
inline in `main()` precisely so it can be tested: ~20 fields each needing
"config unless the flag was set" is exactly the shape where a copy-paste
lands a value in the wrong field and nothing complains (§10a).

---

## 4. Package: `pkg/config`

[pkg/config/config.go](pkg/config/config.go) defines:

* `DefaultPath = "/etc/etcd-walker/config.json"` — the system-wide
  location.
* `type Config` — the JSON schema. All fields are tagged with `json:"…"`
  so the file uses snake_case (`tls_enabled`, `timeout_seconds`) while the
  Go code uses Go-idiomatic CamelCase.
* `Load(path string) (*Config, error)` — `Stat`s the file, returns
  `(nil, nil)` for `ENOENT`, refuses directories, and otherwise
  `json.Unmarshal`s the bytes.

Design notes:

* The package has **no dependencies** beyond the standard library — it
  can be imported from anywhere without dragging tview/tcell with it.
* The "missing file is not an error" contract is what allows `etcd-walker`
  to run as a self-contained binary in a fresh container with just CLI
  flags.

---

## 5. Package: `pkg/model`

This is the only package that imports the etcd client libraries. It hides
the v2/v3 split behind a single `Model` type so the controller never has
to branch on protocol.

### 5.1 The `backend` interface

```go
type backend interface {
    proto() string
    probe() error
    ls(dir string, rev int64) ([]*Node, error)
    get(path string, rev int64) (*Node, error)
    revision() (int64, error)
    set(path, value string) error
    setTTL(path, value string, ttlSeconds int64) error
    setKeep(path, value string, leaseID, ttlSeconds int64) error
    mkdir(path string) error
    del(path string) error
    deldir(path string) error
    renameDir(oldPath, newPath string) error
    renameKey(oldPath, newPath string) error
    copyKey(src, dst string) error
    copyDir(srcDir, dstDir string) error
    search(dir, query string, inValues bool, limit int, rev int64) ([]*Node, bool, error)
    history(key string, limit int) ([]*Revision, bool, error)
    authStatus() (enabled bool, known bool, err error)
    export(dir string, rev int64) (map[string]string, error)
}
```

Every read carries a revision (`0` = as of now) rather than coming in live
and historical variants: with two spellings, a caller browsing the past
could reach live data by picking the wrong method name, and the result
would look plausible. See §14.1.

Two implementations satisfy it:

* `v3Backend` — wraps `go.etcd.io/etcd/client/v3`. It holds three handles: a
  `clientv3.KV` (get/put/delete/range), a `lessor` (a three-method narrowing
  of `clientv3.Lease` — `Grant`/`Revoke`/`TimeToLive` — that
  `*clientv3.Client` satisfies in production and a `fakeLessor` satisfies in
  tests), and the full `*clientv3.Client` for `Auth.AuthStatus` and `Close`.
  Splitting the lease methods out is what made the B-1 fix testable (§12).
  Speaks gRPC, supports auth and TLS. `close()` releases the client;
  `NewModel` calls it on any path that abandons a dialled backend, so a
  failed probe does not leave a gRPC reconnect loop running.
* `v2Backend` — wraps `github.com/coreos/etcd/client` (`clientv2.KeysAPI`),
  speaks HTTP. Username/password are passed through as HTTP basic auth;
  the TLS knobs are ignored (the endpoint is always `http://`).

Most exported `Model` methods are 1:1 wrappers over the interface. A few are
worth calling out:

* `Model.Import(items, overwrite)` — the one operation **not** backed by a
  single interface method: a key/value bulk write implemented in terms of
  `get`/`set`. Keys are normalized; the root path and the reserved `.dir`
  marker are counted as skipped rather than written (the same guard
  `create()` applies, and symmetric with `export`, which omits markers), and
  in skip mode existing keys are left alone.
* `Model.SetTTL(key, value, ttlSeconds)` — pass-through to `setTTL`; sets or
  clears a key's expiry (see §5.5).
* `Model.SetKeepTTL(key, value, leaseID, ttlSeconds)` — pass-through to
  `setKeep`; writes a new value while keeping the key's existing expiry
  (see §5.5).
* `Model.Search(dir, query, inValues, limit)` — recursive, case-insensitive
  substring search over full key paths (and values when `inValues`); returns
  at most `limit` nodes plus a truncation flag. On v3 the sweep is keys-only
  unless values are being searched.
* `Model.History(key, limit)` — the stored versions of a key, newest first
  (v3 only; see §5.6). Returns `[]*Revision` plus a limit-truncation flag.
* `Model.CopyKey` / `Model.CopyDir` — duplicate a key or a whole subtree,
  carrying leases (v3) / TTLs (v2) along; overlapping source/target is
  rejected by `copyDirGuard`. Renames reuse the same copy machinery
  (`copyPrefix` on v3, `copyTree` on v2) and add the delete.
* `probe()` — cheap reachability/auth check used by `NewModel`: a keys-only
  `limit=1` range on v3, a non-recursive root listing on v2 — so startup
  never pulls the whole keyspace.

`authStatus()` returns a `(enabled, known, err)` triple rather than a string
so the controller can distinguish "auth is off" from "couldn't determine"
and render the header label (`ON` / `OFF` / `?`) accordingly. Note that
today only the explicit-`v3` arm of `NewModel` actually calls it — in
`auto` and `v2` modes `authLabel` is never populated and the header shows
`?` (and `v2Backend.authStatus`, though implemented and tested, is dead in
production). Fixing this is on the roadmap (F-2).

### 5.2 Protocol selection

`model.NewModel(opts)` honours `opts.Protocol`:

* `"v3"` — build a v3 backend; surface auth errors with a hint to set
  credentials.
* `"v2"` — build a v2 backend.
* `"auto"` (default) — try v3 first, and fall back to v2 if the v3 probe
  fails. The chosen protocol is exposed via `Model.ProtocolVersion()` so
  the controller can show it in the header.

All three arms validate connectivity with the backend's `probe()` (§5.1)
before returning, so a bad endpoint fails fast at startup instead of on the
first listing.

### 5.3 Synthetic directories on v3

etcd v3 has no native concept of a directory: keys are flat. To present a
hierarchy `etcd-walker` adopts two conventions:

* When listing under `prefix/`, any key whose remaining path contains a
  `/` is rendered as a **synthetic** subdirectory.
* `mkdir(p)` writes a marker key `p/.dir` so that an "empty directory"
  can exist; the controller filters `.dir` markers out of the user-facing
  listing and `export()`/`search()` skip them too. `model.IsReservedName`
  lets the controller refuse creating a key literally named `.dir`, which
  would otherwise be invisible and make its parent a phantom directory.

This keeps the v2 and v3 user experiences indistinguishable.

`ls` on v3 is **keys-only** (`clientv3.WithKeysOnly`): a listing needs names
to build the level, and the details pane re-reads the focused key anyway
(§7.3), so values never cross the wire for browsing. `Node.Value` is empty
in listings by contract.

### 5.4 TLS and timeouts

For v3, `Options.TLSEnabled`, `TLSCAFile`, `TLSCertFile`, `TLSKeyFile`
and `TLSSkipVerify` are folded into a `*tls.Config` and attached to the
gRPC dial options. `TimeoutSeconds` (default 5) becomes the
`DialTimeout` and the per-request `context.WithTimeout` budget so a
broken server cannot hang the UI. Whole-subtree operations get stretched
budgets: directory rename/copy use `4×` (min 20 s, `treeTimeout`), and
export/search use `10×` (min 30 s).

### 5.5 TTL / expiry

`Node` carries two expiry fields: `TTL int64` — the remaining time-to-live in
seconds, where `0` means "no expiry" — and `LeaseID int64` — the v3 lease
currently attached to the key (`0` = none; always `0` on v2). It also carries
revision metadata for the details pane: `CreateRev`/`ModRev` (v3 revisions,
or the v2 created/modified indexes) and `Version` (the v3 per-key counter,
always `0` on v2). The two backends source the expiry fields differently
because etcd implements expiry differently across the protocol versions:

* **v3** has no per-key TTL; expiry is modelled with **leases**. To set a
  TTL, `setTTL` reads the key's *current* lease, grants a fresh one with
  `Lease.Grant(ctx, seconds)`, `Put`s the key with `clientv3.WithLease(id)`,
  and finally hands the superseded lease to `revokeIfOrphaned` so it does not
  linger as an orphan that keeps ticking server-side. A `ttlSeconds <= 0`
  instead does a plain `Put` (detaching any lease, making the key permanent)
  and then reaps the old lease the same way.
  When reading, `get` sees the lease id on the `KeyValue` and resolves the
  live countdown through `Lease.TimeToLive` (helper `ttlForLease`, tolerant:
  returns `0` when the lease id is `0`, the lease has expired, or `b.lessor`
  is `nil` — the unit tests wire up only the fake `KV`). `ls` deliberately does
  **not** resolve the countdown: the list pane never renders TTL and the
  focused key is re-read anyway (§7.3), so paying one `TimeToLive` round-trip
  per leased child would be wasted work. `ls` carries only the cheap
  `LeaseID` from the range response.
* **v2** has native per-key TTL. `setTTL` passes a
  `clientv2.SetOptions{TTL: …}` (zero duration clears it), and `get`/`ls`
  copy `Node.TTL` straight from the v2 response; `LeaseID` stays `0`.

A separate `setKeep` (exposed as `Model.SetKeepTTL`) writes a new value while
**preserving** the existing expiry: v3 re-`Put`s with `WithLease(LeaseID)` so
the running countdown continues unbroken (plain `Put` when `LeaseID == 0`), and
v2 re-applies the remaining `TTL`. This is what the value editor uses — see
§7.4.

Renames and copies preserve expiry the same way: the v3 copy loop re-attaches
each key's lease and the v2 copy re-applies each node's remaining TTL, so
moving or duplicating an expiring key never silently makes it permanent.

**Sharp edge — shared leases.** Re-attaching the *same* lease means a v3 copy
shares one lease with its source, and applications outside this tool routinely
park many keys on one lease. Revoking a shared lease deletes every key attached
to it, so `revokeIfOrphaned` resolves the lease with
`Lease.TimeToLive(..., WithAttachedKeys())` and revokes only when nothing else
is still attached — the edited key was re-pointed by the preceding `Put`, so an
orphan reports an empty key set, and the key itself is tolerated in case of a
stale read. Any uncertainty (no lessor, a failed lookup, a lease already
reporting `TTL < 0`) leaves the lease alone: a leaked lease expires by itself,
a wrongly revoked one takes data with it. This was finding B-1 (§12).

Because the v3 TTL is a *countdown*, it changes every second on the server.
The controller therefore re-reads the focused key on demand rather than
trusting the value cached at list time — see §7.3.

### 5.6 Revision history (v3)

etcd v3's MVCC store retains every version of a key until compaction, and
`history(key, limit)` surfaces that as `[]*Revision{Rev, Version, Value}`,
newest first. Starting from the current `KeyValue`, the walk repeatedly
issues `Get(key, WithRev(ModRevision-1))` — the state of the key one write
earlier — until it reaches the creation (`Version == 1`), collects `limit`
entries (reported via the returned bool), or hits compacted history
(`rpctypes.ErrCompacted`, matched tolerantly by `isCompactedErr`), which
ends the walk *gracefully*: compaction is the natural end of retrievable
history, not an error. Each step is one round-trip, so the walk runs under
the stretched 10× timeout like export/search.

The caller can tell why the list ends where it does: the bool means the
limit was hit; otherwise an oldest entry with `Version > 1` means the rest
was compacted, and `Version == 1` means the full history is present. The
walk covers the key's current incarnation only — a deleted-and-recreated
key starts a fresh history (finding older incarnations would mean scanning
revisions for the delete, far too expensive for a browsing hotkey).

v2 keeps a per-key `modifiedIndex` but no previous values, so its `history`
returns a descriptive "requires etcd v3" error which the controller
surfaces as-is.

---

## 6. Package: `pkg/view`

[pkg/view/view.go](pkg/view/view.go) is a thin wrapper around
[`rivo/tview`](https://github.com/rivo/tview). It owns:

* `App` — the `tview.Application`.
* `Frame` — the outer chrome with the status header.
* `Pages` — a stack of overlay pages used for modal dialogs.
* `Lists` / `List` — the two browser panes and a pointer to the focused one
  (§14.3).
* `Details` — metadata about the highlighted node.
* `ModalEdit` / `ModalScroll` — the two centering helpers. `ModalEdit` takes a
  fixed width and height; `ModalScroll` takes a width and lets the height
  follow the screen, for panels whose content can outgrow the terminal.
* Constructors for the dialogs (create, edit, rename, set-TTL,
  delete-confirm, typed-confirm, file-overwrite prompt, search,
  recursive-find form, results picker — reused by search results, the
  revision picker and the journal viewer — jump, export, JSON import
  file-browser, import-mode prompt, revision detail/diff pane,
  restore-confirm, multi-line editor, hotkeys help).

Key design decisions:

* The view is **passive**. It exposes widgets and helpers but never calls
  the model. The controller installs all input captures and `done`
  handlers.
* Dialogs are added to `Pages` and removed when dismissed, so any
  background list state survives a modal interaction. All dialogs share the
  page name `"modal"`, so a handler must remove the current dialog **before**
  showing a follow-up (e.g. an error) — a deferred remove would delete the
  follow-up instead.
* **A modal that swallows every key cannot scroll.** The hotkey panel closed
  on *any* keystroke, which was implemented as an input capture returning
  `nil` for everything — so the arrows, `PgUp`/`PgDn` and `Home`/`End` never
  reached the `TextView`'s own handler and its content below the fold was
  unreachable. The capture now intercepts only `Esc`/`q`/`Enter`/`Ctrl+H` and
  returns every other event. The panel was *also* a fixed 30 rows on a
  24-row terminal, where scrolling would not have helped either: the rows
  past the screen edge are never drawn, so `ModalScroll` sizes it to the
  screen. `TestHotkeysPanelFitsAShortTerminal` renders it on a simulation
  screen at 80x24 and asserts the frame closes.
* The full-screen multi-line editor swaps the application root
  (`OpenEditor`/`CloseEditor`), hiding the frame and hotkey legend so it can
  use the entire terminal; it is not a `Pages` modal. A consequence: no
  dialog can be shown over it, which is why the value editor closes before
  its policy guard runs (§13.2).
* Anything user-supplied that reaches a title or a `TextView` goes through
  `tview.Escape`. tview's colour-tag parser matches `[a-zA-Z]+` inside
  brackets and consumes it, in `Box` titles as much as in body text, so an
  unescaped `[d]` hotkey hint or a key name containing brackets simply
  disappears — see B-13 in §12.

---

## 7. Package: `pkg/controller`

[pkg/controller/controller.go](pkg/controller/controller.go) is the
biggest file in the project — it is where the application's behaviour
lives.

### 7.1 State

```go
type Controller struct {
    view     *view.View
    model    modelAPI                    // journalling wrapper in production
    policy   Policy                      // read-only / dry-run / protected (§13)
    journal  *Journal                    // what this session changed (§13.4)
    injected map[string]map[string]*model.Node
    endpoint string                      // host:port, recorded in snapshots (§14.2)

    paneState        // the ACTIVE pane, embedded (§14.3)
    inactive paneState
    dual     bool
    active   int

    startupErr error
}

type paneState struct {
    currentDir   string
    currentNodes map[string]*Node  // mapKey → Node
    position     map[string]int    // dir path → cursor index
    ordered      []string          // display names, list order
    lastGoodDir  string            // fallback for failed listings
    rev          int64             // pinned revision, 0 = live (§14.1)
}

type Node struct {
    node  *model.Node
    stale bool  // last on-focus refresh failed; displayed data is cached (§13.3)
}
```

* `model` — an unexported `modelAPI` interface rather than the concrete
  `*model.Model`. In production it holds a `journaling` decorator wrapping the
  real model (§13.4); tests drive the controller with an in-memory fake (see
  `controller_test.go`, which pairs the fake with headless tview widgets).
* `policy` and `journal` — the session's safety configuration and the record
  of what it changed. Both are described in §13, which is where the read-only,
  protected-prefix, dry-run and journal features are documented together.
* `currentDir` — the path the user is currently looking at.
* `currentNodes` — keyed by `mapKey` (`"<base>|dir"` or `"<base>|file"`)
  so a key and a directory with the same basename can coexist.
* `position` — remembers per-directory cursor positions, so coming back
  up from a child puts the cursor back where you left it.
* `injected` — local-only nodes that the controller forces into a
  listing. This handles two edge cases:
  * keys whose name starts with `_` that some etcd v2 listings hide;
  * just-created keys/dirs that should appear immediately even if the
    listing happens to lag.
* `ordered` — the display names of the current list rows (below `[..]`) in
  the exact order `updateList` built them; `search()` resolves the selected
  name back to a row index through it, guaranteeing the two never disagree.
* `lastGoodDir` — the most recent directory that listed successfully; a
  failed listing reports the error and falls back here instead of aborting
  the session (§9.2).
* `rev` — the revision this pane reads at; `0` means the live cluster. Every
  read goes through `paneLs`/`paneGet`, which apply it (§14.1).
* `paneState` is **embedded** so all of the above stay addressable as
  `c.currentDir`, `c.currentNodes` and so on: the controller always acts on
  the active pane, and the second pane is parked in `inactive` (§14.3).
* `startupErr` — captured from `model.NewModel`; instead of crashing the
  process the controller renders the error inside the TUI on
  `Run()`, so a bad config produces a friendly screen rather than a stack
  trace.

### 7.2 Event flow

`setInput()` installs two `SetInputCapture` callbacks:

* On the global `App`: `Ctrl+Q` quits.
* On the `List` widget: every other hotkey (`Ctrl+N`, `Ctrl+D`, `Delete`,
  `Ctrl+E`, `Ctrl+R`, `Ctrl+T`, `Ctrl+V`, `Ctrl+P`, `Ctrl+Y`, `Ctrl+S`, `/`,
  `Ctrl+F`, `Ctrl+J`, `Ctrl+W`, `Ctrl+O`, `Ctrl+A`, `Ctrl+H`, `Backspace`).

  This handler starts by consulting `mutatingKeys`: in a read-only session the
  bindings that change state are refused here, so the dialog never opens
  (§13.2). It is a convenience, not the enforcement point — that is `guarded`.

Each hotkey calls a small method (`create`, `duplicate`, `delete`,
`editMultiline`, `rename`, `setTTL`, `history`, `copyPath`, `copyValue`,
`search`, `findRecursive`, `jump`, `export`, `importJSON`, `showJournal`)
which opens the appropriate dialog and, on submission, calls into the model
and then `updateList()` to refresh the listing. `findRecursive` chains two
dialogs:
the query form, then a results picker whose selection hands the node to
`navigateTo` — the shared helper (also used by `jump`) that enters a
directory or selects a key in its parent. `history` chains up to four
screens (§7.5).

### 7.3 Listing rendering

`updateList()` is the central refresh routine:

1. `makeNodeMap()` — `model.Ls(currentDir)` merged with
   `injected[currentDir]` (the server listing wins on collisions). On error
   the refresh is **non-fatal**: an error modal is shown and the controller
   falls back to `lastGoodDir` (§9.2); on success `lastGoodDir` advances.
2. Sort via `orderedEntries`: directories first, then keys, each
   alphabetical **by basename** — sorting the raw `"<base>|dir"` map keys
   would order `app2/` before `app/` because `|` sorts after alphanumerics.
3. Push `tview.ListItem`s into the view, labelled by `rowLabel`: a folder or
   key glyph, yellow for underscore-prefixed names, or grey and `(cached)`
   when the node is stale (§13.3). Staleness wins over the underscore colour —
   it is the more urgent signal, and nesting the two tags would leave the
   `[-]` reset mismatched.
4. Restore the cursor from `position[currentDir]`.
5. Cache the display ordering in `c.ordered` for `search()`.

The selection handler that calls `fillDetails()` on every highlight move is
installed once in `Run()` via `List.SetChangedFunc`.

`fillDetails()` renders the right-hand pane for the highlighted node:
path info (type, basename, parent, full path, depth), cluster info
(protocol, cluster id) and, for keys, value info — byte size, line count,
SHA-256, the **TTL** (`1h2m3s (3723s)` via `formatTTL`, or `none`) and
revision info (create/mod revision, plus the per-key version on v3). The
preview adapts to the value: JSON objects/arrays are re-indented
(`prettyJSON`), printable text gets a 512-char excerpt, and binary /
non-UTF8 values get an xxd-style hex dump of the first 256 bytes
(`hexDump`). All preview text passes through `tview.Escape` so `[…]`
sequences in values are not eaten as color tags. For directories it instead
does a live `Ls` to report child/subdir/key counts.

Because v3 listings are keys-only (§5.3), this on-focus re-read is also
what populates the value in the details pane and in the edit dialogs.

Because a v3 lease TTL is a live countdown (§5.5), `fillDetails()` re-reads
the focused **key** from the server with `model.Get` on every selection
change, replacing the node cached at list time so the TTL and value stay
current as the user moves the cursor on and off the key. When that read
fails — or the path is no longer a key — it falls back to the cached node
**and says so**: the entry is marked stale, the pane leads with a red banner
naming the error, and the row is re-rendered greyed (§13.3). That visibility
matters because a cached v3 node has no value at all; the silent version of
this fallback is what made B-7 destructive. Directories keep using the cached
node plus the live child count.

### 7.4 Mutations

Every mutating action (`create`, `duplicate`, `delete`, `rename`,
`editMultiline`, `setTTL`, `importJSON`, and `restoreRevision` in
`history.go`) follows the same pattern:

1. Open a dialog from `view`.
2. On `Enter`, remove the dialog page first (§6), then validate input.
3. Hand the actual mutation to `c.guarded(guard{…})` rather than calling the
   model directly. That single funnel applies the session policy — refuse in
   read-only, demand a typed confirmation for protected prefixes or recursive
   deletes — and is what makes the guarantee checkable; a check bolted onto
   the key bindings alone would miss `restoreRevision`, which never touches
   them. See §13.2.
4. On error, surface a red modal with the error text — never crash.
5. On success, update the `injected` cache (so the new state is visible
   even if the server lags), then call `updateList()`.

Two mutating paths re-read the key from the server before acting rather than
trusting the node cached in `currentNodes`: `editMultiline` (before opening
the editor) and `setTTL` (inside its Save handler, immediately before the
write). Both rewrite the key's value, and a cached v3 node's value is empty
until `fillDetails` fetches one — see B-7 in §12.

The mutation itself lands on the `journaling` decorator, not the raw model, so
it is recorded for the session journal and skipped entirely under dry-run
(§13.4).

`create` refuses to overwrite an existing key/directory and rejects the
reserved `.dir` name (§5.3); `rename` and `duplicate` apply the same
target-exists guard.

Renames are implemented as **copy-then-delete** in the model so they work
identically on v2 and v3 even though v3 has no native rename; leases/TTLs
travel with the copied keys (§5.5). `duplicate` (`Ctrl+D`) is the same copy
step without the delete: it prompts for a target path (absolute or relative
to the current directory, prefilled `<source>-copy`) and works for single
keys and whole subtrees.

`setTTL` is keys-only (directories are rejected): the dialog accepts either
a bare number of seconds or a Go duration string (`1h30m`, `90m`, `45s`),
parsed by `parseTTLInput` (`0`/empty clears expiry). It re-writes the key
with its current value via `model.SetTTL`, then refreshes the listing and
details.

A later value edit (`Ctrl+E`) goes through `model.SetKeepTTL`, which
re-attaches the key's lease (v3) or re-applies its TTL (v2), so editing a
value **keeps** the expiry instead of silently dropping it. The controller
passes the `LeaseID`/`TTL` from the focused node, which `fillDetails` keeps
fresh by re-reading the key on focus (§7.3).

### 7.5 Revision history viewer

`Ctrl+V` on a key (directories are refused — they are synthetic and have no
revisions) opens the history flow, implemented in
[pkg/controller/history.go](pkg/controller/history.go) as a chain of
screens that all share the `"modal"` page name and follow the
remove-before-add rule (§6):

1. **Picker** — one row per stored revision: revision number, per-key
   version, size, first-line preview (`revPreview`: escaped, trimmed,
   `(binary)`/`(empty)` placeholders); the newest row is bold-marked
   `(current)`. The title says when the list was cut by `historyLimit`
   (100) or by compaction (§5.6). When only one version is stored the
   picker is skipped in favour of an info modal that says *why* there is
   nothing to browse (single version vs. compacted history).
2. **Detail** — revision metadata (revision, version, size/lines, SHA-256,
   reusing the details-pane helpers) plus the full value: pretty-printed
   when JSON, hex-dumped when binary. `d` opens the diff, `r` the restore
   confirmation (offered only for non-current revisions), `Esc` returns to
   the picker.
3. **Diff** — a line-based LCS diff (`diffLines`) of the selected revision
   against the current value, rendered by `renderDiff` with `-`/`+`
   coloring and long unchanged runs collapsed to `··· N unchanged lines
   ···` around 3 lines of context. Binary values and values whose line
   product exceeds `maxDiffCells` (the DP table bound) fall back to a plain
   notice instead of a diff. The diff compares against `revs[0]` — the
   current value as of the walk — so the whole flow reasons about one
   consistent snapshot.
4. **Restore confirm** — cancel returns to the detail screen; confirm calls
   `restoreRevision`, which re-reads the key and writes the old value via
   `SetKeepTTL` with the key's *live* lease/TTL, honouring the same
   expiry-preservation contract as the value editor (§7.4). If the key
   vanished in the meantime the restore fails with an error modal instead
   of writing blindly. On success the listing refreshes with the cursor
   back on the key and the details pane re-rendered.

---

## 8. Package: `pkg/util/clip`

[pkg/util/clip/clip.go](pkg/util/clip/clip.go) provides one function:
`Copy(text string) error`. The implementation tries, in order:

1. `github.com/atotto/clipboard` — works on desktops with `xclip`,
   `xsel`, `wl-copy`, `pbcopy`, etc.
2. **OSC52** — emit the ANSI escape sequence `ESC ] 52 ; c ; <base64> BEL`
   so the *terminal emulator itself* puts the text on the system
   clipboard. This is what makes `Ctrl+P` / `Ctrl+Y` work over SSH.
3. If running inside `tmux`, the OSC52 sequence is wrapped in
   `tmux passthrough` (`ESC Ptmux; ESC <inner> ESC \\`) so it reaches the
   outer terminal.

The OSC52 path caps the payload at 10 000 bytes (`maxOSC52Len` — many
terminals limit the whole escape sequence). Oversized values fail cleanly
with `ErrTooLarge` instead of silently copying a truncated string; the
error message names both sizes so the user knows why.

Failures are non-fatal: the controller shows "Copied" / "Copy failed" in
a small modal and otherwise carries on.

---

## 9. Cross-cutting concerns

### 9.1 Logging

`logrus` is configured in `main.go` to write to `stderr`, with the level
raised to `Debug` when `-debug=true` (or `"debug": true` in the config)
is set. The TUI itself does not produce log output to stdout, so
`etcd-walker 2>/tmp/walker.log` is the canonical way to capture a debug
trace without disturbing the interface.

### 9.2 Error handling philosophy

* **Startup errors** (bad config, unreachable etcd) are stored on the
  controller and rendered inside the TUI on `Run()`, not printed to
  stderr.
* **Operational errors** (a failed `set`, a denied `del`) become red
  modal dialogs that the user dismisses and continues.
* **Listing failures** (e.g. a timeout while entering a directory) are
  also non-fatal: `updateList` reports the error and falls back to
  `lastGoodDir` — the last directory that listed successfully — showing an
  empty level only if even that fallback fails.
* The process only exits non-zero when `tview` itself fails or the user
  hits `Ctrl+Q`.

### 9.3 Versioning

The current user-facing version is **0.10.0**, duplicated in three places
that must be bumped together (`TestVersionIsConsistentAcrossTheRepo` fails if
they drift, and it has caught a partial bump):

* The header line in [pkg/controller/controller.go](pkg/controller/controller.go)
  — the `appVersion` constant the header string interpolates.
* `VERSION ?= 0.10.0` in the `Makefile` (overridable on the CLI:
  `make debs VERSION=…`).
* The `version=` variable at the top of `build-deb.sh` (the legacy helper;
  `build-deb-arm64.sh` just delegates to it).

---

## 10. Build & packaging

The `Makefile` is the primary entry point and encodes the full cross-build
matrix; the shell scripts remain as thin legacy helpers.

* `make` / `make help` — list every target with the resolved `VERSION`.
* `make x86_64` / `x86_64-static` / `linux-i686` / `freebsd-x86_64` /
  `uconsole` / `pizero2w` / `pizero2w-armhf` / `darwin` / `windows` —
  per-platform binaries written to `dist/` (`-trimpath -ldflags "-s -w"`,
  `CGO_ENABLED=0` for the static/cross targets). `make all` builds them all.
  The `uconsole` (ClockworkPi CM4) and `pizero2w` targets are both
  `linux/arm64`; `pizero2w-armhf` is `linux/arm GOARM=7` for 32-bit Pi OS.
* `make debs` — `.deb`s for `amd64`, `i386`, `arm64` and `armhf` under
  `build/`. Each `build_deb` target stages `usr/bin/<app>`, copies the
  `DEBIAN/` metadata, patches `_version_` and `Architecture:` in
  `DEBIAN/control`, and runs `dpkg-deb --build`. Override the version with
  `make debs VERSION=0.0.40`.
* `make test | test-race | vet | fmt | tidy | run | clean` — the usual
  developer shortcuts.

Equivalent raw commands, if you prefer not to use the Makefile:

* `go build ./cmd/etcd-walker` — normal dynamic build.
* `go build -ldflags "-linkmode external -extldflags -static" …` —
  static binary suitable for distroless / scratch containers.
* `GOOS=linux GOARCH=386 go build …` — 32-bit build.
* `./build-deb.sh [amd64|arm64]` — legacy `.deb` helper. It builds the
  binary statically when the host arch matches (`-linkmode external
  -extldflags -static`) and with `CGO_ENABLED=0 -ldflags "-s -w"` for
  cross-builds, then patches `DEBIAN/control` and runs `dpkg-deb`.
  `build-deb-arm64.sh` is a one-line wrapper that calls `build-deb.sh
  arm64`.

---

## 10a. Testing & CI

Three layers, all runnable from the Makefile:

| Layer | Command | What it covers |
|-------|---------|----------------|
| Unit | `make test` / `make test-race` | Everything below, against in-memory fakes. Hermetic — no network, no screen. |
| Coverage | `make cover` | Prints the total; `coverage.out` is git-ignored. |
| Integration | `make test-integration` | The `integration`-tagged suite in `pkg/model`, against a **real etcd**. |

**The fakes.** `pkg/model` drives the v3 backend through an in-memory
`clientv3.KV` that models revision history, compaction and per-key leases,
plus a `fakeLessor`; the v2 backend gets a fake `KeysAPI` that records the
`SetOptions` each call produced (which is where v2's TTL semantics live).
`pkg/controller` runs against a `fakeModel` and headless tview widgets.

**Driving dialogs without a screen.** The test `ModalEdit` is the identity
function, so `Pages.GetFrontPage()` hands back the widget itself; from there
`InputHandler()(tcell.NewEventKey(...), …)` fires `SetDoneFunc` on an input
and `GetButton(GetButtonIndex("Save"))` fires a form button. That is how
`create`/`duplicate`/`jump`/import validation are covered without a terminal.

**What is deliberately not unit-tested.** `pkg/view` is pure widget
construction (0%), and `main()`/`Run()` are wiring — the logic they used to
hide has been pulled out into `resolve()` and `headerText()`, which are
tested directly.

**Why an integration suite exists at all.** Lease sharing, MVCC revision
walks and prefix-range boundaries are etcd semantics, not ours; a fake can
only assert what we already believe. B-1 is the case in point — the shared-
lease test genuinely deletes the sibling key when the guard is removed, which
no fake proved. The suite writes everything under a per-test
`/etcd-walker-it/<test>-<nanos>` prefix and deletes it on cleanup, and reads
`ETCD_WALKER_TEST_ENDPOINT` / `_USER` / `_PASSWORD` from the environment.

**CI** ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs three jobs
on push and PR: `build` (gofmt check, vet with and without the `integration`
tag, **staticcheck**, build, test, `-race`, coverage artifact), `cross-build` (linux/amd64, linux/arm64, freebsd/amd64 — the
targets the README advertises), and `etcd-integration` (the tagged suite
against an `etcd:v3.5.15` service container). Every job takes its Go version
from `go.mod` via `go-version-file`, so CI cannot drift from the module.

---

## 11. Extending the project

Some natural places to extend:

* **A new backend** (e.g. Consul, ZooKeeper): implement the
  `model.backend` interface and add a switch arm in `model.NewModel`.
  Nothing in `controller` or `view` needs to change.
* **A new hotkey / dialog**: add a constructor in
  [pkg/view/view.go](pkg/view/view.go) and a handler method on
  `Controller`, then wire it in `setInput()`. If the handler *changes*
  anything, route the mutation through `c.guarded(guard{…})` rather than
  calling the model directly, and add the key to `mutatingKeys` so a
  read-only session refuses it before the dialog opens (§13.2).
* **More config fields**: add the field to `pkg/config/config.Config`
  (with a `json:"…"` tag) and thread it through `main.go` — into
  `model.Options` for anything about *reaching* the cluster, or into
  `controller.Policy` for anything about what the session may *do* to it.
* **A new model operation**: add it to the `backend` interface, implement
  it on both backends, and expose it on `Model`. If it mutates, it must
  also appear on `modelAPI`, be overridden in `journaling` (not merely
  promoted from the embedded interface) and be listed in
  `mutatingModelMethods` — otherwise it silently escapes the session
  journal and dry-run. Two tests enforce this; see §13.4.

The clean MVC split, the small surface of the `backend` interface, and the
single `guarded` funnel every mutation passes through are the design
constraints worth preserving as the project grows.

A prioritized feature backlog lives in [ROADMAP.md](ROADMAP.md).

---

## 12. Known limitations & open findings

Findings from the July 2026 review (B-1..B-6), the August 2026 deep review
(B-7..B-13, plus O-1..O-3, which that review first logged as behaviour changes
before they were taken on), and a follow-up review of the 0.9.0 safety work
(B-14..B-16) and of the test/CI round (B-17..B-18). All are fixed; the limitations below stand. Bugs carry a `B-`
id, limitations an `L-` id; the roadmap references them.

### Bugs / hazards — all fixed

| Id  | Where | Issue | Fixed |
|-----|-------|-------|-------|
| B-1 | `model.go` `v3Backend.setTTL` | The post-write `Revoke` of the key's previous lease assumed one-key-per-lease. `copyKey`/`copyDir` deliberately re-attach the source's lease, and external apps commonly attach many keys to one lease — revoking a shared lease **deletes all its other keys**. | 2026-08-08 |
| B-2 | `model.go` `NewModel` | Only the explicit `v3` arm called `authStatus()`; `auto` and `v2` left `authLabel` empty, so the header showed `Auth: ?` in the default mode. `v2Backend.authStatus` was production-dead code. | 2026-08-08 |
| B-3 | `model.go` `NewModel` (`auto`) | When the v3 probe failed and the walker fell back to v2, the freshly dialed v3 `*clientv3.Client` was never `Close()`d — its gRPC reconnect loop kept retrying against the endpoint for the whole session. | 2026-08-08 |
| B-4 | `controller.go` `edit()` | Dead file branch that would have dropped TTLs (it saved via `Set`); its directory branch duplicated `rename()`. | 2026-07-26 |
| B-5 | `model.go` `Model.Import` | Import did not apply the `IsReservedName` guard that `create()` applies: a JSON file containing a `…/.dir` key wrote the marker directly, silently turning its parent into a phantom directory. | 2026-08-08 |
| B-6 | `view.go` legend | Cosmetic: the bottom legend styled the search hotkey with `[::]` instead of `[::b]`, so `[/,Ctrl+S]Search` was the only non-bold entry. | 2026-08-08 |
| B-7 | `controller.go` `editMultiline`, `setTTL` | **Data loss.** Both wrote `val.node.Value` straight from the cache. v3 `ls` is keys-only, so a cached node's `Value` is empty until `fillDetails` fetches it — and `fillDetails` discards its own `Get` error silently. After one failed refresh, `Ctrl+E` opened an empty editor over a live key and `Ctrl+S` wiped it; `Ctrl+T` rewrote the key with the stale value. | 2026-08-11 |
| B-8 | `controller.go` `navigateTo` | Injected *every* jump/find target into the injected cache, which exists only for keys a listing hides (underscore-prefixed, invisible in etcd v2). Entries are never evicted except by an in-app delete/rename, so any key deleted server-side after being visited kept appearing as a ghost row whose details no longer resolved. | 2026-08-11 |
| B-9 | `controller.go` `getPosition` | Returned `0` for "not found", indistinguishable from "first element". A `/`-search matching nothing jumped the cursor to the first row, reading as a hit; post-mutation lookups landed on the wrong row instead of staying put. | 2026-08-11 |
| B-10 | `controller.go` `makeNodeMap`, `rename` | Derived the basename as `FieldsFunc(name, '/')[len-1]`, which panics with index-out-of-range on a name that splits into nothing (`""`, `"/"`). Also diverged from `baseOf`, which every other call site uses to build the same map key. | 2026-08-11 |
| B-11 | `controller.go` `valueStats` | Reported an empty value as 1 line (`Count("\n") + 1`). | 2026-08-11 |
| B-12 | `controller.go` `export`, `view.go` | Export walks the whole subtree under the current directory, but the doc comment, hotkey help and modal title all said "current dir keys" — exporting `/` silently dumped the entire cluster. Wording corrected; behaviour unchanged. | 2026-08-11 |
| B-13 | `history.go` titles and text | tview's colour-tag parser matches `[a-zA-Z]+` inside brackets and consumes it, in `Box` titles (`Print`) as much as in `TextView` text. Every hotkey hint on the revision-history screens was unescaped, so the detail title rendered as `/k @ rev 5 —  restore ·  diff vs current ·  back` — the keys themselves invisible. Key names in those titles were unescaped too. Now routed through `tview.Escape`; the legend in `view.go` already did this for `[Del[]`. | 2026-08-11 |
| O-1 | `controller.go` `export` | Wrote over an existing file with no confirmation. Export now `Stat`s the target and raises an overwrite prompt; the write itself moved into `writeExport`. | 2026-08-11 |
| O-2 | `history.go` `showRevisionDetail` | Rendered a printable revision value in full and uncapped, while the details pane capped at 512 B and hex at 1 kB — a multi-megabyte value stalled the draw. Now capped at `historyValueLimit` (8 kB) with an `[f]` binding that re-renders uncapped, for the hex branch too. | 2026-08-11 |
| O-3 | `history.go` `showRevisionDiff` | Diffed against `revs[0]` — the newest revision *as of when the picker opened* — and labelled it "current". Now re-reads the key, diffs against that, labels the right-hand side with the live `ModRev`, and warns when the key drifted since the history was read. A key that vanished meanwhile errors instead of diffing against a snapshot. | 2026-08-11 |
| B-14 | `controller.go` `confirmTyped` | Declining a protected-path confirmation queued a "Cancelled" notice on `Pages` **and then** ran `onCancel`, which re-opens the value editor via `App.SetRoot`. The notice was hidden behind the editor's root and resurfaced when the editor closed — drawn over a list that already held the focus, i.e. a visible modal the keyboard was not talking to. The notice is now skipped whenever a caller takes the screen back; the re-opened editor is the feedback. | 2026-08-16 |
| B-15 | `controller.go` post-mutation cache updates | Under `-dry-run` the six post-write `injectNode`/`removeInjected`/`reinjectRename` calls still ran, so a rehearsed create of an underscore-prefixed key put a phantom row in the listing (and a rehearsed delete removed a row for a key that still existed) — the ghost-row class of B-8, reintroduced through the rehearsal path. They now go through `injectWritten`/`removeWritten`/`reinjectWritten`, which no-op under dry-run. `navigateTo` deliberately still injects directly: a jumped-to key really is on the server. | 2026-08-16 |
| B-16 | `controller.go`, `history.go` success modals | Dry-run reported completed mutations as fact — "3 written, 0 skipped", "Restored", "Copied" — in the one mode whose entire purpose is to state what *would* happen. Import now reports "N keys would be written"; restore and copy route their header through `writeHeader`, which appends "(dry run — not written)". | 2026-08-16 |
| B-17 | `main.go` flag wiring | Flag registration (`flag.Var` calls) and the bundle handed to `resolve()` were two separate lists. Dropping a field from the struct literal compiled, passed the whole suite, and **nil-panicked on startup** — verified. Registration and bundling are now one `registerFlags(fs)` used by both `main()` and the tests, so there is no second place to forget, with a reflection test asserting no field is nil. | 2026-08-19 |
| B-18 | `main.go` `boolFlag` | `*boolFlag` did not implement `IsBoolFlag`, so the `flag` package treated every boolean as requiring an argument: `etcd-walker -read-only` failed with "flag needs an argument" — the exact form the README's production example uses. Affected `-read-only`, `-dry-run`, `-tls`, `-debug`, `-tls-skip-verify`. `IsBoolFlag` added; the bare form works and `-flag=false` still turns a config setting off. | 2026-08-19 |

**B-1 fix** — `setTTL` now delegates the cleanup to `revokeIfOrphaned`, which
resolves the superseded lease with `TimeToLive(..., WithAttachedKeys())` and
revokes only when no *other* key is still attached (the edited key was already
re-pointed by the preceding `Put`, so an orphan reports an empty key set; the
key itself is tolerated in case of a stale read). Any uncertainty — no lessor,
a failed lookup, a lease already reporting `TTL < 0` — leaves the lease alone:
a leaked lease expires by itself, a wrongly revoked one takes data with it.
To make this testable the v3 backend grew a `lessor` field (a three-method
narrowing of `clientv3.Lease`: `Grant`/`Revoke`/`TimeToLive`) that
`*clientv3.Client` satisfies in production and a `fakeLessor` — backed by the
`fakeKV`'s per-key lease map — satisfies in tests. All lease RPCs go through
it; `b.c` is now only the `Auth` client and `Close`.

**B-4 fix** (2026-07-26) — `edit()` was deleted outright; `Ctrl+E` on a
directory now delegates to the same rename dialog as `Ctrl+R`, with a
regression test pinning the fall-through.

**B-7 fix** — both mutating handlers now re-read the key through
`model.Get` and refuse to act when the read fails or the path is no longer a
key, instead of trusting whatever `fillDetails` last managed to cache. The
editor re-reads before opening (it needs the value to display); `setTTL`
re-reads inside its Save handler, immediately before the write, which also
stops a TTL change from resurrecting a key deleted while the dialog was open.
`restoreRevision` already worked this way — the two Ctrl+E/Ctrl+T paths were
the outliers. This narrows but does not close the read-modify-write window;
roadmap N-1 (CAS-safe writes via `Txn`/`ModRevision`) is the real fix.

### Accepted limitations

| Id  | Limitation |
|-----|-----------|
| L-1 | v3 `ls`/`search`/`export` fetch the full key range in one `Range` (no `WithLimit`+`WithFromKey` pagination). `ls` is keys-only which keeps browsing light, but find/export over millions of keys is slow and memory-hungry. |
| L-2 | The v2 backend always dials plain `http://`; the TLS options are v3-only (documented in README). |
| L-3 | v3 keys containing `//` or a trailing `/` are listed at their normalized path; `Get`/`Set` then target the normalized name, so such keys cannot be opened. |
| L-4 | The `replace google.golang.org/grpc => v1.29.1` pin (needed by the legacy `github.com/coreos/etcd` v2 client, which also drags in the archived `dgrijalva/jwt-go`) holds the whole binary on a June-2020 gRPC. Dropping v2 support is the only clean way out (roadmap F-13). |
| L-5 | Renames/copies are client-side copy-then-delete loops, not transactions — a failure mid-way leaves a partial target (and, for rename, the intact source). v3 could batch per-key `Txn`s but a whole-tree atomic move doesn't fit in one txn anyway. |
| L-6 | `fillDetails` issues a live `Ls` per directory-focus and a `Get` (+`TimeToLive` when leased) per key-focus — cursor movement does network round-trips; sluggish on high-latency clusters. |
| L-7 | OSC52 clipboard fallback refuses payloads > 10 kB (`ErrTooLarge`) rather than truncating. |
| L-8 | No live refresh: listings only update after an action. A v3 `Watch` on the current prefix is roadmap F-1. |

---

## 13. Session safety: policy, guards and stale state

Six features shipped in 0.9.0 (F-4, N-11, N-14, F-5, N-6, N-21) share one
concern: making it hard to change something you did not
mean to, and impossible to mistake cached state for live state. They are
described together because they interlock.

### 13.1 `controller.Policy`

```go
type Policy struct {
    ReadOnly          bool     // refuse every mutating action
    ProtectedPrefixes []string // subtrees needing a typed confirmation
    DryRun            bool     // record changes instead of making them (§13.4)
}
```

`Policy` lives in the controller, not the model, and that is deliberate: it
describes what *this session* is allowed to do, not what the cluster
supports. The model stays a pure etcd client with no opinion about
permission. `main` builds a `Policy` from the config file and flags and
passes it to `NewController(opts, policy)`.

`Policy.protects(path)` returns the covering prefix, if any. A prefix covers
itself and everything beneath it, and nothing else: `/reg` does not protect
`/registry`, which is why the test matches on `target == pfx ||
strings.HasPrefix(target, pfx+"/")` rather than a bare string prefix. A bare
`"/"` protects the whole keyspace; blank entries are ignored so a trailing
comma in `-protect` cannot become a prefix that matches everything.

### 13.2 The `guarded` funnel

Every mutation goes through one function:

```go
c.guarded(guard{action: "delete", paths: []string{name}, do: func() { … }})
```

`guarded` refuses outright in read-only mode, gates on a typed confirmation
when *any* target path is protected, and otherwise runs `do` immediately.
Routing every write through a single funnel is what makes the guarantee
checkable: the revision-restore flow, for instance, never touches the main
list's key bindings, and a check bolted onto those bindings alone would have
missed it. `guard.paths` is a slice for the same reason — one import can
write hundreds of keys, and a single protected target has to gate the batch.

The confirmation asks for the *prefix's* basename rather than the key's. It
reads honestly for a bulk write (there is no single key to name), and it
tells the user which boundary they crossed rather than echoing back what
they already typed.

`guard.onCancel` exists for one caller: the value editor closes before the
confirmation can be shown (a modal cannot draw over the full-screen editor
root), so a declined confirmation re-opens the editor with the user's text
intact rather than discarding their work.

Read-only is additionally enforced at the key bindings via `mutatingKeys`,
purely so the dialog never opens in the first place. `Ctrl+E` is absent from
that map on purpose: the value editor is the only way to read a value past
the details pane's preview cap, so in read-only mode it opens with
`SetDisabled(true)` instead of being blocked, and the save path's guard
refuses the write.

### 13.3 Stale state (N-11)

`fillDetails` re-reads the focused key on every cursor move. It used to
discard the error and silently keep the listing's node — and since v3
listings are keys-only, that node has *no value at all*. That silence is
what turned B-7 into data loss rather than an annoyance.

Now a failed refresh sets `Node.stale`, prints a red banner in the details
pane naming the error, and re-renders the row through `rowLabel` as greyed
and `(cached)`. `setStale` updates the row in place so it appears
immediately rather than at the next full rebuild. Recovery clears both.
Staleness deliberately outranks the underscore-prefix highlight in
`rowLabel`: it is the more urgent signal, and nesting the two colour tags
would leave the `[-]` reset mismatched.

### 13.4 Session journal and dry-run (N-6, N-21)

The journal decorates `modelAPI` rather than hooking the policy guard:

```go
type journaling struct {
    modelAPI          // reads are promoted unchanged
    j      *Journal
    dryRun bool
}
```

That placement is the whole design. The guard sees an *intent* and the
paths it will touch; only the model layer sees the exact arguments and
whether the call actually succeeded. A guard-level hook would log changes
that then failed — and a journal that lists changes which never happened is
worse than no journal at all, especially when it is about to be exported as
a script someone runs.

Dry-run falls out of the same wrapper for free: `apply` records the entry
and simply skips the underlying call. Nothing else in the codebase knows
dry-run exists. The visible consequence is that the listing does not change
after a rehearsed edit — correct, since nothing was written, and the journal
is where the rehearsal is visible.

**Fidelity over plausibility.** `Journal.Script()` emits real `etcdctl`
commands only where the recording supports one: put, del, del --prefix, and
import (whose full payload we hold, sorted so the script is stable between
renderings). Renames, copies and TTL writes become `#` comments describing
the change, because reproducing them needs data the decorator never saw — the
value at rename time, the lease id at replay time. Values that would not
survive a shell script (invalid UTF-8, control characters) likewise become
comments rather than mangled puts. The script header says all of this, so
nobody runs the file believing it is complete.

**The completeness hazard.** Embedding `modelAPI` means a *new* mutating
method would be silently promoted — journalled by nobody — which would
quietly falsify the feature's central claim. Two tests guard it:
`TestModelAPISurfaceIsJournalled` pins the interface's method count so
growing it is a deliberate act, and `TestEveryMutatorIsRecorded` drives every
mutator through the decorator and asserts each leaves exactly one entry.
Deleting any single override makes the second test fail.

### 13.5 Recursive deletes (F-5)

A directory delete is a single `DeleteRange` over a prefix with no undo — the
most destructive thing the tool can do, and previously one stray Enter away
on an ok/cancel dialog. It now goes through `guard.confirmWord`, which
demands the directory's own name be typed out.

`guarded` checks protection *before* `confirmWord`, so deleting a protected
directory asks once — for the prefix — rather than making the user type two
different words for one action. Single keys keep the lighter ok/cancel: the
blast radius is one key, and the journal now records what it was.

---

## 14. Time travel, undo snapshots and two panes

Three features shipped together in 0.10.0. They are grouped here because they
share one idea: *the browser should be able to show more than one moment, and
undo the moment it destroys.*

### 14.1 Time travel (F-19)

etcd's revision counter is global and monotonic. That makes "the keyspace as
it was" a single extra option on the reads the walker already issues, rather
than a feature that has to reconstruct anything: `clientv3.WithRev(rev)` on
the same range/point gets.

**Where the revision lives.** On `paneState.rev`, not on the model. The model
gained an explicit `rev` parameter on every read (`ls`, `get`, `export`,
`search`) and a `revision()` reporting the store's current one; the controller
reads through `paneLs`/`paneGet`, which carry the active pane's value. Putting
it in the pane is what makes "yesterday on the left, today on the right" fall
out of the two-pane work for free.

**Why an explicit parameter and not two method families.** A live `Ls` beside
a historical `LsAt` invites exactly one bug: a read site that picks the live
name while the pane is pinned. The result is not an error — it is today's data
under a historical title, which is worse. With one signature the revision is
impossible to omit; a write path that genuinely means "check the cluster now"
spells that out as `GetAt(key, 0)`.

**A pinned pane is read-only.** `guarded` refuses every mutation while
`rev > 0`, and the binding-level check refuses the dialogs up front, the same
two layers read-only mode uses. `Ctrl+E` still opens — reading a historical
value in full is the point — but disabled. The alternative, letting a write
through, would apply the past to the present through a listing that cannot
show the result.

**Compaction is the boundary, and it is reported.** `applyRevision` performs
the listing *before* committing to the new revision: if the read fails the
pane stays where it was and the error explains that the revision has been
compacted away. Switching first and failing afterwards would strand the user
in a pane where every read errors.

**Input.** The picker takes an absolute revision, `-N` for N revisions back
(resolved against `Revision()`, because nobody knows the absolute number
during an incident), or nothing to return to live. A future revision is
rejected up front: etcd *blocks* on one rather than erroring.

`TestIntegrationReadsAtPastRevision` pins the semantics against a real
cluster — including a deleted key becoming readable again at an earlier
revision, which no fake could establish.

### 14.2 Undo snapshots (N-25)

Before every recursive directory delete the subtree is exported to
`$XDG_STATE_HOME/etcd-walker/snapshots` (`ETCD_WALKER_STATE_DIR` overrides),
and `Ctrl+U` browses those files to restore or discard one.

**Failing closed.** If the snapshot cannot be written, the delete does not
happen. A safety net that silently degrades when the disk is full is worse
than none, because the user has stopped watching for the fall. `-no-snapshot`
is the deliberate opt-out, and the error message names it.

**Restore is an ordinary bulk write.** It goes through `Model.Import`, so it
inherits the policy guard, the typed confirmation for protected prefixes, the
journal and dry-run without a line of its own. The overwrite/skip question is
asked rather than assumed: keys may exist again by the time anyone restores.

**Binary values are base64.** `encoding/json` rewrites invalid UTF-8 as
U+FFFD, so a value containing a raw `0xff` byte stored in the plain map comes
back *different from what was deleted*. An undo that corrupts what it restores
is not an undo, so non-UTF-8 values go into a separate `binary_keys` map,
encoded. Text values stay in `keys` in the same shape as the JSON export, so a
snapshot is still a file a human can read. This was found by
`TestIntegrationDeleteSnapshotRestoreRoundTrip` against a real cluster — the
hermetic tests had happily agreed with the broken behaviour.

**Scope.** Directory deletes only. A single key is recoverable from its own
revision history (§5.6) on v3, and a file per key would bury the snapshots
that matter.

### 14.3 Two panes (F-20)

`Ctrl+B` shows a second pane, `Tab` switches, `F5`/`F6` copy or move the
selected entry into the other pane's directory.

**One embedded state, one swap.** The controller embeds the active
`paneState` and parks the other in `inactive`. Switching is a single struct
assignment:

```go
c.paneState, c.inactive = c.inactive, c.paneState
```

That is the whole design decision. Every handler keeps addressing
`c.currentDir` and `c.currentNodes` and automatically acts on the focused
pane, and a field added to `paneState` cannot be forgotten by the switch —
which a hand-written six-field swap absolutely would, leaving a pane's cursor
and its listing describing different directories.
`TestSwapPanesMovesEveryField` reflects over the struct so growing it stays a
deliberate act.

**Widgets versus state.** `view.Lists[2]` holds both list widgets and
`view.List` points at the focused one. Collapsing back to a single pane moves
the *widget* (`active = 0`) and never the state — swapping there would drop
the user into the other pane's directory, a silent jump. That distinction was
wrong in the first draft and a test caught it.

**Refreshing the other pane.** `withOtherPane` swaps in, runs `updateList`,
and swaps back, because every helper addresses the active pane by definition.
Both lists carry the details-refresh handler, guarded by pane index: rebuilding
the parked list also fires its changed callback, and filling the details pane
from it would describe a node nobody is looking at.

**Cross-pane transfers** reuse `CopyKey`/`CopyDir`/`RenameKey`/`RenameDir`
and go through `guarded` like any other write, so read-only, protected
prefixes, dry-run and the journal all cover them for free. Writing into a pane
that is pinned to a revision is refused: the data would land somewhere that
pane cannot show.

**The focus trap.** `tview` resolves focus when the root is set, by descending
into the root primitive's items. The first draft rooted the layout `Flex`
while it was still empty and filled it afterwards — so nothing had focus, and
because every binding is installed as an *input capture on the list*, the
running application started with the entire keyboard dead while looking
perfectly normal. `NewView` now populates the layout before `SetRoot` and sets
the focus explicitly; `TestNewViewFocusesTheFirstPane` pins it. Only a live
run surfaced this: no unit test that calls handlers directly can.
