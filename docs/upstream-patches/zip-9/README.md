# Upstream submissions: zip 9

## Why this directory exists

The release image scan failed on `zip` 8.6.0 and `zip` 2.x advisories:

| Advisory | Severity | Fixed in |
| --- | --- | --- |
| CVE-2026-115905 | CRITICAL | `zip` 9.0.5 |
| CVE-2026-115907 | HIGH | `zip` 9.0.0 |
| CVE-2026-115908 | HIGH | `zip` 9.0.0 |
| CVE-2026-115909 | HIGH | `zip` 9.0.0 |

Every direct `zip` dependency in this repository is already `=9.1.3`. Each
flagged copy arrives through a third-party crate. Four upstream crates hold the
old major versions.

This repository corrects the build with vendored copies and
`[patch.crates-io]`. A vendored copy makes us the maintainer of that code.
Each submission here ends one vendored copy. Send the submissions, then delete
the matching vendored crate when upstream publishes the fix.

## The submissions

| Directory | Upstream repository | Change |
| --- | --- | --- |
| `xberg/` | `xberg-io/xberg` | Admit `calamine` 0.37, which uses `zip` 9 |
| `zavora-xlsx/` | `zavora-ai/zavora-xlsx` | Move `zavora-xlsx` from `zip` 2 to `zip` 9 |
| `zavora-docx/` | `zavora-ai/zavora-docx` | Move `zavora-docx-opc` from `zip` 8 to `zip` 9 |
| `zavora-slide/` | `zavora-ai/zavora-slide` | Move `zavora-slide-opc` and `zavora-slide-xlsx` from `zip` 8 to `zip` 9 |

`calamine` needs no submission. Upstream corrected it in 0.37.0.
`candle-core` needs no submission. Upstream corrected it in 0.11.1.

## How to submit

1. Read the directory's `ISSUE.md` or `PR.md`.
2. Open the issue or the pull request on the upstream repository.
3. Attach the `.patch` file if the directory holds one.
4. Record the upstream issue or pull-request URL in this file.

## Status

| Upstream | Submitted | URL |
| --- | --- | --- |
| `xberg-io/xberg` | No | — |
| `zavora-ai/zavora-xlsx` | No | — |
| `zavora-ai/zavora-docx` | No | — |
| `zavora-ai/zavora-slide` | No | — |

## Note on xberg

The `xberg` submission holds no patch file. The fix is a dependency range, not
code. This container cannot clone and build third-party sources, so nobody
verified the `calamine` 0.36 -> 0.37 call sites here. The upstream maintainer
owns that step.
