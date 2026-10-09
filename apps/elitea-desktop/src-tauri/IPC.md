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
| `host_sign_out` | — | `boolean`: `true` when the server confirmed the revoke; `false` means it is kept (credentials file) and retried at the next launch |
| `host_wipe` | — | `null` |

The session lives in the credentials file only. A build that used the OS
keychain is not migrated (the host never touches the keychain): after an
upgrade `host_state` reports signed out, and the old device session is revoked
from the web's Settings › Devices or idles out (README, "Upgrading from a
keychain build").

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
| `workspace_remove` | `{id}` | `null` — forgets the workspace, its host data (remembered approvals, copy checkpoints), its turns and its thread history; the folder is never touched. Rejects with `workspace_busy` while a turn runs in it. |
| `workspace_bind_project` | `{id, project_id}` | `Workspace` — rejects with `workspace_busy` while a turn (or an undo) runs in it, `workspace_unknown` for an id it does not know. |
| `workspace_files` | `{workspace_id, query?: string, limit?: number}` | `{path, kind: "file" \| "dir"}[]` — the "@" picker. Workspace-relative paths (no trailing `/`) matching `query` case-insensitively: the name starting with it, then containing it, then the path containing it, then its characters in order; shallower and shorter first. `.gitignore` is honoured, `.git` and `path_deny` matches are left out, symlinks are neither listed nor followed. At most `limit` (default 50, at most 200); a folder past 20 000 entries answers from its first part. Answers while a turn runs. Rejects `local_work_disabled`, `workspace_unknown`, `workspace_unavailable`. |

Workspaces are stored in the app data directory (`workspaces.json`), never
inside the folder.

## The local agent turn (D0, `src/d0/`)

| Command | Arguments | Result |
| --- | --- | --- |
| `agent_turn_start` | `{workspace_id, project_id, conversation_id, application_id, version_id, prompt, plan_mode, mentions?: string[]}` | `{turn_id, execution_id}` |
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
`invalid_request`, `not_signed_in`, and the platform's own codes (`local_turn_conflict`,
`not_found`, …).

A turn is held to the session it started under (the connected origin and
the sign-in; a token refresh keeps it): once the person signs out, signs
in again or connects to another deployment, every further request of the
turn is refused locally with `identity_changed`, so it is never committed
under another identity. `host_sign_out`, `host_wipe`, `host_sign_in` and a
`host_connect` to another deployment also cancel every running turn (it
ends `cancelled` → `done`, `committed: false`) and forget every workspace
session and kept turn: an earlier turn then answers `turn_unknown`.

`plan_mode: true` offers read-only local tools only and no remote toolkits.

`mentions` are the workspace-relative paths the person referenced with "@"
(a folder may end with `/`; at most 50). Each must resolve inside the
workspace, exist as a file or folder (not a symlink) and not match
`path_deny`, else the start is refused with `invalid_request` before any
request. The checked paths are added to the user message, deduplicated, as

```text
<prompt>

Files the user referenced:
- src/main.rs
- docs/
```

and that message is what the turn starts, runs and commits with. File
contents are never inlined: the agent reads them with its own tools.

**AGENTS.md.** At every turn start (plan mode included) the host reads the
workspace's root `AGENTS.md` (name matched case-insensitively; an exact
`AGENTS.md` wins over another spelling in the same folder) and, for each
mention, the `AGENTS.md` of every folder between it and the root, nearest
first, deduplicated. Reads go through the confined workspace: a symlinked
file or folder is not read and a `path_deny` match is refused. All files
together are capped at 32 KiB; a cut file ends with a truncation note and
files past the cap are skipped. They are appended to the agent's system
instructions, after the agent's own instructions (which keep priority, as
the section's header says) and before the memory splice, as one
`## Project instructions (AGENTS.md)` section with one
`<agents_md path="…">` block per file. Edits apply from the next turn.

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

