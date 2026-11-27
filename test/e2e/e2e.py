#!/usr/bin/env python3
"""End-to-end TUI tests for etcd-walker, driven through tmux against real etcd.

The real binary runs inside tmux; the harness types keys, mutates etcd behind
its back (etcdctl in the containers, the v2 HTTP API, the v3 gateway), and
asserts on what the screen shows. Needs docker compose, tmux and python3.

    make e2e-bin                       # builds test/e2e/etcd-walker
    cd test/e2e && docker compose up -d
    python3 e2e.py                     # everything
    python3 e2e.py cas live-basic      # scenarios whose name contains a filter

NOTE: the scenarios wipe the test clusters' keyspaces. Never point this at an
etcd you care about; the compose file binds the clusters to loopback only.
Prefer run.sh (make e2e), which is hermetic: its own compose project on free
ports, its own TLS material, a private tmux socket, a temporary state dir,
and teardown afterwards. Run directly, this file expects the clusters from
docker-compose.yml on the EW_*_PORT ports (defaults 12379/22379/32379/42379),
certs in EW_CERT_DIR, and uses the tmux socket EW_TMUX_SOCKET.
"""
import json, os, re, shlex, subprocess, sys, time, traceback, urllib.request, urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.environ.get("ETCD_WALKER_BIN", os.path.join(HERE, "etcd-walker"))
V3 = os.environ.get("EW_V3_PORT", "12379")
V2 = os.environ.get("EW_V2_PORT", "22379")
AUTH = os.environ.get("EW_AUTH_PORT", "32379")
TLS = os.environ.get("EW_TLS_PORT", "42379")
CERTS = os.environ.get("EW_CERT_DIR", os.path.join(HERE, "certs"))
STATE = os.environ.get("EW_STATE_DIR", os.path.join(HERE, "state"))
# The version the binary should report, read from its single source
# (pkg/version/version.go, DESIGN §9.3) so a release bump needs no edit here.
# EW_EXPECT_VERSION overrides it when the suite runs away from the checkout.
def _source_version():
    src = os.path.join(HERE, "..", "..", "pkg", "version", "version.go")
    try:
        m = re.search(r'^var Version = "(.*)"', open(src).read(), re.M)
        return m.group(1) if m else None
    except OSError:
        return None
VERSION = os.environ.get("EW_EXPECT_VERSION") or _source_version()
# A private tmux server: the suite kills sessions freely and must never touch
# the developer's own tmux.
SOCK = os.environ.get("EW_TMUX_SOCKET", "ew-e2e")
RESULTS = []


def sh(cmd, check=True, inp=None):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True, input=inp)
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd!r} failed: {r.stdout}{r.stderr}")
    return r.stdout


# ---------- etcd helpers ---------------------------------------------------

def ctl(port, *args, user=None, inp=None):
    svc = {V3: "etcd-v3", AUTH: "etcd-auth", V2: "etcd-v2", TLS: "etcd-tls"}[port]
    env = "-e ETCDCTL_API=3 " if port == V2 else ""
    u = f"--user={user} " if user else ""
    if port == TLS:
        u += ("--endpoints=https://127.0.0.1:2379 --cacert=/certs/ca.crt "
              "--cert=/certs/client.crt --key=/certs/client.key ")
    q = " ".join("'" + a.replace("'", "'\\''") + "'" for a in args)
    return sh(f"cd {HERE} && docker compose exec -T {env}{svc} etcdctl {u}{q}", inp=inp)


def v3put(k, v, port=V3, **kw): ctl(port, "put", k, v, **kw)
def v3del(k, port=V3, prefix=False, **kw): ctl(port, "del", k, *(["--prefix"] if prefix else []), **kw)
def v3get(k, port=V3, **kw):
    out = ctl(port, "get", k, "--print-value-only", **kw)
    return out[:-1] if out.endswith("\n") else out


def v2req(method, key, **params):
    url = f"http://127.0.0.1:{V2}/v2/keys{urllib.parse.quote(key)}"
    data = urllib.parse.urlencode(params).encode() if params and method in ("PUT", "POST") else None
    if params and method in ("GET", "DELETE"):
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def v2put(k, v, ttl=None):
    p = {"value": v}
    if ttl: p["ttl"] = ttl
    return v2req("PUT", k, **p)
def v2get(k): return v2req("GET", k).get("node", {}).get("value")
def v2del(k, recursive=False):
    return v2req("DELETE", k, **({"recursive": "true"} if recursive else {}))


def wipe():
    ctl(V3, "del", "", "--prefix")
    for n in v2req("GET", "/").get("node", {}).get("nodes", []) or []:
        v2del(n["key"], recursive=True)


# ---------- tmux helpers ---------------------------------------------------

class TUI:
    def __init__(self, args, name="ew", w=200, h=50):
        self.name = name
        sh(f"tmux -L {SOCK} kill-session -t {name} 2>/dev/null", check=False)
        # In the run's state dir (a temp dir under run.sh), not the source tree.
        self.err = os.path.join(STATE, f"{name}.stderr")
        cmd = (f"cd {HERE} && ETCD_WALKER_STATE_DIR={STATE} "
               f"{BIN} -config /nonexistent {args} 2>{self.err}")
        sh(f"tmux -L {SOCK} new-session -d -s {name} -x {w} -y {h} {shlex.quote(cmd)}")
        time.sleep(0.8)

    def keys(self, *ks, delay=0.25):
        for k in ks:
            if k.startswith("lit:"):
                text = k[4:]
                # tmux takes an argument ending in ";" as a command separator and
                # drops the ";": escape it, or the app is typed something else.
                if text.endswith(";"):
                    text = text[:-1] + "\\;"
                sh(f"tmux -L {SOCK} send-keys -t {self.name} -l -- {shlex.quote(text)}")
            else:
                sh(f"tmux -L {SOCK} send-keys -t {self.name} {k}")
            time.sleep(delay)

    def type(self, text):
        self.keys("lit:" + text)

    def screen(self):
        return sh(f"tmux -L {SOCK} capture-pane -p -t {self.name}", check=False)

    def alive(self):
        return subprocess.run(f"tmux -L {SOCK} has-session -t {self.name}", shell=True).returncode == 0

    def wait(self, text, timeout=6.0, absent=False):
        end = time.time() + timeout
        while time.time() < end:
            s = self.screen()
            if (text in s) != absent:
                return s
            time.sleep(0.2)
        s = self.screen()
        raise AssertionError(f"{'still' if absent else 'never'} saw {text!r} within {timeout}s. Screen:\n{s}")

    def header(self):
        for line in self.screen().splitlines():
            if "Etcd-walker v." in line:
                return line.strip()
        return ""

    def focused_path(self):
        for line in self.screen().splitlines():
            if "Full path:" in line:
                return line.split("Full path:")[1].split("│")[0].strip()
        return None

    def stop(self):
        if self.alive():
            self.keys("C-q", delay=0.1)
        # Quitting must be prompt even when the UI is busy with slow reads.
        deadline = time.time() + 6
        while self.alive() and time.time() < deadline:
            time.sleep(0.1)
        alive = self.alive()
        sh(f"tmux -L {SOCK} kill-session -t {self.name} 2>/dev/null", check=False)
        err = open(self.err).read() if os.path.exists(self.err) else ""
        assert not alive, "C-q did not exit the app"
        assert "panic" not in err, f"panic on stderr:\n{err}"


def goto(t, path):
    """Jump to an absolute key/dir with Ctrl+J."""
    t.keys("C-j")
    t.type(path)
    t.keys("Enter", delay=0.6)


def select(t, path):
    """Put the cursor on a key by jumping to its full path (Ctrl+J)."""
    goto(t, path)
    deadline = time.time() + 6          # a slow link delays the jump's reads
    while t.focused_path() != path and time.time() < deadline:
        time.sleep(0.1)
    fp = t.focused_path()
    assert fp == path, f"jump to {path} focused {fp}:\n{t.screen()}"


# ---------- scenarios -------------------------------------------------------

def sc_version():
    out = sh(f"{BIN} -version")
    assert re.match(rf"etcd-walker {re.escape(VERSION) if VERSION else '[^ ]+'} \(commit ", out), out
    assert "linux/amd64" in out, out
    out2 = sh(f"{BIN} -version -host 203.0.113.1")  # must not try to connect
    assert out2 == out


