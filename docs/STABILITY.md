---
sidebar_position: 5
title: Feature stability
---

# Feature stability

Every optional Kairon feature is either **GA** or **Experimental**. There is no third state and no prose
exception: the label is a field in [`stability.json`](stability.json), and CI checks it against the evidence in
[`COMPATIBILITY.md`](COMPATIBILITY.md).

## What GA means

A feature is GA only when all of these hold:

1. Every row for it in `COMPATIBILITY.md` is `pass`, from a run on real hosts, and the row is dated.
2. The nightly lab workflow has run those rows within the last 14 days. A GA feature whose rows go stale fails the
   nightly job and has to be re-run or demoted.
3. Failure paths are tested (the feature's drills or injected-failure scripts, not only the happy path).
4. It has a runbook or a "Real limits" section that matches what the code does.
5. It exposes metrics for its own health, and the guide names the alerts to set.

Anything else is Experimental. Experimental features still ship and are supported on a best-effort basis; their
interfaces may change in a minor release, and the guide lists which rows are still open and what hardware they need.

## How it is enforced

`scripts/stability-check.py` runs in CI and in `make validate`:

- A feature labelled `ga` with a missing or non-`pass` row fails the build.
- The README feature row must say `(experimental)` exactly when the registry says `experimental`.
- With `--max-age-days 14` (the nightly job) a GA feature's rows must also be recent.

To promote a feature: get its rows green on the lab, set `"label": "ga"` in `stability.json`, remove
`(experimental)` from its README row, guide title and `STATUS.md` entry, and open one pull request that links the
nightly run. The check refuses the change if any row is not `pass`.

## Current state

The table of rows passing per feature is printed by `python3 scripts/stability-check.py` and written to the step
summary of every CI run.
