#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
"""Kairon vs KubeVirt benchmark driver (stdlib only).

Runs ON the single node under test (it reads /proc for memory and CPU) and
talks to the cluster through kubectl. Both platforms go through the same
code path; only the manifests, the status fields and the process names
differ, so the numbers are comparable.

Metrics, per density N:
  running_ms  create -> platform reports Running (Machine status.phase /
              VMI status.phase)
  ready_ms    create -> the guest's sshd sends its banner, probed from the
              node (Kairon: user-mode host forward; KubeVirt: masquerade
              pod IP); no guest agent required
  per_vm_kib  host RSS added per VM: every process of the VM's runtime
              (QEMU, plus virt-launcher/virtqemud/virtlogd for KubeVirt)
              and the growth of the node agents, divided by N
Plus idle control-plane RSS and CPU, and optional live-migration downtime.

  bench.py --platform kairon   --image /var/lib/fluxvm/images/ubuntu.qcow2
  bench.py --platform kubevirt --containerdisk localhost/bench/ubuntu:24.04
"""

import argparse
import datetime
import json
import os
import platform as pyplatform
import socket
import statistics
import subprocess
import sys
import time

PROCS = {
    "kairon": {
        "vm": {"qemu-system-x86_64", "qemu-system-aarch64", "qemu-kvm"},
        "agent": {"kairon-node", "fluxctl", "fluxvm"},
        "control": {"kairon-controller", "kairon-node", "fluxctl", "fluxvm"},
    },
    "kubevirt": {
        "vm": {"qemu-kvm", "qemu-system-x86_64", "qemu-system-aarch64", "virt-launcher",
               "virt-launcher-monitor", "virtqemud", "virtlogd"},
        "agent": {"virt-handler"},
        "control": {"virt-api", "virt-controller", "virt-handler", "virt-operator", "virt-exportproxy"},
    },
}


def log(msg):
    print(f"[{datetime.datetime.now(datetime.timezone.utc):%H:%M:%S}] {msg}", file=sys.stderr, flush=True)


def kubectl(args, inp=None, check=True):
    cmd = os.environ.get("KUBECTL", "kubectl").split() + args
    r = subprocess.run(cmd, input=inp, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)}: {r.stderr.strip()}")
    return r.stdout


def proc_name(pid):
    try:
        with open(f"/proc/{pid}/cmdline", "rb") as f:
            argv0 = f.read().split(b"\0", 1)[0].decode(errors="replace")
        return os.path.basename(argv0)
    except OSError:
        return ""


# Comma-separated cmdline substrings to ignore, e.g. a second FluxVM daemon
# on the same host: BENCH_EXCLUDE=/etc/fluxvm.toml
EXCLUDE = [x for x in os.environ.get("BENCH_EXCLUDE", "").split(",") if x]


def cmdline(pid):
    try:
        with open(f"/proc/{pid}/cmdline", "rb") as f:
            return f.read().replace(b"\0", b" ").decode(errors="replace")
    except OSError:
        return ""


def pids(names):
    out = []
    for p in os.listdir("/proc"):
        if p.isdigit() and proc_name(p) in names:
            if EXCLUDE and any(x in cmdline(p) for x in EXCLUDE):
                continue
            out.append(int(p))
    return out


def rss_kib(names):
    total = 0
    for pid in pids(names):
        try:
            with open(f"/proc/{pid}/status") as f:
                for line in f:
                    if line.startswith("VmRSS:"):
                        total += int(line.split()[1])
        except OSError:
            pass
    return total


def cpu_ticks(names):
    total = 0
    for pid in pids(names):
        try:
            with open(f"/proc/{pid}/stat") as f:
                fields = f.read().rsplit(")", 1)[1].split()
            total += int(fields[11]) + int(fields[12])
        except (OSError, IndexError, ValueError):
            pass
    return total


def idle_cpu_millicores(names, seconds):
    hz = os.sysconf("SC_CLK_TCK")
    a = cpu_ticks(names)
    time.sleep(seconds)
    b = cpu_ticks(names)
    return round((b - a) / hz / seconds * 1000, 1)


