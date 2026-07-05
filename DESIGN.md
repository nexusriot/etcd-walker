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
│   │   └── helpers_test.go
│   ├── controller/          UI state + keybindings + business glue
│   │   ├── controller.go
│   │   ├── controller_test.go  fakeModel + headless tview drive the flows
│   │   └── helpers_test.go
│   ├── view/                tview-based TUI rendering
│   │   └── view.go
│   └── util/clip/           clipboard with OSC52 fallback
│       ├── clip.go
│       └── clip_test.go
│
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
package, almost all with sibling `_test.go` files (only `pkg/view`, pure
widget construction, has none) — so newcomers can read it end-to-end in a
single sitting.

---

## 3. Runtime startup flow

The boot sequence ([cmd/etcd-walker/main.go](cmd/etcd-walker/main.go)) is:

1. Define typed flag wrappers (`stringFlag`, `boolFlag`) that record
   whether each flag was explicitly set on the command line — this is what
   makes "CLI overrides config" precise per field.
2. Parse `flag.Parse()`.
3. Seed hard-coded defaults (host `127.0.0.1`, port `2379`,
   protocol `auto`, timeout `5s`).
4. Call `config.Load(path)` to read the JSON file. Missing file is **not**
   an error; `Load` returns `(nil, nil)` so the program just keeps the
   defaults.
5. Apply config-file values, then overlay any explicitly-set CLI flags.
6. Build a `model.Options` struct and hand it to
   `controller.NewController(opts, debug)`.
7. `ctrl.Run()` enters the tview main loop and blocks until the user
   quits.

This three-tier precedence (defaults → file → flags) is implemented
field-by-field in `main.go` so a partial config (e.g. only `tls_enabled`)
combines cleanly with a partial set of CLI flags.

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
    ls(dir string) ([]*Node, error)
    get(path string) (*Node, error)
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
    search(dir, query string, inValues bool, limit int) ([]*Node, bool, error)
    authStatus() (enabled bool, known bool, err error)
    export(dir string) (map[string]string, error)
}
```

Two implementations satisfy it:

* `v3Backend` — wraps `go.etcd.io/etcd/client/v3`. It holds both a
  `clientv3.KV` (for get/put/delete/range) and the full `*clientv3.Client`
  (needed for lease operations and `Auth.AuthStatus`). Speaks gRPC, supports
  auth and TLS.
* `v2Backend` — wraps `github.com/coreos/etcd/client` (`clientv2.KeysAPI`),
  speaks HTTP. Username/password are passed through as HTTP basic auth;
  the TLS knobs are ignored (the endpoint is always `http://`).

Most exported `Model` methods are 1:1 wrappers over the interface. A few are
worth calling out:

* `Model.Import(items, overwrite)` — the one operation **not** backed by a
  single interface method: a key/value bulk write implemented in terms of
  `get`/`set`, with a normalize-and-skip policy.
* `Model.SetTTL(key, value, ttlSeconds)` — pass-through to `setTTL`; sets or
  clears a key's expiry (see §5.5).
* `Model.SetKeepTTL(key, value, leaseID, ttlSeconds)` — pass-through to
  `setKeep`; writes a new value while keeping the key's existing expiry
  (see §5.5).
* `Model.Search(dir, query, inValues, limit)` — recursive, case-insensitive
  substring search over full key paths (and values when `inValues`); returns
  at most `limit` nodes plus a truncation flag. On v3 the sweep is keys-only
  unless values are being searched.
* `Model.CopyKey` / `Model.CopyDir` — duplicate a key or a whole subtree,
  carrying leases (v3) / TTLs (v2) along; overlapping source/target is
  rejected by `copyDirGuard`. Renames reuse the same copy machinery
  (`copyPrefix` on v3, `copyTree` on v2) and add the delete.
* `probe()` — cheap reachability/auth check used by `NewModel`: a keys-only
  `limit=1` range on v3, a non-recursive root listing on v2 — so startup
  never pulls the whole keyspace.

`authStatus()` returns a `(enabled, known, err)` triple rather than a string
so the controller can distinguish "auth is off" from "couldn't determine"
and render the header label (`ON` / `OFF` / `?`) accordingly.

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
  and finally `Revoke`s the superseded lease so it does not linger as an
  orphan that keeps ticking server-side. A `ttlSeconds <= 0` instead does a
  plain `Put` (detaching any lease, making the key permanent) and then revokes
  the old lease too. The revoke is best-effort and guarded on `b.c != nil`;
  because the tool attaches exactly one key per lease, reaping the old lease
  never takes another key with it.
  When reading, `get` sees the lease id on the `KeyValue` and resolves the
  live countdown through `Lease.TimeToLive` (helper `ttlForLease`, tolerant:
  returns `0` when the lease id is `0`, the lease has expired, or `b.c` is
  `nil` — the unit tests wire up only the fake `KV`). `ls` deliberately does
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

Because the v3 TTL is a *countdown*, it changes every second on the server.
The controller therefore re-reads the focused key on demand rather than
trusting the value cached at list time — see §7.3.

---

## 6. Package: `pkg/view`

