# Desktop host IPC

The contract between the bundled elitea-web desktop build and the Tauri
host (ADR-0029). Every command is listed in `build.rs` and allowed in
`capabilities/default.json` for the `main` window only; no remote origin
gets IPC. Nothing here ever returns a refresh token, a toolkit secret or an
MCP OAuth token.

Call commands with `invoke(name, args)`. **Argument keys are snake_case,
exactly as written below** (the local-work commands are declared with
`rename_all = "snake_case"`).

Errors: a failed connection / sign-in command (`host_*`) rejects with one
string, a message written for a person. A failed workspace or turn command
(everything from **Workspaces** on) rejects with an object

```ts
type IpcError = { code: string; message: string };
```

`code` is a stable machine code the UI branches on (`workspace_busy`,
`agent_version_mismatch`, `turn_expired`, `storage`, …; the refusal codes
are listed under `agent_turn_start`), `message` is written for a person.
The web client (`shared/desktop/workspaceIpc.ts`) turns it into a
`WorkspaceIpcError`.

## Connection and sign-in (`src/commands.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `host_state` | — | `HostState` |
| `host_connect` | `url` | `DeploymentInfo` |
| `host_sign_in` | — | `HostState` |
| `host_sign_in_cancel` | — | `null` (the waiting `host_sign_in` rejects with "sign-in was cancelled or timed out") |
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
  project_id: number | null; // the project local turns run in; a turn needs one
  is_git: boolean;          // inside a git work tree (computed when listed)
};
```

| Command | Arguments | Result |
| --- | --- | --- |
| `workspace_open` | — | `Workspace \| null` — the native folder dialog; `null` when cancelled. Opening a folder that is already a workspace returns that workspace. |
| `workspace_list` | — | `Workspace[]` |
| `workspace_remove` | `{id}` | `null` — forgets the workspace, its host data (remembered approvals, copy checkpoints) and its turns; the folder is never touched. Rejects with `workspace_busy` while a turn runs in it. |
| `workspace_bind_project` | `{id, project_id}` | `Workspace` — rejects with `workspace_busy` while a turn (or an undo) runs in it, `workspace_unknown` for an id it does not know. |

Workspaces are stored in the app data directory (`workspaces.json`), never
inside the folder.

## The local agent turn (D0, `src/d0/`)

| Command | Arguments | Result |
| --- | --- | --- |
| `agent_turn_start` | `{workspace_id, project_id, conversation_id, application_id, version_id, prompt, plan_mode}` | `{turn_id, execution_id}` |
| `agent_turn_cancel` | `{turn_id}` | `null` when the turn was running (or was already cancelled): it stops and commits nothing. Rejects `turn_not_cancellable` once the agent's run has ended (the turn is being committed, or it ended): nothing was stopped. `turn_unknown` / `turn_expired` for a turn the host does not keep. |
| `agent_turn_status` | `{turn_id}` | `{state: "running" \| "committing" \| "done", done: DonePayload \| null}` — `done` is the `done` event's payload once it was sent. Rejects `turn_unknown` / `turn_expired` for a turn the host does not keep (an app restart forgets every turn). |
| `approval_respond` | `{request_id, decision: "allow_once" \| "allow_always" \| "deny"}` | `null` (rejects `approval_closed` when the question is no longer open, `invalid_request` for another decision) |
| `turn_changes` | `{turn_id}` | `{files: FileChange[]}` |
| `checkpoint_restore` | `{turn_id, path?: string}` | `{restored: string[]}` |

`agent_turn_start` resolves the agent version, checks it, and starts the
local turn on the platform before it resolves; the run then continues in
the background and reports through events. `conversation_id` is the
conversation's numeric id or its UUID. The conversation must hold the agent
as a participant pinned to `version_id` (`entity_settings.version_id`) or
not pinned to any version; when every entry of the agent is pinned to
another version the start is refused with `agent_version_mismatch`. A refusal rejects the call **and** is sent
as an `error` event plus a `status` `error` event of a fresh `turn_id`.
Refusal codes (the rejection's `code` and the `error` event's): `local_work_disabled`,
`secrets_withheld` ("this agent needs secrets; run it in the cloud"),
`pipeline_unsupported`, `nested_agents_unsupported`,
`platform_mcp_unsupported`, `unknown_tool_kind`, `toolkit_ref_missing`,
`model_unresolved`, `agent_not_in_conversation`, `agent_version_mismatch`,
`workspace_unknown`, `workspace_unbound` (no project bound yet),
`workspace_project_mismatch`, `workspace_busy`,
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
that changed nothing. It rejects (`workspace_busy`) while a turn runs in
the workspace, and with `no_checkpoint` for a turn that changed files
without a checkpoint (a folder too large to checkpoint: the turn ran, but
cannot be undone here).

The host keeps the last 20 turns of each workspace for `turn_changes` and
`checkpoint_restore`; an older turn rejects with `turn_expired`, an id it
never ran with `turn_unknown`.

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
`error` event comes before `status` `committing`. `done` is always the last
event of a started turn, and the host holds the workspace (`workspace_busy`)
until it is sent, so the UI is busy until `done`, not until `error`.
Events are not replayed: a UI that may have missed one (the window was
hidden, a cancel was answered `turn_unknown` or `turn_not_cancellable`)
asks `agent_turn_status` and takes its `done` payload as the event.

`message_ids` are the question's and the answer's message UUIDs, in that
order, when committed. `approval_request.can_remember` is true when
`allow_always` is remembered for this workspace (a simple command or a
file change); otherwise `allow_always` answers once. Remote toolkit
confirmations (`confirmation_required`) never remember.