def sc_header_version(port, proto):
    t = TUI(f"-host 127.0.0.1 -port {port} -protocol {proto}")
    try:
        h = t.header()
        assert f"Etcd-walker v.{VERSION or ''}" in h and f"protocol: {proto}" in h, h
        assert "LIVE" not in h, h
    finally:
        t.stop()


def put(proto, k, v, **kw):
    (v3put if proto == "v3" else v2put)(k, v, **kw)
def get(proto, k): return v3get(k) if proto == "v3" else v2get(k)
def delete(proto, k, recursive=False):
    if proto == "v3": v3del(k + ("/" if recursive else ""), prefix=recursive)
    else: v2del(k, recursive=recursive)
def port_of(proto): return V3 if proto == "v3" else V2


def sc_live_basic(proto):
    wipe()
    put(proto, "/app/a", "1"); put(proto, "/b", "2"); put(proto, "/m", "3")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        t.wait("📁 app/")
        # create appears without any key press
        put(proto, "/new-key", "hello")
        t.wait("new-key", timeout=4)
        # delete disappears
        delete(proto, "/new-key")
        t.wait("new-key", timeout=4, absent=True)
        # nested change below a visible dir: new top-level dir appears
        put(proto, "/zdir/deep/k", "v")
        t.wait("zdir/", timeout=4)
        # cursor stays on the same entry when a row is inserted above it
        select(t, "/m")
        assert t.focused_path() == "/m", t.screen()
        put(proto, "/a0", "x")
        t.wait("a0", timeout=4)
        time.sleep(0.5)
        assert t.focused_path() == "/m", "cursor moved off /m:\n" + t.screen()
        # focused key's value updates live in the details pane
        put(proto, "/m", "VALUE-CHANGED-LIVE")
        t.wait("VALUE-CHANGED-LIVE", timeout=4)
        # inside a directory
        t.keys("Home") ; goto(t, "/app/")
        t.wait("[ /app/ ]")
        put(proto, "/app/inner", "q")
        t.wait("inner", timeout=4)
        # sibling with shared string prefix must not matter, and directory
        # removed entirely while we are in it -> empty listing, no crash
        delete(proto, "/app", recursive=True)
        t.wait("inner", timeout=4, absent=True)
        assert t.alive()
    finally:
        t.stop()


