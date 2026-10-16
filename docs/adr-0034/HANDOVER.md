# ADR-0034 desktop ⇄ cloud handoff — handover (2026-10-16)

The ADR is in elitea-docs (`docs/internal/03-architecture/adrs/adr-0034-desktop-cloud-handoff.mdx`, PRs #50–#54, all merged).

The plan issue is elitea-ng/elitea-platform#1187.

`CONTRACT.md` (the wire contract) and `DEVIATIONS.md` (what each unit actually did) sit next to this file. They were the shared working notes for the implementation units.

## Branches, in merge order (one PR at a time)

| # | Branch | State | PR |
|---|---|---|---|
| 1 | `feat/handoff-server` | **merged** (987c69c8a) | #1294 |
| 2 | `feat/handoff-desktop` | open; 2 review passes done, 5 findings being fixed (see below) | #1301 |
| 3 | `feat/desktop-agent-sessions` | pushed, not reviewed. #1212 multi-turn memory. Based on `feat/handoff-desktop`, so rebase it after #1301 merges | — |
| 4 | `feat/handoff-live-events-client` | pushed. Web/desktop client half of the live handoff state events. Based on `feat/handoff-desktop` | — |
| 5 | `feat/handoff-executor` | pushed. Integrated with the merged server. The last items may be WIP; check the head commit message | — |
| 6 | `feat/egress-proxy` → `feat/egress-proxy-wiring` | pushed. Wiring is based on the executor branch (migration shared 0169) | — |

Every PR gets `/code-review` twice before merge, and CI must be green.

## Open items

### #1301 (desktop): review findings
1. A lost begin response strands the thread in HANDING_OFF. On a transport failure, abandon or check the state.
2. A cancel that lands after complete erases the fresh cloud copy.
3. The secret scan skips files with an early NUL byte. Decode UTF-16 and run high-confidence rules on binaries.
4. Leave-on-laptop is checked before `path_deny`. PathDeny must win.
5. Trusted worktree parents are never forgotten. Call `forget_worktree_parent` from `workspace_remove`.

Check the branch head: a `wip(handoff)` commit lists which of these are done.

### Executor
1. The worker RPC for `RecordExecutorEvent` (`handoff.scan_blocked`, `pull_request.opened`) is missing. Today these are only logged.
2. A dead-worker reaper is missing. A CLOUD_RUNNING thread whose worker died must return to CLOUD_IDLE once the lease expires.
3. The `workspace-run-docker` CI job (Docker bridge plus egress proof) has never run.

Check the branch head for a `wip(executor)` commit.

### Merge notes
- After #1301 merges, the executor branch's `libs/rust/workspace-store` is byte-identical to the desktop version at e9357d919. Re-sync it if #1301 changed it later.
- `manifest_test.go` `Head(shared)`: the egress wiring adds 0169. Keep 169 when merging.
- Server contract the clients rely on:
  - `409 thread_busy` (`Retry-After: 1`) means resend the same request.
  - `ref_conflict` with `details.current` is a real conflict.

### Demo blockers (not started)
- #1082: answers are stored twice on the Rust runtime.
- #1218: default coding agent and "Continue?".
- #1213: one round trip before the first token.

### Deployment, still to do
- Enable the code sandbox on technicaldomain (k3s): runner image, workspace-run network policies, `worker.runtime.workspaceRuns` config.
- RustFS bucket encryption (`PutBucketEncryption`), or the operator attestation.
- Enable the `cloud_executor` policy (platform and project) and the device switch `local_work.cloud_handoff`.
- For allowlists: the egress proxy key pair.

### Environment notes
- The podman VM is broken (`ssh handshake EOF`) and was not restarted. Local tests used a no-podman Postgres plus an S3 stand-in.
- Disk is tight. Shared Rust target: `~/projects/eliteaai/_scratch/rust-target-handoff`.
