## Etcd-walker
_Interactive TUI for browsing and managing etcd v2 / v3 datastores_

`etcd-walker` is a terminal application that lets you navigate an etcd
key-value store as if it were a filesystem. You can browse, create, edit,
rename, delete and export keys and directories from a single curses-style
interface, with full support for etcd v2 (HTTP) and etcd v3 (gRPC) — including
authentication and TLS.

![Profiles](resources/screenshot-v2.png)

Grab the latest pre-built binaries / `.deb` packages here:
[latest version](https://github.com/nexusriot/etcd-walker/releases/latest)

---

### Features

- File-explorer style navigation of etcd keys/directories
- Create / read / update / delete keys and directories
- Rename keys and directories (including recursive directory rename)
- Quick search inside the current level (`/` or `Ctrl+S`)
- Jump to an absolute or relative path (`Ctrl+J`)
- Multi-line editor for large key values (`Ctrl+E`)
- Export the current directory to JSON (`Ctrl+W`)
- Copy a key path or value to the system clipboard, with OSC52 fallback
  for SSH / tmux sessions (`Ctrl+P`, `Ctrl+Y`)
- Etcd v2 and v3 support, plus an `auto` mode that probes v3 first and
  falls back to v2
- Authentication (etcd v3, username + password)
- Full TLS / mTLS support (CA, client cert/key, optional skip-verify)
- Hidden / underscore-prefixed key support, highlighted in yellow
- Optional JSON config file (`/etc/etcd-walker/config.json`)
- Configurable per-operation timeout

---

### Hotkeys

| Key             | Action                                       |
|-----------------|----------------------------------------------|
| `Enter`         | Enter directory                              |
| `Backspace`     | Go up one directory                          |
| `Ctrl+N`        | Create new key or directory                  |
| `Delete`        | Delete current key/directory (with confirm)  |
| `Ctrl+E`        | Edit value (multi-line) / rename directory   |
| `Ctrl+R`        | Rename key or directory                      |
| `Ctrl+S` or `/` | Quick search inside the current level        |
| `Ctrl+J`        | Jump to absolute or relative path            |
| `Ctrl+W`        | Export current directory to a JSON file      |
| `Ctrl+P`        | Copy current path to clipboard               |
| `Ctrl+Y`        | Copy current key value to clipboard          |
| `Ctrl+H`        | Show in-app hotkeys help                     |
| `Ctrl+Q`        | Quit                                         |

---

### Configuration

`etcd-walker` reads its settings from three sources, applied in order
(later sources override earlier ones):

1. **Hard-coded defaults**
2. **JSON config file** — `/etc/etcd-walker/config.json` by default,
   overridable with `-config /path/to/file.json`. The file is **optional**;
   if it does not exist `etcd-walker` silently uses defaults.
3. **Command-line flags** — override individual fields from the config file.

#### Config file (JSON)

Default location: `/etc/etcd-walker/config.json`

Full schema (every field is optional):

```json
{
  "host": "127.0.0.1",
  "port": "2379",
  "protocol": "v3",
  "debug": false,

  "username": "root",
  "password": "supersecretpassword",

  "tls_enabled": false,
  "tls_ca_file":   "/etc/etcd-walker/ca.crt",
  "tls_cert_file": "/etc/etcd-walker/client.crt",
  "tls_key_file":  "/etc/etcd-walker/client.key",
  "tls_skip_verify": false,

  "timeout_seconds": 5
}
```

Field reference:

| Field             | Type    | Default     | Notes                                                |
|-------------------|---------|-------------|------------------------------------------------------|
| `host`            | string  | `127.0.0.1` | etcd host                                            |
| `port`            | string  | `2379`      | etcd port                                            |
| `protocol`        | string  | `auto`      | `v2`, `v3`, or `auto` (try v3 then fall back to v2)  |
| `debug`           | bool    | `false`     | Enable debug-level logging on stderr                 |
| `username`        | string  | _empty_     | etcd v3 auth username                                |
| `password`        | string  | _empty_     | etcd v3 auth password                                |
| `tls_enabled`     | bool    | `false`     | Use HTTPS / TLS for etcd v3                          |
| `tls_ca_file`     | string  | _empty_     | CA cert for verifying the server                     |
| `tls_cert_file`   | string  | _empty_     | Client certificate for mutual TLS                    |
| `tls_key_file`    | string  | _empty_     | Client private key for mutual TLS                    |
| `tls_skip_verify` | bool    | `false`     | Skip server cert validation (insecure)               |
| `timeout_seconds` | int     | `5`         | Per-operation timeout against etcd (`0` → 5)         |

#### Command-line flags

```
-config string             path to JSON config file (default "/etc/etcd-walker/config.json")
-host string               etcd host (e.g. 127.0.0.1)
-port string               etcd port (e.g. 2379)
-protocol string           etcd protocol: v2, v3, auto (default: auto)
-username string           etcd auth username
-password string           etcd auth password (consider using config file)
-tls bool                  enable TLS/HTTPS for etcd v3
-tls-ca string             path to CA certificate file
-tls-cert string           path to client certificate file (mTLS)
-tls-key string            path to client key file (mTLS)
-tls-skip-verify bool      skip server certificate verification (insecure)
-timeout string            etcd operation timeout in seconds
-debug bool                enable debug logging
```

Flags that are explicitly set on the command line always win over the
config file. Flags that are omitted leave the config file value untouched.

#### Configuration examples

Minimal — connect to a local insecure etcd v3:

```json
{ "protocol": "v3" }
```

Authenticated v3 over plain TCP:

```json
{
  "host": "etcd.internal",
  "port": "2379",
  "protocol": "v3",
  "username": "root",
  "password": "supersecretpassword"
}
```

Authenticated v3 over mutual TLS:

```json
{
  "host": "etcd.example.com",
  "port": "2379",
  "protocol": "v3",
  "username": "root",
  "password": "supersecretpassword",
  "tls_enabled": true,
  "tls_ca_file":   "/etc/etcd-walker/ca.crt",
  "tls_cert_file": "/etc/etcd-walker/client.crt",
  "tls_key_file":  "/etc/etcd-walker/client.key",
  "timeout_seconds": 10
}
```

One-shot connection without a config file:

```bash
./etcd-walker \
  -host etcd.example.com -port 2379 \
  -protocol v3 -tls \
  -tls-ca /etc/etcd-walker/ca.crt \
  -tls-cert /etc/etcd-walker/client.crt \
  -tls-key  /etc/etcd-walker/client.key \
  -username root -password 'supersecretpassword'
```

---

### Authentication

Since v0.3.2 `etcd-walker` supports authentication. Authentication is
**v3 only** — the etcd v2 backend will ignore `username` / `password`.

If the server has auth enabled and the credentials are missing or wrong,
`etcd-walker` shows the etcd error in-app and the header reports
`Auth: required` so the misconfiguration is easy to spot.

---

### Building

Standard build:

```bash
go build ./cmd/etcd-walker
```

Static build (no libc dependency, useful inside scratch / distroless
containers):

```bash
go build -o etcd-walker_linux_x64_static \
  -ldflags "-linkmode external -extldflags -static" \
  ./cmd/etcd-walker
```

Verify with `ldd`:

```bash
ldd etcd-walker
```

32-bit (i686) build:

```bash
GOOS=linux GOARCH=386 go build -o etcd-walker_linux_i686 ./cmd/etcd-walker
```

FreeBSD build:

```bash
GOOS=freebsd GOARCH=amd64 go build -o etcd-walker_freebsd_x86_64 ./cmd/etcd-walker
```

#### Building a `.deb` package

Install the build dependencies once:

```bash
sudo apt-get install git devscripts build-essential lintian upx-ucl golang
```

Then run the appropriate script:

```bash
./build-deb.sh           # amd64
./build-deb-arm64.sh     # arm64
```

The resulting package is written to `build/etcd-walker_<version>_<arch>.deb`.

---

### Running

```
./etcd-walker [-config path] [-host host] [-port port] [-protocol v2|v3|auto] \
              [-username user] [-password pass] [-debug] \
              [-tls] [-tls-ca path] [-tls-cert path] [-tls-key path] \
              [-tls-skip-verify] [-timeout seconds]
```

Default values: host `127.0.0.1`, port `2379`, protocol `auto`,
debug `false`, timeout `5s`.

---

### Starting etcd for development / testing

Run a throwaway etcd in Docker:

```bash
docker run -d --restart unless-stopped -p 2379:2379 --name etcd \
  quay.io/coreos/etcd:v3.3.27 /usr/local/bin/etcd \
  -advertise-client-urls http://0.0.0.0:2379 \
  -listen-client-urls    http://0.0.0.0:2379
```

Smoke-test from the host:

```bash
curl -L http://localhost:2379/v2/keys/test -XPUT -d value="test value"
```

Then point `etcd-walker` at it:

```bash
./etcd-walker -host 127.0.0.1 -port 2379 -protocol auto
```

---

### Architecture

For an in-depth look at the project's internal structure, package layout
and design decisions, see [DESIGN.md](DESIGN.md).
