#!/usr/bin/env python3
"""Build a FreeBSD binary package (.pkg) for etcd-walker without FreeBSD.

A pkg(8) package is a compressed tar whose first two members are
+COMPACT_MANIFEST and +MANIFEST (JSON, a subset of UCL), followed by the
payload under its absolute install path. `pkg create` is only available on
FreeBSD, but the format is simple and stable, so the Makefile writes it
directly from the cross-compiled static binary:

    mkpkg.py --version 0.11.0 --arch amd64 --binary dist/... \
             --license LICENSE --out build/etcd-walker-0.11.0-freebsd-amd64.pkg

Install on FreeBSD with `pkg add ./etcd-walker-…pkg`.
"""
import argparse, hashlib, io, json, lzma, os, tarfile, time

PREFIX = "/usr/local"
# Go arch -> (pkg ABI arch, legacy "arch" field). The OS major version is a
# wildcard: the binary is static and runs on every supported release, and
# pkg(8) matches the ABI with fnmatch.
ARCHES = {
    "amd64": ("amd64", "x86:64"),
    "arm64": ("aarch64", "aarch64:64"),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--arch", required=True, choices=sorted(ARCHES))
    ap.add_argument("--binary", required=True)
    ap.add_argument("--license", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    abi_arch, legacy_arch = ARCHES[a.arch]
    # pkg versions may not contain '-' (it separates name and version in a
    # package file name); 0.12.0-rc1 becomes 0.12.0.rc1.
    version = a.version.replace("-", ".")
    payload = {
        f"{PREFIX}/bin/etcd-walker": (open(a.binary, "rb").read(), 0o755),
        f"{PREFIX}/share/licenses/etcd-walker/LICENSE": (open(a.license, "rb").read(), 0o644),
    }

    compact = {
        "name": "etcd-walker",
        "origin": "sysutils/etcd-walker",
        "version": version,
        "comment": "Terminal UI for browsing and editing etcd (v2 and v3)",
        "maintainer": "nexus.riot@gmail.com",
        "www": "https://github.com/nexusriot/etcd-walker",
        "abi": f"FreeBSD:*:{abi_arch}",
        "arch": f"freebsd:*:{legacy_arch}",
        "prefix": PREFIX,
        "flatsize": sum(len(d) for d, _ in payload.values()),
        "licenselogic": "single",
        "licenses": ["MIT"],
        "desc": ("etcd-walker is a keyboard-driven terminal UI for etcd: browse the key\n"
                 "space as a tree over the v3 or v2 API, edit values with conflict-safe\n"
                 "saves, view revision history, refresh live via watches."),
        "categories": ["sysutils"],
        "deps": {},
    }
    full = dict(compact)
    # "1$" marks the hash as hex SHA-256 (pkg's PKG_HASH_TYPE_SHA256_HEX).
    full["files"] = {p: "1$" + hashlib.sha256(d).hexdigest() for p, (d, _) in payload.items()}

    now = int(os.environ.get("SOURCE_DATE_EPOCH", time.time()))
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tf:
        def add(name, data, mode):
            ti = tarfile.TarInfo(name)
            ti.size, ti.mode, ti.mtime = len(data), mode, now
            ti.uid = ti.gid = 0
            ti.uname, ti.gname = "root", "wheel"
            tf.addfile(ti, io.BytesIO(data))
        add("+COMPACT_MANIFEST", json.dumps(compact, indent=1).encode(), 0o644)
        add("+MANIFEST", json.dumps(full, indent=1).encode(), 0o644)
        for path, (data, mode) in payload.items():
            add(path, data, mode)  # absolute, as pkg-create writes them
    os.makedirs(os.path.dirname(a.out) or ".", exist_ok=True)
    with open(a.out, "wb") as f:
        f.write(lzma.compress(buf.getvalue(), format=lzma.FORMAT_XZ))
    print(">>", a.out)


if __name__ == "__main__":
    main()
