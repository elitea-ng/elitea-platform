# Context settings and measured usage: rehearsal acceptance

Status: deployed settings and ordinary measured-context acceptance pass,
2026-09-17. Actual compaction and recovery acceptance remain open.

## Delivered boundary

The implementation through `f03ce096` follows
[context progress](context-progress-events.md),
[policy delivery](context-policy-delivery.md), and
[UI policy controls](context-policy-ui.md).
The compatible worker was replaced before Main, followed by the web image.
Main started healthy; worker and web started without restart loops.

| Service | Local image | Image digest |
| --- | --- | --- |
| Rust worker | `elitea-worker-rust:context-progress-20260917` | `sha256:9cfc91b5f8d1b837d9b9d34c1846349c7f54ab616f317490d5ed7981f7cf9ec6` |
| Main | `elitea-main:context-progress-20260917` | `sha256:b35f2a1f4b79e4bb8e53ac52d47d74f769f9b4ab2ad3667d37f53a80161a8578` |
| Web, including the numbering fix below | `elitea-web:context-progress-list-fix-20260917` | `sha256:d712fd77f4d368e5f1a666c5f3b0595b08ed6f8aa83f0619ea33d78abb70249c` |

The replacement retained environment values, database connections, secrets,
TLS mounts, networks, resource limits, entrypoints, and commands. No database
schema or data migration was performed. The product and agent-state databases
remain the existing rehearsal databases. The separate Centry deployment was
not restarted. Docker temporarily stopped responding during builds, then
recovered without an engine restart.

Main was built from the current worktree, which also contains preserved,
uncommitted gate-5 history changes. Their presence in this image is not gate-5
acceptance; that work remains deferred and separately tracked.

## Browser evidence

Fresh headed Playwright, with no intercepted requests, verified both Full and
Balanced profile saves and reloads. The account was returned to Balanced.
The deployed settings explain the output reservation, safety margin, and 90%
trigger. The summary field reads “Additional summary guidance (optional)” and
explains that the platform owns the structured summary contract.

A normal model request completed and produced a real persisted context
measurement. A new Chrome tab opened the completed conversation and inspected
the usage dialog. A separate fresh Playwright process verified the same
measurement, execution identity, and answer before and after idle reload.

The recorded measurement was:

| Field | Value |
| --- | ---: |
| Phase | measured |
| Mode | Balanced |
| Admitted total window | 128,000 |
| Output reservation | 16,000 |
| Safety margin | 1,280 |
| Usable input | 110,720 |
| Estimated input | 90 |
| Compaction trigger | 99,648 |
| Compaction target | 77,504 |

This proves capping against the admitted model snapshot, not a claim about the
provider's largest available context window. The low percentage displays as 0%
while the dialog retains the nonzero estimate. The terminal record is inactive.

The initial exact-echo test was unsuitable because the model declined to repeat
the marker. A later reload locator matched both the chat title and user message;
the independent reload check resolves that harness issue. Neither is counted as
a runtime failure or a passed end-to-end script. Successful settings requests,
live measurements, and the final independent reload proof are recorded separately.

Temporary evidence: `elitea-context-live-settings.png`,
`elitea-context-live-reload-result.json`, `elitea-context-live-reload-before.png`,
and their scripts/logs under `/private/tmp`. Images and response metadata were
inspected. No credentials or raw provider inputs are included in this record.

## Numbered-answer rendering correction

The live arithmetic prompt exposed an independent UI issue. Markdown parses a
bare `4.` as an ordered list beginning at four. The web renderer discarded that
starting number, so the browser displayed one. `shared/ui/Token/Token.tsx` now
passes the parsed start to the native ordered-list element. It does not alter
stored model output or streaming semantics. The existing ordered-list check now
exercises a non-default starting number; all 16 Token checks, TypeScript, changed
Oxlint, and the complete container UI build pass.
After the UI-only replacement, fresh headed Playwright confirms `start="4"`
on the saved response and unchanged answer/context data across reload. The
deployed screenshot is inspected; no new provider request is needed for this check.

## Remaining proof

This does not exercise the compaction threshold, summary quality, a dedicated
summary-provider call, model-local child compaction, or crash replacement during
summary generation. It also does not close continuation, tool-output editing,
diagnostics, full-window transport capacity, or durable child activity beyond
replay retention. Production capability registration remains disabled.
