#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""check_rbac_coverage.py -- cross-checks each in-cluster component's actual
internal/kube.Client method calls against the RBAC (ClusterRole/Role) grants
its own ServiceAccount holds in charts/kairon/templates/all.yaml.

Why this exists: two real, silent-in-production RBAC gaps have already been
found by hand this way -- kairon-ui's ClusterRole was missing get/list/watch
for six dashboard routes (fixed in 3a75458/4ba53f8), and kairon-controller's
ClusterRole is missing "delete" on machinesnapshots even though
reconcileMachineSnapshotSchedules' own keepLast pruning
(internal/controller/machinesnapshotschedule.go's pruneScheduledSnapshots)
calls Kube.DeleteMachineSnapshot every time a schedule has more than
keepLast ready snapshots (fixed alongside this script). Both gaps existed
because internal/kube's fake httptest.Server test doubles never enforce
RBAC, so `go test ./...` is silent about a real apiserver's 403 -- only
`helm template` plus a careful manual read of the ClusterRole ever caught
either one. This script automates that cross-check so a third one can't slip
in the same way.

How it works:
  1. Parses internal/kube/client.go itself to learn, for every exported
     *Client method, which Kubernetes (apiGroup, resource, verb) it needs --
     read directly from that method's own request path/verb, not guessed
     from its name, so this stays correct as methods are added or renamed.
  2. Renders charts/kairon/templates/all.yaml via `helm template` with every
     conditional RBAC-affecting flag turned on, so every ClusterRole/Role
     this chart can ever produce is present in one pass (mirrors this
     project's own documented gotcha: kairon-ui's whole RBAC block only
     renders with ui.enabled=true/ui.token=set, which is exactly how the
     first gap above went unnoticed).
  3. For each of kairon-controller/kairon-node/kairon-ui, statically greps
     that component's own source directories for which *Client methods it
     actually calls, and checks the rendered rules for its ServiceAccount's
     ClusterRole/Roles grant the (apiGroup, resource, verb) each call needs.

Known, deliberate scope limits (real limits, not hidden gaps):
  - Only cross-checks kairon-controller, kairon-node, and kairon-ui. Neither
    kairon-csi-controller nor kairon-csi-node calls internal/kube.Client at
    all (they only implement the CSI gRPC interface; their own RBAC backs
    the upstream CSI sidecar containers, not code in this repo), and
    kaironctl runs under the invoking operator's own kubeconfig/RBAC, not a
    ServiceAccount this chart grants -- there is nothing here to check for
    either.
  - Checks the "watch" verb for Watch* Client methods (internal/kube/watch.go,
    used by kairon-node's event-driven reconcile). List/Get call sites still
    do not imply a watch grant.
  - Ignores `resourceNames` scoping -- a rule granting "get"/"patch" on
    "secrets" scoped to resourceNames: ["kairon-ui-users"] counts as full
    coverage for GetSecret/PatchSecretStringData, without checking the
    specific object name matches. In practice every such call site already
    targets exactly the one name the Role was scoped to.
  - Does not distinguish ClusterRole (cluster-wide) from a namespaced Role
    scope; both simply contribute (resource, verb) grants for their
    ServiceAccount. Good enough here since every namespaced Role in this
    chart binds to the same namespace the component itself runs in.

Run: python3 scripts/check_rbac_coverage.py
"""

import re
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
CLIENT_GO = ROOT / "internal/kube/client.go"
CHART = ROOT / "charts/kairon"

# Extra --set flags needed so `helm template` renders every RBAC-affecting
# conditional block at once (see the module docstring's point 2). Kept in
# one place so a newly added conditional RBAC block gets a reminder here
# too, the same way README/ROADMAP call out this exact gotcha elsewhere.
HELM_SET_FLAGS = [
    "ui.enabled=true",
    "ui.token=test-token",
    "controller.cordonEvacuation.enabled=true",
    "controller.memoryPressureEvacuation.enabled=true",
    "controller.dr.enabled=true",
    "node.livenessLease.enabled=true",
    "node.csi.chap.enabled=true",
    "node.cbtBackup.enabled=true",
    "node.thirdPartyCSIDrivers.example\\.csi=/run/example.sock",
    "network.ciliumAttach.enabled=true",
    "network.ciliumPolicySync.enabled=true",
    "ui.auth.rbacConsoleCheck=true",
    "ui.auth.users[0].username=admin",
    "ui.auth.users[0].passwordHash=x",
    "ui.auth.users[0].admin=true",
]

# Method names whose (apiGroup, resource, verb) can't be derived from their
# own request path text (SubjectAccessReview posts a fixed literal path but
# its method name carries no Get/List/Create/... verb prefix to key off of).
OVERRIDES = {
    "SubjectAccessReview": ("authorization.k8s.io", "subjectaccessreviews", "create"),
    # RecordEvent posts a core/v1 Event -- its name doesn't start with any
    # of VERB_PREFIXES (a plain Get/List/Create/... prefix would misread as
    # "this is an Event *reader*"), so it needs the same explicit override
    # SubjectAccessReview above does.
    "RecordEvent": ("", "events", "create"),
    # Thin wrappers that only delegate to *WithSelector helpers (v0.6
    # node-scoped list). Bodies have no path literal for the regex to read.
    "ListMachines": ("kairon.zyvor.dev", "machines", "list"),
    "ListMachineMigrations": ("kairon.zyvor.dev", "machinemigrations", "list"),
    # Watch lives in internal/kube/watch.go; same resources as the List forms.
    "WatchMachines": ("kairon.zyvor.dev", "machines", "watch"),
    "WatchMachineMigrations": ("kairon.zyvor.dev", "machinemigrations", "watch"),
    # Merge-patches status.lastRunError; "Set" isn't a verb prefix.
    "SetMachineSnapshotScheduleError": ("kairon.zyvor.dev", "machinesnapshotschedules/status", "patch"),
    # external-dns's CRD, built by dnsEndpointPath.
    "GetDNSEndpoint": ("externaldns.k8s.io", "dnsendpoints", "get"),
    "CreateDNSEndpoint": ("externaldns.k8s.io", "dnsendpoints", "create"),
    "PatchDNSEndpoint": ("externaldns.k8s.io", "dnsendpoints", "patch"),
    "DeleteDNSEndpoint": ("externaldns.k8s.io", "dnsendpoints", "delete"),
    # ?dryRun=All writes; the apiserver still authorizes the real verb.
    "DryRunPatchMachine": ("kairon.zyvor.dev", "machines", "patch"),
    "DryRunCreateMachineMigration": ("kairon.zyvor.dev", "machinemigrations", "create"),
}

VERB_PREFIXES = [
    ("List", "list"),
    ("Get", "get"),
    ("Create", "create"),
    ("Delete", "delete"),
    ("Patch", "patch"),
    ("Update", "update"),
    ("Watch", "watch"),
]

COMPONENTS = {
    "kairon-controller": {
        "dirs": ["internal/controller", "internal/leaderelection", "cmd/kairon-controller"],
        "rbac_names": ["kairon-controller", "kairon-controller-leader-election", "kairon-node-liveness-lease-reader", "kairon-controller-dr-peers"],
    },
    "kairon-node": {
        "dirs": ["internal/agent", "internal/nodeliveness", "cmd/kairon-node"],
        "rbac_names": ["kairon-node", "kairon-node-liveness-lease", "kairon-node-csi-chap", "kairon-node-backup-secrets"],
    },
    "kairon-ui": {
        "dirs": ["internal/uiapi", "cmd/kairon-ui"],
        "rbac_names": ["kairon-ui", "kairon-ui-shared-state", "kairon-ui-users-secret"],
    },
}


def extract_client_methods():
    """Returns {method_name: (apiGroup, resource, verb)} learned directly
    from internal/kube Client sources, per the module docstring."""
    sources = [CLIENT_GO]
    watch_go = ROOT / "internal/kube/watch.go"
    if watch_go.exists():
        sources.append(watch_go)
    methods = {}
    unresolved = []
    func_re = re.compile(r"func \(c \*Client\) (\w+)\(ctx context\.Context[^\n]*\{(.*?)\n\}\n", re.DOTALL)
    for src_path in sources:
        src = src_path.read_text()
        for m in func_re.finditer(src):
            name, body = m.group(1), m.group(2)
            if not name[0].isupper():
                continue  # unexported helper (request/doRequest), not a public API call site
            if name in OVERRIDES:
                methods[name] = OVERRIDES[name]
                continue
            verb = next((v for pfx, v in VERB_PREFIXES if name.startswith(pfx)), None)
            if verb is None:
                unresolved.append((name, "no verb prefix"))
                continue
            apigroup = resource = None
            mm = re.search(r'namespaced?(?:Object)?Path\(ns,\s*"([a-z]+)"', body)
            if mm:
                apigroup, resource = "kairon.zyvor.dev", mm.group(1)
            elif "leasePath(" in body:
                apigroup, resource = "coordination.k8s.io", "leases"
            else:
                mm = re.search(r"/apis/([a-zA-Z0-9.\-]+)/v[0-9a-zA-Z]+/(?:namespaces/%s/)?([a-z]+)", body)
                if mm:
                    apigroup, resource = mm.group(1), mm.group(2)
                else:
                    mm = re.search(r"/api/v1/(?:namespaces/%s/)?([a-z]+)", body)
                    if mm:
                        apigroup, resource = "", mm.group(1)
            if resource is None:
                unresolved.append((name, "no resource match"))
                continue
            if '"/status"' in body:
                resource += "/status"
            methods[name] = (apigroup, resource, verb)
    if unresolved:
        print("FATAL: could not derive RBAC requirements for these Client methods:", file=sys.stderr)
        for name, reason in unresolved:
            print(f"  - {name}: {reason}", file=sys.stderr)
        print(
            "Add an entry to OVERRIDES in scripts/check_rbac_coverage.py (or fix the method\n"
            "to follow the Get/List/Create/Delete/Patch/Update + namespacePath/namespacedObjectPath\n"
            "convention every other method already follows) before this check can run.",
            file=sys.stderr,
        )
        sys.exit(1)
    return methods


def render_chart():
    cmd = ["helm", "template", str(CHART)]
    for flag in HELM_SET_FLAGS:
        cmd += ["--set", flag]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, check=False)
    except FileNotFoundError:
        print("FATAL: `helm` not found on PATH -- this check renders charts/kairon to learn what RBAC is actually granted, so it needs a real helm binary (see https://helm.sh). Skipping it silently would risk re-hiding exactly the class of bug it exists to catch.", file=sys.stderr)
        sys.exit(1)
    if out.returncode != 0:
        print("FATAL: helm template failed:\n" + out.stderr, file=sys.stderr)
        sys.exit(1)
    return out.stdout


def parse_granted(rendered_yaml):
    """Returns {role_name: {(apiGroup, resource): {verbs}}} across every
    rendered ClusterRole and Role."""
    granted = {}
    for doc in yaml.safe_load_all(rendered_yaml):
        if not doc or doc.get("kind") not in ("ClusterRole", "Role"):
            continue
        name = doc["metadata"]["name"]
        bucket = granted.setdefault(name, {})
        for rule in doc.get("rules", []):
            verbs = set(rule.get("verbs", []))
            for group in rule.get("apiGroups", [""]):
                for resource in rule.get("resources", []):
                    bucket.setdefault((group, resource), set()).update(verbs)
    return granted


def find_needed(dirs, methods):
    """Returns {method_name used} found via static grep of dirs (excluding
    _test.go and internal/kube itself)."""
    used = set()
    patterns = {name: re.compile(r"\." + re.escape(name) + r"\(") for name in methods}
    for d in dirs:
        base = ROOT / d
        if not base.exists():
            continue
        for path in base.rglob("*.go"):
            if path.name.endswith("_test.go"):
                continue
            text = path.read_text()
            for name, pat in patterns.items():
                if name not in used and pat.search(text):
                    used.add(name)
    return used


def main():
    methods = extract_client_methods()
    granted = parse_granted(render_chart())

    errors = []
    for component, cfg in COMPONENTS.items():
        used = find_needed(cfg["dirs"], methods)
        for method in sorted(used):
            apigroup, resource, verb = methods[method]
            covered = any(
                verb in granted.get(role, {}).get((apigroup, resource), set())
                for role in cfg["rbac_names"]
            )
            if not covered:
                errors.append(
                    f"{component}: calls {method}() needing {verb} on "
                    f"{apigroup or 'core'}/{resource}, but none of its RBAC "
                    f"grants ({', '.join(cfg['rbac_names'])}) include that verb "
                    f"for that resource"
                )

    if errors:
        print("RBAC COVERAGE CHECK FAILED")
        for e in errors:
            print(" -", e)
        sys.exit(1)
    print(f"rbac coverage: OK ({sum(len(find_needed(c['dirs'], methods)) for c in COMPONENTS.values())} call sites checked across {len(COMPONENTS)} components)")


if __name__ == "__main__":
    main()
