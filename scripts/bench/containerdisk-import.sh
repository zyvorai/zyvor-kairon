#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Wraps a qcow2/raw file as a KubeVirt containerDisk (disk/disk.img, owned
# by uid 107) and imports it into the node's containerd, so both benchmark
# sides boot the same bytes without a registry.
#
#   containerdisk-import.sh IMAGE_FILE [REF]   (default REF localhost/bench/disk:latest)
#
# CTR overrides the import command (default: sudo k3s ctr -n k8s.io).
set -euo pipefail

FILE="${1:?usage: containerdisk-import.sh IMAGE_FILE [REF]}"
REF="${2:-localhost/bench/disk:latest}"
CTR="${CTR:-sudo k3s ctr -n k8s.io}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

python3 - "$FILE" "$REF" "$WORK/image.tar" <<'EOF'
import hashlib, io, json, os, sys, tarfile

src, ref, out = sys.argv[1:4]
layer_path = out + ".layer"
with tarfile.open(layer_path, "w", format=tarfile.PAX_FORMAT) as t:
    d = tarfile.TarInfo("disk")
    d.type, d.mode, d.uid, d.gid = tarfile.DIRTYPE, 0o555, 107, 107
    t.addfile(d)
    info = t.gettarinfo(src, arcname="disk/disk.img")
    info.mode, info.uid, info.gid, info.uname, info.gname = 0o440, 107, 107, "", ""
    with open(src, "rb") as f:
        t.addfile(info, f)

h = hashlib.sha256()
with open(layer_path, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
diff_id = h.hexdigest()
arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(os.uname().machine, os.uname().machine)
config = json.dumps({"architecture": arch, "os": "linux", "config": {},
                     "rootfs": {"type": "layers", "diff_ids": ["sha256:" + diff_id]}}).encode()
config_name = hashlib.sha256(config).hexdigest() + ".json"
manifest = json.dumps([{"Config": config_name, "RepoTags": [ref], "Layers": [diff_id + "/layer.tar"]}]).encode()

def add_bytes(t, name, data):
    i = tarfile.TarInfo(name)
    i.size = len(data)
    t.addfile(i, io.BytesIO(data))

with tarfile.open(out, "w", format=tarfile.PAX_FORMAT) as t:
    add_bytes(t, config_name, config)
    add_bytes(t, "manifest.json", manifest)
    t.add(layer_path, arcname=diff_id + "/layer.tar")
os.remove(layer_path)
EOF

$CTR images import "$WORK/image.tar"
echo "imported $REF" >&2
