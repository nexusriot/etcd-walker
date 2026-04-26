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
│   └── main.go              entry point: flags + config → Controller
│
├── pkg/
│   ├── config/              JSON config loader
│   │   └── config.go
│   ├── model/               etcd abstraction (v2 / v3 behind one iface)
│   │   └── model.go
│   ├── controller/          UI state + keybindings + business glue
│   │   └── controller.go
│   ├── view/                tview-based TUI rendering
│   │   └── view.go
│   └── util/clip/           clipboard with OSC52 fallback
│       └── clip.go
│
├── resources/               screenshots, icons
├── DEBIAN/                  Debian packaging metadata
├── build-deb.sh             builds an amd64 .deb
├── build-deb-arm64.sh       builds an arm64 .deb
├── go.mod / go.sum
├── LICENSE
├── README.md
└── DESIGN.md                this document
```

The codebase is intentionally small (≈ 2 kLoC) and flat — one file per
package — so newcomers can read it end-to-end in a single sitting.

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
    ls(dir string) ([]*Node, error)
    get(path string) (*Node, error)
    set(path, value string) error
    del(path string) error
    mkdir(path string) error
    deldir(path string) error
    renameDir(oldPath, newPath string) error
    renameKey(oldPath, newPath string) error
    export(dir string) (map[string]string, error)
    authStatus() string
    proto() string
}
```

Two implementations satisfy it:

* `v3Backend` — wraps `go.etcd.io/etcd/client/v3` (`clientv3.KV`),
  speaks gRPC, supports auth and TLS.
* `v2Backend` — wraps `github.com/coreos/etcd/client` (`clientv2.KeysAPI`),
  speaks HTTP, ignores auth/TLS knobs.

### 5.2 Protocol selection

`model.NewModel(opts)` honours `opts.Protocol`:

* `"v3"` — build a v3 backend; surface auth errors with a hint to set
  credentials.
* `"v2"` — build a v2 backend.
* `"auto"` (default) — try v3 first, and fall back to v2 if the v3 probe
  fails. The chosen protocol is exposed via `Model.ProtocolVersion()` so
  the controller can show it in the header.

### 5.3 Synthetic directories on v3

etcd v3 has no native concept of a directory: keys are flat. To present a
hierarchy `etcd-walker` adopts two conventions:

* When listing under `prefix/`, any key whose remaining path contains a
  `/` is rendered as a **synthetic** subdirectory.
* `mkdir(p)` writes a marker key `p/.dir` so that an "empty directory"
  can exist; the controller filters `.dir` markers out of the user-facing
  listing and `export()` skips them too.

This keeps the v2 and v3 user experiences indistinguishable.

### 5.4 TLS and timeouts

For v3, `Options.TLSEnabled`, `TLSCAFile`, `TLSCertFile`, `TLSKeyFile`
and `TLSSkipVerify` are folded into a `*tls.Config` and attached to the
gRPC dial options. `TimeoutSeconds` (default 5) becomes the
`DialTimeout` and the per-request `context.WithTimeout` budget so a
broken server cannot hang the UI.

---

## 6. Package: `pkg/view`

[pkg/view/view.go](pkg/view/view.go) is a thin wrapper around
[`rivo/tview`](https://github.com/rivo/tview). It owns:

* `App` — the `tview.Application`.
* `Frame` — the outer chrome with the status header.
* `Pages` — a stack of overlay pages used for modal dialogs.
* `List` — the left pane: the directory listing.
* `Details` — the right pane: metadata about the highlighted node.
* Constructors for the dialogs (create, edit, rename, delete-confirm,
  search, jump, export, multi-line editor, hotkeys help).

Key design decisions:

* The view is **passive**. It exposes widgets and helpers but never calls
  the model. The controller installs all input captures and `done`
  handlers.
* Dialogs are added to `Pages` and removed when dismissed, so any
  background list state survives a modal interaction.
* The full-screen multi-line editor is a separate `Pages` entry rather
  than a modal so it can use the entire terminal.

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
    model        *model.Model
    currentDir   string
    currentNodes map[string]*Node            // mapKey → Node
    position     map[string]int              // dir path → cursor index
    injected     map[string]map[string]*model.Node
    startupErr   error
}
```

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
* `startupErr` — captured from `model.NewModel`; instead of crashing the
  process the controller renders the error inside the TUI on
  `Run()`, so a bad config produces a friendly screen rather than a stack
  trace.

### 7.2 Event flow

`setInput()` installs two `SetInputCapture` callbacks:

* On the global `App`: `Ctrl+Q` quits.
* On the `List` widget: every other hotkey (`Ctrl+N`, `Delete`,
  `Ctrl+E`, `Ctrl+R`, `Ctrl+P`, `Ctrl+Y`, `Ctrl+S`, `/`, `Ctrl+J`,
  `Ctrl+W`, `Ctrl+H`, `Backspace`).

Each hotkey calls a small method (`create`, `delete`, `editMultiline`,
`rename`, `copyPath`, `copyValue`, `search`, `jump`, `export`) which
opens the appropriate dialog and, on submission, calls into the model and
then `updateList()` to refresh the listing.

### 7.3 Listing rendering

`updateList()` is the central refresh routine:

1. `model.Ls(currentDir)` — fetch children from etcd.
2. `makeNodeMap()` — merge that result with `injected[currentDir]`.
3. Sort: directories first, then keys, both alphabetical.
4. Push `tview.ListItem`s into the view, applying yellow styling to
   underscore-prefixed names.
5. Restore the cursor from `position[currentDir]`.
6. Bind a selection handler that calls `fillDetails()` whenever the
   highlight moves.

`fillDetails()` shows path, cluster ID, protocol and auth in the header
area, plus per-node metadata: byte size, line count, SHA-256 of the
value, and either a 512-char preview or a `<binary>` indicator for
non-UTF-8 data.

### 7.4 Mutations

Every mutating action (`create`, `delete`, `rename`, `editMultiline`)
follows the same pattern:

1. Open a dialog from `view`.
2. On `Enter`, validate input and call the matching `model` method.
3. On error, surface a red modal with the error text — never crash.
4. On success, update the `injected` cache (so the new state is visible
   even if the server lags), then call `updateList()`.

Renames are implemented as **copy-then-delete** in the model so they work
identically on v2 and v3 even though v3 has no native rename.

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
* The process only exits non-zero when `tview` itself fails or the user
  hits `Ctrl+Q`.

### 9.3 Versioning

The user-facing version string is hard-coded in two places:

* The header line in [pkg/controller/controller.go](pkg/controller/controller.go)
  (`"Etcd-walker v.0.4.0 …"`).
* The `version=` variable at the top of `build-deb.sh` /
  `build-deb-arm64.sh`.

Both must be bumped together when cutting a release.

---

## 10. Build & packaging

* `go build ./cmd/etcd-walker` — normal dynamic build.
* `go build -ldflags "-linkmode external -extldflags -static" …` —
  static binary suitable for distroless / scratch containers.
* `GOOS=linux GOARCH=386 go build …` — 32-bit build.
* `./build-deb.sh [amd64|arm64]` — assembles a `.deb` under
  `build/etcd-walker_<version>_<arch>/`, copies the `DEBIAN/` metadata,
  patches `Architecture:` and `_version_` in `DEBIAN/control`, builds the
  binary (statically when the host arch matches, with `CGO_ENABLED=0`
  for cross-builds) and runs `dpkg-deb --build`.

Cross-compilation for arm64 is delegated to a thin
`build-deb-arm64.sh` wrapper that calls `build-deb.sh arm64`.

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
