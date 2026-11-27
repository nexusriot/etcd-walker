#!/usr/bin/env python3
"""Regenerate the README screenshots from the real binary.

Seeds a throwaway etcd v3 with realistic data, drives etcd-walker inside a
private tmux server to four states, and saves each screen as an ANSI dump;
ansi2png.py renders them. It WIPES the target etcd — point it at a scratch
container only:

    docker run -d --rm --name ew-shots -p 127.0.0.1:52379:2379 \
      quay.io/coreos/etcd:v3.5.21 etcd --listen-client-urls=http://0.0.0.0:2379 \
      --advertise-client-urls=http://127.0.0.1:52379
    make e2e-bin && cp test/e2e/etcd-walker tools/screenshots/
    cd tools/screenshots && python3 shots.py
    for f in *.ansi; do python3 ansi2png.py "$f" "../../resources/${f%.ansi}.png"; done
    docker rm -f ew-shots

Needs tmux; ansi2png.py needs Pillow and the DejaVu + Noto Color Emoji fonts.
"""
import json, os, shlex, subprocess, time, base64, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(HERE, "etcd-walker")
PORT = os.environ.get("EW_V3_PORT", "52379")
SOCK = "ew-shots"
W, H = 160, 46


def sh(cmd, check=True):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd}: {r.stderr}")
    return r.stdout


def gw(path, body):
    req = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}", data=json.dumps(body).encode(), method="POST")
    return json.loads(urllib.request.urlopen(req, timeout=5).read() or b"{}")


def b64(s): return base64.b64encode(s.encode()).decode()
def put(k, v, lease=None):
    body = {"key": b64(k), "value": b64(v)}
    if lease: body["lease"] = lease
    return int(gw("/v3/kv/put", body)["header"]["revision"])
def delete(k): gw("/v3/kv/deleterange", {"key": b64(k)})
def lease(ttl): return gw("/v3/lease/grant", {"TTL": ttl})["ID"]


def keys(*ks, delay=0.35):
    for k in ks:
        if k.startswith("lit:"):
            text = k[4:]
            # tmux takes an argument ending in ";" as a command separator and
            # drops the ";": escape it, or the app is typed something else.
            if text.endswith(";"):
                text = text[:-1] + "\\;"
            sh(f"tmux -L {SOCK} send-keys -t s -l -- {shlex.quote(text)}")
        else:
            sh(f"tmux -L {SOCK} send-keys -t s {k}")
        time.sleep(delay)


def screen(): return sh(f"tmux -L {SOCK} capture-pane -p -t s")


def wait(text, timeout=8):
    end = time.time() + timeout
    while time.time() < end:
        if text in screen():
            return
        time.sleep(0.2)
    raise RuntimeError(f"never saw {text!r}:\n{screen()}")


def capture(name):
    time.sleep(0.8)
    open(os.path.join(HERE, name + ".ansi"), "w").write(sh(f"tmux -L {SOCK} capture-pane -e -p -t s"))
    print("captured", name)


def start(args):
    sh(f"tmux -L {SOCK} kill-server 2>/dev/null", check=False)
    cmd = (f"cd {HERE} && TERM=xterm-256color ETCD_WALKER_STATE_DIR={HERE}/state "
           f"{BIN} -config /nonexistent -host 127.0.0.1 -port {PORT} -protocol v3 {args} 2>{HERE}/shots.err")
    sh(f"tmux -L {SOCK} new-session -d -s s -x {W} -y {H} {shlex.quote(cmd)}")
    time.sleep(1.2)


def goto(path):
    keys("C-j"); keys("lit:" + path); keys("Enter", delay=0.8)


NGINX = """server {
    listen 8080;
    server_name shop.internal;

    location /api/ {
        proxy_pass http://api;
        proxy_read_timeout 30s;
    }

    location / {
        root /srv/www;
        try_files $uri /index.html;
    }
}
"""


def seed():
    gw("/v3/kv/deleterange", {"key": b64("\0"), "range_end": b64("\0")})
    put("/services/api/config", json.dumps({"replicas": 3, "image": "registry.local/api:2.14.1",
        "resources": {"cpu": "500m", "memory": "512Mi"},
        "features": {"rateLimit": True, "cacheTTL": "30s"}, "owners": ["platform-team"]}))
    put("/services/api/version", "2.14.1")
    for ip in ("10.0.4.21", "10.0.4.22", "10.0.4.23"):
        put(f"/services/api/endpoints/{ip}:8080", json.dumps({"healthy": True, "zone": "eu-west-1a"}))
    put("/services/web/nginx.conf", NGINX)
    put("/services/web/version", "5.3.0")
    put("/services/worker/concurrency", "16")
    put("/feature-flags/new-checkout", "true")
    put("/feature-flags/dark-mode", "false")
    put("/feature-flags/search-v2", "rollout:25%")
    put("/locks/billing-job", "worker-7f9c", lease=lease(3600))
    put("/_meta/schema-version", "12")


def main():
    seed()
    # -- 1. two panes: live on the left, the same directory 'before' on the right
    before = put("/services/api/endpoints/10.0.4.22:8080", json.dumps({"healthy": True, "zone": "eu-west-1b"}))
    put("/services/api/endpoints/10.0.4.24:8080", json.dumps({"healthy": True, "zone": "eu-west-1c"}))
    delete("/services/api/endpoints/10.0.4.21:8080")
    put("/services/api/endpoints/10.0.4.23:8080", json.dumps({"healthy": False, "zone": "eu-west-1a"}))
    start("-live -dual")
    wait("● LIVE")
    goto("/services/api/endpoints/")
    keys("Tab")
    goto("/services/api/endpoints/")
    keys("C-g"); keys("lit:" + str(before)); keys("Enter", delay=1.0)
    keys("Enter")                                     # dismiss the "Revision N" note
    keys("Down", "Down")
    keys("Tab")
    goto("/services/api/endpoints/10.0.4.23:8080")
    capture("01-two-panes-live-and-past")

    # -- 2. details pane: JSON value, revisions, TTL
    start("-live")
    wait("● LIVE")
    goto("/services/api/config")
    wait("replicas")
    capture("02-details-json")

    # -- 3 + 4. conflict-safe save: someone else edits nginx.conf meanwhile
    goto("/services/web/nginx.conf")
    keys("C-e", delay=0.8)
    wait("Edit (multiline)")
    keys("Down", "Down", "Down", "Down", "Down", "Down", "End")
    keys("Enter"); keys("lit:        proxy_set_header X-Request-Id $request_id;")
    put("/services/web/nginx.conf", NGINX.replace("listen 8080;", "listen 8443 ssl;")
        .replace("proxy_read_timeout 30s;", "proxy_read_timeout 60s;"))
    time.sleep(1.5)
    keys("C-s", delay=1.0)
    wait("changed on the server")
    capture("03-conflict-dialog")
    keys("Enter", delay=1.0)                          # diff
    wait("conflict on")
    capture("04-conflict-diff")
    keys("Escape", "Escape", "Escape", "C-q")


if __name__ == "__main__":
    try:
        main()
    finally:
        sh(f"tmux -L {SOCK} kill-server 2>/dev/null", check=False)
