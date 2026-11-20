#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
"""Render reviewed recipes into Machine, warm-pool or immutable fleet templates."""
from __future__ import annotations
import argparse
import copy
import json
import re
from pathlib import Path
from urllib.parse import urlsplit

CATALOG = Path(__file__).parent / "catalog"
API = "kairon.zyvor.dev/v1"
FLEET = "fleet.kairon.zyvor.dev/v1alpha1"


def name(value: str) -> str:
    if not isinstance(value, str) or len(value) > 63 or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", value):
        raise ValueError("name and namespace must be DNS labels of at most 63 characters")
    return value


def validate(recipe: dict) -> None:
    if set(recipe) != {"schemaVersion", "id", "version", "description", "requirements", "spec"}:
        raise ValueError("recipe has missing or unknown fields")
    if recipe["schemaVersion"] != 1 or type(recipe["schemaVersion"]) is not int:
        raise ValueError("unsupported recipe schemaVersion")
    name(recipe["id"])
    if not isinstance(recipe["version"], str) or not re.fullmatch(r"\d+\.\d+\.\d+", recipe["version"]):
        raise ValueError("recipe version must be numeric major.minor.patch")
    if not isinstance(recipe["description"], str) or not recipe["description"]:
        raise ValueError("description is required")
    if not isinstance(recipe["requirements"], list) or not recipe["requirements"] or any(not isinstance(x, str) or not x for x in recipe["requirements"]):
        raise ValueError("requirements must be non-empty strings")
    spec = recipe["spec"]
    # Local recipes cannot inject credentials, node placement, privileged devices or mutable image locations.
    if not isinstance(spec, dict) or set(spec) - {"resources", "runtime", "network", "cloudInit", "guestAgent", "powerState"}:
        raise ValueError("recipe has unsupported Machine spec fields")
    resources = spec.get("resources", {})
    if set(resources) != {"cpu", "memory"} or not isinstance(resources.get("cpu"), str) or not re.fullmatch(r"[1-9]\d*", resources.get("cpu", "")) or not isinstance(resources.get("memory"), str) or not re.fullmatch(r"[1-9]\d*(?:Mi|Gi)", resources.get("memory", "")):
        raise ValueError("recipe needs positive whole vCPUs and Mi/Gi memory")
    if spec.get("runtime") != {"backend": "qemu"} or spec.get("powerState") != "Running":
        raise ValueError("catalog v1 recipes use QEMU and Running")
    if spec.get("network") != {"mode": "user"}:
        raise ValueError("catalog v1 uses user networking; isolated tap policies require an operator-authored template")
    if spec.get("guestAgent") != {"enabled": True}:
        raise ValueError("catalog v1 requires a prepared qemu-guest-agent image")
    ci = spec.get("cloudInit", {})
    if not isinstance(ci, dict) or set(ci) - {"packages", "runCmd"}:
        raise ValueError("catalog cloudInit permits packages and runCmd only")
    for field in ("packages", "runCmd"):
        if field in ci and (not isinstance(ci[field], list) or any(not isinstance(x, str) or not x for x in ci[field])):
            raise ValueError("cloudInit values must be arrays of non-empty strings")


def load(recipe_id: str) -> dict:
    name(recipe_id)
    recipe = json.loads((CATALOG / (recipe_id + ".json")).read_text())
    validate(recipe)
    if recipe["id"] != recipe_id:
        raise ValueError("recipe ID must match its filename")
    return recipe


