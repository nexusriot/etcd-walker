#!/usr/bin/env bash
# Hermetic end-to-end run: brings up its own etcd clusters (v3, v2, auth, TLS)
# under a unique compose project on free loopback ports, runs the tmux-driven
# suite against them with the real binary, and tears everything down — also on
# failure or Ctrl+C. Nothing outside this run is touched: not your etcd, not
# your tmux server (the suite uses a private socket), not your state dir.
#
#   make e2e                      # build + run everything
#   test/e2e/run.sh cas live      # only scenarios whose name contains a filter
#   EW_KEEP=1 test/e2e/run.sh     # leave the clusters up afterwards (debugging)
#
# Needs: docker compose v2, tmux, python3, openssl.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

for tool in docker tmux python3 openssl; do
  command -v "$tool" >/dev/null || { echo "e2e: $tool is required" >&2; exit 2; }
done
docker compose version >/dev/null 2>&1 || { echo "e2e: docker compose v2 is required" >&2; exit 2; }

BIN="${ETCD_WALKER_BIN:-$HERE/etcd-walker}"
[ -x "$BIN" ] || { echo "e2e: no binary at $BIN — run 'make e2e-bin' first" >&2; exit 2; }
export ETCD_WALKER_BIN="$BIN"

export COMPOSE_PROJECT_NAME="ew-e2e-$$"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ew-e2e.XXXXXX")"
export EW_CERT_DIR="$WORK/certs"
export EW_STATE_DIR="$WORK/state"
export EW_TMUX_SOCKET="ew-e2e-$$"
mkdir -p "$EW_CERT_DIR" "$EW_STATE_DIR"

# Four distinct free loopback ports.
read -r EW_V3_PORT EW_V2_PORT EW_AUTH_PORT EW_TLS_PORT < <(python3 - <<'EOF'
import socket
socks = [socket.socket() for _ in range(4)]
for s in socks:
    s.bind(("127.0.0.1", 0))
print(*[s.getsockname()[1] for s in socks])
for s in socks:
    s.close()
EOF
)
export EW_V3_PORT EW_V2_PORT EW_AUTH_PORT EW_TLS_PORT

cleanup() {
  local rc=$?
  tmux -L "$EW_TMUX_SOCKET" kill-server 2>/dev/null || true
  if [ "$rc" -ne 0 ]; then
    # A failing run on CI is only debuggable from its log: say what the
    # clusters were doing (slow applies, fsync warnings, OOM, restarts).
    echo "e2e: failed (exit $rc) — cluster state and recent etcd logs follow"
    docker compose ps -a 2>&1 || true
    docker stats --no-stream 2>&1 | grep -E "NAME|$COMPOSE_PROJECT_NAME" || true
    docker compose logs --no-color --tail=150 2>&1 \
      | grep -E '"level":"(warn|error|fatal|panic)"|took too long|slow|fdatasync|OOM|Killed' \
      | tail -80 || true
  fi
  if [ -z "${EW_KEEP:-}" ]; then
    docker compose down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$WORK"
  else
    echo "e2e: EW_KEEP set — project $COMPOSE_PROJECT_NAME and $WORK left in place"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

# TLS material: a throwaway CA, a server cert valid for 127.0.0.1/localhost,
# a client cert for mTLS, and a second CA nobody trusts (for the negative case).
(
  cd "$EW_CERT_DIR"
  q() { openssl "$@" >/dev/null 2>&1; }
  q req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=ew-e2e-ca" -keyout ca.key -out ca.crt
  q req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=ew-e2e-rogue-ca" -keyout rogue.key -out rogue.crt
  printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth,clientAuth\n' > server.ext
  q req -newkey rsa:2048 -nodes -subj "/CN=127.0.0.1" -keyout server.key -out server.csr
  q x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile server.ext -out server.crt
  printf 'extendedKeyUsage=clientAuth\n' > client.ext
  q req -newkey rsa:2048 -nodes -subj "/CN=root" -keyout client.key -out client.csr
  q x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile client.ext -out client.crt
  chmod 644 ./*.key   # the etcd container reads them as another uid
)

echo "e2e: project $COMPOSE_PROJECT_NAME  v3:$EW_V3_PORT v2:$EW_V2_PORT auth:$EW_AUTH_PORT tls:$EW_TLS_PORT"
docker compose up -d --wait --quiet-pull >/dev/null
python3 "$HERE/e2e.py" "$@"
