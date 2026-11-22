#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""Validate partner metadata and local qualification evidence (not code loading)."""
import argparse
import json
from pathlib import Path
import jsonschema


def validate(path: Path) -> dict:
    schema = json.loads((Path(__file__).parent / "extensions.schema.json").read_text())
    manifest = json.loads(path.read_text())
    jsonschema.Draft202012Validator(schema).validate(manifest)
    qualification = manifest["qualification"]
    if qualification["status"] == "tested":
        # Reports must be local to the manifest directory, never fetched or executed.
        report_path = (path.parent / qualification["report"]).resolve()
        if not report_path.is_relative_to(path.parent.resolve()):
            raise ValueError("qualification report must be within the manifest directory")
        report = json.loads(report_path.read_text())
        expected = {"create", "running-ready", "stop", "restart-ready", "delete-complete"}
        if report.get("schemaVersion") != 1 or report.get("scope") != qualification["scope"] or report.get("passed") is not True:
            raise ValueError("qualification report does not record a passing lifecycle test")
        cases = report.get("cases", [])
        if not isinstance(cases, list) or not all(isinstance(c, dict) for c in cases):
            raise ValueError("qualification cases must be objects")
        if not expected.issubset({c.get("name") for c in cases if c.get("result") == "pass"}) or any(str(c.get("result", "")).startswith("fail:") for c in cases):
            raise ValueError("qualification report is missing passing lifecycle cases")
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path)
    args = parser.parse_args()
    try:
        manifest = validate(args.manifest)
    except (ValueError, OSError, jsonschema.ValidationError) as exc:
        print(f"extension invalid ({type(exc).__name__})")
        return 1
    print(f'{manifest["id"]}@{manifest["version"]}: {manifest["qualification"]["status"]}')
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
