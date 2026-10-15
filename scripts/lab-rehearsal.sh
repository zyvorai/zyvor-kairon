#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# Run only on a configured disposable Kairon/CSI lab; see the guide.
set -euo pipefail
compatible=${1:?usage: lab-rehearsal.sh COMPATIBLE.json BREAKING.json}
breaking=${2:?usage: lab-rehearsal.sh COMPATIBLE.json BREAKING.json}
output=$(mktemp -d "${TMPDIR:-/tmp}/kairon-rehearsal-lab.XXXXXX")
command -v kaironctl >/dev/null
command -v python3 >/dev/null
kaironctl rehearsal run --file "$compatible" --report "$output/compatible.json"
if kaironctl rehearsal run --file "$breaking" --report "$output/breaking.json"; then
  echo 'FAIL: the breaking upgrade was not detected' >&2
  exit 1
fi
python3 - "$output" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
good = json.loads((root / 'compatible.json').read_text())
bad = json.loads((root / 'breaking.json').read_text())
assert good['phase'] == 'Succeeded' and good['cleaned']
assert bad['phase'] == 'Failed' and bad['cleaned']
assert all(r['exitCode'] == 0 for r in good['results'])
assert any(r['stage'] == 'Baseline' and r['exitCode'] == 0 for r in bad['results'])
assert any(r['stage'] == 'Changes' and r['exitCode'] == 0 for r in bad['results'])
assert any(r['stage'] == 'Verify' and r['exitCode'] != 0 for r in bad['results'])
assert all(x['deleted'] for report in [good, bad] for x in report['resources'])
assert not good['reservationPending'] and not bad['reservationPending']
print('PASS: compatible upgrade works; breaking upgrade detected; resources cleaned.')
print(f'Evidence: {root}')
PY
