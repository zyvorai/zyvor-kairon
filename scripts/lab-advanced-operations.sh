#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# Applies a real configuration change; use disposable lab resources only.
set -euo pipefail
change=${1:?usage: lab-advanced-operations.sh CHANGE.json RECOVERY.json INCIDENT.json BUNDLE.tar.gz RTT.json}
recovery=${2:?recovery plan required}
incident=${3:?incident plan required}
bundle=${4:?redacted incident bundle required}
rtts=${5:?measured RTT template required}
output=$(mktemp -d "${TMPDIR:-/tmp}/kairon-operations-lab.XXXXXX")
kaironctl changeplan preview --file "$change" --output "$output/prepared.json"
kaironctl changeplan apply --file "$output/prepared.json" --output "$output/change.json"
kaironctl recovery verify --file "$recovery" --report "$output/recovery.json"
kaironctl incident rehearse --bundle "$bundle" --file "$incident" --report "$output/incident.json"
kaironctl placement collect --rtts "$rtts" --window 10s --output "$output/traffic.json"
kaironctl placement recommend --telemetry "$output/traffic.json" > "$output/placement.json"
python3 - "$output" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
load = lambda name: json.loads((root / name).read_text())
assert load('change.json')['phase'] == 'Committed'
for name in ['recovery.json', 'incident.json']:
    report = load(name)
    assert report['phase'] == 'Succeeded' and report['cleaned']
assert load('recovery.json')['applicationRTOSeconds'] >= 0
assert load('incident.json')['investigation']
assert load('placement.json')['estimatedCostAfter'] <= load('placement.json')['estimatedCostBefore']
print(f'PASS: lab workflows completed; evidence in {root}')
PY
