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
| `host_sign_out` | — | `null`, at once: the session is forgotten locally, the revoke is queued (credentials file, `pending-revoke`) and sent in the background; one that does not get through is retried at the next launch |
| `host_wipe` | — | `null` |

The session lives in the credentials file only. A build that used the OS
keychain is not migrated (the host never touches the keychain): after an
upgrade `host_state` reports signed out, and the old device session is revoked
from the web's Settings › Devices or idles out (README, "Upgrading from a
keychain build").

## Network (`src/net.rs`)

The webview's `fetch` (`apps/elitea-web/src/shared/desktop/hostFetch.ts`).
Every request goes out on the host's one pooled HTTP client, the same one the
host's own calls use. It replaces `tauri-plugin-http`, which built a client
(and loaded the TLS roots) for every request.

| Command | Arguments | Result |
| --- | --- | --- |
| `http_fetch` | the request body as the **raw** IPC payload (a `Uint8Array`, sent as is), and the invoke header `x-elitea-fetch`: percent-encoded JSON `{id, method, url, headers: [name, value][]}` (at most 256 KiB) | `{status, statusText, headers: [name, value][], url, hasBody}`, the response head. The body stays with the host under `id` until it has been read to the end, cancelled, or left unread too long (below). |
| `http_read_body` | `{id}` | the next chunk as raw bytes (an `ArrayBuffer`); **zero bytes means the end**, and the id is then forgotten |
| `http_cancel` | `{id}` | `null`. Aborts a pending `http_fetch`, or ends a body or stream; a pending read rejects with `aborted`. An id the host has not seen yet is remembered for 30 s (at most 1024 ids), and an `http_fetch` with it is then refused with `aborted` before anything is sent: IPC calls can overtake each other. |

`id` is chosen by the page, so an abort can name a request before its head
arrives; an id still in flight is refused. The rules:

- only `http`/`https` URLs on an origin the host granted (the stored
  deployment at launch, then each `host_connect`), and no userinfo;
  otherwise `url_not_allowed`, and nothing is sent;
- redirects are never followed: a 3xx is returned as it is;
- the fetch spec's forbidden request headers (`Cookie`, `Host`, `Origin`,
  `Content-Length`, `Sec-*`, `Proxy-*`, …) are dropped; `Origin:
  tauri://localhost` is sent; there is no cookie jar;
- a HEAD request and a null-body status (1xx, 204, 205, 304) have
  `hasBody: false` and nothing to read; the host keeps nothing;
- a request body is at most **160 MiB**: the largest single-request
  upload the web app makes is an artifact (one multipart `POST`, whose
  deployment default is 150 MiB), plus the multipart envelope. The page
  refuses a larger body before buffering it where it can tell the size
  (a `Blob`, `File`, buffer, string or `FormData`), the host before
  sending anything (`body_too_large`). An abort while the page is still
  reading the body ends the call at once, and nothing reaches the host;
- the IPC's postMessage fallback (used only when its custom protocol is
  unavailable, e.g. blocked by a CSP) carries a binary body as one JSON
  number per byte, tens of bytes of memory per byte sent; there a body is
  at most **16 MiB**, else `body_too_large` with a message naming the
  fallback channel;
- at most **256 requests are open** at once, pending, streaming and unread
  alike. When the table is full, the longest-unread response that nobody
  has read for at least 5 s is dropped to make room; with none, the new
  request is refused with `too_many_requests` (nothing is sent). A
  response nobody reads for 60 s is dropped (a stream with a read waiting
  on it is not idle, so SSE is unaffected); a later read answers
  `unknown_request`. A head that does not arrive within **120 s** fails
  the request (`network`) and frees its slot; a body, once streaming, has
  no deadline. The page also cancels a body it can no longer read (its
  `Response` was garbage-collected) and a HEAD or null-body response at
  once;
- a page reload cancels everything the old page had in flight.

Errors reject with `{code, message}`: `aborted` (the page maps it to an
`AbortError`), `url_not_allowed`, `invalid_request`, `body_too_large`,
`too_many_requests`, `unknown_request`, `network` (mapped to a
`TypeError`, as `fetch` throws).