def ssh_banner(ip, port):
    """True once an SSH server answers. A NAT/port forward can accept the
    TCP connection before the guest listens, so the banner is the signal."""
    try:
        with socket.create_connection((ip, port), timeout=1) as c:
            c.settimeout(2)
            return c.recv(8).startswith(b"SSH-")
    except OSError:
        return False


def kairon_manifest(ns, name, a):
    idx = int(name.rsplit("-", 1)[1])
    return {
        "apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
        "metadata": {"name": name, "namespace": ns, "labels": {"bench": "kairon"}},
        "spec": {
            "image": {"path": a.image},
            "resources": {"cpu": str(a.cpu), "memory": a.memory},
            "runtime": {"backend": "qemu"},
            "cloudInit": {"hostname": name, "user": "bench", "sshAuthorizedKeys": [a.ssh_key]},
            "network": {"mode": "user", "forwards": [{"hostPort": a.port_base + idx, "guestPort": a.ready_port}]},
            "powerState": "Running",
        },
    }


def kubevirt_manifest(ns, name, a):
    return {
        "apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
        "metadata": {"name": name, "namespace": ns, "labels": {"bench": "kubevirt"}},
        "spec": {
            "runStrategy": "Always",
            "template": {
                "metadata": {"labels": {"bench": "kubevirt"}},
                "spec": {
                    "domain": {
                        "cpu": {"cores": a.cpu},
                        "memory": {"guest": a.memory},
                        "devices": {
                            "disks": [{"name": "root", "disk": {"bus": "virtio"}},
                                      {"name": "cloudinit", "disk": {"bus": "virtio"}}],
                            "interfaces": [{"name": "default", "masquerade": {}}],
                        },
                    },
                    "networks": [{"name": "default", "pod": {}}],
                    "volumes": [
                        {"name": "root", "containerDisk": {"image": a.containerdisk, "imagePullPolicy": "IfNotPresent"}},
                        {"name": "cloudinit", "cloudInitNoCloud": {"userData": (
                            f"#cloud-config\nhostname: {name}\nusers:\n  - name: bench\n"
                            f"    sudo: ALL=(ALL) NOPASSWD:ALL\n    ssh_authorized_keys: [{json.dumps(a.ssh_key)}]\n")}},
                    ],
                },
            },
        },
    }


def kairon_status(item, a):
    # User-mode networking: the guest's sshd is reached via its host forward.
    st = item.get("status") or {}
    idx = int(item["metadata"]["name"].rsplit("-", 1)[1])
    return st.get("phase") == "Running", ("127.0.0.1", a.port_base + idx)


def kubevirt_status(item, a):
    st = item.get("status") or {}
    for iface in st.get("interfaces") or []:
        if iface.get("ipAddress"):
            return st.get("phase") == "Running", (iface["ipAddress"].split("/")[0], a.ready_port)
    return st.get("phase") == "Running", None


PLATFORMS = {
    "kairon": {"create": kairon_manifest, "kind": "machines", "watch": "machines", "status": kairon_status},
    "kubevirt": {"create": kubevirt_manifest, "kind": "virtualmachines", "watch": "virtualmachineinstances", "status": kubevirt_status},
}


def delete_and_wait(ns, delete_kind, watch_kind, selector, timeout):
    # A loaded API server (or KubeVirt's virt-api webhook) can drop part of a
    # label-selected delete, so it is re-sent until everything is gone.
    deadline = time.time() + timeout
    next_delete = 0.0
    while time.time() < deadline:
        if time.time() >= next_delete:
            kubectl(["delete", delete_kind, "-n", ns, "-l", selector, "--wait=false"], check=False)
            next_delete = time.time() + 30
        if not kubectl(["get", watch_kind, "-n", ns, "-l", selector, "-o", "name"], check=False).strip() and \
                not kubectl(["get", delete_kind, "-n", ns, "-l", selector, "-o", "name"], check=False).strip():
            return
        time.sleep(2)
    raise RuntimeError(f"{watch_kind} with {selector} still present after {timeout}s")


