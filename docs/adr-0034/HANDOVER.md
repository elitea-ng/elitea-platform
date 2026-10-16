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

### #1301 (desktop): review findings, all fixed (head `3b65c2b72`)
All five fixes are in, each with tests:
1. A lost begin response no longer strands the thread.
2. A cancel after complete keeps the cloud copy.
3. The secret scan now covers UTF-16 and binary files.
4. `path_deny` takes precedence over leave-on-laptop.
5. `forget_worktree_parent` is called on `workspace_remove`.

The dependabot coverage gate also had a fix.

`*.jks` and `*.keystore` were added to the default exclusions, both on the desktop and in the server's `cloudexecutor.go`. The ADR text in elitea-docs does not list them yet.

**Left:**
- CI on `3b65c2b72` was still queued at handover. Confirm it is green.
- Then run the two `/code-review` passes again before merging.

### Executor (head `69f415c22`)
1. **Audit RPC (`WorkspaceRecordEvent`): partial.**
   - Done: it is claim- and fence-bound and carries no path fields. The worker uses a bounded queue, and its Postgres and worker tests pass.
   - Left:
     - `TestWorkspaceStoreIsRegisteredOnTheControlListenerOnly` (`internal/transport/runtimegrpc`) fails. Most likely the new method needs adding to its list.
     - Re-run the full worker `cargo test --lib`.
2. **Dead-worker reaper: done.**
   - The 30 s sweeper ends a CLOUD_RUNNING turn whose execution has had no live lease for 30 s. The thread goes back to CLOUD_IDLE, or RECLAIMING if a take back is pending, and a `handoff.turn_reaped` audit row is written.
   - Its Postgres tests pass.
3. **Docker CI job never run.** The `workspace-run-docker` job (Docker bridge plus egress proof) has not run yet.

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