[pkg/view/view.go](pkg/view/view.go) is a thin wrapper around
[`rivo/tview`](https://github.com/rivo/tview). It owns:

* `App` — the `tview.Application`.
* `Frame` — the outer chrome with the status header.
* `Pages` — a stack of overlay pages used for modal dialogs.
* `List` — the left pane: the directory listing.
* `Details` — the right pane: metadata about the highlighted node.
* Constructors for the dialogs (create, edit, rename, set-TTL,
  delete-confirm, search, recursive-find form, search-results picker, jump,
  export, JSON import file-browser, import-mode prompt, multi-line editor,
  hotkeys help).

Key design decisions:

* The view is **passive**. It exposes widgets and helpers but never calls
  the model. The controller installs all input captures and `done`
  handlers.
* Dialogs are added to `Pages` and removed when dismissed, so any
  background list state survives a modal interaction. All dialogs share the
  page name `"modal"`, so a handler must remove the current dialog **before**
  showing a follow-up (e.g. an error) — a deferred remove would delete the
  follow-up instead.
* The full-screen multi-line editor swaps the application root
  (`OpenEditor`/`CloseEditor`), hiding the frame and hotkey legend so it can
  use the entire terminal; it is not a `Pages` modal.

---

## 7. Package: `pkg/controller`

[pkg/controller/controller.go](pkg/controller/controller.go) is the
biggest file in the project — it is where the application's behaviour
lives.

### 7.1 State

```go
type Controller struct {
    debug        bool
    view         *view.View
    model        modelAPI                    // *model.Model in production
    currentDir   string
    currentNodes map[string]*Node            // mapKey → Node
    position     map[string]int              // dir path → cursor index
    injected     map[string]map[string]*model.Node
    ordered      []string                    // display names, list order
    lastGoodDir  string                      // fallback for failed listings
    startupErr   error
}
```

* `model` — an unexported `modelAPI` interface rather than the concrete
  `*model.Model`; production wiring is unchanged, but tests drive the
  controller with an in-memory fake (see `controller_test.go`, which pairs
  the fake with headless tview widgets).
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
* `startupErr` — captured from `model.NewModel`; instead of crashing the
  process the controller renders the error inside the TUI on
  `Run()`, so a bad config produces a friendly screen rather than a stack
  trace.

### 7.2 Event flow

`setInput()` installs two `SetInputCapture` callbacks:

* On the global `App`: `Ctrl+Q` quits.
* On the `List` widget: every other hotkey (`Ctrl+N`, `Ctrl+D`, `Delete`,
  `Ctrl+E`, `Ctrl+R`, `Ctrl+T`, `Ctrl+P`, `Ctrl+Y`, `Ctrl+S`, `/`,
  `Ctrl+F`, `Ctrl+J`, `Ctrl+W`, `Ctrl+O`, `Ctrl+H`, `Backspace`).

Each hotkey calls a small method (`create`, `duplicate`, `delete`,
`editMultiline`, `rename`, `setTTL`, `copyPath`, `copyValue`, `search`,
`findRecursive`, `jump`, `export`, `importJSON`) which opens the appropriate
dialog and, on submission, calls into the model and then `updateList()` to
refresh the listing. `findRecursive` chains two dialogs: the query form,
then a results picker whose selection hands the node to `navigateTo` — the
shared helper (also used by `jump`) that enters a directory or selects a key
in its parent.

### 7.3 Listing rendering

`updateList()` is the central refresh routine:

1. `makeNodeMap()` — `model.Ls(currentDir)` merged with
   `injected[currentDir]` (the server listing wins on collisions). On error
   the refresh is **non-fatal**: an error modal is shown and the controller
   falls back to `lastGoodDir` (§9.2); on success `lastGoodDir` advances.
2. Sort via `orderedEntries`: directories first, then keys, each
   alphabetical **by basename** — sorting the raw `"<base>|dir"` map keys
   would order `app2/` before `app/` because `|` sorts after alphanumerics.
3. Push `tview.ListItem`s into the view, applying yellow styling to
   underscore-prefixed names.
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
current as the user moves the cursor on and off the key. The lookup falls
back to the cached node on any error (e.g. an injected entry that is not
yet readable). Directories keep using the cached node plus the live child
count.

### 7.4 Mutations

Every mutating action (`create`, `duplicate`, `delete`, `rename`,
`editMultiline`, `setTTL`, `importJSON`) follows the same pattern:

1. Open a dialog from `view`.
2. On `Enter`, remove the dialog page first (§6), then validate input and
   call the matching `model` method.
3. On error, surface a red modal with the error text — never crash.
4. On success, update the `injected` cache (so the new state is visible
   even if the server lags), then call `updateList()`.

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

The current user-facing version is **0.7.0**, hard-coded in three places
that must be bumped together when cutting a release:

* The header line in [pkg/controller/controller.go](pkg/controller/controller.go)
  (`"Etcd-walker v.0.7.0 …"`).
* `VERSION ?= 0.7.0` in the `Makefile` (overridable on the CLI:
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

## 11. Extending the project

Some natural places to extend:

* **A new backend** (e.g. Consul, ZooKeeper): implement the
  `model.backend` interface and add a switch arm in `model.NewModel`.
  Nothing in `controller` or `view` needs to change.
* **A new hotkey / dialog**: add a constructor in
  [pkg/view/view.go](pkg/view/view.go) and a handler method on
  `Controller`, then wire it in `setInput()`.
* **More config fields**: add the field to `pkg/config/config.Config`
  (with a `json:"…"` tag), thread it through `main.go`, and consume it in
  `model.Options`.

The clean MVC split and the small surface of the `backend` interface are
the two design constraints worth preserving as the project grows.
