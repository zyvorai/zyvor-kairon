# Agreeing vectors.json with FluxVM BPF

`vectors.json` pins 17 cases for the userspace engine. Nothing in this
repo runs them against `fluxvm_tc.bpf.o` yet.

## Planned CI step

1. Build or download `fluxvm_tc.bpf.o` from the matching FluxVM release.
2. Run the 17 vectors against it.
3. Fail the job if any vector diverges.

## Status

Design only. The vectors file and the userspace engine already exist.