## Thread history (`src/history.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `thread_history` | `{workspace_id, conversation_id}` | `{turns: StoredTurn[]}` — the turns of that thread the host recorded for the signed-in account, oldest first. `[]` when it has none (the thread ran on the web or another machine, or before this version) or runs without a store. `conversation_id` is the id the turns started with or the conversation's UUID. Rejects `invalid_request`, `not_signed_in`, `storage`, or the platform's code when the user lookup fails. |
| `thread_history_delete` | `{workspace_id, conversation_id}` | `{deleted: number}` — forgets that thread's stored turns (the signed-in account's only). The conversation on the server is untouched. |

```ts
type StoredTurn = {
  turn_id: string;
  conversation_id: string;          // as the turn was started (the UI's id)
  conversation_uuid: string | null;
  prompt: string;                   // as typed; the "@" list is `mentions`
  mentions: string[];
  started_at: number;               // Unix ms
  finished_at: number | null;       // when `done` was recorded
  events: AgentEvent[];             // the turn's `agent://event` stream (below)
  changes: FileChange[] | null;     // what it changed, as it ended
  events_truncated: boolean;        // the turn outgrew the per-turn cap
  state: "done" | "running" | "interrupted"; // interrupted: never ended (the app quit)
  live: boolean;                    // turn_changes / checkpoint_restore still answer for it
};
```

The host records every event of a started turn as it emits it, so the
history survives a webview reload or crash; replaying a turn is folding its
`events` exactly as the live stream (consecutive `text_delta` events are
stored as one, numbered by the last of them). A refused start is not
recorded. Bounds: a string in one event is cut at 16 KiB, a turn's events
at 4 MiB (past it only `status`, `error` and `done` are kept and
`events_truncated` is set), a stored diff at 64 KiB (1 MiB per turn), and a
thread keeps its newest 200 turns within 32 MiB.

The store is `threads.sqlite` in the app data directory (owner-only, like
the credentials file; SQLite WAL, versioned with `PRAGMA user_version`).
Rows are keyed by the deployment origin, the signed-in user's id
(`GET /api/v2/social/author`, looked up once per sign-in), the workspace
and the conversation: another account or deployment never reads them.
**Sign-out keeps the history** (it is keyed by user, so the next sign-in of
the same account sees it again, and another account does not);
`workspace_remove` deletes the workspace's rows for every account.

## App and native shell (`src/platform.rs`, `src/local_commands.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `app_platform` | — | `AppPlatform` (below). Never rejects. |
| `reveal_path` | `{workspace_id, path}` | `null` — shows the file or folder in Finder / the file manager. |
| `open_path` | `{workspace_id, path}` | `null` — opens it with its default app. |

```ts
type AppPlatform = {
  os: "macos" | "linux" | "windows";
  titlebar_overlay: boolean;      // the web content runs under the title bar (macOS): draw a
                                  // `data-tauri-drag-region` and leave room for the window controls
  traffic_light_inset_px: number; // room to leave on the left of the top bar (90 on macOS, else 0)
  vibrancy: boolean;              // the window is transparent over the system sidebar material
                                  // (macOS): a sidebar painted transparent shows it
};
```

`path` of `reveal_path` / `open_path` is **workspace-relative** (`""` or `.`
is the workspace folder itself; a trailing `/` is ignored). The host
resolves it through the same confined view as the agent's tools: an
absolute path, a `..` escape, a symlink (or an in-workspace symlink on the
way that leads out) and a `path_deny` match are refused with
`invalid_request`; a missing path with `not_found`. Also
`local_work_disabled`, `workspace_unknown`, `workspace_unavailable`.
`open_path` refuses with `open_refused` anything the OS would run rather
than show (an app bundle, a script — `.command`, `.sh`, `.py`, … —, an
installer, a file with an execute bit): offer `reveal_path` instead. An OS
failure rejects with `os_refused`. The webview's drag region works through
`core:window:allow-start-dragging` and `core:window:allow-internal-toggle-maximize`
(double-click to zoom); `core:window:allow-set-theme` lets the UI make the
native window follow its light/dark mode. Those are the only window
permissions granted. `log:allow-log` (`plugin:log|log`) lets the UI append
scrubbed diagnostics to the host log file (README, "Logs").

