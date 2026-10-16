# Workspace handoff — Phase 1 wire contract (ADR-0034)

Binding for every Phase 1 unit (server PR, desktop PR, executor PR). The ADR is
`elitea-docs/docs/internal/03-architecture/adrs/adr-0034-desktop-cloud-handoff.mdx`
(read it first; decisions are numbered D1–D15 below). If the code forces a deviation, the
implementer records it in `DEVIATIONS.md` next to this file and in the PR body — never silently.

Phase 1 scope: COMPACT segments only; take-back fast path + worktree + discard (no clean merge,
no markers, no turn-k); one sandbox per turn; GitHub `open_pull_request`. Mobile is out of scope.

## 1. Names

- `projectID` int, `conversationUUID` uuid (the server conversation of the Local work thread).
- States: `LOCAL`, `HANDING_OFF`, `CLOUD_IDLE`, `CLOUD_RUNNING`, `RECLAIMING` (D7). Flag `reclaim_pending`.
- Refs: `refs/elitea/base`, `refs/elitea/upstream` (optional), `refs/elitea/handoff/1` (Phase 1: N is always 1
  per handoff; a later send of the same thread after release starts a new handoff, refs reset),
  `refs/elitea/cloud`.
- Segment: a **git bundle v2** file (plain, unencrypted — D4), kind `COMPACT` in Phase 1, content address =
  lowercase hex SHA-256 of the file bytes. Object format `sha1` (Phase 1 refuses `sha256` repos with
  `422 unsupported_object_format`).
- Bucket: type `workspace`, reserved name `elitea-workspaces`, key
  `{keyPrefix}/p/{projectID}/b/elitea-workspaces/o/t/{conversationUUID}/seg/{sha256}.seg` (D3).
- `executed_by` value for cloud turns: `cloud_executor`; meta also `workspace_epoch` (int), `workspace_commit` (sha).

## 2. Desktop/web REST routes (`/api/v2/elitea_core/...`, D14)

All JSON. Errors are the existing desktop error envelope (`desktopwire.WriteError`: `{"error": {"code","message"}}`
— follow what `localturns` does). Auth: native token (desktop) unless noted; permission
`models.chat.messages.create`; caller must be the conversation creator unless noted.

### 2.1 Begin — `POST workspace_handoff/prompt_lib/{projectID}/{conversationUUID}`
Native token only (`403 handoff_requires_token` otherwise). Conversation must have `source='local_work'`,
state `LOCAL` (no row = LOCAL), no uncommitted local turn (`409 local_turn_in_progress`).
```json
req  {"device_id":"<native client device id>","history_mode":"none",
      "stats":{"files":42,"bytes":1234567,"unpacked_bytes":5555555,"excluded":["./.env"],"scan_findings":0},
      "has_upstream":true,"note":"<handoff note text, <=8 KiB>"}
201  {"handoff_id":"<uuid>","epoch":1,"state":"HANDING_OFF",
      "allowed_refs":["refs/elitea/base","refs/elitea/upstream","refs/elitea/handoff/1"],
      "expected_old":{"refs/elitea/base":null,"refs/elitea/upstream":null,"refs/elitea/handoff/1":null},
      "limits":{"max_segment_bytes":209715200,"max_file_bytes":26214400,"max_files":20000,
                "max_unpacked_bytes":2147483648,"project_workspace_bytes_left":53687091200},
      "expires_at":"<rfc3339, now+15m>"}
```
Errors: `403 cloud_handoff_disabled` (effective policy off: platform ∧ project ∧ device `local_work_cloud_handoff`),
`409 thread_executor_is_cloud`, `409 handoff_in_progress`, `409 local_turn_in_progress`,
`413 workspace_quota_exceeded`, `422 invalid_handoff`, `422 history_mode_not_allowed`.

### 2.2 Upload grant — `POST workspace_store_upload/prompt_lib/{projectID}/{conversationUUID}`
```json
req  {"epoch":1,"sha256":"<hex>","bytes":123456}
200  {"exists":false,
      "put":{"url":"<presigned PUT>","headers":{"Content-Type":"application/x-git-bundle"}},
      "multipart":null,            // or {"upload_id","part_size","part_urls":[...],"complete_url"} when bytes > 64 MiB
      "expires_at":"<rfc3339 <=15m>"}
```
`exists:true` (and no put) when that digest is already stored for this thread. Backends without presign:
`put.url` points at the ADR-0016 streaming facade route — the client does not care.
Errors: `409 stale_epoch`, `413 segment_too_large`, `413 workspace_quota_exceeded`.