def pinned_image(location: str, digest: str, fmt: str = "qcow2") -> dict:
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("digest must be sha256: followed by 64 lowercase hex digits")
    if fmt not in ("raw", "qcow2", "ova", "vmdk", "vhd", "vhdx"):
        raise ValueError("unsupported image format")
    if location.startswith("oci://"):
        ref = location[6:]
        # Require explicit registry and digest pinning; not an ordinary container image.
        if not re.fullmatch(r"[a-z0-9][a-z0-9.:-]*/[a-z0-9][a-z0-9._/-]*(?:@sha256:[0-9a-f]{64})?", ref) or any(p in ("", ".", "..") for p in ref.split("/")[1:]):
            raise ValueError("OCI image needs an explicit registry/repository and optional matching digest")
        if "@" in ref and ref.split("@", 1)[1] != digest:
            raise ValueError("OCI reference and supplied digest disagree")
        source = {"oci": ref.split("@", 1)[0] + "@" + digest, "format": fmt}
    else:
        u = urlsplit(location)
        if u.scheme != "https" or not u.hostname or u.username or u.password or u.fragment or u.query:
            raise ValueError("image URL must be HTTPS without credentials/query/fragment")
        source = {"httpURL": location, "format": fmt}
        if fmt in ("ova", "vmdk", "vhd", "vhdx"):
            source["repair"] = True
    return {"source": source, "digest": digest}


def render(recipe: dict, object_name: str, namespace: str, image: dict, *,
           kind: str = "Machine", ttl: int = 900, replicas: int = 2) -> dict:
    validate(recipe)
    name(object_name); name(namespace)
    if not isinstance(ttl, int) or isinstance(ttl, bool) or ttl < 1:
        raise ValueError("TTL must be a positive integer")
    if not isinstance(replicas, int) or isinstance(replicas, bool) or replicas < 0:
        raise ValueError("replicas must be a non-negative integer")
    source = image.get("source", {})
    if set(image) != {"source", "digest"} or len(set(source) & {"oci", "httpURL"}) != 1:
        raise ValueError("use a digest-pinned image source")
    location = "oci://" + source["oci"] if "oci" in source else source["httpURL"]
    checked = pinned_image(location, image["digest"], source.get("format", "qcow2"))
    if image != checked:
        raise ValueError("unexpected image source options")
    spec = copy.deepcopy(recipe["spec"])
    spec["image"] = checked
    labels = {"ecosystem.zyvor.dev/recipe": recipe["id"], "ecosystem.zyvor.dev/recipe-version": recipe["version"]}
    metadata = {"name": object_name, "namespace": namespace, "labels": labels}
    if kind == "Machine":
        spec["ttlSeconds"] = ttl
        return {"apiVersion": API, "kind": kind, "metadata": metadata, "spec": spec}
    template = {"labels": labels, "spec": spec}
    if kind == "MachinePool":
        # Pool members must stay alive until claimed. Claim TTL belongs on MachineClaim.
        return {"apiVersion": API, "kind": kind, "metadata": metadata, "spec": {"replicas": replicas, "template": template}}
    if kind == "MachineTemplateVersion":
        return {"apiVersion": FLEET, "kind": kind, "metadata": metadata,
                "spec": {"version": recipe["version"], "maxTTLSeconds": ttl, "template": template}}
    raise ValueError("unsupported render kind")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("recipe", nargs="?")
    parser.add_argument("--list", action="store_true")
    parser.add_argument("--name")
    parser.add_argument("--namespace", default="default")
    parser.add_argument("--image", help="HTTPS disk URL or oci://registry/containerDisk reference")
    parser.add_argument("--digest", help="Disk SHA-256, or OCI manifest/index SHA-256")
    parser.add_argument("--format", default="qcow2")
    parser.add_argument("--kind", choices=["Machine", "MachinePool", "MachineTemplateVersion"], default="Machine")
    parser.add_argument("--ttl", type=int, default=900)
    parser.add_argument("--replicas", type=int, default=2)
    args = parser.parse_args()
    try:
        if args.list:
            for path in sorted(CATALOG.glob("*.json")):
                recipe = load(path.stem)
                print(f'{recipe["id"]}\t{recipe["version"]}\t{recipe["description"]}')
            return 0
        if not all((args.recipe, args.name, args.image, args.digest)):
            parser.error("recipe, --name, --image and --digest are required")
        obj = render(load(args.recipe), args.name, args.namespace, pinned_image(args.image, args.digest, args.format),
                     kind=args.kind, ttl=args.ttl, replicas=args.replicas)
        print(json.dumps(obj, indent=2))
        return 0
    except (ValueError, KeyError, OSError) as exc:
        parser.error(str(exc))
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