def sc_live_toggle(proto):
    wipe()
    put(proto, "/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        t.wait("a")
        assert "LIVE" not in t.header()
        put(proto, "/offline-change", "x")
        time.sleep(1.5)
        assert "offline-change" not in t.screen(), "listing changed with live mode off"
        t.keys("C-l")
        t.wait("● LIVE")
        # changes made while live was off must show as soon as it is turned on
        t.wait("offline-change", timeout=3)
        t.keys("C-l")
        t.wait("LIVE", absent=True)
        put(proto, "/after-off", "x")
        time.sleep(1.5)
        assert "after-off" not in t.screen()
    finally:
        t.stop()


def sc_live_disconnect(proto):
    wipe()
    put(proto, "/a", "1")
    svc = "etcd-v3" if proto == "v3" else "etcd-v2"
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 2")
    try:
        t.wait("● LIVE")
        sh(f"cd {HERE} && docker compose stop {svc}")
        t.wait("⚠ LIVE", timeout=15)
        assert t.alive()
        sh(f"cd {HERE} && docker compose start {svc}")
        t.wait("● LIVE", timeout=25)
        put(proto, "/after-restart", "x")
        t.wait("after-restart", timeout=8)
    finally:
        sh(f"cd {HERE} && docker compose start {svc}", check=False)
        t.stop()


def edit_value(t, text):
    t.keys("C-e", delay=0.6)
    t.wait("Edit (multiline)")
    t.keys("End")
    t.type(text)


def sc_cas(proto):
    wipe()
    put(proto, "/k", "base")
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        select(t, "/k")
        # 1. clean save
        edit_value(t, "+mine1")
        t.keys("C-s", delay=0.8)
        t.wait("Edit (multiline)", absent=True)
        assert get(proto, "/k") == "base+mine1", get(proto, "/k")
        # 2. conflict -> diff -> back -> overwrite
        edit_value(t, "+mine2")
        put(proto, "/k", "theirs")
        t.keys("C-s", delay=0.8)
        t.wait("changed on the server")
        assert get(proto, "/k") == "theirs", "conflicting save was written"
        t.keys("Enter", delay=0.6)          # diff (first button)
        t.wait("conflict on /k")
        s = t.screen()
        assert "- theirs" in s and "+ base+mine1+mine2" in s, s
        t.keys("Escape", delay=0.5)
        t.wait("changed on the server")
        t.keys("Tab", "Enter", delay=0.6)   # overwrite
        t.wait("changed on the server", absent=True)
        assert get(proto, "/k") == "base+mine1+mine2", get(proto, "/k")
        # 3. conflict -> Esc keeps the text, editor reopened
        edit_value(t, "+mine3")
        put(proto, "/k", "theirs2")
        t.keys("C-s", delay=0.8)
        t.wait("changed on the server")
        t.keys("Escape", delay=0.6)
        t.wait("Edit (multiline)")
        assert "+mine3" in t.screen()
        # ... then reload: editor shows the server's value, edits dropped
        t.keys("C-s", delay=0.8)
        t.wait("changed on the server")
        t.keys("Tab", "Tab", "Enter", delay=0.6)   # reload
        t.wait("Edit (multiline)")
        s = t.screen()
        assert "theirs2" in s and "+mine3" not in s, s
        t.keys("Escape", delay=0.5)
        assert get(proto, "/k") == "theirs2"
        # 4. deleted while editing -> recreate
        edit_value(t, "+mine4")
        delete(proto, "/k")
        t.keys("C-s", delay=0.8)
        t.wait("was deleted on the server")
        t.keys("Enter", delay=0.8)                 # recreate
        t.wait("was deleted on the server", absent=True)
        assert get(proto, "/k") == "theirs2+mine4", get(proto, "/k")
        # 5. deleted while editing -> discard writes nothing
        edit_value(t, "+mine5")
        delete(proto, "/k")
        t.keys("C-s", delay=0.8)
        t.wait("was deleted on the server")
        t.keys("Tab", "Tab", "Enter", delay=0.6)   # discard
        time.sleep(0.5)
        assert get(proto, "/k") in ("", None), "discard resurrected the key"
    finally:
        t.stop()


def sc_cas_live(proto):
    """Live refresh while the editor is open must not defeat the CAS check."""
    wipe()
    put(proto, "/k", "base")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        select(t, "/k")
        edit_value(t, "+mine")
        put(proto, "/k", "theirs")
        time.sleep(1.5)  # let the live refresh re-read /k underneath the editor
        t.keys("C-s", delay=0.8)
        t.wait("changed on the server")
        assert get(proto, "/k") == "theirs"
        t.keys("Escape", delay=0.6)   # keep editing
        t.wait("Edit (multiline)")
        t.keys("Escape", delay=0.5)   # leave the editor without saving
    finally:
        t.stop()


def sc_cas_ttl(proto):
    wipe()
    if proto == "v3":
        lease = ctl(V3, "lease", "grant", "300").split()[1]
        ctl(V3, "put", "/t", "v", f"--lease={lease}")
    else:
        v2put("/t", "v", ttl=300)
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        select(t, "/t")
        t.wait("TTL")
        edit_value(t, "2")
        t.keys("C-s", delay=0.8)
        assert get(proto, "/t") == "v2"
        if proto == "v3":
            out = ctl(V3, "get", "/t", "-w", "json")
            assert '"lease"' in out, "lease dropped by CAS save: " + out
        else:
            assert v2req("GET", "/t")["node"].get("ttl"), "TTL dropped by CAS save"
    finally:
        t.stop()


def sc_dual_live(proto):
    wipe()
    put(proto, "/left/a", "1"); put(proto, "/right/b", "2")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -dual")
    try:
        t.wait("● LIVE")
        t.keys("Tab")
        goto(t, "/right/")
        t.keys("Tab")
        goto(t, "/left/")
        put(proto, "/right/new-right", "x")
        put(proto, "/left/new-left", "y")
        t.wait("new-right", timeout=4)
        t.wait("new-left", timeout=4)
        # F5 copy into the other pane, which is live: shows once, no dupes
        select(t, "/left/a")
        t.keys("F5", delay=0.8)
        t.keys("Enter")
        s = t.wait("[ /right/ ]")
        assert t.screen().count(" a ") <= 2
    finally:
        t.stop()


def sc_timetravel_live():
    wipe()
    v3put("/tt", "old")
    rev = json.loads(ctl(V3, "get", "/tt", "-w", "json"))["header"]["revision"]
    v3put("/tt", "new")
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        t.wait("● LIVE")
        t.keys("C-g", delay=0.5)
        t.type(str(rev))
        t.keys("Enter", delay=0.8)
        t.wait(f"rev {rev}")
        t.keys("Enter", delay=0.4)        # dismiss the "Revision N" info
        v3put("/added-later", "x")
        time.sleep(1.5)
        assert "added-later" not in t.screen(), "pinned pane was refreshed"
        t.keys("C-g", delay=0.5)
        t.keys("C-u", delay=0.3)          # clear the prefilled revision
        t.keys("Enter", delay=0.8)        # empty -> live
        t.wait("added-later", timeout=4)
    finally:
        t.stop()


def sc_auth():
    ctl(AUTH, "del", "", "--prefix", user="root:rootpw")
    ctl(AUTH, "put", "/s", "secret", user="root:rootpw")
    t = TUI(f"-port {AUTH} -protocol v3 -username root -password rootpw -live")
    try:
        t.wait("Auth: ON")
        t.wait("● LIVE")
        ctl(AUTH, "put", "/s2", "x", user="root:rootpw")
        t.wait("s2", timeout=4)
    finally:
        t.stop()
    # read-only RBAC user: browsing + live works, a save fails with a clear error
    t = TUI(f"-port {AUTH} -protocol v3 -username ro -password ropw -live")
    try:
        t.wait("● LIVE")
        ctl(AUTH, "put", "/s3", "x", user="root:rootpw")
        t.wait("s3", timeout=4)
        select(t, "/s")
        edit_value(t, "!")
        t.keys("C-s", delay=0.8)
        t.wait("permission")            # "permission denied" wraps in the modal
        assert v3get("/s", port=AUTH, user="root:rootpw") == "secret"
    finally:
        t.stop()
    # wrong password: startup error, no crash, Ctrl+Q exits
    t = TUI(f"-port {AUTH} -protocol v3 -username root -password nope -live")
    try:
        t.wait("authentication", timeout=8)
        assert "invalid user ID or password" in t.screen()
    finally:
        t.stop()


def sc_unreachable():
    t = TUI("-host 127.0.0.1 -port 1 -protocol v3 -live -timeout 1")
    try:
        t.wait("error", timeout=10)
        assert t.alive()
    finally:
        t.stop()


def sc_busy(proto):
    """A write storm must be coalesced; the UI must stay responsive."""
    wipe()
    put(proto, "/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        if proto == "v3":
            # The etcd 3.5 image has no shell; go through the gRPC gateway.
            import base64
            for i in range(100, 400):
                body = json.dumps({"key": base64.b64encode(f"/storm/k{i}".encode()).decode(),
                                   "value": base64.b64encode(f"v{i}".encode()).decode()}).encode()
                urllib.request.urlopen(urllib.request.Request(
                    f"http://127.0.0.1:{V3}/v3/kv/put", data=body, method="POST"), timeout=5).read()
        else:
            for i in range(100, 400):
                v2put(f"/storm/k{i}", f"v{i}")
        t.wait("storm/", timeout=5)
        goto(t, "/storm/")
        t.keys("End", delay=0.5)
        t.wait("k399", timeout=8)
        t.keys("Home", delay=0.5)
        t.wait("k100", timeout=4)
        # responsive: help opens and closes
        t.keys("C-h", delay=0.6)
        t.wait("Live refresh")
        t.keys("Escape")
        t.wait("Live refresh", absent=True)
    finally:
        t.stop()


def sc_v2_ttl_expiry():
    wipe()
    v2put("/exp", "x", ttl=3)
    v2put("/stay", "y")
    t = TUI(f"-port {V2} -protocol v2 -live")
    try:
        t.wait("exp")
        t.wait("exp", timeout=10, absent=True)
        assert "stay" in t.screen()
    finally:
        t.stop()


def sc_v3_lease_expiry():
    wipe()
    lease = ctl(V3, "lease", "grant", "3").split()[1]
    ctl(V3, "put", "/exp", "x", f"--lease={lease}")
    v3put("/stay", "y")
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        t.wait("exp")
        t.wait("exp", timeout=12, absent=True)
        assert "stay" in t.screen()
    finally:
        t.stop()


def sc_odd_keys(proto):
    wipe()
    put(proto, "/with space", "1")
    put(proto, "/ünïcødé", "2")
    put(proto, "/[bracket]", "3")
    put(proto, "/apple", "4")
    put(proto, "/app/x", "5")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        t.wait("with space"); t.wait("ünïcødé"); t.wait("[bracket]")
        goto(t, "/app/")
        t.wait("[ /app/ ]")
        put(proto, "/apple", "changed")     # sibling with shared prefix
        put(proto, "/app/y", "6")
        t.wait("y", timeout=4)
        assert "apple" not in t.screen()
    finally:
        t.stop()


def sc_readonly_dryrun_live(proto):
    wipe()
    put(proto, "/k", "v")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -read-only")
    try:
        t.wait("READ-ONLY")
        t.wait("● LIVE")
        put(proto, "/k2", "v")
        t.wait("k2", timeout=4)
    finally:
        t.stop()
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -dry-run")
    try:
        t.wait("DRY-RUN")
        select(t, "/k")
        edit_value(t, "X")
        t.keys("C-s", delay=0.8)
        assert get(proto, "/k") == "v", "dry-run wrote"
    finally:
        t.stop()



# ---------- round 2: corner cases -------------------------------------------

def sc_recreated_cluster():
    """The cluster is wiped and recreated (restore from backup, new volume):
    its revision goes BACKWARDS. A watch resuming at the old revision would
    wait for a future revision and miss everything — a frozen tree under a
    healthy LIVE header."""
    wipe()
    for i in range(30):
        v3put(f"/bump/k{i}", "x")   # push the revision well up
    v3put("/a", "1")
    t = TUI(f"-port {V3} -protocol v3 -live -timeout 2")
    try:
        t.wait("● LIVE")
        sh(f"cd {HERE} && docker compose rm -sf etcd-v3 && docker compose up -d etcd-v3")
        time.sleep(3)
        t.wait("● LIVE", timeout=25)
        v3put("/fresh-after-recreate", "x")
        t.wait("fresh-after-recreate", timeout=8)
        t.wait("bump/", timeout=4, absent=True)
    finally:
        t.stop()


def sc_paused_server(proto):
    """A hung (not refusing) server: ping must time out, UI stay responsive,
    and Ctrl+Q must still quit promptly."""
    wipe()
    put(proto, "/a", "1")
    svc = "etcd-v3" if proto == "v3" else "etcd-v2"
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 2")
    try:
        t.wait("● LIVE")
        sh(f"cd {HERE} && docker compose pause {svc}")
        t.wait("⚠ LIVE", timeout=20)
        t.keys("C-h", delay=0.6)
        t.wait("Live refresh", timeout=3)
        t.keys("Escape", delay=0.4)
        sh(f"cd {HERE} && docker compose unpause {svc}")
        t.wait("● LIVE", timeout=20)
        put(proto, "/after-unpause", "x")
        t.wait("after-unpause", timeout=8)
        # now quit while hung
        sh(f"cd {HERE} && docker compose pause {svc}")
        time.sleep(1)
        t0 = time.time()
        t.keys("C-q", delay=0.3)
        while t.alive() and time.time() - t0 < 5:
            time.sleep(0.2)
        assert not t.alive(), "Ctrl+Q did not quit while the server was hung"
    finally:
        sh(f"cd {HERE} && docker compose unpause {svc}", check=False)
        t.stop()


def sc_conflict_big_and_binary():
    wipe()
    big = "\n".join(f"line {i}" for i in range(3000))
    v3put("/big", big)
    t = TUI(f"-port {V3} -protocol v3")
    try:
        select(t, "/big")
        t.keys("C-e", delay=1.0)
        t.wait("Edit (multiline)")
        t.type("X")                             # caret starts at the top
        v3put("/big", big + "\ntheirs")
        t.keys("C-s", delay=1.0)
        t.wait("changed on the server")
        t0 = time.time()
        t.keys("Enter", delay=0.5)              # diff
        t.wait("conflict on /big", timeout=5)
        assert time.time() - t0 < 5
        assert "too large to diff" in t.screen() or "+++ your edit" in t.screen(), t.screen()
        t.keys("Escape", delay=0.5)
        t.keys("Tab", "Enter", delay=0.8)       # overwrite
        t.wait("changed on the server", absent=True)
        v = v3get("/big")
        assert v.startswith("Xline 0") and not v.endswith("theirs"), (v[:20], v[-20:])
    finally:
        t.stop()
    # binary value written via the gateway
    import base64
    body = json.dumps({"key": base64.b64encode(b"/bin").decode(),
                       "value": base64.b64encode(b"\xff\xfe\x00\x01abc").decode()}).encode()
    urllib.request.urlopen(urllib.request.Request(f"http://127.0.0.1:{V3}/v3/kv/put", data=body, method="POST")).read()
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        select(t, "/bin")
        edit_value(t, "Z")
        v3put("/bin", "text-now")
        t.keys("C-s", delay=1.0)
        t.wait("changed on the server")
        t.keys("Enter", delay=0.5)
        t.wait("conflict on /bin")
        assert "binary value" in t.screen(), t.screen()
        t.keys("Escape", delay=0.4)
        t.keys("Tab", "Tab", "Tab", "Enter", delay=0.6)   # keep editing
        t.wait("Edit (multiline)")
        t.keys("Escape")
    finally:
        t.stop()


def sc_protected_conflict():
    wipe()
    v3put("/prod/cfg", "v1")
    t = TUI(f"-port {V3} -protocol v3 -protect /prod")
    try:
        select(t, "/prod/cfg")
        edit_value(t, "+mine")
        v3put("/prod/cfg", "theirs")
        t.keys("C-s", delay=0.8)
        t.wait("prod")                        # typed confirmation prompt
        t.type("prod")
        t.keys("Enter", delay=0.8)
        t.wait("changed on the server")
        assert v3get("/prod/cfg") == "theirs"
        t.keys("Tab", "Enter", delay=0.8)     # overwrite: no second confirmation
        t.wait("changed on the server", absent=True)
        assert v3get("/prod/cfg") == "v1+mine", v3get("/prod/cfg")
    finally:
        t.stop()


def sc_refresh_during_dialog(proto):
    """A refresh landing while a dialog has the keyboard must not steal focus
    or drop what is being typed."""
    wipe()
    put(proto, "/target/deep", "x")
    put(proto, "/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        t.keys("C-j")
        t.type("/tar")
        put(proto, "/noise1", "x"); time.sleep(1.2)
        put(proto, "/noise2", "x"); time.sleep(1.2)
        t.type("get/")
        t.keys("Enter", delay=0.8)
        t.wait("[ /target/ ]")
        # and with the value editor open
        select(t, "/target/deep")
        edit_value(t, "-typed")
        for i in range(3):
            put(proto, f"/target/n{i}", "x"); time.sleep(0.7)
        t.type("-more")
        s = t.screen()
        assert "Edit (multiline)" in s and "x-typed-more" in s, s
        t.keys("C-s", delay=0.8)
        assert get(proto, "/target/deep") == "x-typed-more"
        t.wait("n2", timeout=4)
    finally:
        t.stop()


def sc_resize_and_toggle(proto):
    wipe()
    put(proto, "/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        for _ in range(20):
            t.keys("C-l", delay=0.05)
        time.sleep(0.5)
        assert "LIVE" not in t.header(), "even number of toggles must end off"
        t.keys("C-l")
        t.wait("● LIVE")
        for w, h in ((80, 24), (40, 12), (220, 60), (120, 30)):
            sh(f"tmux -L {SOCK} resize-window -t {t.name} -x {w} -y {h}")
            put(proto, f"/r{w}", "x")
            time.sleep(0.6)
            assert t.alive()
        t.wait("r120", timeout=4)
    finally:
        t.stop()


def sc_dual_toggle_hidden_pane(proto):
    wipe()
    put(proto, "/l/a", "1"); put(proto, "/r/b", "2")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -dual")
    try:
        t.wait("● LIVE")
        t.keys("Tab"); goto(t, "/r/"); t.keys("Tab")
        t.keys("C-b", delay=0.5)               # hide the right pane
        put(proto, "/r/while-hidden", "x")
        time.sleep(1)
        t.keys("C-b", delay=0.8)               # show it again
        t.wait("while-hidden", timeout=4)
        put(proto, "/r/after-show", "x")
        t.wait("after-show", timeout=4)
    finally:
        t.stop()


def sc_v2_hidden_injected():
    """v2 hides _-prefixed keys from listings; one created in-session is
    injected into the listing and must survive live refreshes."""
    wipe()
    v2put("/a", "1")
    t = TUI(f"-port {V2} -protocol v2 -live")
    try:
        t.wait("● LIVE")
        t.keys("C-n", delay=0.6)
        t.type("_hidden")
        t.keys("Tab"); t.type("hv")
        t.keys("Tab", "Tab", "Enter", delay=0.8)   # Save button
        t.wait("_hidden", timeout=4)
        v2put("/b", "2")
        t.wait(" b ", timeout=4)
        assert "_hidden" in t.screen(), "live refresh dropped the injected _hidden key"
    finally:
        t.stop()


def sc_watched_dir_deleted_parent():
    wipe()
    v3put("/p/q/r/k", "1")
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        goto(t, "/p/q/r/")
        t.wait("[ /p/q/r/ ]")
        v3del("/p/", prefix=True)
        t.wait(" k ", timeout=4, absent=True)
        v3put("/p/q/r/back", "1")             # recreated under the same watch
        t.wait("back", timeout=4)
        t.keys("BSpace", delay=0.6)
        t.wait("[ /p/q/ ]")
    finally:
        t.stop()


ROUND2 = [
    ("r2-recreated-cluster", sc_recreated_cluster),
    ("r2-paused-v3", lambda: sc_paused_server("v3")),
    ("r2-paused-v2", lambda: sc_paused_server("v2")),
    ("r2-conflict-big-binary", sc_conflict_big_and_binary),
    ("r2-protected-conflict", sc_protected_conflict),
    ("r2-dialog-refresh-v3", lambda: sc_refresh_during_dialog("v3")),
    ("r2-dialog-refresh-v2", lambda: sc_refresh_during_dialog("v2")),
    ("r2-resize-toggle-v3", lambda: sc_resize_and_toggle("v3")),
    ("r2-resize-toggle-v2", lambda: sc_resize_and_toggle("v2")),
    ("r2-dual-hidden-v3", lambda: sc_dual_toggle_hidden_pane("v3")),
    ("r2-dual-hidden-v2", lambda: sc_dual_toggle_hidden_pane("v2")),
    ("r2-v2-hidden-injected", sc_v2_hidden_injected),
    ("r2-deleted-parent", sc_watched_dir_deleted_parent),
]


# ---------- round 3: auth lifecycle, compaction, load ------------------------

def gateway_put(port, k, v):
    import base64
    body = json.dumps({"key": base64.b64encode(k.encode()).decode(),
                       "value": base64.b64encode(v.encode()).decode()}).encode()
    urllib.request.urlopen(urllib.request.Request(
        f"http://127.0.0.1:{port}/v3/kv/put", data=body, method="POST"), timeout=5).read()


def sc_auth_token_expiry():
    """Simple auth tokens expire (--auth-token-ttl, 5s here). The watch and
    the on-focus reads must keep working across expiry."""
    ctl(AUTH, "del", "", "--prefix", user="root:rootpw")
    ctl(AUTH, "put", "/a", "1", user="root:rootpw")
    t = TUI(f"-port {AUTH} -protocol v3 -username root -password rootpw -live")
    try:
        t.wait("Auth: ON")
        t.wait("● LIVE")
        for i in range(3):
            time.sleep(7)                      # > token TTL, no traffic but pings
            ctl(AUTH, "put", f"/after-expiry-{i}", "x", user="root:rootpw")
            t.wait(f"after-expiry-{i}", timeout=8)
        assert "⚠" not in t.header(), t.header()
    finally:
        t.stop()


def sc_permission_revoked():
    """The user's read permission is revoked mid-session: the header must say
    so instead of showing a healthy LIVE over a tree that can no longer load."""
    R = "root:rootpw"
    ctl(AUTH, "role", "add", "tmp", user=R)
    ctl(AUTH, "role", "grant-permission", "tmp", "--prefix=true", "read", "/", user=R)
    try: ctl(AUTH, "user", "add", "tmpu:tmppw", user=R)
    except RuntimeError: pass
    ctl(AUTH, "user", "grant-role", "tmpu", "tmp", user=R)
    t = TUI(f"-port {AUTH} -protocol v3 -username tmpu -password tmppw -live")
    try:
        t.wait("Auth: ON")
        t.wait("● LIVE")
        ctl(AUTH, "role", "revoke-permission", "tmp", "/", "--prefix=true", user=R)
        ctl(AUTH, "put", "/poke", "x", user=R)
        t.wait("⚠ LIVE", timeout=15)
        assert t.alive()
        ctl(AUTH, "role", "grant-permission", "tmp", "--prefix=true", "read", "/", user=R)
        t.wait("● LIVE", timeout=20)
        ctl(AUTH, "put", "/poke2", "x", user=R)
        t.wait("poke2", timeout=10)
    finally:
        try: ctl(AUTH, "user", "delete", "tmpu", user=R)
        except RuntimeError: pass
        try: ctl(AUTH, "role", "delete", "tmp", user=R)
        except RuntimeError: pass
        t.stop()


def sc_compaction_while_live():
    wipe()
    v3put("/c", "1")
    old = json.loads(ctl(V3, "get", "/c", "-w", "json"))["header"]["revision"]
    for i in range(5):
        v3put("/c", str(i))
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        t.wait("● LIVE")
        head = json.loads(ctl(V3, "get", "/c", "-w", "json"))["header"]["revision"]
        ctl(V3, "compact", str(head))
        v3put("/post-compact", "x")
        t.wait("post-compact", timeout=6)
        assert "⚠" not in t.header()
        t.keys("C-g", delay=0.5); t.type(str(old)); t.keys("Enter", delay=1.0)
        t.wait("compacted", timeout=4)
        t.keys("Enter", delay=0.4)
        assert "@ rev" not in t.screen(), "pane switched to a compacted revision"
        v3put("/still-live", "x")
        t.wait("still-live", timeout=6)
    finally:
        t.stop()


def sc_auto_protocol_live():
    wipe()
    v3put("/auto-a", "1")
    t = TUI(f"-port {V3} -protocol auto -live")
    try:
        t.wait("protocol: v3")
        t.wait("● LIVE")
        v3put("/auto-b", "2")
        t.wait("auto-b", timeout=4)
    finally:
        t.stop()


def sc_sustained_load(proto):
    """Continuous writes for ~10s: the UI must stay responsive (cursor moves
    land quickly) and the final state must be shown once the storm stops."""
    wipe()
    for i in range(20):
        put(proto, f"/load/k{i:02d}", "0")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        goto(t, "/load/")
        t.wait("k19")
        import threading
        stop = threading.Event()
        def writer():
            n = 0
            while not stop.is_set():
                if proto == "v3":
                    gateway_put(V3, f"/load/k{n % 20:02d}", str(n))
                else:
                    v2put(f"/load/k{n % 20:02d}", str(n))
                n += 1
        th = threading.Thread(target=writer); th.start()
        worst = 0.0
        try:
            t.keys("Home", delay=0.3)
            for i in range(1, 12):
                before = t.focused_path()
                t0 = time.time()
                sh(f"tmux -L {SOCK} send-keys -t {t.name} Down")
                while t.focused_path() == before and time.time() - t0 < 5:
                    time.sleep(0.05)
                worst = max(worst, time.time() - t0)
                assert t.focused_path() != before, f"cursor stuck on {before} under load"
        finally:
            stop.set(); th.join()
        assert worst < 2.0, f"slowest cursor move under load: {worst:.2f}s"
        put(proto, "/load/k00", "FINAL")
        t.keys("Home", delay=0.3); t.keys("Down", delay=0.5)
        t.wait("FINAL", timeout=4)
        print(f"   worst cursor latency under load ({proto}): {worst:.2f}s", flush=True)
    finally:
        t.stop()


ROUND3 = [
    ("r3-auth-token-expiry", sc_auth_token_expiry),
    ("r3-permission-revoked", sc_permission_revoked),
    ("r3-compaction-live", sc_compaction_while_live),
    ("r3-auto-protocol", sc_auto_protocol_live),
    ("r3-load-v3", lambda: sc_sustained_load("v3")),
    ("r3-load-v2", lambda: sc_sustained_load("v2")),
]


# ---------- gap scenarios (coverage review after F-1/N-1) ---------------------

def list_rows(t):
    """Entry names in the (first) list pane, without the [..] row."""
    rows = []
    for line in t.screen().splitlines():
        if len(line) > 2 and line[1] in "║│":
            end = line.find(line[1], 2)
            cell = line[2:end if end > 0 else None].strip()
            if cell.startswith("📁"):
                cell = cell[1:].strip()
            if cell and cell != "[..]" and not cell.startswith(("─", "═")):
                rows.append(cell)
    return rows


def sc_search_live(proto):
    """Quick search typed while a live refresh inserts rows above the target
    must still land on the target (stale-ordering bug, gap review)."""
    wipe()
    for k in ("/m1", "/m2", "/target"):
        put(proto, k, "x")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        t.keys("/")
        t.type("target")
        for k in ("/a1", "/a2", "/a3"):
            put(proto, k, "x")
        time.sleep(1.5)                   # refreshes land under the open dialog
        t.keys("Escape", delay=0.3)       # close the autocomplete dropdown
        t.keys("Enter", delay=0.6)
        assert t.focused_path() == "/target", f"search landed on {t.focused_path()}\n{t.screen()}"
    finally:
        t.stop()


def sc_ttl_dialog_live(proto):
    wipe()
    put(proto, "/t", "v"); put(proto, "/stay", "s")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        select(t, "/t")
        t.keys("C-t", delay=0.5)
        t.type("4")
        t.keys("Tab", "Enter", delay=0.8)  # Save
        t.wait("TTL", timeout=3)
        assert get(proto, "/t") == "v", "TTL change altered the value"
        t.wait(" t ", timeout=3)
        deadline = time.time() + 15
        while "t" in list_rows(t) and time.time() < deadline:
            time.sleep(0.3)
        assert "t" not in list_rows(t), f"expired key still listed: {list_rows(t)}"
        assert "stay" in list_rows(t)
    finally:
        t.stop()


def sc_ui_mutations_live(proto):
    """Create/rename/duplicate/delete from the UI with live refresh racing
    every write: no duplicate rows, no ghosts, listing == server."""
    wipe()
    put(proto, "/seed", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        t.keys("C-n", delay=0.6)
        t.type("created"); t.keys("Tab"); t.type("cv")
        t.keys("Tab", "Tab", "Enter", delay=1.2)
        t.wait("created")
        time.sleep(1.2)
        assert list_rows(t).count("created") == 1, list_rows(t)
        assert get(proto, "/created") == "cv"

        select(t, "/created")
        t.keys("C-r", delay=0.6)
        t.keys("C-u"); t.type("renamed")
        t.keys("Tab", "Enter", delay=1.2)
        time.sleep(1.2)
        rows = list_rows(t)
        assert "renamed" in rows and "created" not in rows, rows

        select(t, "/renamed")
        t.keys("C-d", delay=0.6)
        t.keys("Tab", "Enter", delay=1.2)
        t.keys("Escape", delay=0.3)        # dismiss "Copied"
        time.sleep(1.2)
        rows = list_rows(t)
        assert rows.count("renamed-copy") == 1 and rows.count("renamed") == 1, rows

        select(t, "/renamed-copy")
        t.keys("DC", delay=0.6)            # Delete
        t.keys("Enter", delay=1.0)         # ok
        t.keys("Escape", delay=0.3)        # dismiss "Deleted"
        time.sleep(1.2)
        rows = list_rows(t)
        assert "renamed-copy" not in rows, rows
        assert sorted(rows) == ["renamed", "seed"], rows
    finally:
        t.stop()


def sc_delete_dir_undo_live(proto):
    wipe()
    for i in range(3):
        put(proto, f"/victim/k{i}", f"v{i}")
    put(proto, "/zkeep", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("victim/")
        t.keys("Home", "Down", delay=0.4)
        assert t.focused_path() == "/victim", t.focused_path()
        t.keys("DC", delay=0.6)
        t.wait("victim")                   # typed confirmation
        t.type("victim")
        t.keys("Enter", delay=1.2)
        t.keys("Escape", delay=0.3)
        t.wait("victim/", absent=True, timeout=4)
        t.keys("C-u", delay=0.6)
        t.wait("undo snapshots")
        t.keys("Enter", delay=0.6)         # restore newest
        t.keys("Enter", delay=1.0)         # overwrite
        t.keys("Escape", delay=0.4)        # dismiss result
        t.wait("victim/", timeout=4)
        assert get(proto, "/victim/k2") == "v2"
    finally:
        t.stop()


def sc_config_file_live():
    wipe()
    v3put("/cfg-a", "1")
    cfg = os.path.join(STATE, "e2e-config.json")
    with open(cfg, "w") as f:
        json.dump({"host": "127.0.0.1", "port": V3, "protocol": "v3", "live": True}, f)
    t = TUI(f"-config {cfg}")
    try:
        t.wait("● LIVE")
        t.wait("protocol: v3")
        v3put("/cfg-b", "2")
        t.wait("cfg-b", timeout=4)
    finally:
        t.stop()
    t = TUI(f"-config {cfg} -live=false")
    try:
        t.wait("cfg-b")
        assert "LIVE" not in t.header(), "-live=false did not override the config file"
    finally:
        t.stop()


def sc_rapid_nav_live():
    wipe()
    v3put("/n1/n2/n3/k", "x")
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        t.wait("● LIVE")
        for _ in range(12):
            goto(t, "/n1/n2/n3/")
            t.keys("BSpace", "BSpace", delay=0.05)
        time.sleep(0.8)
        t.wait("[ /n1/ ]")
        v3put("/elsewhere", "x")           # outside the current dir
        v3put("/n1/fresh", "x")
        t.wait("fresh", timeout=4)
        assert "elsewhere" not in t.screen()
        assert "⚠" not in t.header(), t.header()
    finally:
        t.stop()


def sc_v2_dir_ttl():
    wipe()
    v2req("PUT", "/tdir", dir="true", ttl="3")
    v2put("/tdir/k", "x")
    v2put("/keep", "y")
    t = TUI(f"-port {V2} -protocol v2 -live")
    try:
        t.wait("tdir/")
        t.wait("tdir/", timeout=12, absent=True)
        assert "keep" in list_rows(t)
    finally:
        t.stop()


def sc_journal_after_cas(proto):
    wipe()
    put(proto, "/j", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        select(t, "/j")
        edit_value(t, "2")
        t.keys("C-s", delay=0.8)
        t.keys("C-a", delay=0.6)
        t.wait("edit /j")
        t.keys("Escape")
    finally:
        t.stop()


def sc_history_restore_live():
    wipe()
    for v in ("v1", "v2", "v3"):
        v3put("/h", v)
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        select(t, "/h")
        t.keys("C-v", delay=0.8)
        t.keys("Down", "Down", "Enter", delay=0.5)   # oldest revision
        t.wait("v1")
        t.type("r")
        t.wait("restore")
        t.keys("Enter", delay=1.0)                   # confirm
        t.keys("Enter", delay=0.5)                   # dismiss "Restored"
        assert v3get("/h") == "v1", v3get("/h")
        t.keys("Escape", delay=0.3)
        t.wait("v1", timeout=4)
    finally:
        t.stop()


def sc_big_dir_live():
    wipe()
    for i in range(3000):
        gateway_put(V3, f"/big/k{i:04d}", "x")
    t = TUI(f"-port {V3} -protocol v3 -live")
    try:
        goto(t, "/big/")
        t.keys("End", delay=0.5)
        t.wait("k2999", timeout=6)
        t0 = time.time()
        gateway_put(V3, "/big/k9999", "x")
        t.keys("End", delay=0.2)
        deadline = time.time() + 6
        while "k9999" not in t.screen() and time.time() < deadline:
            t.keys("End", delay=0.2)
        assert "k9999" in t.screen(), t.screen()
        print(f"   3000-key dir: change visible after {time.time() - t0:.2f}s", flush=True)
    finally:
        t.stop()


def tls_args(ca="ca.crt", cert="client.crt", key="client.key", extra=""):
    a = f"-host 127.0.0.1 -port {TLS} -protocol v3 -tls {extra}"
    if ca: a += f" -tls-ca {CERTS}/{ca}"
    if cert: a += f" -tls-cert {CERTS}/{cert} -tls-key {CERTS}/{key}"
    return a


def sc_tls():
    ctl(TLS, "del", "", "--prefix")
    ctl(TLS, "put", "/tls-a", "1")
    t = TUI(tls_args(extra="-live"))
    try:
        t.wait("[TLS]")
        t.wait("● LIVE")
        ctl(TLS, "put", "/tls-b", "2")
        t.wait("tls-b", timeout=4)
        select(t, "/tls-a")
        edit_value(t, "+mine")
        ctl(TLS, "put", "/tls-a", "theirs")
        t.keys("C-s", delay=0.8)
        t.wait("changed on the server")
        t.keys("Tab", "Enter", delay=0.8)
        assert ctl(TLS, "get", "/tls-a", "--print-value-only").strip() == "1+mine"
    finally:
        t.stop()
    # negative: server cert from a CA we do not trust
    t = TUI(tls_args(ca="rogue.crt", extra="-timeout 2"))
    try:
        t.wait("error", timeout=12)
        # The real cause, not just "context deadline exceeded".
        assert "certificate" in t.screen().lower(), t.screen()
    finally:
        t.stop()
    # negative: no client certificate on an mTLS-only server
    t = TUI(tls_args(cert=None, extra="-timeout 2"))
    try:
        t.wait("error", timeout=12)
        s = t.screen().lower()
        assert "tls:" in s and "certificate" in s, "missing client cert not explained:\n" + t.screen()
    finally:
        t.stop()
    # skip-verify with a client cert works (no CA given)
    t = TUI(tls_args(ca=None, extra="-tls-skip-verify -live"))
    try:
        t.wait("● LIVE", timeout=8)
        t.wait("tls-b")
    finally:
        t.stop()


def sc_move_between_live_panes(proto):
    wipe()
    put(proto, "/src/mv", "1"); put(proto, "/src/stay", "2"); put(proto, "/dst/other", "3")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -dual")
    try:
        t.wait("● LIVE")
        t.keys("Tab"); goto(t, "/dst/"); t.keys("Tab")
        select(t, "/src/mv")
        t.keys("F6", delay=1.0)
        t.keys("Enter", delay=0.5)
        time.sleep(1.2)
        s = t.screen()
        assert get(proto, "/dst/mv") == "1" and get(proto, "/src/mv") in ("", None)
        left = [l.split("│")[0] for l in s.splitlines()]
        assert s.count(" mv ") == 1, f"expected mv exactly once (right pane):\n{s}"
    finally:
        t.stop()


def sc_special_chars(proto):
    """Text that is hostile to shells and to tview's tag parser must be saved
    byte for byte, and shown in the listing and details intact."""
    wipe()
    put(proto, "/sp", "x")
    tricky = 'a $HOME `id` \\n "q" \'s\' [red]tag[-] ünï ✓ %d {{x}} end;'
    t = TUI(f"-port {port_of(proto)} -protocol {proto}")
    try:
        select(t, "/sp")
        edit_value(t, tricky)
        t.keys("C-s", delay=1.0)
        assert get(proto, "/sp") == "x" + tricky, repr(get(proto, "/sp"))
        t.wait("[red]tag[-]", timeout=4)          # shown literally in the preview
    finally:
        t.stop()


GAPS = [("g-config-live", sc_config_file_live), ("g-rapid-nav", sc_rapid_nav_live),
        ("g-v2-dir-ttl", sc_v2_dir_ttl), ("g-history-restore", sc_history_restore_live),
        ("g-big-dir", sc_big_dir_live), ("g-tls", sc_tls)]
for _p in ("v3", "v2"):
    GAPS += [
        (f"g-search-live-{_p}", lambda p=_p: sc_search_live(p)),
        (f"g-ttl-dialog-{_p}", lambda p=_p: sc_ttl_dialog_live(p)),
        (f"g-ui-mutations-{_p}", lambda p=_p: sc_ui_mutations_live(p)),
        (f"g-delete-undo-{_p}", lambda p=_p: sc_delete_dir_undo_live(p)),
        (f"g-journal-{_p}", lambda p=_p: sc_journal_after_cas(p)),
        (f"g-move-panes-{_p}", lambda p=_p: sc_move_between_live_panes(p)),
        (f"g-special-chars-{_p}", lambda p=_p: sc_special_chars(p)),
    ]


# ---------- network faults (EW_FAULTS=1; needs root: tc/netem via nsenter) ----

def netem(svc, spec):
    """Apply a netem qdisc to a cluster's container interface ("" clears)."""
    cid = sh(f"cd {HERE} && docker compose ps -q {svc}").strip()
    pid = sh(f"docker inspect -f '{{{{.State.Pid}}}}' {cid}").strip()
    sh(f"nsenter -t {pid} -n tc qdisc del dev eth0 root 2>/dev/null", check=False)
    if spec:
        sh(f"nsenter -t {pid} -n tc qdisc add dev eth0 root netem {spec}")


def svc_of(proto): return "etcd-v3" if proto == "v3" else "etcd-v2"


def sc_fault_latency(proto):
    """300 ms each way (~600 ms RTT): cursor moves, live updates and a
    conflict-checked save all still work, just slower."""
    wipe()
    for i in range(6):
        put(proto, f"/lat/k{i}", str(i))
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 5")
    try:
        goto(t, "/lat/")
        t.wait("k5")
        netem(svc_of(proto), "delay 300ms")
        t.keys("Home", delay=0.3)
        worst = 0.0
        for _ in range(4):
            before = t.focused_path(); t0 = time.time()
            sh(f"tmux -L {SOCK} send-keys -t {t.name} Down")
            while t.focused_path() == before and time.time() - t0 < 8:
                time.sleep(0.05)
            worst = max(worst, time.time() - t0)
        put(proto, "/lat/new", "x")
        t.wait("new", timeout=8)
        select(t, "/lat/k1")
        edit_value(t, "+slow")
        t.keys("C-s", delay=3.0)
        t.wait("Edit (multiline)", absent=True, timeout=8)
        assert get(proto, "/lat/k1") == "1+slow", get(proto, "/lat/k1")
        assert "⚠" not in t.header(), t.header()
        print(f"   {proto} worst cursor move at 600ms RTT: {worst:.2f}s", flush=True)
    finally:
        netem(svc_of(proto), "")
        t.stop()


def sc_fault_loss(proto):
    """30% packet loss for a while: the session survives and converges."""
    wipe()
    put(proto, "/loss/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 3")
    try:
        goto(t, "/loss/")
        t.wait("● LIVE")
        netem(svc_of(proto), "loss 30%")
        for i in range(5):
            put(proto, f"/loss/k{i}", "x")
            time.sleep(1)
        netem(svc_of(proto), "")
        put(proto, "/loss/after", "x")
        t.wait("after", timeout=15)
        t.wait("k4", timeout=5)
        t.wait("● LIVE", timeout=15)
    finally:
        netem(svc_of(proto), "")
        t.stop()


def sc_fault_blackhole(proto):
    """100% loss — packets vanish, no RST: the nastiest outage. The header
    must turn red, a save must fail without losing the text, and everything
    must recover once the network does."""
    wipe()
    put(proto, "/bh/k", "orig")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 2")
    try:
        select(t, "/bh/k")
        t.wait("● LIVE")
        edit_value(t, "+precious")
        netem(svc_of(proto), "loss 100%")
        t.keys("C-s", delay=1.0)
        t.wait("Failed to save", timeout=20)
        t.wait("⚠ LIVE", timeout=20)
        t.keys("Enter", delay=0.8)              # dismiss -> editor comes back
        t.wait("Edit (multiline)")
        assert "orig+precious" in t.screen(), "text lost after a failed save:\n" + t.screen()
        netem(svc_of(proto), "")
        # The editor is full-screen (no header), so measure recovery by
        # retrying the save until it lands — same CAS base every time.
        t0 = time.time()
        while True:
            t.keys("C-s", delay=1.0)
            deadline = time.time() + 10
            while time.time() < deadline and "Failed to save" not in t.screen() \
                    and "Edit (multiline)" in t.screen():
                time.sleep(0.2)
            if "Failed to save" in t.screen():
                assert time.time() - t0 < 60, "save still failing 60s after the network came back"
                t.keys("Enter", delay=1.0)      # back to the editor, text intact
                assert "orig+precious" in t.screen()
                time.sleep(2)
                continue
            break
        print(f"   {proto} save succeeded {time.time() - t0:.1f}s after the blackhole lifted", flush=True)
        assert get(proto, "/bh/k") == "orig+precious", get(proto, "/bh/k")
        t.wait("● LIVE", timeout=25)
        put(proto, "/bh/after", "x")
        t.wait("after", timeout=10)
    finally:
        netem(svc_of(proto), "")
        t.stop()


def sc_fault_blackhole_startup():
    """Starting against a blackholed cluster must fail within the timeout and
    say so — not hang forever on a blank screen."""
    netem("etcd-v3", "loss 100%")
    t = TUI(f"-port {V3} -protocol v3 -live -timeout 2")
    try:
        t0 = time.time()
        t.wait("error", timeout=20)
        print(f"   startup against a blackhole failed after {time.time() - t0:.1f}s", flush=True)
        assert t.alive()
    finally:
        netem("etcd-v3", "")
        t.stop()


def sc_fault_flapping(proto):
    """The link flaps every second for 15 s while writes continue: no crash,
    no stuck red header, and the final state is shown."""
    wipe()
    put(proto, "/flap/a", "0")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 2")
    try:
        goto(t, "/flap/")
        t.wait("● LIVE")
        for i in range(8):
            netem(svc_of(proto), "loss 100%"); time.sleep(1)
            netem(svc_of(proto), ""); put(proto, "/flap/a", str(i)); time.sleep(1)
        put(proto, "/flap/final", "x")
        t.wait("final", timeout=20)
        t.wait("● LIVE", timeout=20)
        select(t, "/flap/a")
        t.wait("7", timeout=5)
    finally:
        netem(svc_of(proto), "")
        t.stop()


FAULTS = [("f-blackhole-startup", sc_fault_blackhole_startup)]
for _p in ("v3", "v2"):
    FAULTS += [
        (f"f-latency-{_p}", lambda p=_p: sc_fault_latency(p)),
        (f"f-loss-{_p}", lambda p=_p: sc_fault_loss(p)),
        (f"f-blackhole-{_p}", lambda p=_p: sc_fault_blackhole(p)),
        (f"f-flapping-{_p}", lambda p=_p: sc_fault_flapping(p)),
    ]
if not os.environ.get("EW_FAULTS"):
    FAULTS = []


# ---------- round B: long outages, state changing during them ----------------

def put_inside(proto, k, v):
    """Write from inside the cluster's container — bypasses its (impaired)
    network, so the data changes while the client is cut off."""
    if proto == "v3":
        ctl(V3, "put", k, v)
    else:
        sh(f"cd {HERE} && docker compose exec -T -e ETCDCTL_API=2 etcd-v2 etcdctl --endpoints=http://127.0.0.1:2379 --no-sync set {k} {v} >/dev/null")


def sc_long_blackhole(proto):
    """75 s of silence — long enough for TCP retransmit back-off to grow — with
    writes landing on the server meanwhile. After the network returns, the
    header must recover and the writes made during the outage must show."""
    wipe()
    put(proto, "/lb/a", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 2")
    try:
        goto(t, "/lb/")
        t.wait("● LIVE")
        netem(svc_of(proto), "loss 100%")
        t.wait("⚠ LIVE", timeout=20)
        for i in range(3):
            put_inside(proto, f"/lb/during{i}", "x")
        time.sleep(60)
        netem(svc_of(proto), "")
        t0 = time.time()
        t.wait("during2", timeout=40)
        t.wait("● LIVE", timeout=20)
        print(f"   {proto} outage writes visible {time.time() - t0:.1f}s after a 75s blackhole", flush=True)
        put(proto, "/lb/after", "x")
        t.wait("after", timeout=10)
    finally:
        netem(svc_of(proto), "")
        t.stop()


def sc_long_blackhole_nolive():
    """The same 75 s blackhole without live mode (no watch stream, so no
    keepalive traffic): after the network returns, re-entering the directory
    must show the outage's writes within a bounded time."""
    wipe()
    v3put("/nl/a", "1")
    t = TUI(f"-port {V3} -protocol v3 -timeout 2")
    try:
        goto(t, "/nl/")
        t.wait(" a ")
        netem("etcd-v3", "loss 100%")
        put_inside("v3", "/nl/during", "x")
        time.sleep(75)
        netem("etcd-v3", "")
        t0 = time.time()
        while time.time() - t0 < 90:
            t.keys("Escape", delay=0.2)
            goto(t, "/nl/")
            if "during" in t.screen():
                break
            time.sleep(2)
        took = time.time() - t0
        assert "during" in t.screen(), f"no recovery {took:.0f}s after the outage:\n{t.screen()}"
        print(f"   non-live v3 usable again {took:.1f}s after a 75s blackhole", flush=True)
    finally:
        netem("etcd-v3", "")
        t.stop()


def sc_compaction_during_outage():
    """The watch's resume point is compacted away while the client is cut off:
    it must fall back to a full re-read, not error forever or go silent."""
    wipe()
    v3put("/co/a", "0")
    t = TUI(f"-port {V3} -protocol v3 -live -timeout 2")
    try:
        goto(t, "/co/")
        t.wait("● LIVE")
        netem("etcd-v3", "loss 100%")
        t.wait("⚠ LIVE", timeout=20)
        for i in range(20):
            put_inside("v3", "/co/a", str(i))
        put_inside("v3", "/co/new-during", "x")
        head = json.loads(ctl(V3, "get", "/co/a", "-w", "json"))["header"]["revision"]
        ctl(V3, "compact", str(head))
        netem("etcd-v3", "")
        t.wait("new-during", timeout=40)
        t.wait("● LIVE", timeout=20)
        v3put("/co/after", "x")
        t.wait("after", timeout=10)
    finally:
        netem("etcd-v3", "")
        t.stop()


def sc_rtt_beyond_timeout(proto):
    """RTT above the operation timeout: reads fail, the focused row is marked
    (cached), the header goes red — and once the link is healthy again the
    marker and the warning both clear."""
    wipe()
    put(proto, "/slow/a", "1"); put(proto, "/slow/b", "2")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live -timeout 1")
    try:
        select(t, "/slow/a")
        netem(svc_of(proto), "delay 1500ms")
        t.keys("Down", delay=4)                    # focus /slow/b over a dead-slow link
        t.wait("(cached)", timeout=10)
        t.wait("⚠ LIVE", timeout=20)
        t.keys("Escape")
        netem(svc_of(proto), "")
        t.wait("● LIVE", timeout=25)
        t.wait("(cached)", absent=True, timeout=10)
    finally:
        netem(svc_of(proto), "")
        t.stop()


def sc_sigterm(proto):
    put(proto, "/sig", "x")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        t.wait("● LIVE")
        pid = sh(f"tmux -L {SOCK} list-panes -t {t.name} -F '#{{pane_pid}}'").strip()
        sh(f"pkill -TERM -P {pid} etcd-walker || kill -TERM {pid}", check=False)
        deadline = time.time() + 5
        while t.alive() and time.time() < deadline:
            time.sleep(0.1)
        assert not t.alive(), "SIGTERM did not stop the app"
        err = open(t.err).read()
        assert "panic" not in err, err
    finally:
        sh(f"tmux -L {SOCK} kill-session -t {t.name} 2>/dev/null", check=False)


def sc_extreme_keys(proto):
    wipe()
    long_name = "L" * 300
    deep = "/" + "/".join(f"d{i}" for i in range(40))
    put(proto, f"/x/{long_name}", "v")
    put(proto, deep + "/leaf", "deep-value")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        goto(t, deep + "/")
        t.wait("leaf", timeout=6)
        put(proto, deep + "/leaf2", "x")
        t.wait("leaf2", timeout=6)
        goto(t, "/x/")
        t.wait("LLLLLLLLLL", timeout=6)
        goto(t, f"/x/{long_name}")          # the path wraps in the details pane
        t.keys("C-e", delay=0.8)
        t.wait("Edit (multiline)")
        t.keys("Escape")
    finally:
        t.stop()


def sc_dir_becomes_key(proto):
    """The directory on screen is deleted and a *key* of the same name created
    in its parent: the pane must not wedge, and the parent must show the key."""
    wipe()
    put(proto, "/shape/x/inner", "1")
    t = TUI(f"-port {port_of(proto)} -protocol {proto} -live")
    try:
        goto(t, "/shape/x/")
        t.wait("inner")
        delete(proto, "/shape/x", recursive=True)
        put(proto, "/shape/x", "now-a-key")
        t.wait("inner", absent=True, timeout=6)
        t.keys("BSpace", delay=0.8)
        t.wait("[ /shape/ ]")
        rows = list_rows(t)
        assert rows == ["x"], rows
        select(t, "/shape/x")
        t.wait("now-a-key", timeout=4)
    finally:
        t.stop()


ROUNDB = [("b-compaction-outage", sc_compaction_during_outage),
          ("b-long-blackhole-nolive-v3", sc_long_blackhole_nolive)]
for _p in ("v3", "v2"):
    ROUNDB += [
        (f"b-long-blackhole-{_p}", lambda p=_p: sc_long_blackhole(p)),
        (f"b-rtt-beyond-timeout-{_p}", lambda p=_p: sc_rtt_beyond_timeout(p)),
    ]
if not os.environ.get("EW_FAULTS"):
    ROUNDB = []
for _p in ("v3", "v2"):
    ROUNDB += [
        (f"b-sigterm-{_p}", lambda p=_p: sc_sigterm(p)),
        (f"b-extreme-keys-{_p}", lambda p=_p: sc_extreme_keys(p)),
        (f"b-dir-becomes-key-{_p}", lambda p=_p: sc_dir_becomes_key(p)),
    ]

SCENARIOS = [
    ("version-flag", sc_version),
    ("header-v3", lambda: sc_header_version(V3, "v3")),
    ("header-v2", lambda: sc_header_version(V2, "v2")),
]
for proto in ("v3", "v2"):
    SCENARIOS += [
        (f"live-basic-{proto}", lambda p=proto: sc_live_basic(p)),
        (f"live-toggle-{proto}", lambda p=proto: sc_live_toggle(p)),
        (f"cas-{proto}", lambda p=proto: sc_cas(p)),
        (f"cas-live-{proto}", lambda p=proto: sc_cas_live(p)),
        (f"cas-ttl-{proto}", lambda p=proto: sc_cas_ttl(p)),
        (f"dual-live-{proto}", lambda p=proto: sc_dual_live(p)),
        (f"busy-{proto}", lambda p=proto: sc_busy(p)),
        (f"odd-keys-{proto}", lambda p=proto: sc_odd_keys(p)),
        (f"ro-dry-{proto}", lambda p=proto: sc_readonly_dryrun_live(p)),
        (f"disconnect-{proto}", lambda p=proto: sc_live_disconnect(p)),
    ]
SCENARIOS += [
    ("timetravel-live", sc_timetravel_live),
    ("auth", sc_auth),
    ("unreachable", sc_unreachable),
    ("v2-ttl-expiry", sc_v2_ttl_expiry),
    ("v3-lease-expiry", sc_v3_lease_expiry),
] + ROUND2 + ROUND3 + GAPS + FAULTS + ROUNDB


def ensure_auth():
    """Idempotently enable auth on the auth cluster (a recreated container
    loses its data, and with it the users and auth itself)."""
    r = subprocess.run(f"cd {HERE} && docker compose exec -T etcd-auth etcdctl auth status",
                       shell=True, capture_output=True, text=True)
    if "Authentication Status: true" in r.stdout or "user name is empty" in r.stderr:
        return
    for cmd in ("user add root:rootpw", "user grant-role root root", "user add ro:ropw",
                "role add reader", "role grant-permission reader --prefix=true read /",
                "user grant-role ro reader", "auth enable"):
        sh(f"cd {HERE} && docker compose exec -T etcd-auth etcdctl {cmd}")


def main():
    os.makedirs(STATE, exist_ok=True)
    ensure_auth()
    filt = sys.argv[1:]
    fails = 0
    for name, fn in SCENARIOS:
        if filt and not any(f in name for f in filt):
            continue
        t0 = time.time()
        try:
            fn()
            print(f"PASS {name} ({time.time()-t0:.1f}s)", flush=True)
        except Exception:
            fails += 1
            print(f"FAIL {name} ({time.time()-t0:.1f}s)\n" + traceback.format_exc(), flush=True)
            sh(f"tmux -L {SOCK} kill-server 2>/dev/null", check=False)
    print(f"\n{fails} failed")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