### `app://command`

Menu items, shortcuts and OS actions that the UI must carry out arrive as
one event, `app://command`, emitted to the `main` window:

```ts
type AppCommand = { id: string; args?: object };
```

| `id` | `args` | Sent when |
| --- | --- | --- |
| `new_thread` | — | File ▸ New Thread (⌘N / Ctrl+N) |
| `settings` | — | Elitea ▸ Settings… (⌘, ; File ▸ Settings… on Linux/Windows) |
| `command_palette` | — | View ▸ Command Palette… (⌘K) |
| `toggle_sidebar` | — | View ▸ Toggle Sidebar (⌘\\) |
| `toggle_changes` | — | View ▸ Toggle Changes Panel (⌘⌥\\) |
| `back` | — | View ▸ Back (⌘[) |
| `forward` | — | View ▸ Forward (⌘]) |
| `workspace_opened` | `{workspace_id: string}` | A folder became (or already was) a workspace through File ▸ Open Folder… (⌘O), a drop on the window, or a drop on the dock icon / "Open With" in Finder. One event per folder; the workspace is already in `workspace_list`, so the UI selects it. |
| `workspace_open_failed` | `{message: string}` | One of those folders could not be opened (the first failure; a message for a person). |
| `files_dropped` | `{paths: string[]}` | Files (not folders) were dropped on the window: their **absolute** paths, in drop order. The host does nothing else with them; a path inside a workspace can be made relative against `Workspace.path` (e.g. for `mentions`). |
| `focus_turn` | `{workspace_id: string, turn_id: string}` | Reserved: show this turn. Not sent yet — the notification plugin has no click callback on desktop, so clicking a notification only activates the app. |

**Open Folder… is host-side**: the menu item runs the native folder picker
in the host (the same picker and `WorkspaceStore::add` as `workspace_open`)
and then sends `workspace_opened`; there is no `open_folder` command id for
the UI to handle. Cancelling the picker sends nothing. Menu-forwarded ids
also bring the window forward first. Events are not replayed: a folder
dropped on the dock icon while the app starts is opened before the UI
listens — it is in `workspace_list` either way. In the desktop build the
UI should not also bind these shortcuts in the page: the menu accelerator
fires the command, and a page keydown handler for the same keys could run
the action twice.

Standard actions are the OS's own and never reach the UI: Edit's
undo/redo/cut/copy/paste/select all, Hide, Quit, Minimize, Zoom, Full
Screen, Close Window (on macOS it hides the window; the dock icon shows
it again), the zoom items (Actual Size ⌘0, Zoom In ⌘=, Zoom Out ⌘-, the
host sets the webview zoom), Reload ⌘R (debug builds only) and Help ▸
Elitea Help (opens `<deployment>/docs/` in the browser when connected).

Notifications and the dock badge are the host's own, from the
`agent://event` stream (README, "Native features"); the UI does nothing
for them.

## Events

Two event names, both emitted to the `main` window only: `app://command`
(above) and `agent://event`. The webview may listen
(`core:event:allow-listen` / `allow-unlisten`), never emit.

`agent://event`:

```ts
type AgentEvent = { turn_id: string; seq: number; kind: string; payload: object };
```

`seq` starts at 0 for each turn and grows by one per event, in emission
order. Kinds and payloads:

| `kind` | `payload` |
| --- | --- |
| `status` | `{phase: "resolving" \| "starting" \| "running" \| "committing" \| "done" \| "cancelled" \| "error", message?: string, project_instructions?: string[]}` — `project_instructions` (on `running` only, when any) lists the AGENTS.md paths the turn applies, in order |
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