def pct(values, q):
    if not values:
        return None
    s = sorted(values)
    return s[min(len(s) - 1, int(round(q * (len(s) - 1))))]


def run_density(a, p, n, procs):
    ns, selector = a.namespace, f"bench={a.platform}"
    names = [f"bench-{n}-{i}" for i in range(n)]
    vm0, agent0 = rss_kib(procs["vm"]), rss_kib(procs["agent"])
    load_before = os.getloadavg()[0]
    docs = {"apiVersion": "v1", "kind": "List", "items": [p["create"](ns, nm, a) for nm in names]}
    t0 = time.monotonic()
    kubectl(["apply", "-f", "-"], inp=json.dumps(docs))
    running, ready = {}, {}
    deadline = t0 + a.timeout
    while len(ready) < n and time.monotonic() < deadline:
        items = json.loads(kubectl(["get", p["watch"], "-n", ns, "-l", selector, "-o", "json"]))["items"]
        now = time.monotonic()
        for item in items:
            nm = item["metadata"]["name"]
            if nm not in names:
                continue
            is_running, addr = p["status"](item, a)
            if is_running and nm not in running:
                running[nm] = (now - t0) * 1000
            if nm in running and nm not in ready and addr and ssh_banner(*addr):
                ready[nm] = (time.monotonic() - t0) * 1000
        time.sleep(0.5)
    time.sleep(a.settle)
    vm_kib = rss_kib(procs["vm"]) - vm0
    agent_kib = rss_kib(procs["agent"]) - agent0
    res = {
        "n": n,
        "running": len(running), "ready": len(ready),
        "running_ms_p50": pct(list(running.values()), 0.5), "running_ms_max": max(running.values(), default=None),
        "ready_ms_p50": pct(list(ready.values()), 0.5), "ready_ms_max": max(ready.values(), default=None),
        "vm_rss_kib": vm_kib, "agent_rss_growth_kib": agent_kib,
        "per_vm_kib": round((vm_kib + max(agent_kib, 0)) / n) if n else None,
        # Host 1-minute load before/after: other tenants skew a shared host.
        "loadavg_1m": [round(load_before, 2), round(os.getloadavg()[0], 2)],
    }
    if a.migrate_target and n == 1 and len(running) == 1:
        res["migration"] = migrate(a, names[0])
    log(f"N={n}: running {len(running)}/{n} ready {len(ready)}/{n} "
        f"p50 running={res['running_ms_p50']} ready={res['ready_ms_p50']} per-VM={res['per_vm_kib']} KiB")
    delete_and_wait(ns, p["kind"], p["watch"], selector, a.timeout)
    return res


def migrate(a, name):
    if a.platform != "kairon":
        # KubeVirt reports migration start/end, not guest downtime.
        return {"note": "KubeVirt does not report guest downtime"}
    mm = {
        "apiVersion": "kairon.zyvor.dev/v1", "kind": "MachineMigration",
        "metadata": {"name": f"{name}-mig", "namespace": a.namespace},
        "spec": {"machineName": name, "targetNode": a.migrate_target, "strategy": "live"},
    }
    kubectl(["apply", "-f", "-"], inp=json.dumps(mm))
    deadline = time.time() + a.timeout
    st = {}
    while time.time() < deadline:
        st = json.loads(kubectl(["get", "machinemigration", "-n", a.namespace, mm["metadata"]["name"], "-o", "json"])).get("status") or {}
        if st.get("phase") in ("Succeeded", "Failed", "NeedsRecovery"):
            break
        time.sleep(1)
    kubectl(["delete", "machinemigration", "-n", a.namespace, mm["metadata"]["name"]], check=False)
    return {"phase": st.get("phase"), "downtime_ms": st.get("downtimeMs")}


def default_ssh_key():
    for name in ("id_ed25519.pub", "id_rsa.pub"):
        path = os.path.expanduser(f"~/.ssh/{name}")
        if os.path.exists(path):
            with open(path) as f:
                return f.read().strip()
    return "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBenchBenchBenchBenchBenchBenchBenchBenchBen bench"


