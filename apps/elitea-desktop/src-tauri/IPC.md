# Desktop host IPC

The contract between the bundled elitea-web desktop build and the Tauri
host (ADR-0029). Every command is listed in `build.rs` and allowed in
`capabilities/default.json` for the `main` window only; no remote origin
gets IPC. Nothing here ever returns a refresh token, a toolkit secret or an
MCP OAuth token.

Call commands with `invoke(name, args)`. **Argument keys are snake_case,
exactly as written below** (the local-work commands are declared with
`rename_all = "snake_case"`). A failed command rejects with one string, a
message written for a person.

## Connection and sign-in (`src/commands.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `host_state` | — | `HostState` |
| `host_connect` | `url` | `DeploymentInfo` |
| `host_sign_in` | — | `HostState` |
| `host_access_token` | — | `{token, expiresIn}` or `null` |
| `host_refresh` | — | `"refreshed" \| "ended" \| "unavailable" \| "upgrade_required"` |
| `host_sign_out` | — | `boolean`: `true` when the server confirmed the revoke; `false` means it is kept (keychain) and retried at the next launch |
| `host_wipe` | — | `null` |

## Workspaces (`src/local_commands.rs`, `src/workspaces.rs`)

```ts
type Workspace = {
  id: string;               // 32 hex characters
  path: string;             // canonical absolute path
  name: string;             // the folder's name
  project_id: number | null; // the project local turns run in
  is_git: boolean;          // inside a git work tree (computed when listed)
};
```

| Command | Arguments | Result |
| --- | --- | --- |
| `workspace_open` | — | `Workspace \| null` — the native folder dialog; `null` when cancelled. Opening a folder that is already a workspace returns that workspace. |
| `workspace_list` | — | `Workspace[]` |
| `workspace_remove` | `{id}` | `null` — forgets the workspace and its host data (remembered approvals, copy checkpoints); the folder is never touched. |
| `workspace_bind_project` | `{id, project_id}` | `Workspace` |

Workspaces are stored in the app data directory (`workspaces.json`), never
inside the folder.

## The local agent turn (D0, `src/d0/`)

| Command | Arguments | Result |
| --- | --- | --- |
| `agent_turn_start` | `{workspace_id, project_id, conversation_id, application_id, version_id, prompt, plan_mode}` | `{turn_id, execution_id}` |
| `agent_turn_cancel` | `{turn_id}` | `null` (rejects for an unknown turn) |
| `approval_respond` | `{request_id, decision: "allow_once" \| "allow_always" \| "deny"}` | `null` (rejects when the question is no longer open) |
| `turn_changes` | `{turn_id}` | `{files: FileChange[]}` |
| `checkpoint_restore` | `{turn_id, path?: string}` | `{restored: string[]}` |

`agent_turn_start` resolves the agent version, checks it, and starts the
local turn on the platform before it resolves; the run then continues in
the background and reports through events. `conversation_id` is the
conversation's numeric id or its UUID. The conversation must hold the agent
as a participant on `version_id`. A refusal rejects the call **and** is sent
as an `error` event plus a `status` `error` event of a fresh `turn_id`.
Refusal codes (in the `error` event): `local_work_disabled`,
`secrets_withheld` ("this agent needs secrets; run it in the cloud"),
`pipeline_unsupported`, `nested_agents_unsupported`,
`platform_mcp_unsupported`, `unknown_tool_kind`, `toolkit_ref_missing`,
`model_unresolved`, `agent_not_in_conversation`, `agent_version_mismatch`,
`workspace_unknown`, `workspace_project_mismatch`, `workspace_busy`,
`invalid_request`, and the platform's own codes (`local_turn_conflict`,
`not_found`, …).

`plan_mode: true` offers read-only local tools only and no remote toolkits.

```ts
type FileChange = {
  path: string;   // workspace-relative
  status: "added" | "modified" | "deleted" | "renamed";
  added: number;  // lines
  removed: number;
  diff: string;   // unified diff; empty for a binary file
};
```

`turn_changes` covers files the turn's file tools (`write_file`,
`edit_file`, `apply_patch`) changed, against what they held before the turn
first changed them. A file only a shell command changed is not listed (D0),
but `checkpoint_restore` undoes it with the rest of the turn.

`checkpoint_restore` without `path` puts the whole workspace back to the
turn's checkpoint (taken before its first change); with `path` only that
file. It answers the files written back or deleted, and `[]` for a turn
that changed nothing. It rejects while a turn runs in the workspace.

## Events

One event name, `agent://event`, emitted to the `main` window:

```ts
type AgentEvent = { turn_id: string; seq: number; kind: string; payload: object };
```

`seq` starts at 0 for each turn and grows by one per event, in emission
order. Kinds and payloads:

| `kind` | `payload` |
| --- | --- |
| `status` | `{phase: "resolving" \| "starting" \| "running" \| "committing" \| "done" \| "cancelled" \| "error", message?: string}` |
| `text_delta` | `{text}` — model text as it streams |
| `tool_call` | `{call_id, tool, args_summary, remote: boolean}` |
| `tool_result` | `{call_id, ok: boolean, summary, truncated: boolean}` |
| `approval_request` | `{request_id, tool, title, detail, command?: string[], paths?: string[], reason, can_remember: boolean}` |
| `error` | `{code, message}` |
| `done` | `{committed: boolean, conversation_id, message_ids: string[], changed_files: number}` |

Order of a turn: `status` `resolving` → `status` `starting` → `status`
`running` → (`text_delta`, `tool_call`, `approval_request`, `tool_result`)* →
`status` `committing` → `status` `done` | `error` → `done`. A cancelled turn
ends `status` `cancelled` → `done` with `committed: false`; nothing is
committed and its execution expires on the server. A run that fails after
the start is still committed, as a failed answer (`is_error`), and its
`error` event comes before `status` `committing`.

`message_ids` are the question's and the answer's message UUIDs, in that
order, when committed. `approval_request.can_remember` is true when
`allow_always` is remembered for this workspace (a simple command or a
file change); otherwise `allow_always` answers once. Remote toolkit
confirmations (`confirmation_required`) never remember.