### 2.3 Publish — `POST workspace_store_publish/prompt_lib/{projectID}/{conversationUUID}`
```json
req  {"epoch":1,"kind":"COMPACT","segment_sha256":"<hex>","bytes":123456,"prerequisites":[],
      "refs":[{"name":"refs/elitea/base","old":null,"new":"<sha1>"}, ...],"object_format":"sha1"}
200  {"seq":1,"refs":{"refs/elitea/base":"<sha1>", ...},"superseded":[]}
```
Main HEADs the object BEFORE opening the transaction (size + checksum), then one short tx (D2/D15).
Identical replay (same segment + refs already applied) → the original 200. Errors: `409 ref_conflict`
`{"current":{name:sha}}` in `error.details`, `409 stale_epoch`, `403 ref_not_allowed`,
`422 segment_missing`, `422 checksum_mismatch`, `422 unsupported_object_format`.

### 2.4 Complete — `POST workspace_handoff_complete/prompt_lib/{projectID}/{handoffID}`
```json
req  {"epoch":1,"handoff_ref":"refs/elitea/handoff/1","sha":"<sha1>"}
200  {"state":"CLOUD_IDLE","epoch":2}
```
Main checks `workspace_refs` has that sha (not the bytes). Appends the system-authored event message.

### 2.5 Fetch plan — `GET workspace_store_fetch/prompt_lib/{projectID}/{conversationUUID}?since_seq=0`
```json
200  {"head_seq":3,"refs":{"refs/elitea/cloud":"<sha1>", ...},
      "segments":[{"seq":3,"kind":"COMPACT","sha256":"<hex>","bytes":1,"prerequisites":[],
                   "refs":{...},"url":"<presigned GET>","expires_at":"..."}]}
```
Phase 1: always the newest COMPACT (≥ since_seq) only.

### 2.6 Reclaim — `POST workspace_handoff_reclaim/prompt_lib/{projectID}/{conversationUUID}`
Desktop only. `CLOUD_IDLE` → `RECLAIMING` (epoch+1). `CLOUD_RUNNING` → sets `reclaim_pending`, 202, the
transition happens at turn end/cancel (turn cancelled after 60 s).
```json
req  {"epoch":2,"device_id":"..."}
200  {"state":"RECLAIMING","epoch":3,"cloud_sha":"<sha1>|null"}
202  {"state":"CLOUD_RUNNING","reclaim_pending":true,"epoch":2,"waiting_for_execution_id":"<uuid>"}
```

### 2.7 Reclaim request — `POST workspace_handoff_reclaim_request/prompt_lib/{projectID}/{conversationUUID}`
Web/mobile (cookie or token). Owner only. Sends the owner a notification (existing notifications channel,
kind `workspace_reclaim_requested`); no state change. `202 {}`.

### 2.8 Release — `POST workspace_handoff_release/prompt_lib/{projectID}/{conversationUUID}`
```json
req  {"epoch":3,"outcome":"applied|worktree|discarded","applied_sha":"<sha1>|null"}
200  {"state":"LOCAL","epoch":4,"cloud_copy_delete_after":"<rfc3339, now+return_grace_days>"}
```
Appends "Work returned to <device name>".

### 2.9 State / erase — `GET|DELETE workspace_handoff/prompt_lib/{projectID}/{conversationUUID}`
GET (cookie or token, project member who can read the conversation):
```json
200  {"state":"CLOUD_IDLE","epoch":2,"reclaim_pending":false,"handoff_id":"...","device_name":"...",
      "history_mode":"none","has_upstream":true,"stats":{...},"cloud_sha":"...","updated_at":"...",
      "delete_after":"...|null","workspace_bytes":123}
```
No row → `{"state":"LOCAL","epoch":0}`. DELETE: owner, or `models.project_settings.edit`; cancels a running
turn; state → LOCAL; erases objects (async sweeper, `deleted_at` after empty listing); system event message
"the cloud turns' file changes were discarded". `200 {"state":"LOCAL","epoch":N}`.

### 2.10 Changed contracts
- `local_turn` start/commit: `409 thread_executor_is_cloud` when state ≠ LOCAL.
- `local_work_thread` fence (all four start paths) relaxed only while state ∈ `CLOUD_*`.
- Regenerate: refuses a response with `meta.executed_by == "desktop"` (`409 local_work_turn`); only the newest
  cloud turn; strips `workspace_epoch`/`workspace_commit` too.
- Conversation responses gain `workspace_executor`: `local|cloud|returning` (absent/`local` when no row).
- Client contract version: minor bump; new ops added to the contract lock.

## 3. Policy (D11)

- Native client policy key `local_work_cloud_handoff` bool, default false (platformconfig/nativeclientpolicy.go).
- Platform section `cloud_executor` (platformconfig): `enabled=false, shell=true, network=true,
  network_allowlist=[], history_full_allowed=false, max_segment_mb=200, max_file_mb=25, max_files=20000,
  max_unpacked_mb=2048, max_project_workspace_gb=50, idle_retention_days=30, return_grace_days=7,
  turn_wall_seconds, cpu, memory, disk` (last four: chart-provided).
- Project setting: tenant `configuration` row `type='cloud_executor'`, `section='project_settings'`, keys
  `enabled, shell, network, network_allowlist, history, (optional lower caps)`; edit permission
  `models.project_settings.edit`; can only narrow.