def host_info():
    model = ""
    try:
        with open("/proc/cpuinfo") as f:
            for line in f:
                if line.startswith("model name"):
                    model = line.split(":", 1)[1].strip()
                    break
        with open("/proc/meminfo") as f:
            mem_kib = int(f.readline().split()[1])
    except OSError:
        mem_kib = 0
    return {"cpu": model, "cores": os.cpu_count(), "mem_gib": round(mem_kib / 1048576, 1),
            "kernel": pyplatform.release(), "hostname": socket.gethostname()}


def platform_version(name):
    if name == "kubevirt":
        out = kubectl(["get", "kubevirt", "-A", "-o", "jsonpath={.items[0].status.observedKubeVirtVersion}"], check=False)
        return out.strip()
    return os.environ.get("BENCH_KAIRON_VERSION", "")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--platform", choices=PLATFORMS, required=True)
    ap.add_argument("--image", help="kairon: spec.image.path on the node")
    ap.add_argument("--containerdisk", help="kubevirt: containerDisk image (pre-pulled or imported)")
    ap.add_argument("--sizes", default=os.environ.get("BENCH_SIZES", "1,5,10"))
    ap.add_argument("--cpu", type=int, default=1)
    ap.add_argument("--memory", default=os.environ.get("BENCH_MEMORY", "512Mi"))
    ap.add_argument("--namespace", default=os.environ.get("BENCH_NAMESPACE", "bench"))
    ap.add_argument("--ready-port", type=int, default=int(os.environ.get("BENCH_READY_PORT", "22")))
    ap.add_argument("--port-base", type=int, default=int(os.environ.get("BENCH_PORT_BASE", "42200")),
                    help="kairon: first host port forwarded to a guest's ready port")
    ap.add_argument("--timeout", type=int, default=600)
    ap.add_argument("--settle", type=int, default=20, help="seconds to wait before sampling RSS")
    ap.add_argument("--idle-seconds", type=int, default=60)
    ap.add_argument("--migrate-target", default="", help="node for a live migration at N=1")
    ap.add_argument("--ssh-key", default=os.environ.get("BENCH_SSH_KEY") or default_ssh_key(),
                    help="public key for the identical cloud-init seed both sides boot with")
    ap.add_argument("--out", default="")
    a = ap.parse_args()
    if a.platform == "kairon" and not a.image:
        ap.error("--image is required for kairon")
    if a.platform == "kubevirt" and not a.containerdisk:
        ap.error("--containerdisk is required for kubevirt")

    p, procs = PLATFORMS[a.platform], PROCS[a.platform]
    kubectl(["create", "namespace", a.namespace], check=False)
    log(f"idle control plane: sampling {a.idle_seconds}s")
    control = {"rss_kib": rss_kib(procs["control"]),
               "cpu_millicores": idle_cpu_millicores(procs["control"], a.idle_seconds),
               "processes": sorted({proc_name(x) for x in pids(procs["control"])})}
    runs, error = [], ""
    for n in [int(x) for x in a.sizes.replace(" ", ",").split(",") if x]:
        try:
            runs.append(run_density(a, p, n, procs))
        except RuntimeError as e:
            error = f"N={n}: {e}"
            log(error)
            break
    result = {
        "platform": a.platform, "version": platform_version(a.platform),
        "date": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "host": host_info(),
        "vm": {"cpu": a.cpu, "memory": a.memory, "image": a.image or a.containerdisk, "ready_port": a.ready_port},
        "control_plane_idle": control, "density": runs,
    }
    if error:
        result["error"] = error
    out = a.out or f"docs/benchmarks/{a.platform}-{datetime.date.today():%Y-%m-%d}.json"
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    with open(out, "w") as f:
        json.dump(result, f, indent=2)
        f.write("\n")
    log(f"wrote {out}")
    failed = bool(error) or any(r["ready"] < r["n"] for r in runs)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