**Copies of an upload** (a 150 MiB body, on the custom-protocol IPC). The
counts are by reading each copy site, not by profiling; the IPC's own
copies inside WebKit and wry are outside this code and not counted.

| | before (frame) | now (raw payload + header) |
| --- | --- | --- |
| page: `Request.arrayBuffer()` | 150 MiB | 150 MiB |
| page: frame (metadata + body concatenated) | +150 MiB | — |
| host: the IPC's buffer (lent to the command for its whole life) | 150 MiB | 150 MiB |
| host: the command's own copy | +150 MiB (`Bytes::copy_from_slice`) | ≤ 320 KiB (64 KiB pieces, 4 queued + 1 in flight) |
| **peak held by this code** | **~600 MiB** | **~300 MiB** |

Tauri 2.12 lends the command the IPC buffer (`tauri::ipc::Request` borrows
it) and has no way to take it, so the host streams from the borrowed buffer
instead of copying it whole; a body up to 256 KiB is copied, as before.

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
| `workspace_remove` | `{id}` | `null` — forgets the workspace, its host data (remembered approvals, copy checkpoints, its local index), its turns and its thread history; the folder is never touched. Rejects with `workspace_busy` while a turn runs in it. |
| `workspace_bind_project` | `{id, project_id}` | `Workspace` — rejects with `workspace_busy` while a turn (or an undo) runs in it, `workspace_unknown` for an id it does not know. |
| `workspace_files` | `{workspace_id, query?: string, limit?: number}` | `{path, kind: "file" \| "dir"}[]` — the "@" picker. Workspace-relative paths (no trailing `/`) matching `query` case-insensitively: the name starting with it, then containing it, then the path containing it, then its characters in order; shallower and shorter first. `.gitignore` is honoured, `.git` and `path_deny` matches are left out, symlinks are neither listed nor followed. At most `limit` (default 50, at most 200); a folder past 20 000 entries answers from its first part. Answers while a turn runs. Rejects `local_work_disabled`, `workspace_unknown`, `workspace_unavailable`. |

Workspaces are stored in the app data directory (`workspaces.json`), never
inside the folder.

## The local agent turn (D0, `src/d0/`)

| Command | Arguments | Result |
| --- | --- | --- |
| `agent_turn_start` | `{workspace_id, project_id, conversation_id, application_id, version_id, prompt, plan_mode, mentions?: string[], skills?: string[]}` | `{turn_id, execution_id}` |
| `agent_turn_cancel` | `{turn_id}` | `null` when the turn was running (or was already cancelled): it stops and commits nothing. Rejects `turn_not_cancellable` once the agent's run has ended (the turn is being committed, or it ended): nothing was stopped. `turn_unknown` / `turn_expired` for a turn the host does not keep. |
| `agent_turn_status` | `{turn_id}` | `{state: "running" \| "committing" \| "done", done: DonePayload \| null}` — `done` is the `done` event's payload once it was sent. Rejects `turn_unknown` / `turn_expired` for a turn the host does not keep (an app restart forgets every turn). |
| `approval_respond` | `{request_id, decision: "allow_once" \| "allow_always" \| "deny"}` | `null` (rejects `approval_closed` when the question is no longer open, `invalid_request` for another decision) |
| `turn_changes` | `{turn_id}` | `{files: FileChange[], latest: boolean, undone: boolean}` |
| `checkpoint_preview` | `{turn_id}` | `{restored: string[], deleted: string[]}` — a dry run of restoring the folder to before the turn |
| `checkpoint_restore` | `{turn_id, path?: string, confirm_older?: boolean}` | `{restored: string[]}` |