- Effective = ceiling ∧ project ∧ device; recomputed at begin and at every cloud claim.

## 4. Executor (PR 3; listed so PR 1 leaves room)

gRPC on the runtime plane (mTLS, claim + fence bound): `WorkspaceFetchPlan`, `WorkspaceUpload`,
`WorkspacePublish`, `WorkspacePullRequestDiff`. Runtime-context response field `workspace` served only when
`X-Elitea-Runtime-Context-Accept` lists it: `{"thread":"<uuid>","epoch":2,"expected_old_cloud":"<sha>|null",
"limits":{...},"network":true,"shell":true,"network_allowlist":[],"has_upstream":true}`. PR 1 implements the
state transitions `CLOUD_IDLE→CLOUD_RUNNING→CLOUD_IDLE` and the publish rule for `refs/elitea/cloud` in the Go
service so PR 3 only adds transport.

## 5. Desktop IPC (PR 2, Tauri, snake_case)

`handoff_preview {workspace_id, conversation_uuid}` → `{files, bytes, unpacked_bytes, excluded[], findings[{path,line,rule}], has_upstream, project_workspace_bytes_left}`;
`handoff_send {workspace_id, conversation_uuid, leave_on_laptop[]}` (progress events on `handoff://event`
`{conversation_uuid, phase: snapshot|scan|bundle|upload|publish|complete|error, done, total, message}`);
`handoff_status {conversation_uuid}`; `handoff_reclaim {conversation_uuid}`;
`handoff_preflight {conversation_uuid}` → `{preflight_id, outcome: fast_path|conflicts|error, changed_paths[], conflict_paths[], skipped_paths[], cloud_turns}`;
`handoff_apply {preflight_id, mode: apply|worktree|discard}` → `{applied, outcome, checkpoint_id, worktree_path|null}`;
`handoff_erase {conversation_uuid}`.
Phase 1 note: with `fast_path` false (folder changed since send) the only modes offered are `worktree` and `discard`.

## 6. Amendment 2026-10-15 (owner; elitea-docs PR #51) — configurable exclusions, explicit include, env harvest, cancel

### 6.1 Effective policy before begin — `GET workspace_handoff_policy/prompt_lib/{projectID}`
Native token or cookie. Returns the caller's effective handoff policy (platform ceiling ∧ project ∧ device):
```json
200 {"enabled":true,"shell":true,"network":true,"history_modes":["none"],
     "exclusions":["<platform patterns>","<project additions>"],   // merged, in order; user additions stay on the desktop
     "path_deny":["<effective local_work_path_deny>"],            // hard deny, never includable
     "allow_user_include":true,
     "env_harvest":false,"env_deny":["PATH","HOME","SSH_*", "..."],"env_allow":null,
     "limits":{"max_segment_bytes":...,"max_file_bytes":...,"max_files":...,"max_unpacked_bytes":...,
               "project_workspace_bytes_left":...},
     "env_limits":{"max_vars":200,"max_bytes":65536}}
```
The desktop calls it in `handoff_preview`; the begin response's `limits` stay authoritative.

### 6.2 Begin request additions (§2.1)
```json
"included_excluded":["./.env"],           // explicit includes; 422 include_not_allowed if policy off,
                                          // 422 path_denied if any matches path_deny
"env":{"DATABASE_URL":"postgres://..."}   // optional; 422 env_harvest_disabled if off,
                                          // 422 env_name_denied {names}, 413 env_too_large
```
Main re-applies the effective policy. It stores `env` as ONE per-thread vault secret (owner-only, erased with the
thread or when a later begin sends `env:{}`), never in the bundle, never logged. The audit records `included_excluded`
paths and env NAMES only. `stats.excluded` stays the list of excluded paths. The manifest also lists `included_excluded`.

### 6.3 Cancel
`DELETE workspace_handoff/...` also accepts `HANDING_OFF` (owner only) → `LOCAL`, and erases the thread objects and the env secret.

### 6.4 Executor
The runtime-context `workspace` field gains `"env_secret":true|false`. When true, the worker redeems the env values as a
claim-bound runtime secret (ADR-0011 path, not in the JSON field), adds them only to the `run_command` sandbox
environment, and redacts every value (each value ≥ 4 chars, plus its base64 form) from tool output, the transcript and
the trace before anything leaves the worker. The re-scan on hydrate skips `included_excluded` paths (read from the
manifest in the handoff commit trailer `Elitea-Included`).

### 6.5 Desktop
Per-folder handoff settings (stored in the desktop's local state, never in the repo): user exclusion patterns,
explicit includes, and selected env var names (values are re-read at send, never stored). The preview shows exclusions
grouped by source (platform / project / user / path_deny), includes with their scan findings (warning + re-confirm),
and, when harvest is on, the selectable env names (deny-filtered, values masked, none preselected).