`agent_turn_start` resolves the agent version, checks it, and starts the
local turn on the platform before it resolves; the run then continues in
the background and reports through events. `project_id` is a project's id
as the server holds it (1 to 2147483647; anything else is refused with
`invalid_request` before any request, and so is `workspace_bind_project`'s).
`conversation_id` is the conversation's numeric id or its UUID. The conversation must hold the agent
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
`workspace_project_mismatch`, `workspace_busy`, `skill_unknown`,
`skill_invalid`, `skill_too_large`, `invalid_request`, `not_signed_in`, and the platform's own codes (`local_turn_conflict`,
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
workspace, exist as a file or folder (not a symlink), not match
`path_deny` and hold no line break or other control character in its name,
else the start is refused with `invalid_request` before any request. The checked paths are added to the user message, deduplicated, as

```text
<prompt>

Files the user referenced:
- src/main.rs
- docs/
```

and that message is what the turn starts, runs and commits with. File
contents are never inlined: the agent reads them with its own tools.

**Skills.** `skills` are skills the person picked in the composer's "/"
menu, by name (the UI sends the skill its message starts with as
`/<name>`; the prompt itself is sent as typed). Only the agent version's
own skills can be picked, as with the web chat's `~skill`: each name is
matched, ignoring case and surrounding blanks, against the `skills` of the
resolved version (or equals a skill's frozen `id`), so the skill is read
server-side, under the person's own permissions, with the definition.
At most 5 names, each non-blank, at most 256 bytes and without control
characters, else `invalid_request` before any request. A name the version
has no skill with instructions for is refused with `skill_unknown`, and
more than 64 KiB of picked skill text together with `skill_too_large`
(refused, not cut). Whether or not a skill is picked, **every** skill of
the version goes through the runtime's own instruction admission
(`instruction_authority::check_skills`: each snapshot's `id`, `scope` and
`revision` present and within bounds, `revision` the SHA-256 of the
instructions, no two skills under one name, the catalogue's bounds; then
`InstructionPlan::admit` with the project context): a skill it refuses
fails the start with `skill_invalid`, naming the skill. All of these after
the definition is read and before the turn is started on the platform
(the runtime loads every attached skill, so such a turn could not run).
The picked skills are appended to the agent's instructions, before the
AGENTS.md section, as one `## Skill for this turn` section with one
`<invoked_skill nonce="…" name="…">` block per skill, ending with
`## End of skill instructions`; the name is escaped as an attribute value
and the text is framed as AGENTS.md text is (below). Every attached skill
also stays in the runtime's catalogue, for `load_skill`, as before.

**Precedence.** The system instructions are in the order of their
authority, and each framed section's preamble says so: the agent's own
instructions first; then the picked skill, which they outrank and which
outranks AGENTS.md; then AGENTS.md, which both outrank (a nested
AGENTS.md is more specific than the root one); then the memory splice.

**AGENTS.md.** At every turn start (plan mode included) the host reads the
workspace's root `AGENTS.md` (name matched case-insensitively; an exact
`AGENTS.md` wins over another spelling in the same folder) and, for each
mention, the `AGENTS.md` of every folder between it and the root, nearest
first, deduplicated. Reads go through the confined workspace: a symlinked
file or folder is not read and a `path_deny` match is refused. All files
together are capped at 32 KiB; a cut file ends with a truncation note and
files past the cap are skipped. They are appended to the agent's system
instructions, after the agent's own instructions and any picked skill
(which outrank it, as the section's preamble says) and before the memory
splice, as one `## Project instructions (AGENTS.md)` section with one
`<agents_md nonce="…" path="…">` block per file. The path is escaped as an
attribute value (`&`, `"`, `<`, `>` and control characters as entities).
Edits apply from the next turn.

**Nonce framing** (AGENTS.md and skills alike, `src/d0/framing.rs`). Every
turn draws a fresh random 128-bit nonce `N` (32 hex digits). A block opens
with `<tag nonce="N" …>` and ends only at `</tag nonce="N">`; each section's
preamble says so, and that everything before that closing tag — including
text that looks like a closing tag, a heading or the end of the section —
is the block's content. The text is left byte for byte as written
(Markdown headings, `Vec<u8>`, `#include`, shell comments and tags of its
own included), except for the one sequence that could end the block: the
exact closing tag with this turn's nonce (ASCII case ignored), which the
text cannot know in advance; should it occur, its `</` becomes `<\/`.

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

**Undo is for the newest turn.** `turn_changes`' `latest` is `true` for the
newest turn of its workspace that changed files and was not undone; `undone`
for a turn whose changes were undone (by its own undo, or by restoring the
folder to before an earlier turn). A turn's checkpoint is the whole folder
before its first change, so:

- **The newest turn** (`latest`): `checkpoint_restore` without `path` undoes
  it (the UI's "Undo turn"); with `path` it reverts that one file.
- **An older turn**: `checkpoint_restore` without `path` is refused with
  `undo_not_latest` — putting the folder back to that checkpoint also
  reverts every later turn and the person's own edits since. The UI offers
  "Restore folder to before this turn" instead: it asks the host for
  `checkpoint_preview` (what would be written back and deleted now, later
  turns' files and the person's edits included; nothing is changed), lists
  it in its confirmation, and only then calls `checkpoint_restore` with
  `confirm_older: true`. That turn and every later turn of the workspace are
  then `undone`.
- **One file of an older turn** (`path`): reverted only while the file still
  holds exactly what that turn left in it (a hash taken when the turn
  ended); a later turn's change or the person's edit refuses it with
  `file_changed_since`.

Restores and `turn_changes` go through the workspace's **current** session
(after a policy change, the rebuilt one, under the policy of now:
`local_work_disabled` refuses a restore). Checkpoints belong to the
workspace, not the session, so the current session reaches an older turn's
checkpoint — unless the folder's checkpoint store is another kind now (it
became, or stopped being, a git work tree): `session_replaced`.

A restore answers the files written back or deleted, and `[]` for a turn
that changed nothing. It rejects (`workspace_busy`) while a turn runs in
the workspace, `already_undone` for an undone turn, and `no_checkpoint` for
a turn that changed files without a checkpoint (a folder too large to
checkpoint: the turn ran, but cannot be undone here).

The host keeps the last 20 turns of each workspace for `turn_changes`,
`checkpoint_preview` and `checkpoint_restore`; an older turn rejects with `turn_expired`, an id it
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
stored as appended chunks, each numbered by the last delta it holds; folding
concatenates them as it does live deltas). Recording never blocks the
stream: events go to a writer thread that commits them in batches (on every
non-text event, every 500 ms or 64 KiB, before a read, at a turn's end and
at exit), so a crash loses at most the last batch. A turn's end never waits
for the writer either (what the turn still holds is handed over at once, in
order), and `thread_history` / `thread_history_delete` wait for it off the
host's async workers. A thread is one thread whichever spelling of the
conversation (its id or its UUID) its turns started with: reads, deletes and
the bounds below all count them together. A refused start is not
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

## Local index (`src/index.rs`, `libs/rust/local-index`)

A workspace's code index (ADR-0029 decision 7): the Inventory knowledge
graph of the folder, parsed on this machine and kept in the app data
directory (`workspaces/<id>/index/index.sqlite`, owner-only), never in the
folder. It is turned on per workspace, and only while the policy allows
both local work and the index (`local_work.allowed && local_work.local_index`);
otherwise every command but `index_disable` and `index_remove` rejects with
`local_index_disabled`, and turns are offered no index tools. Nothing is
sent anywhere: there are no embeddings yet.

```ts
type IndexStatus = {
  state: "off" | "building" | "ready" | "stale" | "stale_policy" | "error";
  files: number;          // documents of the last build
  entities: number;
  relations: number;
  vectors: number;        // 0 until embeddings exist
  last_run: string | null; // when the last build was committed (UTC, ISO 8601)
  changed_files: number;  // files turns changed since the last build
  error: string | null;   // why the last refresh failed
};
```

`off`: not turned on (or turned off). `building`: a refresh runs or waits
its turn (the previous build, if any, still answers). `ready`: built,
nothing known to have changed. `stale`: built, but turns changed files
since (an incremental refresh starts on its own about 3 s after the turn
that changed them, once changes stop), or the folder has not been checked
since the app started (it is when the index opens: below).
`stale_policy`: the build on disk was made under another file access
policy (`local_work.path_deny` changed): it is never loaded or answered
from, turns are offered no index tools, and the index is rebuilt from
nothing when it next opens; the fingerprint of the policy a build was made
under is committed with it. `error`: the last refresh failed; the previous
build, if any, still answers. A cancelled refresh goes back to the state it
started from and is recorded as cancelled, not as an error, so `last_run`
stays the last completed build's.

**Opened lazily.** `index_status` never opens an index or starts a refresh:
until the index is open it answers from a small read of its file (`stale`
or `stale_policy` with the last build's counts, or `off`). An index opens,
and checks the folder for what changed while it was closed, on
`index_open` (the folder's session page), on a turn in the folder, or on
`index_enable` / `index_refresh`. Refreshes run one at a time across every
folder; a waiting one reports `queued`.

| Command | Arguments | Result |
| --- | --- | --- |
| `index_status` | `{workspace_id}` | `IndexStatus` — never opens the index (above). |
| `index_open` | `{workspace_id}` | `IndexStatus` — opens the index (if on) and starts checking the folder; `off` when not turned on. |
| `index_enable` | `{workspace_id}` | `IndexStatus` — turns the index on and starts building it (incrementally, over what an earlier build kept). |
| `index_disable` | `{workspace_id}` | `IndexStatus` (`off`) — stops a running refresh and stops offering the index; the build is kept for when it is turned on again. Allowed whatever the policy. |
| `index_refresh` | `{workspace_id, full?: boolean}` | `IndexStatus` — starts a refresh: only files whose size or mtime changed are read again; `full` rebuilds from nothing. Rejects `index_off` (not turned on), `index_busy` (`full` while one runs). |
| `index_cancel` | `{workspace_id}` | `boolean` — stops the running (or waiting) refresh at its next checkpoint (nothing it did is kept); `false` when none runs. Never rejects. |
| `index_remove` | `{workspace_id, confirm: true}` | `null` — deletes the index (it can be built again). Rejects `confirmation_required` without `confirm: true`. Allowed whatever the policy. |

Every command is async. Each folder's index has its own slot: turning it
off and removing it (`index_remove`, `workspace_remove`, the Doctor's
`index.move_aside` and `index.rebuild`) hold that slot from closing the
index to deleting its files, so no status read or turn reopens it in
between, and a turn that still holds the closed index's tools is answered
`index.closed` ("The workspace index was removed or turned off").

Every command also rejects `workspace_unknown`; an index that cannot be
opened rejects `index_refused` (its directory is a symlink, another user's
or inside the folder), `index_newer_schema` (written by a newer app) or
`index_storage`. Progress arrives as `index://event` (below).

The index is kept across sign-out and `host_wipe`: it holds the code
structure of the person's own folder and no account data. `index_remove`
or `workspace_remove` deletes it.

A turn in a workspace whose index is on and built is offered its tools
(toolset `workspace_index`, reported `remote: false`): `search_knowledge_graph`,
`get_entity_details`, `get_related_entities`, `query_graph`,
`list_entity_types`, `impact_analysis` and `get_entity_content`, with the
cloud Inventory tools' names and arguments. They only read, so they run
without approval and in plan mode too. An answer from a build that may be
out of date starts with a one-line `Note:`. A toolkit tool never takes one
of these names (it gets a `_2` suffix), whether or not the index is on.

## App and native shell (`src/platform.rs`, `src/local_commands.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `app_platform` | — | `AppPlatform` (below). Never rejects. |
| `reveal_path` | `{workspace_id, path}` | `null` — shows the file or folder in Finder / the file manager. |
| `open_path` | `{workspace_id, path}` | `null` — opens it with its default app. |
| `doctor_run` | `{scope?: "local"}` | `Check[]` — the Doctor's checks (README, "Diagnostics"); `"local"` checks this computer's files only (no network: what the launch notice uses). Never rejects. |
| `doctor_fix` | `{fix_id, confirm?: boolean}` | `{message}` — applies the repair a check named in `fix_id`, then the UI runs the checks again. A check with `fix_confirm` names what its repair deletes: the UI shows it and passes `confirm: true` only once the person confirmed; without it that repair is refused and nothing is deleted. Rejects with the reason when the repair fails or `fix_id` is unknown. |
| `app_ready` | — | `AppCommand[]` — the page now listens to `app://command` (call it once the listener is live); the commands the host sent before that, oldest first. Never rejects. |

```ts
type Check = {
  id: string;            // credentials, dir.config, dir.data, dir.logs, history, workspaces,
                         // index.<workspace id> (one per workspace with a local index),
                         // deployment, session, local_work, pending_revokes
  title: string;
  status: "ok" | "warn" | "fail";
  message: string;       // for a person; never a secret
  fix_id?: string;       // credentials.tighten | credentials.move_aside | dir.tighten.{config,data,logs}
                         // | history.tighten | history.move_aside | workspaces.drop_missing
                         // | workspaces.move_aside | revokes.retry
                         // | index.tighten:<workspace id> | index.move_aside:<workspace id>
                         // | index.rebuild:<workspace id>
  fix_label?: string;
  fix_confirm?: string;  // what the repair deletes; ask before passing `confirm: true`
};
```

The repairs go through the app's own paths. `workspaces.drop_missing`
removes each folder that is **gone** (its parent folder is there, it is not,
and it is not on a `/Volumes/<drive>` that is not connected) exactly as
`workspace_remove` does — refused while a turn runs in it (named in the
message, left in the list), its remembered approvals, undo checkpoints, kept
turns and thread history deleted — and only with `confirm`. A folder the OS
will not let the app read (macOS privacy protection, permissions) is reported
with how to allow it (System Settings › Privacy & Security › Files and
Folders, or Full Disk Access) and one on a drive that is not connected as
unavailable; neither is ever offered for removal. `credentials.move_aside`
moves the file aside, then signs out on this computer as `host_sign_out`
does (cached token and unsaved session forgotten, every turn cancelled and
forgotten, the webview's data cleared and `signed_out` sent on
`app://command`); the moved file is not trusted, so it is never read and its
session is not revoked on the server (the message says so).

A workspace's local index (its directory, `index.sqlite`, `-wal` and `-shm`:
owner-only, not a symlink; `PRAGMA quick_check`; schema version) is checked
when the workspace has one: `index.tighten` restricts it to you,
`index.move_aside` closes it and moves its directory aside (it is off until
turned on again), `index.rebuild` (only with `confirm`) closes it, deletes it
and turns it on again so it is built from nothing. One written by a newer app
asks for an update and has no repair. The workspace id after the `:` must
name a workspace in the list.

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
| `run_diagnostics` | — | Help ▸ Run Diagnostics…: open the Doctor. Sent live, never queued (the connect screen listens without `app_ready`). |
| `signed_out` | — | The session ended on this computer outside a sign-out the page asked for (the Doctor moved the stored sign-in aside): drop what the page shows of it (the desktop shell reloads to the connect screen). Sent live, never queued. |
| `focus_turn` | `{workspace_id: string, turn_id: string}` | Reserved: show this turn. Not sent yet — the notification plugin has no click callback on desktop, so clicking a notification only activates the app. |

**Open Folder… is host-side**: the menu item runs the native folder picker
in the host (the same picker and `WorkspaceStore::add` as `workspace_open`)
and then sends `workspace_opened`; there is no `open_folder` command id for
the UI to handle. Cancelling the picker sends nothing. Menu-forwarded ids
also bring the window forward first. Nothing is lost before the page
listens: until it calls `app_ready`, commands wait in the host (the newest
64), and `app_ready` returns them — a folder dropped on the dock icon while
the app starts is selected once the UI is up. A page (re)load holds commands
again until the new page calls `app_ready`. In the desktop build the
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

Three event names, all emitted to the `main` window only: `app://command`
(above), `agent://event` and `index://event`. The webview may listen
(`core:event:allow-listen` / `allow-unlisten`), never emit.

`index://event`:

```ts
type IndexEvent = {
  workspace_id: string;
  phase: "queued" | "listing" | "parsing" | "building" | "saving" | "ready" | "stale" | "cancelled" | "error";
  message: string | null; // progress text, or why it failed
  status: IndexStatus;    // as index_status would answer now
};
```

A refresh reports `queued` while another folder's refresh runs, then
`listing`, `parsing` / `building` progress, then `saving` and `ready`, or
ends `cancelled` or `error`. `stale` is sent when a turn's changes make a
ready index out of date (its refresh follows a few seconds later). A
closed index (removed, turned off, or reopened under another policy)
reports nothing more.

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
file change); otherwise `allow_always` answers once and the UI does not
offer it. It is false for every `run_command` on a machine that may run
commands unconfined (the OS sandbox cannot be enforced): every command is
asked there, so a remembered choice would never apply. `can_remember` is
computed from the very precondition the host's `remember` checks; should
storing the choice still fail (the scope the person picked does not match,
or the store cannot be written), the call is approved once and a warning
logged — an approval is never turned into an error. Remote toolkit
confirmations (`confirmation_required`) never remember.
