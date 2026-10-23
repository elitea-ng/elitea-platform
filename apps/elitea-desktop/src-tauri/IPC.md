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
| `workspace_remove` | `{id}` | `null` — forgets the workspace, its host data (remembered approvals, its approval rules, copy checkpoints, its local index), its turns and its thread history; the folder is never touched. Rejects with `workspace_busy` while a turn runs in it. |
| `workspace_bind_project` | `{id, project_id}` | `Workspace` — rejects with `workspace_busy` while a turn (or an undo) runs in it, `workspace_unknown` for an id it does not know. |
| `workspace_agent_settings_get` | `{id}` | `{step_limit: number \| null, allow_model_override: boolean \| null}` — the workspace's agent settings: `step_limit` is the model steps per run of its turns, and it wins over the agent's `meta.step_limit` (`null`: the agent's value, else the default, 100); `allow_model_override: false` runs every turn on the agent's own model (the composer hides its model chip; `null`/`true`: a model may be chosen per turn, issue 1228). Kept in the workspace's host data (`workspaces/<id>/agent_settings.json`, owner-only), never in the folder; a file that is not private to the user is refused (`storage`). |
| `workspace_agent_settings_set` | `{id, settings: {step_limit: number \| null, allow_model_override?: boolean \| null}}` | the stored settings — they apply from the next turn. Rejects `invalid_request` for a step limit outside 10 to 200, `storage` for an id it does not know. |
| `workspace_files` | `{workspace_id, query?: string, limit?: number}` | `{path, kind: "file" \| "dir"}[]` — the "@" picker. Workspace-relative paths (no trailing `/`) matching `query` case-insensitively: the name starting with it, then containing it, then the path containing it, then its characters in order; shallower and shorter first. `.gitignore` is honoured, `.git` and `path_deny` matches are left out, symlinks are neither listed nor followed. At most `limit` (default 50, at most 200); a folder past 20 000 entries answers from its first part. Answers while a turn runs. Rejects `local_work_disabled`, `workspace_unknown`, `workspace_unavailable`. |
| `commands_list` | `{workspace_id}` | `{name, description: string \| null, argument_hint: string \| null, source: "workspace" \| "user", path, ignored: string[]}[]` — the "/" menu's custom commands, by name (see **Custom commands** below): the workspace's `.elitea/commands/` then `.claude/commands/`, then the person's own `<app config dir>/commands/` (created `0700` when missing); the first of a name wins, so a workspace command shadows the person's, even one that cannot be read (too large, not UTF-8, denied, a symlink): that one is not listed, and no other file of its name is offered or run in its place. `path` is workspace-relative, or relative to the person's folder. `ignored` names what the file asks for that the host ignores (front matter keys such as `allowed-tools` and `model`, `! shell lines`, `@ file includes`). Answers while a turn runs. Rejects `local_work_disabled`, `workspace_unknown`, `workspace_unavailable`. |

Workspaces are stored in the app data directory (`workspaces.json`), never
inside the folder.

## Approval rules (`src/rules.rs`, `libs/rust/local-tools/src/execpolicy.rs`)

The person's command rules, layer 3 of the approvals engine
(`libs/rust/local-tools/src/approvals.rs`): ordered **prefix rules** in a
JSON form of the `codex-rs/execpolicy` design. **User rules** live in
`<app config>/rules.json` (every workspace), **workspace rules** in
`<app data>/workspaces/<id>/rules.json`; both owner-only (`0600` in `0700`),
opened without following a symlink, and refused when another user's or
readable/writable by others. Layer 3 reads the workspace's rules, then the
person's; a `forbidden` beats a `prompt` beats an `allow` across both. A
file that is refused or fails validation loads **no** rules (the error says
why; the defaults still apply). A session loads both when it opens and
again at a turn's start when either file changed.

```ts
type RuleFile = {
  version: 1;
  host_executables?: { name: string; paths: string[] }[]; // absolute paths a program name may be given as
  rules: Rule[];
};
type Rule = {
  pattern: (string | string[] | { any: true })[]; // argv prefix; a list = alternatives; {any: true} = any one word ("*" is the literal word)
  decision?: "allow" | "prompt" | "forbidden"; // default "allow"
  justification?: string;           // shown to the person and the model
  match?: (string | string[])[];    // examples that must match (checked at load)
  not_match?: (string | string[])[];// examples that must not match
  sandbox?: "read-only" | "workspace-write" | "full-access"; // widest sandbox an allow covers (default workspace-write)
  network?: boolean;                // whether an allow covers a call asking for the network
  origin?: "repository";            // imported from a proposal: its justification is shown to the person, never the model
};
type LoadError = { file: string; rule_index?: number /* 1-based */; example?: string;
                   expected?: "match" | "not_match"; got?: string; message: string };
type ScopeState = { scope: "workspace" | "user"; path: string; file: RuleFile | null; error: LoadError | null;
                    version: string /* the file's digest, "absent" without one */ };
type Proposal = { path: string; count: number; file: RuleFile | null /* as an import copies it */; error: LoadError | null;
                  warnings: string[] /* what an import leaves out or narrows */; digest: string };
type PutOutcome = { saved: boolean; file: RuleFile | null; error: LoadError | null; conflict: boolean };
type RuleMatch = { scope: "workspace" | "user" | "legacy"; index: number; decision: Rule["decision"]; pattern: string;
                   justification?: string; origin?: "repository" };
```

| Command | Arguments | Result |
| --- | --- | --- |
| `rules_get` | `{workspace_id}` | `{scopes: ScopeState[], proposed: Proposal \| null}` — the workspace's then the person's rules, each with the error that keeps it from loading; `proposed` is what the folder's `.elitea/rules.json` proposes (read confined to the folder, `path_deny` honoured), never loaded until imported. Rejects `local_work_disabled`, `workspace_unknown`. |
| `rules_put` | `{workspace_id, scope: "workspace" \| "user", file: RuleFile, version}` | `PutOutcome` — validated before anything is written: an unknown field, an empty pattern or a disagreeing example gives `{saved: false, error}` and the file is left as it was. `version` is the `ScopeState.version` the edit was made from; a file changed since gives `{saved: false, conflict: true}` (the panel reloads and says so). |
| `rules_explain` | `{workspace_id, command}` | `{verdict: "allow" \| "ask" \| "deny", source: "policy" \| "plan_mode" \| "workspace" \| "user" \| "remembered" \| "default", reason, rule: RuleMatch \| null, justification: string \| null}` — how the layers would decide `command` now, as a confined `workspace-write` call without the network (an open session answers with its own engine). Rejects `invalid_request` for an empty command. |
| `rules_import_proposed` | `{workspace_id, digest}` | `PutOutcome` — copies the folder's proposed rules, exactly as `rules_get` listed them (`digest`: a proposal changed since gives `{saved: false, conflict: true}`), into the workspace's rules file: the person's explicit import. A repository can never grant itself permissions without it, and an import is bounded: `host_executables` pins are never imported (a pin widens what an allow matches), an `allow` is narrowed to `workspace-write` without the network, a rule whose examples need the dropped pins is left out (each listed in `warnings`), and every rule is marked `origin: "repository"`. |

Matching: tokens compare in order as an argv prefix. An `allow` covers only
the first segment of a **simple** command (never a compound one) and never
a call asking for a wider sandbox or the network than it names; a `prompt`
or `forbidden` matches any segment, the inner command of `sh -c`, `env`,
`sudo` included. A program name matches the same bare name; a path matches
a pinned name only when `host_executables` lists it, and an unpinned one
when it is the file the bare name runs (`prompt`/`forbidden` match any path
with that base name). Rules never lift the `local_work` policy ceiling,
plan mode, or the rule that every command is asked on an unconfined
machine. `forbidden` rules are advisory: they see the commands the analysis
can find, not what a script starts; the sandbox is the boundary.

"Always allow" (`approval_respond` with `allow_always`) for a command appends
`{pattern: <resolved program + argv prefix>, decision: "allow", justification:
"Allowed from the approval dialog on <date>", match: [<the approved argv>],
sandbox, network}` to the workspace's rules file. Command choices an older
host kept in `choices.json` are moved into it once; file choices stay in
`choices.json`.

Rules migrated from an older host's workspace settings are labelled
`legacy#N` (their own numbering), apart from the workspace file's
`workspace#N`. A rule that allows a command which would run unconfined does
not decide it: the defaults ask, with the reason that it would run
unsandboxed. When the workspace's rules file is refused or broken, "always
allow" is kept in `choices.json` instead (and moved into the file once it
loads again).

A `forbidden` rule refuses the call inline (no question) and the model sees
`{"status": "error", "code": "local_tools.denied", "message": "forbidden by
workspace#3: …", "error": "forbidden_by_rule", "justification": "…",
"rule": "workspace#3"}`, so it can take the alternative the justification
names instead of retrying. A rule imported from a repository leaves the
`justification` out (it is the repository's text, not the person's).

## The local agent turn (D0, `src/d0/`)

| Command | Arguments | Result |
| --- | --- | --- |
| `agent_turn_start` | `{workspace_id, project_id, conversation_id, application_id, version_id, prompt, plan_mode, mentions?: string[], skills?: (string \| {source: "project", id: number \| string})[], compact?: boolean, model?: {name: string, reasoning_effort?: "none" \| "low" \| "medium" \| "high"}, command?: {name, arguments?: string}, images?: {source: "clipboard" \| "file", token: string}[]}` | `{turn_id, execution_id}` |
| `attachment_add` | `{workspace_id, path}` | `{token, name, mime, bytes, width, height, thumbnail}` — attaches the image file at the **absolute** `path` (a `files_dropped` path, or a picked file) to the workspace's next turn ("Image input", below). Blocking work runs off the async workers. Rejects `path_denied`, `not_found`, `image_too_large`, `invalid_request` (not PNG/JPEG/WebP/GIF, a relative path, a link, not a regular file), `workspace_unknown`, `local_work_disabled`, `not_signed_in`. |
| `attachment_add_bytes` | `{workspace_id, mime, data, name?}` | The same answer for a pasted image: `data` is base64, at most 10 MiB decoded; `mime` is what the clipboard said and is ignored (the type is sniffed from the bytes); `name` defaults to `pasted-image`. Rejects `image_too_large`, `invalid_request`, `workspace_unknown`, `local_work_disabled`. |
| `attachment_remove` | `{workspace_id, token}` | `null`. Forgets an image the person took off the composer (an unknown token is not an error). |
| `agent_prewarm` | `{project_id, application_id, version_id, conversation_id?}` | `{version: boolean, participant: boolean}` — what a turn to that agent version (and conversation) would find cached now. The UI calls it when the composer gets focus with an agent selected: the host reads the version and the conversation's answering participant ahead (only what it has not cached), starts no execution and never reads the memory recall. Rejects `invalid_request`, `local_work_disabled`, `not_signed_in`; a failed read is only not cached. |
| `agent_turn_cancel` | `{turn_id}` | `null` when the turn was running (or was already cancelled): it stops and commits nothing. Rejects `turn_not_cancellable` once the agent's run has ended (the turn is being committed, or it ended): nothing was stopped. `turn_unknown` / `turn_expired` for a turn the host does not keep. |
| `agent_turn_status` | `{turn_id}` | `{state: "running" \| "committing" \| "done", done: DonePayload \| null}` — `done` is the `done` event's payload once it was sent, updated when the commit after it settles (`committed`, `commit`). `committing` is the moment between the run's end and `done`. Rejects `turn_unknown` / `turn_expired` for a turn the host does not keep (an app restart forgets every turn). |
| `approval_respond` | `{request_id, decision: "allow_once" \| "allow_always" \| "deny" \| "deny_with_feedback" \| "continue" \| "cancel" \| "accept" \| "decline", feedback?: string}` (`continue` is `allow_once` and `cancel` is `deny`, the MCP sign-in card's answers; `accept` and `decline` answer a `plan_proposed` card, where only `accept` accepts, and on any other card both deny; `deny_with_feedback` denies any card (declines a plan card) and tells the agent why. `feedback`, taken by `decline` and `deny_with_feedback`, is the person's text for the model, kept to 2 KiB of UTF-8 (the UI's box stops there too), control characters other than newlines and tabs dropped; it travels only with a denial, never an approval, and `deny_with_feedback` without any is `deny`) | `null` (rejects `approval_closed` when the question is no longer open, `invalid_request` for another decision) |
| `turn_changes` | `{turn_id}` | `{files: FileChange[], latest: boolean, undone: boolean}` |
| `checkpoint_preview` | `{turn_id}` | `{restored: string[], deleted: string[]}` — a dry run of restoring the folder to before the turn |
| `checkpoint_restore` | `{turn_id, path?: string, confirm_older?: boolean}` | `{restored: string[]}` |
| `turn_commit_retry` | `{execution_id}` | `null` once the turn's failed commit (`commit_failed`) is saved: sent again with the same execution id and body, in its conversation's order. Rejects `commit_unknown` when the host keeps no failed commit of that execution (it was saved, the app restarted, or the session signed out), or with the commit's refusal, which is also sent as another `commit_failed`. |

**One round trip before the first token.** The host keeps, for the app's
session only (never on disk; dropped on sign-out, a new sign-in or another
deployment), the last resolved agent version per project, agent and
version, with its `ETag`, and each conversation's answering participant and
UUID per agent version. A cold turn reads the version and the conversation
together. A warm one admits the cached version at once and sends the
local turn's start right away, on the cached participant, with the
version's revalidation (`If-None-Match`; a `304` costs nothing) and the
conversation's re-read (its message count decides the session's seed)
beside it. A changed version is admitted again before the model's first
request; a refused or unreadable one (`403`, `404`, a local refusal) stops
the turn with its code before any model request, and its execution
expires on the server. A participant that changed, or a start the platform
refuses for it (`404`, `422`), is started again on the fresh one, once.
The start itself is never cached: its memory recall is computed fresh at
every turn.

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
`platform_mcp_unsupported` (a platform MCP server the platform gave no
`toolkit_ref`: a server older than client contract 1.16),
`unknown_tool_kind`, `toolkit_ref_missing`,
`model_unresolved`, `model_pinned`, `model_unavailable`,
`model_override_disabled`, `agent_not_in_conversation`, `agent_version_mismatch`,
`workspace_unknown`, `workspace_unbound` (no project bound yet),
`workspace_project_mismatch`, `workspace_busy`, `skill_unknown`,
`skill_invalid`, `skill_too_large`, `command_unknown`, `model_no_vision`,
`attachment_unknown`, `too_many_images`, `invalid_request`, `not_signed_in`, and the platform's own codes (`local_turn_conflict`,
`not_found`, …).

**Image input** (#1229). A message can carry up to five images; they go to
the model with the prompt as `image_url` data URIs (`{"type":"image_url",
"image_url":{"url":"data:<mime>;base64,…"}}` after the text part, the cloud
worker's shape). The webview never sends a file's bytes over IPC: a dropped
or picked file is registered by its path (`attachment_add`), a pasted image
once (`attachment_add_bytes`); the host answers a `token`, which
`agent_turn_start.images` names. The host checks, bounds and **holds the image
in memory** (at most 32 images and 64 MiB; the oldest go first; dropped on
sign-out; a token names one workspace and is used up by the turn that
starts with it, a refused start keeps it):

* **Types and limits**, enforced here and not only in the UI: PNG, JPEG,
  WebP and GIF, sniffed from the bytes (never the extension); a source over
  10 MiB is refused (`image_too_large`); an image over 2048 px on its longest
  side or 5 MiB is shrunk (JPEG stays JPEG, any other becomes PNG, a GIF keeps
  its first frame; a PNG that stays over 5 MiB is sent as JPEG on white; one
  no quality brings under the limit is refused `image_too_large`); more than
  five in a turn is `too_many_images`; an unknown, sent or other-workspace
  token is `attachment_unknown`.
* **Confined reads.** A path is read through a workspace rooted at its
  parent folder (at the workspace itself when it is inside it, so `path_deny`
  applies) with the session deny list: credentials and the app's own data are
  refused `path_denied` wherever they are. The file is opened `O_NOFOLLOW`
  and must be a regular file.
* **Vision gate.** Images go only to a model the catalogue marks
  `supports_vision: true` (`ConfigurationModelItem`; one the catalogue does not
  describe does not count): the turn is refused `model_no_vision`, never
  sent without them, and the images stay attached so the person can choose
  another model (`model`) and send again. A model the person chose is checked
  before the local turn starts; the agent's own (or the thread's kept) model
  is known under the session, so its refusal comes after the start (the
  execution expires, no model request is made).
* **History and commit.** The commit's `user_message` gains `images:
  [{name, mime, bytes}]` (a server that does not know it ignores it); the
  bytes are not uploaded. `thread_history` turns carry `images: [{name, mime,
  bytes, thumbnail}]`, a JPEG thumbnail of at most 64 KiB as a data URI
  (`threads.sqlite`, schema 3). The session store keeps no image: a later
  turn (and a "Continue?" after the step limit) sees a note for it.
* **`read_file` on an image.** `read_file` on a PNG, JPEG, WebP or GIF
  returns `{kind: "image", path, mime, bytes, width, height}` and the image
  itself to a model that sees images (same bounds), and the same metadata
  with a `hint` ("this model cannot view images") to one that does not. Any
  other binary file answers `{kind: "binary", path, bytes, hint}`. Because
  chat completions' tool messages are text-only, the model request carries
  the image in a following user message named `tool_image`. The cloud
  executor's `read_file` must return the same result (the handoff contract).

A turn is held to the session it started under (the connected origin and
the sign-in; a token refresh keeps it): once the person signs out, signs
in again or connects to another deployment, every further request of the
turn is refused locally with `identity_changed`, so it is never committed
under another identity. `host_sign_out`, `host_wipe`, `host_sign_in` and a
`host_connect` to another deployment also cancel every running turn (it
ends `cancelled` → `done`, `committed: false`) and forget every workspace
session and kept turn: an earlier turn then answers `turn_unknown`.

**Plan mode** (#1225) is the session's permission profile, not a second
agent. `plan_mode: true` starts the turn in the `plan` profile: read-only
local tools, the index tools, the planner (`update_plan`, `start_step`,
`complete_step`, `get_plan_status`, `delete_plan`, offered to every turn)
and `exit_plan_mode {plan}` (Markdown, at most 16 KiB; offered only in
plan mode, and local-only: the cloud executor has no plan mode). No
writes, no `run_command` or `run_tests` (a remembered "always allow" for a
test runner does not apply; `read_output` stays) and no remote toolkits.
`exit_plan_mode` sends `plan_proposed {request_id, call_id, plan}`; it is
answered with
`approval_respond` `accept` or `decline` (with `feedback`), never by a rule
or a remembered choice, and "always" is never offered. On `accept` the
session switches to the `build` profile and the **same run** goes on: adk
2.2 resolves the toolsets again after every tool batch, so the model's next
step is offered every tool (remote toolkits included), and the host sends
`plan_accepted {call_id}` (the UI turns its plan switch off). The system
prompt was written for plan mode, so every later model step of the run
also carries a `## Environment (plan accepted)` system section that
replaces it: the environment gathered again in `build` (the sandbox line,
the write and command guidance, the tools offered now). The tool
answers `{accepted: true, message}`. Unless the model wrote a plan with
steps in this run (`update_plan`), the host copies the accepted Markdown
into the planner, replacing a previous turn's plan (done or not), and
`plan_updated` follows: a `# heading` is the title, each top-level list
item a step (nested items, fenced code and thematic breaks are not). On
`decline` the turn
stays in plan mode and the tool answers `{accepted: false, feedback?,
message}`; the model revises the plan. The turn's `hitl_exchanges` record
it as `kind: "plan_approval"`, `decision: "accept" | "decline"`, `value`
the feedback. The next turn starts in the profile its own `plan_mode` says.

**Sub-agents** (#1226). Every workspace turn, plan mode included, is
offered the runtime's sub-agent tools (`elitea-agent-runtime`
`subagents`, the `elitea_subagents` toolset; a remote tool may not take
their names): `spawn_agent {task, label?, max_steps?}` starts a read-only
child and returns `{agent_id, status: "running"}` at once, `send_input
{agent_id, message}` continues one, `wait_agent {agent_ids?, timeout_seconds?}`
returns `{results: [{agent_id, status, result?, steps, input_tokens,
output_tokens, error?}]}` once all settled or at the timeout (default 300 s,
at most 900; no `agent_ids`: every child still running, else every one the
turn started, so a spawn and a wait can share one round of calls), and `close_agent {agent_id}` stops one. A child has a fresh
context (the task only, under the environment section and the workspace's
AGENTS.md, never the agent's instructions or the conversation), the
read-only local tools and the index tools (no writes, commands, remote
toolkits, `exit_plan_mode` or sub-agent tools), at most 30 model steps
unless the task asks for up to 60, and 60k cumulative input tokens: past
either it stops with `budget_exhausted` and what it had. At most 4 run at
once and 12 start in one turn; a refusal is a tool result the model reads.
A child calls `/llm` with the turn's execution id (the turn's spend, under
the gateway's budget), quiet: its text reaches the UI as `subagent_event`s,
not as the turn's `text_delta`. Its tool calls are in the commit's
`tool_calls`, after the `spawn_agent` call, each carrying
`parent_agent_call_id` (the `spawn_agent` call id), `parent_agent_name`
(the label, else the agent id), `parent_agent_path`
(`[{name, call_id, sibling_ordinal}]`) and `sibling_ordinal`, the shape the
cloud writes for a nested agent's calls; they count toward the 512-call
cap. An approval a child's read needs (a workspace rule that asks) is the
turn's own prompt. The turn's end, its cancel and a cloud handoff stop
every child (`subagent_finished` `cancelled`); none outlives the turn.

**Decline with feedback** (#1225). A call the person denies (a local tool's
approval or a remote toolkit's confirmation) answers the model with a typed
failure: the usual `{status: "error", code, message}` plus `ok: false` and
`error: {code: "declined_by_person", message, feedback?}`, where `feedback`
is what the person wrote (`deny_with_feedback`), in their own words. The
model reads it and adjusts its next step; the turn goes on. The turn's
`hitl_exchanges` record the denial (`decision: "reject"`) with the feedback
as `value`. A rule's or the policy's refusal is not a person's and keeps
its own shape.

**Model per turn** (issue 1228). `model` runs the turn on another chat
model of the project than the agent's (`reasoning_effort` with it, for a
model whose catalogue row says `supports_reasoning`), and keeps it as the
thread's model: a later turn without `model` runs on it too, across
restarts (the session's state, `elitea.model.v1`; a cancelled turn's choice
is dropped with the turn, while a trim of the session's oldest turns and a
re-seed from the server keep it). Choosing the agent's own model without an
effort of its own goes back to the agent's model. The choice drives the
turn's model requests only — the model name and effort `/llm` gets, the
edit tools (`EditProfile` by model name) and the context limits (another
model's from the catalogue; an effort chosen for the agent's own model
keeps the version's frozen ones) — and never changes the agent version on
the server; the agent's `max_tokens` is cut to the chosen model's
`max_output_tokens`. A reasoning effort goes to `/llm` without a
temperature (the gateway refuses the two together); for a chosen model
whose catalogue row says `supports_reasoning: false` the effort is not
sent at all and the agent's temperature is (a catalogue that cannot answer:
as chosen). It is checked before the local turn starts,
against the project's chat models (`GET /configurations/models/{project_id}?section=llm&include_shared=true`,
on the turn's credentials, kept a minute): a model the catalogue does not
list is refused `model_unavailable`; when the catalogue cannot answer the
choice goes to the gateway, which governs every request (a model the
project may not use fails the turn with `model_gateway.forbidden`, shown
and committed as a failed turn). The thread's kept choice is checked the
same way when a turn names no `model`: one the catalogue no longer lists is
dropped (cleared from the session) and the turn runs on the agent's model,
with a `status` `starting` `message` that says so and `running`'s
`model.chosen: false` — the turn is not refused. A refused choice
(`model_pinned`, `model_override_disabled`, `model_unavailable`) says
nothing about the agent version: the version the session keeps stays kept
and is not read again. An agent version whose `llm_settings` has
`pin_model: true` refuses any other model with `model_pinned`, and a
workspace whose settings say `allow_model_override: false` refuses it with
`model_override_disabled` (a kept choice is then not applied: the turn runs
on the agent's model). The model a turn ran on is on its `status`
`running` event (`model`) and in the commit (`assistant_message.model_name`,
client contract 1.17).

**Reasoning** (issue 1228). A model's reasoning streams as `thinking_delta`
events (a `thinking_end` closes each block) and each model step's reasoning
is committed as one `thinking_steps` entry in the runtime's shape (`type:
"ChatGeneration"`, `thinking` cut at 32 KiB, `timestamp_start`,
`timestamp_finish`, `message.response_metadata.model_name`; at most 512
steps and 2 MiB in all, the rest is not committed), so the web app shows it
on the answer (`listMessageTraces`); once a step does not fit the 2 MiB, no
later step is committed (no gaps), and a cut step ends in `…` within its
32 KiB. The thread history keeps at most 1 MiB of a turn's `thinking_delta`
text, a budget of its own beside the turn's 4 MiB of events: past it the
reasoning is not stored (each `thinking_end` still is), and the turn is not
marked `events_truncated` for it. The session keeps it as the step's
`Part::Thinking`; it is sent back to the provider only for a family that
requires it (`openai_compatible: false` in the catalogue: signed blocks on
the Anthropic route, which does not exist yet, issue 1227), never on the
OpenAI-compatible chat route. Reasoning text is stored only in the server
transcript, the thread history and the session (the same owner-only files)
and never logged.

**Remote tools.** A credentialed toolkit (`remote_toolkit`) and a platform
MCP server (`platform_mcp`, `mcp_elitea_internal_*`, when the resolved
definition gives it a `toolkit_ref`; issue 1231) are the same kind of tool to
the model: each selected tool is one tool named `<toolkit>_<tool>`, and a
toolkit or server with every tool selected (`all_tools`) is one tool
`<toolkit>_call` that takes `tool_name` and `arguments`. Every call goes to
`executeRemoteToolkitTool` (a platform server's with toolkit id 0) with
its own `Idempotency-Key`, reused on each of its retries; the desktop never
connects to an MCP server itself and holds no credential of one.

**MCP sign-in.** A remote tool whose MCP server needs the person's sign-in
answers 409 `mcp_authorization_required`. The host then asks
`approval_request {kind: "mcp_authorization", call_id, tool, toolkit, title:
"<toolkit> needs your sign-in", detail, server_url, authorize_path?, reason:
"", can_remember: false}`: `server_url` is the server's address (http(s),
without credentials, query or fragment), `authorize_path` the web app page
that signs in to that toolkit (`/app/<project>/mcps/all/<toolkit>` for an MCP
toolkit, `/app/<project>/toolkits/all/<toolkit>` otherwise; absent for a
platform server), which the UI opens in the system browser. Nothing else of
the 409 (the server's OAuth metadata, its `WWW-Authenticate`, the call's
arguments) reaches an event, a log line, the model or the commit, and no
token ever reaches the desktop: the browser signs in against the platform.
Answer `continue` (or `allow_once`) once signed in: the host sends the same
tool call again as a new call (a new `Idempotency-Key`, with the
`authorization_retry.mcp_authorization_reference` the 409 named, when it named
one). `cancel` (or `deny`), no answer within 10 minutes, or a second
`mcp_authorization_required` end that tool call only, as a failed tool step
the model reads (`authorization_cancelled`, `authorization_timeout`,
`authorization_required`); the turn goes on. The UI shows the question as a
card in the transcript and drops it when the call's `tool_result` arrives.
The commit's `hitl_exchanges` hold it as kind `mcp_authorization`. At the
10-minute wait the host closes the question itself: `approval_closed
{request_id, reason: "timeout"}`, an exchange with `decision: "timeout"`, and
a late `continue` answers `approval_closed`. Whether the page is the MCP
page is the platform's word: the 409's `authorization_required.toolkit_type`
(`mcp` or `mcp_*`) when it names this toolkit, else the definition's `type`.

A platform MCP server's definition entry (client contract 1.16) names its
tools (`tools: [{name, description}]`): its one generic `<toolkit>_call` tool
lists them in its description (names, and descriptions up to 6 KiB in all)
and as the `tool_name` enum; when the guardrails blocked some, the platform
lists the remaining ones in `selected_tools` and each becomes its own tool
with its own description.

The edit tools follow the agent's model (#1214): a model named `gpt-5*`,
`*codex*`, `o3*` or `o4*` is offered `write_file` and `apply_patch`
described as the Codex `*** Begin Patch` envelope; every other model
`write_file`, `edit_file`, `multi_edit` and `apply_patch` (described as a
unified diff first). The agent's `version_details.meta.edit_format`
(`"auto"`, `"str_replace"`, `"patch"`) overrides the model. Tool names never
change, and `apply_patch` accepts both formats whatever its description.

`compact: true` (the composer's `/compact`) compacts the session's context
at the turn's first model call, whatever its size (**Context window**,
below).

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
The picked skills go in the turn's host context message (**Prompt
layout**, below), after the environment's volatile part and before the
memory recall, as one `## Skill for this turn` section with one
`<invoked_skill nonce="…" name="…">` block per skill, ending with
`## End of skill instructions`; the name is escaped as an attribute value
and the text is framed as AGENTS.md text is (below). Every attached skill
also stays in the runtime's catalogue, for `load_skill`, as before.

**Project skills.** A `skills` entry `{source: "project", id}` is one of
the bound project's own skills (the menu's "Project skills": `listSkills`,
less the names the version carries), by its id (a positive integer, or its
decimal string; else `invalid_request`). The host never takes its text
from the UI: after the version is read and before the turn is started, it
reads the skill itself under the person's token (all picked ids at once,
an id picked twice read once)
(`GET /api/v2/elitea_core/skill/prompt_lib/{project_id}/{id}`, `getSkill`:
the `base` version's `name` and `instructions`); one the person cannot read
(`403`, `404`) or without instructions is refused with `skill_unknown`.
It is admitted with the runtime's own bounds
(`instruction_authority::check_skills`, as a `project:<project_id>`
snapshot whose revision is the digest of what was read), else
`skill_invalid`, and applied exactly like a picked version skill (one more
`<invoked_skill>` block, after the version's picks; one with a picked
version skill's name is not applied twice). It counts toward the same
limits: at most 5 picks of both kinds together (`invalid_request`, before
any request) and 64 KiB of picked skill text together (`skill_too_large`,
refused, not cut). It is **not** added to the runtime's `load_skill`
catalogue: that stays the version's. The cloud web chat's `~` menu still
offers only the version's skills.

**Custom commands.** `command` is a custom command the person picked in
the "/" menu (`commands_list`): `name` as listed (`name`, or `folder:name`
for a file in a sub-folder; lowercase letters, digits, `-` and `_`, else
`invalid_request`) and the `arguments` they typed after it (at most
16 KiB, no control character but newlines and tabs). The host never takes
the template from the UI: at the start, before any request, it reads the
command again the way `commands_list` does (so an edit since the menu was
listed applies; a command gone, symlinked or denied by now is refused with
`command_unknown`, never replaced by a same-named one further down; only
the files that could be that name are read) and expands it: `$ARGUMENTS` becomes all of the
arguments, `$1`…`$9` their shell-split words (empty when absent), in one
pass, and nothing else is expanded. The turn's `prompt` is then the
literal `/<name> <arguments>`, whatever the UI sent: the thread history,
the local turn's start (`user_input`) and its commit keep that, with the
"@" list under it. Only the model's message is the expansion:

```text
## Command /<name> (from the workspace, .claude/commands/<name>.md)
<preamble: the template ranks like AGENTS.md; the agent's instructions, the
environment and a picked skill outrank it; it grants no tools, model,
commands or approvals; where the block ends>

<command_template nonce="N" path=".claude/commands/<name>.md">
<the expanded template, framed as AGENTS.md text is>
</command_template nonce="N">

Arguments: <the arguments as typed>
```

followed by the "@" list. Reads are confined like AGENTS.md's: a
workspace file goes through the confined workspace (`O_NOFOLLOW`, a
symlinked file or folder is not read, `path_deny` and the session's deny
list are honoured, and a folder's names are read from the folder opened
without following any symlink); the person's folder, its sub-folders and its files are
opened without following a symlink and must be the person's own and
writable by no one else. At most 200 commands, 16 KiB per file, UTF-8.
Front matter (a `---` block at the top, flat `key: value` lines): only
`description` (at most 200 characters; else the template's first line)
and `argument-hint` are read. Every other key (Claude Code's
`allowed-tools`, `model`, …), `!` shell lines and `@file` includes are
ignored on purpose and stay plain text: a repository can never grant
tools, pick the model, run a command or change the approvals through a
command file. Command names and files are local: the server conversation
(and so the cloud handoff, and a session re-seeded from it) holds only the
literal `/<name> <arguments>`, not the expansion; the local session that
ran the turn keeps the expansion.

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
files past the cap are skipped. They end the system instructions, after
the agent's own instructions and the environment section (which outrank
them, as does any picked skill, as the section's preamble says), as one
`## Project instructions (AGENTS.md)` section with one
`<agents_md nonce="…" path="…">` block per file. The path is escaped as an
attribute value (`&`, `"`, `<`, `>` and control characters as entities).
Edits apply from the next turn.

**Nonce framing** (AGENTS.md and skills alike, `src/d0/framing.rs`). Every
thread has a random 128-bit nonce `N` (32 hex digits), drawn when its
session is created in `sessions.sqlite` and again whenever the session is
re-seeded from the server (a fork is a new conversation, so a new session);
a turn without the store draws a fresh one. It is kept only in host data,
never logged, never written into the workspace and sent nowhere but inside
the prompt to `/llm`; one nonce per thread keeps the system prompt
byte-identical across turns (#1227). A block opens
with `<tag nonce="N" …>` and ends only at `</tag nonce="N">`; each section's
preamble says so, and that everything before that closing tag — including
text that looks like a closing tag, a heading or the end of the section —
is the block's content. The text is left byte for byte as written
(Markdown headings, `Vec<u8>`, `#include`, shell comments and tags of its
own included), except for the one sequence that could end the block: the
exact closing tag with this thread's nonce (ASCII case ignored), which the
text cannot know in advance; should it occur, its `</` becomes `<\/`.

**Prompt layout** (#1227, `src/d0/turn.rs`, `src/d0/model.rs`). The system
message is the thread's stable prefix: the agent's instructions, the
stable part of the environment section (OS, sandbox, network, mode, tools),
then the AGENTS.md section, plus what the runtime adds (the skill
catalogue). Tools are sorted by name. Everything that changes every turn
goes in one host-authored message (`{"role": "user", "name":
"host_context"}`) placed right before the person's message of the turn, at
every step of it: `## Environment (this turn)` (the date and the git line),
the picked skills, then the memory recall, last, by the cloud's splice (its
text and count are what a web turn receives; only its place moved). The
message is not stored in the session: the next turn has its own, and no
recall is reused. Sub-agents keep their own one-shot prompt. On a model the
catalogue marks `openai_compatible: false` (Anthropic) the body carries
`cache_control: {type: "ephemeral"}` on the system message (as one text
part), on the last tool and on the request's last message; the gateway's
Bifrost core forwards them to Anthropic and reports cache writes as
`prompt_tokens_details.cache_write_tokens`. A compaction summary call sends
none.

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
`edit_file`, `multi_edit`, `apply_patch`) changed, against what they held before the turn
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

## Background processes (`src/d0/processes.rs`, `libs/rust/local-tools/src/process.rs`, #1223)

The agent can start a command in the background (`run_command` with
`background: true`, or `tty: true` for an interactive program on a
pseudo-terminal), read its output later (`process_output`, from a byte
offset; `screen: true` gives a terminal process's rendered screen), type
into it (`process_write`), wait for it (`process_wait`), stop it
(`process_kill`) and list them (`process_list`). These tools and the
extended `run_command` schema are **local-only**: the cloud executor has no
persistent processes, so its tool list keeps the plain `run_command`.

| Command | Arguments | Result |
| --- | --- | --- |
| `process_list` | `{workspace_id}` | `ProcessInfo[]` — the workspace session's processes, running and ended (an ended one stays listed until the session ends), oldest first; `[]` when the workspace has no session yet |
| `process_read` | `{workspace_id, process_id, since?: number}` | `{data: string, offset: number, next_offset: number, dropped_bytes: number, screen?: string}` — the output from byte `since`, or without it the latest 256 KiB the buffer holds (a pane opened late catches up with it), at most 256 KiB; `offset` is the byte offset of `data`'s first byte (offsets count the process's raw bytes; `data` is that text cut at a whole character, any invalid UTF-8 replaced). `dropped_bytes` is what the buffer no longer holds between `since` and `data`; `screen` (terminal processes) is the rendered screen, rows joined by `\n`. Rejects `process_unknown`, `workspace_unknown` |
| `process_write` | `{workspace_id, process_id, data: string}` | `null` — the person's keystrokes into the pane (a terminal process's input, or a piped process's stdin), at most 64 KiB, sent as typed (never classified: the person typed it). Rejects `process_unknown`, `process_ended`, `invalid_request` |
| `process_resize` | `{workspace_id, process_id, rows: number, cols: number}` | `null` — a terminal process's size (1 to 1000 each; it starts at 40 × 120); a piped process ignores it. Rejects `process_unknown`, `invalid_request` |
| `process_kill` | `{workspace_id, process_id}` | `null` — stops the process's whole process group (`SIGTERM`, then `SIGKILL` after 2 s); an ended one is not an error. Rejects `process_unknown` |
| `process_pin` | `{workspace_id, process_id, pinned: boolean}` | `null` — a pinned process outlives the end of its turn (until the session ends, below); unpinning one whose turn has ended and that the agent did not keep stops it. Rejects `process_unknown` |

```ts
type ProcessInfo = {
  process_id: string;          // "p_" + 8 hex digits
  call_id: string;             // the run_command call that started it
  command: string;
  cwd: string;                 // workspace-relative ("." for the root)
  tty: boolean;                // on a pseudo-terminal
  sandbox: "read-only" | "workspace-write" | "full-access";
  network: boolean;
  loopback: boolean;
  keep_alive: "turn" | "session"; // "session": the person approved keeping it past its turn
  pinned: boolean;
  status: "running" | "exited" | "killed" | "timed_out";
  exit_code?: number;          // exited
  signal?: string;             // killed: "SIGTERM", "SIGKILL", …
  started_at: number;          // Unix milliseconds
  ended_at?: number;
  bytes: number;               // output so far: the next byte's offset
};
```

**Lifecycle.** At most 8 live processes per workspace session (a ninth
`run_command` is refused with `local_tools.too_many_processes`); each runs
at most 4 hours (then `timed_out`). Every process is stopped (its process
group: `SIGTERM`, then `SIGKILL` after 2 s) when the turn that started it
ends, unless it is pinned or the agent asked to keep it
(`keep_alive: "session"`, approved by the person once per process); every
one, pinned and kept ones included, when the session ends: the workspace is
removed, its policy changes (the session is reopened), the person signs out
or connects elsewhere, a thread of the workspace is handed off to the cloud
(before the send starts), and when the app quits (the quit waits at most
2 s for them).

**Output.** A piped process's stdout and stderr share one pipe, as a
terminal shows them (`stream: "stdout"`); a terminal process's output is
the terminal's (`stream: "pty"`, escape codes included; the model reads the
rendered screen or the stream without them). Each process keeps 1 MiB: the
first 64 KiB and the latest bytes after it, with offsets that only grow; a
read from an offset the buffer dropped reports `dropped_bytes`. The buffer
lives in memory only (never uploaded except as the bounded tool results in
the commit) and goes with the session.

**Sandbox.** Every process starts through the same sandbox as
`run_command` (the session's deny list, the scrubbed environment;
`TERM=xterm-256color` instead of `dumb` for a terminal process) in its own
process group, and keeps the sandbox it started with: a later approval
never widens a running process, and a process started `read-only` stays
read-only whatever is typed into it. What the model types
(`process_write`) is classified like a command line, line by line: a
destructive line is asked. `loopback: true` (with the network off) lets a
command listen on and connect to `127.0.0.1` / `::1` only: Seatbelt allows
exactly those addresses; bubblewrap keeps its private network namespace,
whose own loopback reaches only the processes of that one command (a server
and a client must run in the same command there); Landlock alone cannot
scope addresses, so a loopback command there gets TCP and is asked every
time. A loopback call is asked; "always allow" remembers it for that
program and argv prefix in this workspace only.

Events go on `process://event` (`{workspace_id, seq, kind, payload}`,
`seq` per workspace from 0):

| `kind` | `payload` |
| --- | --- |
| `process_started` | `ProcessInfo` |
| `process_output` | `{process_id, stream: "stdout" \| "pty", data: string, offset: number}` — at most 20 per second per process, at most 16 KiB of output each; `offset` is the byte offset of `data`'s first byte. A process writing faster than that skips ahead: the next `offset` is past the end of the previous `data` (`process_read` fills the gap) |
| `process_updated` | `ProcessInfo` — pinned or unpinned |
| `process_exited` | `ProcessInfo` — ended (`status` final), after its last `process_output` |

While the turn that started a process runs, its turn's `agent://event`
stream also carries `process_started` `{process_id, call_id, command, tty,
sandbox, loopback}`, `tool_output_delta` `{call_id, process_id, stream,
data, offset}` (the same throttle) and `process_exited` `{process_id,
call_id, status, exit_code?}`, for the `run_command` card.

## Local MCP servers (`src/mcp.rs`, `src/mcp_commands.rs`, `libs/rust/local-tools/src/mcp/`, #1230)

The person's own stdio MCP servers on this device (ADR-0029 decision 4), for
local turns only: gated by the policy's `local_work_local_mcp` (with
`allowed`; absent means off). Off, no server starts, no MCP tool is offered
and every MCP call is denied; turning it off reopens the session, which stops
the running servers. These tools are **local-only**: the cloud executor never
runs a stdio server (its contract test `local_mcp_tools_are_local_only`
names the exclusion), and sub-agents are never offered them.

**Settings.** Two files in host data, never in the workspace folder:
`<app config dir>/mcp.json` (every workspace) and
`<app data>/workspaces/<id>/mcp.json` (one workspace; a server of the same
name replaces the user one there). Each is a `0600` file in a `0700` folder,
read without following a symlink, and refused (no server runs from it, the
turn says so in a `status` message) when another user owns it, others can
read it or it does not validate. The Doctor's `mcp.config` check covers the
user file (`mcp.tighten`, `mcp.move_aside`).

```ts
type McpFile = {
  version: 1;
  servers: Record<string, ServerConfig>; // name: [A-Za-z0-9_-]{1,32}, at most 32
  answered_proposals?: string[];         // workspace file: the proposals answered (file SHA-256s)
};
type ServerConfig = {                    // unknown keys are refused
  command: string;               // absolute path to an executable regular file outside the workspace
  args?: string[];               // at most 64, each at most 4 KiB
  env?: Record<string, string>;  // keys ^[A-Z_][A-Z0-9_]*$, at most 64, values at most 4 KiB
  cwd?: "workspace" | "none";    // the workspace root (default), or a private temporary folder
  network?: boolean;             // default false; capped by the policy's network
  enabled?: boolean;             // default true
  allowed_tools?: string[] | null; // null: every tool the server lists
  read_only_tools?: string[];    // the person marks them read-only
  trust_read_only_hints?: boolean; // also trust the server's readOnlyHint
  source?: string;               // "user", or "proposal:<sha256>" for an accepted proposal
};
type ServerStatus = {
  name: string;
  scope: "user" | "workspace";
  state: "idle" | "starting" | "ready" | "failed" | "stopped";
  tools: string[];               // the server's own tool names, when ready
  error?: string;
  stderr_tail: string;           // the last 8 KiB of a 64 KiB stderr ring (never logged or uploaded)
  enforcement?: "full" | "partial" | "none" | "container";
  network: boolean;
  sandbox?: "read-only" | "workspace-write" | "full-access";
};
type ProposedServer = {
  name: string;
  command: string;               // as the file writes it
  args: string[];
  env_keys: string[];
  env_preview: Record<string, string>; // each value masked: two characters and its length
  problem?: string;              // why it cannot be accepted as written
};
type Proposal = { file: string; file_sha256: string; servers: ProposedServer[] };
```

| Command | Arguments | Result |
| --- | --- | --- |
| `mcp_list` | `{workspace_id?}` | `{allowed: boolean, user?: {path, servers, error?}, workspace?: {path, servers, error?}, status: ServerStatus[], proposals: Proposal[]}` — both files (`error`: why one is not used), the workspace session's servers (`[]` before its first turn) and the folder's unanswered proposals (only while `allowed`). Rejects `workspace_unknown` |
| `mcp_save` | `{scope: "user" \| "workspace", name, server: ServerConfig, workspace_id?}` | `null` — adds or replaces `name` (`source` becomes `"user"`); open sessions take it now (a changed server stops; it starts again with the next turn). `scope: "workspace"` needs `workspace_id`. Rejects `invalid_request`, `mcp_settings_refused`, `workspace_unknown` |
| `mcp_remove` | `{scope, name, workspace_id?}` | `boolean` — whether it was there; a running server of that name stops. Rejects as `mcp_save` |
| `mcp_restart` | `{workspace_id, name}` | `ServerStatus` — stops the server in the workspace's session and starts it again through the turn's path (sandbox, approval rules; a start that would ask is refused outside a turn, nobody is there to answer the card). Rejects `mcp_not_started` (the session has not run a turn), `mcp_server_unknown`, `local_mcp_disabled` |
| `mcp_status` | `{workspace_id?}` | `(ServerStatus & {workspace_id})[]` — that workspace's session's servers, or every open session's |
| `mcp_proposal_answer` | `{workspace_id, file_sha256, accept: string[]}` | `{accepted: string[]}` — copies the named servers (none: declined) of the proposal whose file hashes to `file_sha256` into the workspace's file, with `source: "proposal:<sha256>"`, the command found on the app's `PATH` when the file names a bare program, the network off; the proposal is not shown again. Nothing starts until the next turn. Rejects `conflict` (the file changed since it was shown), `invalid_request` (a server that cannot be accepted, or whose name the workspace already has), `local_mcp_disabled` |

**Proposals.** When a workspace is opened (`workspace_open`), the host reads
`.mcp.json`, `.cursor/mcp.json` and `.vscode/mcp.json` through the
workspace's confined reads (no symlink followed, `path_deny` honoured) and,
while the policy allows local MCP servers, sends each file it has not seen
answered on `mcp://event` as `{kind: "mcp_proposal", workspace_id, file,
file_sha256, servers: ProposedServer[]}`; `mcp_list` returns the same
proposals at any time. Only stdio servers are proposed (`url` and other
`type`s are left out); a relative command is never accepted. A later
change to the file is a new proposal; it never updates an accepted entry.

**In a turn.** The first turn that may offer MCP tools starts every
enabled server that has not started, one after the other (each at most
10 s for `initialize` and `tools/list`). A start goes through the same
sandbox as `run_command` (the session's deny list; `read-only` when every
tool is marked read-only, else `workspace-write` capped by the policy), in
its own process group, with only the scrubbed passthrough environment
(`PATH`, `HOME`, `LANG`, …, `TMPDIR` its own private folder) plus the
`env` its entry names, never the app's own. A start is an approval
(`tool: "mcp_start"`): allowed when confined without the network; asked
when it gets the network (once per workspace session for the same settings) or
the machine cannot confine it (asked every time); an approval rule on its
command line decides first. A server that fails is said in the turn's
`status` message (`phase: "running"`) and its tools are left out; the
turn goes on. Servers keep running for the app session and stop (their
process group: `SIGTERM`, then `SIGKILL`) when their entry changes or goes,
when the session ends (the workspace is removed, its policy changes,
sign-out, a handoff) and when the app quits.

Their tools are named `mcp__<server>__<tool>` (sanitised to
`[A-Za-z0-9_-]`, at most 64; a collision gets `_2`, …; a remote toolkit's
tool never takes an `mcp__` name), sorted by server and tool so the tool
list stays the same across a thread's turns. A tool's description and
every result's text reach the model framed under the thread's nonce
(`<mcp_tool_description …>`, `<mcp_result …>`), descriptions cut at 2 KiB
and results at the command output budget. A call is an approval
(`tool: "mcp_call"`): a tool marked read-only runs without asking (and in
plan mode, where the others are not offered); any other is asked on first
use, and "always allow" remembers that server's tool in this workspace.
The `approval_request` card carries `mcp: {server, tool?, read_only}`
(and, for a start, `network` and `unconfined`). The commit's `local_work`
report lists each call among its `commands` as `mcp <server> <tool>` with
`mcp: {server, tool, read_only}`, never its arguments.

## Thread history (`src/history.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `thread_history` | `{workspace_id, conversation_id}` | `{turns: StoredTurn[]}` — the turns of that thread the host recorded for the signed-in account, oldest first. `[]` when it has none (the thread ran on the web or another machine, or before this version) or runs without a store. `conversation_id` is the id the turns started with or the conversation's UUID. Rejects `invalid_request`, `not_signed_in`, `storage`, or the platform's code when the user lookup fails. |
| `thread_history_delete` | `{workspace_id, conversation_id}` | `{deleted: number}` — forgets that thread's stored turns (the signed-in account's only) and the agent's session of that conversation (below). The conversation on the server is untouched. |
| `thread_history_fork` | `{workspace_id, source_conversation_id, target_conversation_id, target_conversation_uuid, up_to_turn_id?, message_ids?}` | `{copied: number}` — the local half of a fork. The UI first forks the conversation on the server (`forkConversation`), then copies the source thread's stored turns into the new conversation, in the same workspace: the turns oldest first up to and including `up_to_turn_id` (all of them when absent), never a turn still running, each with a new `turn_id` and all its events. `message_ids` is the fork answer's `forked_messages` (source message UUID → the copy's UUID): every id of a copied `done` event's `message_ids` it names is rewritten, so the new thread's turns name the new conversation's messages and can be fork points in turn. One SQLite transaction: all of the turns or none. The source thread is unchanged; the signed-in account's rows only. The new thread's agent session is not copied: its first turn seeds it from the server conversation. `0` without a store. Rejects `invalid_request` (a malformed id, `target_conversation_uuid` not a UUID), `turn_unknown` (`up_to_turn_id` is not a turn of the thread here), `turn_running`, `thread_not_empty` (the target has turns already: nothing copied), `not_signed_in`, `storage`. Files in the folder are not changed. |

**Fork point.** A stored turn is one exchange: the prompt and its answer,
whose server message UUIDs its `done` event lists (`message_ids`: question,
answer). "Fork from here" on a turn forks the server conversation at the
turn's answer and copies the turns up to that turn (`up_to_turn_id`). A
thread the host has no record of (it ran on the web or another machine)
forks on the server only, and copies nothing here.

```ts
type StoredTurn = {
  turn_id: string;
  conversation_id: string;          // as the turn was started (the UI's id)
  conversation_uuid: string | null;
  prompt: string;                   // as typed; the "@" list is `mentions`
  mentions: string[];
  images: { name: string; mime: string; bytes: number; thumbnail: string | null }[]; // attached images: thumbnails only
  started_at: number;               // Unix ms
  finished_at: number | null;       // when `done` was recorded
  events: AgentEvent[];             // the turn's `agent://event` stream (below)
  changes: FileChange[] | null;     // what it changed, as it ended
  events_truncated: boolean;        // the turn outgrew the per-turn cap
  state: "done" | "running" | "interrupted"; // interrupted: never ended (the app quit)
  live: boolean;                    // turn_changes / checkpoint_restore still answer for it
  origin: "local" | "cloud";        // cloud: imported when the thread came back (ADR-0034)
};
```

The host records every event of a started turn as it emits it, so the
history survives a webview reload or crash; replaying a turn is folding its
`events` exactly as the live stream (consecutive `text_delta` events, and
consecutive `thinking_delta` events, are stored as appended chunks of their
kind, each numbered by the last delta it holds; folding
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
`events_truncated` is set), its `thinking_delta` text at 1 MiB apart (past
it dropped, the turn not marked), a stored diff at 64 KiB (1 MiB per turn), and a
thread keeps its newest 200 turns within 32 MiB.

The store is `threads.sqlite` in the app data directory (owner-only, like
the credentials file; SQLite WAL, versioned with `PRAGMA user_version`).
Rows are keyed by the deployment origin, the signed-in user's id
(`GET /api/v2/social/author`, looked up once per sign-in), the workspace
and the conversation: another account or deployment never reads them.
**Sign-out keeps the history** (it is keyed by user, so the next sign-in of
the same account sees it again, and another account does not);
`workspace_remove` deletes the workspace's rows for every account.

## Agent session memory (`src/sessions.rs`, `src/seed.rs`)

Every turn of a Local work thread sees the earlier ones: the model is sent
the thread's user and assistant messages, tool calls and tool results, in
order. The host keeps them as one adk **session** per conversation in
`sessions.sqlite` in the app data directory (the runtime's `StateStore`;
owner-only like `threads.sqlite`: `0700` directory, `0600` file created before
SQLite opens it, no symlink followed, a symlinked, foreign or group/world
accessible file refused, WAL). Nothing in this section is an agent-facing
command except `session_info`.

| Command | Arguments | Result |
| --- | --- | --- |
| `session_info` | `{conversation_uuid}` | `{turns, bytes, seeded_from_server} \| null` — the signed-in account's session of that conversation: how many turns it holds, its size in bytes, and whether it agrees with the server conversation. `null` without one (or without a store). Rejects `invalid_request`, `not_signed_in`, `storage`. For the Doctor and the context meter. |

* **The server conversation is canonical; the session is a cache.** Keyed by
  `sha256(deployment origin, user id, conversation UUID)`, never by workspace
  or numeric id: another account or deployment never reads another's session.
  Rows are adk `Event` JSON (plain `serde_json`) tagged with the turn's
  execution id and `executed_by` (`desktop` | `cloud_executor`).
* **Seeding.** A session that is missing, empty, marked for re-seeding, or
  whose `server_cursor` (the conversation's message-group count when it last
  agreed) differs from the conversation detail every turn already reads, is
  rebuilt from the server before the turn: `GET conversation?messages_limit=200`
  (a conversation with no messages costs no request), the answers' tool steps
  from `message_traces` / `message_trace` (the newest 128), folded into events
  (`seed.rs`). Answers whose meta says `executed_by: cloud_executor` keep that
  tag. A take-back (`handoff_apply`, or a re-sent release) marks the thread's
  session for re-seeding so the next local turn folds the cloud's turns in.
* **Bounds.** A tool response is stored cut at 64 KiB (`…[truncated N bytes;
  re-run or re-read]`); a session holds 32 MiB, past which the oldest whole
  turns go (never half a call/response pair) and it re-seeds next time.
* **A cancelled turn leaves nothing in the session; a turn the server refused
  to save marks it for re-seeding.** A different `path_deny` drops the stored
  tool responses' bodies and re-seeds without them.
* **Lifecycle.** Deleted with the thread (`thread_history_delete`), with the
  workspace (`workspace_remove`) and on a wipe (`host_wipe`); kept on a plain
  sign-out. If the file is refused the turn still runs, seeded from the server
  each time, and a warning is logged. The Doctor check `sessions` reports an
  untrusted, loose, damaged or newer-schema file; `sessions.tighten` and
  `sessions.move_aside` (moved aside, never deleted) repair it.

## Local index (`src/index.rs`, `libs/rust/local-index`)

A workspace's code index (ADR-0029 decision 7): the Inventory knowledge
graph of the folder, parsed on this machine and kept in the app data
directory (`workspaces/<id>/index/index.sqlite`, owner-only), never in the
folder. It is turned on per workspace, and only while the policy allows
both local work and the index (`local_work.allowed && local_work.local_index`);
otherwise `index_status` answers `off` with `policy_off: true` (and
`on_disk`, whether an index is on this computer), every other command but
`index_disable` and `index_remove` rejects with `local_index_disabled`, and
turns are offered no index tools. Nothing is
sent anywhere: there are no embeddings yet.

```ts
type IndexStatus = {
  state: "off" | "building" | "ready" | "stale" | "stale_policy" | "error";
  files: number;          // documents of the last build
  entities: number;
  relations: number;
  vectors: number;        // 0 until embeddings exist
  chunks: number;         // search chunks of the last build
  last_run: string | null; // when the last build was committed (UTC, ISO 8601)
  changed_files: number;  // files turns changed since the last build
  error: string | null;   // why the last refresh failed
  policy_off: boolean;    // the policy turns the index off (state "off")
  on_disk: boolean;       // an index exists on this computer
  watching: boolean;      // the folder is watched (below): outside changes refresh it
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
build, if any, still answers. A cancelled refresh goes back to the state
(and, after a failure, the error) it started from and is recorded as
cancelled, not as an error, so `last_run` stays the last completed build's.
A cancel stops the parse between files, so it ends promptly and frees the
next folder's refresh.

**The policy is live.** `index_status`, every command that opens or
refreshes, a turn, and the refresh after a turn's changes read the policy
first. Turned off by it, an open index is closed. Made under another
`path_deny`, it is closed (the tools a turn holds answer `index.closed`)
and reopened under the new one, `stale_policy` until its rebuild commits.
No refresh runs with an older policy's folder view.

**Opened lazily.** `index_status` never opens an index or starts a refresh:
until the index is open it answers from a small read of its file (`stale`
or `stale_policy` with the last build's counts, or `off`). An index opens,
and checks the folder for what changed while it was closed, on
`index_open` (the folder's session page), on a turn in the folder, or on
`index_enable` / `index_refresh`. Refreshes run one at a time across every
folder; a waiting one reports `queued`.

**Watched.** While an index is open and turned on, its folder is watched
for changes made outside the app (an editor, `git pull`, a build): once the
folder has been quiet for 3 s, and at most every 30 s, an incremental
refresh runs under the policy then, as after a turn (no event is sent for
the watch itself; the refresh reports as any other). A change only counts
where a build would look: not under `.git`, not denied (`path_deny`, the
host's deny list), not ignored by a `.gitignore` (or `.git/info/exclude`);
a changed `.gitignore` always counts. When the system drops events (an
inotify overflow, FSEvents asking for a rescan) the refresh that follows
stat-walks the whole folder (never a rebuild). Turning the index off,
removing or closing it, a policy change and sign-out stop the watch
(sign-out keeps the index); opening the index again, or signing in while
it is still open, restarts it. `watching` says whether it runs. The watch
never reads a file.

| Command | Arguments | Result |
| --- | --- | --- |
| `index_status` | `{workspace_id}` | `IndexStatus` — never opens an index that is not open (above); `policy_off` instead of rejecting. |
| `index_open` | `{workspace_id}` | `IndexStatus` — opens the index (if on) and starts checking the folder; `off` when not turned on. |
| `index_enable` | `{workspace_id}` | `IndexStatus` — turns the index on and starts building it (incrementally, over what an earlier build kept). |
| `index_disable` | `{workspace_id}` | `IndexStatus` (`off`) — stops a running refresh and stops offering the index; the build is kept for when it is turned on again. Allowed whatever the policy. |
| `index_refresh` | `{workspace_id, full?: boolean}` | `IndexStatus` — starts a refresh: only files whose size or mtime changed are read again; `full` rebuilds from nothing. Rejects `index_off` (not turned on), `index_busy` (`full` while one runs). |
| `index_cancel` | `{workspace_id}` | `boolean` — stops the running (or waiting) refresh at its next checkpoint, between files of the parse too (nothing it did is kept); `false` when none runs. Never rejects, and never waits behind an index being opened. |
| `index_remove` | `{workspace_id, confirm: true}` | `null` — deletes the index (it can be built again). Rejects `confirmation_required` without `confirm: true`. Allowed whatever the policy. |

Every command is async. Each folder's index has its own slot: turning it
off and removing it (`index_remove`, `workspace_remove`, the Doctor's
`index.move_aside` and `index.rebuild`) hold that slot from closing the
index to deleting its files, so no status read or turn reopens it in
between, and a turn that still holds the closed index's tools is answered
`index.closed` ("The workspace index was removed or turned off"). A
removal also drops the folder's entry from the host. A turn's change signal
and `index_cancel` never take that slot, so neither waits behind an index
being opened.

Every command also rejects `workspace_unknown`; an index that cannot be
opened rejects `index_refused` (its directory is a symlink, another user's
or inside the folder), `index_newer_schema` (written by a newer app) or
`index_storage`. Progress arrives as `index://event` (below).

The index is kept across sign-out and `host_wipe`: it holds the code
structure of the person's own folder and no account data. `index_remove`
or `workspace_remove` deletes it.

A turn in a workspace whose index is on and built is offered its tools
(toolset `workspace_index`, reported `remote: false`): `search_workspace`
(local only, below), then `search_knowledge_graph`,
`get_entity_details`, `get_related_entities`, `query_graph`,
`list_entity_types`, `impact_analysis` and `get_entity_content`, with the
cloud Inventory tools' names and arguments. They only read, so they run
without approval and in plan mode too. An answer from a build that may be
out of date starts with a one-line `Note:`. A toolkit tool never takes one
of these names (it gets a `_2` suffix), whether or not the index is on.

`search_workspace {query, top_k = 10 (≤ 50), path_glob?, kind = code | docs
| any, mode = auto | lexical | semantic}` answers a JSON list of
`{path, line_start, line_end, snippet, score, matched_by[], entity_ref?}`:
the build's search chunks (every line of every text file, cut by the
cloud index's chunkers, with the lines those leave out as chunks of their
own; lock files, minified bundles and data files over 256 KB are not
chunked) ranked by BM25 over their text and an identifier column
(`camelCase` / `snake_case` split, path parts), fused by weighted
reciprocal rank (k = 60) with the code graph's name matches and their
one-hop neighbours (`matched_by`: `lexical`, `graph`, `neighbour`).
`entity_ref` is the entity a hit holds, as `get_entity_details` takes it.
The chunks are committed in the same transaction as the build, so they
always describe the documents it holds (a build reads, chunks and parses
the folder in bounded batches — each parsed language together, other files
512 at a time — staging the chunks in a temporary table on the writer's
connection that no reader sees, and moves them in at commit); an index built before chunks
existed reads every file once on its next refresh to cut them (nothing is
parsed again).

### Index embeddings (`src/embeddings.rs`, `libs/rust/local-embed`)

A local index needs no embedding model: the text index and the code graph
answer on their own. Vectors are an option per workspace, **off by
default**, in one of two ways the policy may allow (both also need
`allowed && local_index`):

* `local:<model id>` — `local_work.index_local_embeddings` (server default
  true; absent = false). A model of the pinned catalogue run on this
  computer by candle (Metal on Apple silicon); nothing leaves the device.
* `remote:<dim>:<model>` — `local_work.index_remote_embeddings` (default
  false). The deployment's `/llm/v1/embeddings` with the native token and
  `X-Project-Id` = the folder's bound project. Background embedding sends
  no `X-Elitea-Execution-Id`; a `search_workspace` call inside a turn
  sends that turn's id for its query.

| Command | Arguments | Result |
| --- | --- | --- |
| `index_embeddings_get` | `{workspace_id}` | `string` — the setting (`off` when unset), from the open index or a small read of its file |
| `index_embeddings_set` | `{workspace_id, embeddings}` | `string` — the stored value. `remote:<model>` is probed with one short embedding in the bound project and stored as `remote:<dim>:<model>`. Rejects `index_off`, `local_index_disabled`, `embeddings_unknown`, `embeddings_not_allowed` (the policy does not allow that way now), `workspace_unbound`, `embeddings_probe_failed`. `off` is always accepted. |
| `index_embeddings_status` | `{workspace_id}` | `EmbedStatus \| null` — `null` when the folder does not embed or its index is not open |
| `index_embeddings_rebuild` | `{workspace_id, full?}` | `null` — embed now whatever the power state (until done); `full` deletes the folder's vectors first. Rejects `embeddings_off`, `index_off` |
| `index_models` | `{}` | `{models: ModelInfo[], local_allowed, remote_allowed, cache_bytes, mirror: string \| null}` — `mirror`: the host downloads come from when the policy names one |
| `index_remote_models` | `{workspace_id}` | `{name, display_name: string \| null}[]` — the bound project's embedding models (its own and shared: `GET /api/v2/configurations/models/{project_id}?section=embedding&include_shared=true`, client contract 1.9), for the picker. Rejects `embeddings_not_allowed`, `workspace_unbound`, `models_unavailable` (the UI then takes a typed name) |
| `index_model_download` | `{model_id, confirm: true}` | `ModelInfo` — starts (or resumes) the download in the background; progress on `index://event`. Rejects `confirmation_required` (no `confirm`: the person must have seen the size), `model_unknown`, `embeddings_not_allowed`, `model_busy` |
| `index_model_cancel` | `{model_id}` | `boolean` — whether a download ran; what was fetched is kept for a resume |
| `index_model_import` | `{model_id, folder?}` | `ModelInfo \| null` — copies the model's files from `folder`, or (without it) from the folder the person picks in the native dialog (`null`: cancelled); no network; each file is checked against its pinned SHA-256 |
| `index_model_remove` | `{model_id}` | `ModelInfo` — deletes the model's files; `model_busy` while it downloads |

Models are downloaded only when the person asks (`confirm`), from Hugging
Face's CDN at a pinned revision (or, when `local_work.index_model_mirror_url`
names one, from that `https` mirror laid out the same way:
`<mirror>/<repo>/resolve/<revision>/<file>`), each file resumable (`.part` + `Range`)
and verified against the SHA-256 compiled into the app before it is ever
loaded. They live once per computer under `<app data>/models/<sha256>/`
(owner-only), shared by every workspace.

```ts
type ModelInfo = {
  id: string; display_name: string; summary: string; license: string;
  download_bytes: number; ram_mb: number; dimension: number;
  downloading: boolean;
} & ({ state: "missing" } | { state: "partial"; have: number; total: number } | { state: "ready" });

type EmbedStatus = {
  choice: string; state: "waiting" | "running" | "paused" | "done" | "error";
  done: number; total: number;               // distinct chunk texts
  held: "turn" | "on_battery" | "low_power" | null;
  error: string | null; forced: boolean;
  eta_seconds: number | null;                // left at the pace so far, while running
};
```

A pass is resumable: vectors are stored batch by batch, keyed by chunk
text, so a pass the app quit in the middle of embeds only what is still
missing. About 20 s after start-up the app reopens every turned-on index
whose chosen model has fewer vectors than distinct chunk texts, and the
pass resumes. The files a turn changed and the files a message
`@`-references are embedded first (the latest 200 per workspace).

`index://event` carries two more payloads beside a build's (which has no
`kind`):

```ts
{ kind: "model"; model: string; phase: "downloading" | "ready" | "cancelled" | "failed";
  done: number; total: number; error: string | null }
{ kind: "embeddings"; workspace_id: string; status: EmbedStatus }
```

One background thread embeds every open, turned-on index whose setting
names an embedder: after each build, in batches of 32 distinct chunk texts,
at the lowest thread priority (CPU work on half the cores), pausing between
batches while any turn runs, and only on mains power outside Low Power
Mode. One model is in memory at a time, dropped after two idle minutes.
Vectors (int8, `[scale][dim]`, keyed by the chunk text's hash: unchanged
chunks keep theirs across refreshes) are stamped `origin:model:dim`; only
queries embedded the same way are compared with them. With vectors,
`search_workspace` adds a `semantic` list to the fusion (text 0.3, vectors
0.7; `mode: semantic` uses the vectors alone). **The policy is read on
every pass**: a way it no longer allows makes no vectors, its vectors are
deleted, and searches lose their dense side, as a `path_deny` change drops
what it covers. Turning embeddings `off` deletes them too.

## Cloud handoff (`src/handoff.rs`, ADR-0034)

Send a Local work thread to the cloud executor and take it back. The
folder work is `elitea-local-tools`' `handoff` module (every git call
through its hardened git); the bytes move through `elitea-workspace-store`
on the app's one HTTP client with the native token. Rejections are
`HandoffError = { code, message, details }` (`details` is `null` unless
noted). `conversation_uuid` is the server conversation's UUID.

| Command | Arguments | Result |
| --- | --- | --- |
| `handoff_preview` | `{workspace_id, conversation_uuid, leave_on_laptop?: string[]}` | `Preview` — what a send would transfer under the effective policy and the folder's settings; sends nothing |
| `handoff_send` | `{workspace_id, conversation_uuid, leave_on_laptop?: string[], confirm_includes?: boolean}` | `{handoff_id, state, epoch, files, excluded, included, env_vars}` — progress on `handoff://event` |
| `handoff_cancel` | `{conversation_uuid}` | `{cancelled: boolean}` — stops a running send; after begin the host cancels the handoff on the server (`DELETE workspace_handoff`), so the thread is `LOCAL` again; the send rejects `handoff_cancelled` |
| `handoff_status` | `{conversation_uuid, project_id?}` | `HandoffStatus`: the server's state (contract §2.9) plus `executor: "local" \| "cloud" \| "returning"`, `read_only: boolean`, `sent_from_here: boolean` |
| `handoff_status_many` | `{conversation_uuids: string[]}` | `{statuses: Record<uuid, HandoffStatus>}` — for the sidebar: a thread this desktop never sent is `LOCAL` with no request; the rest from the host's records, refreshed at most once per project per call and cached 15 s |
| `handoff_reclaim` | `{conversation_uuid}` | `{state: "RECLAIMING", epoch, cloud_sha}`, or `{state: "CLOUD_RUNNING", reclaim_pending: true, …}` while a cloud turn runs |
| `handoff_preflight` | `{conversation_uuid}` | `{preflight_id, outcome: "fast_path" \| "conflicts", changed_paths, conflict_paths, skipped_paths, cloud_turns, modes, files: ChangedFile[], diffs_truncated: boolean}` |
| `handoff_apply` | `{preflight_id, mode: "apply" \| "worktree" \| "discard"}` | `{applied, outcome: "applied" \| "worktree" \| "discarded", checkpoint_id, worktree_path, branch, imported_turns, released, release_refused: string \| null, copied: string[]}` |
| `handoff_undo` | `{checkpoint_id}` | `{restored: string[]}` — restores the folder to before a take-back |
| `handoff_erase` | `{conversation_uuid, project_id?}` | the server's answer; the local folder is kept |
| `handoff_settings_get` | `{workspace_id}` | `{settings: FolderHandoffSettings, policy: HandoffPolicy, env_offered: EnvVar[], env_notice: string \| null}` |
| `handoff_settings_set` | `{workspace_id, settings: FolderHandoffSettings}` | as `handoff_settings_get`; `invalid_request` for a bad pattern, an include that escapes the folder, or an env name that is not a variable name |

```ts
type ExcludeReason = "platform" | "project" | "user" | "eliteaignore" | "path_deny" | "leave_on_laptop" | "not_a_file";
type Finding = { path: string; line: number; rule: string }; // never the matched text
type Preview = {
  files: number; bytes: number; unpacked_bytes: number;
  // includable: the policy allows including it and it is not path_deny, left on the laptop or not a file
  excluded: { path: string; reason: ExcludeReason; tracked: boolean; includable: boolean }[];
  included: { path: string; reason: ExcludeReason }[];  // sent because the folder's settings include them
  findings: Finding[];          // outside included files: they block the send
  include_findings: Finding[];  // in included files: warnings; the send needs confirm_includes
  has_upstream: boolean;
  project_workspace_bytes_left: number | null; // from the policy route; null when the server has none
  caps: { violations: { cap: string; limit: number; actual: number; path: string | null }[];
          largest: { path: string; bytes: number }[] };
  shadow: boolean; // the folder is not a repository: a shadow git directory in the app data is used
  policy: HandoffPolicy;
  // null when the policy has no env harvest
  // notice: the login shell could not be read (failed, timed out); nothing is offered then
  env: { offered: EnvVar[]; selected: string[]; missing: string[]; vars: number; bytes: number; over_limit: boolean;
         notice: string | null } | null;
};
type HandoffPolicy = {
  source: "server" | "defaults"; // "defaults": the server has no policy route (404, an older server); the ADR's defaults are shown
  exclusions: { platform: string[]; project: string[]; user: string[]; path_deny: string[] };
  allow_user_include: boolean;
  env_harvest: boolean;
  env_limits: { max_vars: number; max_bytes: number };
  limits: { max_segment_bytes: number; max_file_bytes: number; max_files: number; max_unpacked_bytes: number;
            project_workspace_bytes_left: number | null };
};
type EnvVar = { name: string; length: number }; // the value is never sent to the UI
type FolderHandoffSettings = { exclude_patterns: string[]; include: string[]; env_names: string[] };
type ChangedFile = { path: string; status: "added" | "modified" | "deleted"; added: number; removed: number; diff: string };
type HandoffEvent = { conversation_uuid: string;
  phase: "snapshot" | "scan" | "bundle" | "begin" | "upload" | "publish" | "complete" | "cancelled" | "error";
  done: number; total: number; message: string | null };
```

- **Effective policy before begin.** Preview, send and the settings read
  `GET workspace_handoff_policy/prompt_lib/{project}` (contract §6.1): the
  caps, the quota left, the exclusions, `allow_user_include`, `path_deny`
  and the env policy. A 404 (an older server) falls back to the ADR's
  defaults (`policy.source: "defaults"`); any other failure rejects. The
  server's `exclusions` are one merged list; patterns of the ADR's default
  list are shown as platform, the rest as project, unless the server sends
  `exclusions_platform` / `exclusions_project`. `enabled: false` rejects
  `cloud_handoff_disabled`. The begin answer's `limits` stay authoritative.
- **Folder settings.** Per workspace, in the app data
  (`handoff/settings/<workspace id>.json`, owner-only), never in the folder:
  the person's exclusion patterns (matched at any depth; a leading `/`
  anchors at the top), explicit includes (an exact path, or a pattern
  anchored at the top) and the selected env variable NAMES (values are read
  again at send and never stored, logged, emitted or recorded).
- **Explicit include.** Only when `allow_user_include`; `path_deny` is never
  includable. Included files are scanned: their findings
  (`include_findings`) do not block, but `handoff_send` rejects
  `include_confirm_required` (`details.findings`) unless
  `confirm_includes: true`, which the UI passes only after the person
  confirmed for this send. The begin call sends `included_excluded`; the
  handoff commit carries one `Elitea-Included: ./<path>` trailer per file.
- **Env harvest.** Only when `env_harvest`. The source is the person's
  login-shell environment, read once per preview, settings read or send the
  way editors read it (`$SHELL -l -i -c` printing `env -0` between markers,
  from the home directory, no stdin, 10 s timeout; `/bin/zsh` on macOS or
  `/bin/bash` when `$SHELL` is unset) and merged over the desktop process's
  environment. A shell that fails or times out offers nothing and sets
  `notice`; it is not an error. Local `run_command` is unchanged. The names
  are filtered by `env_deny` (patterns, `*` wildcard) and, when set,
  `env_allow`; nothing is preselected. At most `env_limits.max_vars`
  variables and `max_bytes` (names plus values); over that the send rejects
  `env_too_large`. The values go only into the begin call's `env`.
- **Policy.** Preview and send need the device policy's `local_work.allowed`
  and `local_work.cloud_handoff` (`cloud_handoff_disabled`). The server
  checks the platform ceiling and the project setting at begin.
- **Cancel.** A failure after begin (the begin answer's caps, an upload or
  publish failure, `handoff_cancel`) cancels the handoff on the server
  (`DELETE workspace_handoff`, accepted in `HANDING_OFF`, contract §6.3), so
  the thread is `LOCAL` again at once.
- **Send.** The snapshot is the checkpoint set (tracked plus untracked,
  not ignored) built in a temporary index: the folder's index, `HEAD`,
  stash and working tree are not changed. The mandatory exclusions,
  `path_deny`, `.eliteaignore` and `leave_on_laptop` apply to tracked files
  too. History mode `none`: an orphan base, an orphan upstream (the remote
  URL without credentials), the snapshot. A secret finding rejects with
  `secret_found` (`details.findings`), before any request; there is no
  override: leave the file on the laptop or edit it. A cap rejects with
  `handoff_too_large` (`details` = `caps`), checked against the defaults
  before begin and against the begin response's `limits` before upload.
  When the project's remaining cloud storage (`project_workspace_bytes_left`)
  is the ONLY cap over, the code is the server's `workspace_quota_exceeded`
  instead, with the same `caps` plus `details.bytes_left` and
  `details.bytes_needed`; any other cap, alone or with the quota, stays
  `handoff_too_large`. A publish that meets a busy thread (`409
  thread_busy`) is sent again unchanged by the store client for up to 30 s
  (it is never a conflict); after that the error code is `thread_busy`.
  The workspace is held (`workspace_busy`) while a send runs. Server codes
  pass through (`local_turn_in_progress`, `handoff_in_progress`,
  `thread_executor_is_cloud`, `workspace_quota_exceeded`,
  `history_mode_not_allowed`, …). A folder inside a repository but not its
  top level is refused (`handoff_unsupported`), so is a sha256 repository
  (`unsupported_object_format`).
- **Read-only.** While a thread is not `LOCAL`, `agent_turn_start` with its
  UUID rejects `thread_executor_is_cloud` and `handoff_status` answers
  `read_only: true`; the UI shows the transcript read-only.
- **Take back.** `handoff_reclaim`, then `handoff_preflight` once the state
  is `RECLAIMING` (else `handoff_not_reclaimed`). The preflight verifies the
  segment (`integrity_failed` for a tampered one) and writes nothing to the
  folder or any ref. `fast_path`: the folder and `HEAD` are as at send;
  `apply` writes the cloud's changed paths as uncommitted changes (`HEAD`
  and the index untouched), keeps the cloud commits on
  `elitea/cloud/<first 8 of the UUID>` and takes a checkpoint first
  (`checkpoint_id`, for `handoff_undo`). `conflicts`: only `worktree` (the
  cloud tip beside the folder, `<folder>-elitea-cloud-<short>`) and
  `discard` are offered (`modes`); the worktree gets the folder's excluded
  files (`.env`, keys; `copied`) and opens as a workspace (host git trusts a
  linked worktree of an open workspace). `files` are the per-file diffs of
  what the take-back writes (256 KiB of diff text at most, text files only;
  `diffs_truncated`). `handoff_apply` refuses with
  `folder_changed_since_preflight` when the folder moved after the
  preflight. Excluded paths, symlinks and submodule entries are never
  deleted or overwritten (`skipped_paths`); a file the cloud created that
  the exclusions match is written only where no local file exists.
- **Release and history.** `handoff_apply` releases the thread and imports
  its cloud turns into the thread history (`StoredTurn.origin: "cloud"`,
  changed files from the cloud commits' diffs). A release the server did
  not acknowledge (`released: false`) is kept and sent again by the next
  `handoff_status`; until then the thread stays read-only. A release the
  server refuses for good (`release_refused`: `handoff_erased` when the
  cloud copy was erased meanwhile, `stale_epoch`, `handoff_not_found`) is
  not kept: the thread follows the server's state (`LOCAL` after an erase,
  so `released` is `true` and the cloud turns are still imported). The
  release carries the epoch the preflight read, so a take-back asked while
  a cloud turn ran (202 `reclaim_pending`, `RECLAIMING` one epoch later)
  releases with the later epoch.
- **Records.** What take-back needs is kept in the app data
  (`handoff/threads/<uuid>.json`, owner-only); a shadow repository in
  `handoff/shadow/`. Nothing is written into the folder but the take-back's
  own files.
- **Store URLs.** The upload grant's `put.headers` are all sent (on S3 and
  RustFS they bind `x-amz-checksum-sha256`, so the store refuses a PUT
  without it or with other bytes). Main answers its streaming facade and
  the multipart completion as origin-relative paths (`/api/v2/…`); they
  are resolved against the deployment and carry the bearer. A presigned URL
  on another host never does.

### Cloud handoff: live run

`src/handoff_live_tests.rs` runs this host against a real elitea-main
(ignored by default). It covers policy → preview → send of a repository
with staged, unstaged, untracked, ignored, excluded (`.env`, a project
pattern) and explicitly included files and a harvested env variable; the
state and the web's take-back request (its notification row); a cloud turn;
take-back (reclaim, preflight with diffs, apply, release, import into the
thread history, undo); take-back while a cloud turn runs; cancel during
`HANDING_OFF`; erase in `CLOUD_IDLE`; an erase that beats the release; a
stale epoch; and the quota. It needs:

- elitea-main with a database (migrations applied) and an S3-compatible
  store whose bucket reports default encryption (or the operator
  attestation for a store that cannot report it); the platform
  `cloud_executor` section enabled with `env_harvest` and
  `allow_user_include`, the native client policy's `local_work_allowed` and
  `local_work_cloud_handoff`, and a project `cloud_executor` setting with
  `{"enabled": true, "env_harvest": true, "exclusions_add": ["secrets/**"]}`;
- a native access token (`elnat_…`) of a project member with
  `models.chat.messages.create`, and optionally a second member's;
- `ELITEA_HANDOFF_LIVE_SIM`: the cloud-turn simulator, a program that plays
  the cloud executor until the worker serves workspace turns. Its source is
  `services/elitea-main/cmd/handoff-live-cloudturn/main.go`: it drives Main's
  `workspacehandoff` and `workspacestore` services, which are `internal`
  packages, so it has to sit inside the elitea-main module (as the other
  `cmd/` tools do). It is behind the `handoff_live` build tag, so
  `go build ./...`, vet, lint and CI never compile it. Build it from a tree
  that has the handoff server (PR #1294, or main once it merged):

  ```sh
  cd services/elitea-main && go build -tags handoff_live -o /tmp/cloudturn-sim ./cmd/handoff-live-cloudturn
  ```

  (Without the server packages the tagged build fails on the imports; that
  is why the tag exists on this branch.) `-project P -conversation UUID
  -bundle FILE -commit SHA [-hold FILE]` writes a question and an answer,
  starts the cloud turn on the answer, uploads and publishes
  `refs/elitea/cloud` as `cloud_executor` and ends the turn (after FILE
  exists, with `-hold`), printing `{state, epoch, cloud_sha, seq,
  execution_id, answer_uuid, answer_meta}`; `-sql QUERY` prints rows as a
  JSON array of arrays of strings. It runs with Main's `DATABASE_URL` and
  S3 storage environment (`STORAGE_BACKEND=s3`, `STORAGE_CONTAINER`,
  `S3_*`).

```sh
ELITEA_HANDOFF_LIVE_URL=http://127.0.0.1:39880 ELITEA_HANDOFF_LIVE_PROJECT=1 \
ELITEA_HANDOFF_LIVE_TOKEN=elnat_… ELITEA_HANDOFF_LIVE_OTHER_TOKEN=elnat_… \
ELITEA_HANDOFF_LIVE_SIM=/path/to/cloudturn-sim \
cargo test --lib handoff::tests::live -- --ignored --test-threads=1
```

The quota case narrows the project setting to `max_project_workspace_gb: 0`
for one send and restores it.

## App and native shell (`src/platform.rs`, `src/local_commands.rs`)

| Command | Arguments | Result |
| --- | --- | --- |
| `app_platform` | — | `AppPlatform` (below). Never rejects. |
| `context_menu_show` | `{request_id: string, items: Item[]}`; `Item` is `{id, label, enabled?}`, `{separator: true}` or a submenu `{label, enabled?, children: {id, label, enabled?}[]}` (one level) | `null`, once the menu is closed — pops the OS's native context menu at the cursor (`src/context_menu.rs`). The outcome arrives on `app://context-menu` as `{request_id, id}`: `id` is the id the page gave the item, or `null` when the menu was dismissed (sent by the host ~150 ms after the menu closed with no choice). The page ignores events whose `request_id` is not its own. `request_id`: ≤ 64 chars of `[A-Za-z0-9_-]`. At most 60 choosable items across the menu and its submenu; a non-separator needs a unique `id` (≤ 64 chars) and a `label` (≤ 120); rejects with the reason for anything else. |
| `activity_cloud_run` | `{conversation_uuid: string, running: boolean, waiting?: boolean, workspace_id?: string, workspace_name?: string}` | `null` — the page tells the host a cloud turn of this window is streaming (`running`) and/or waits on the person (`waiting`: an approval or continue prompt); both `false` ends it (completion and errors both send that). The menu-bar item shows it like a local turn (`◐ m:ss`, a "Running in cloud" entry); a waiting thread counts in `⚠ n`, adds an "Approve in cloud" entry and is counted on the Dock badge with the local approvals. A click sends `focus_turn`. The chat stream to a cloud thread runs in the page (the web chat transport), so the host cannot see it itself. |
| `reveal_path` | `{workspace_id, path}` | `null` — shows the file or folder in Finder / the file manager. |
| `open_path` | `{workspace_id, path}` | `null` — opens it with its default app. |
| `doctor_run` | `{scope?: "local"}` | `Check[]` — the Doctor's checks (README, "Diagnostics"); `"local"` checks this computer's files only (no network: what the launch notice uses). Never rejects. |
| `doctor_fix` | `{fix_id, confirm?: boolean}` | `{message}` — applies the repair a check named in `fix_id`, then the UI runs the checks again. A check with `fix_confirm` names what its repair deletes: the UI shows it and passes `confirm: true` only once the person confirmed; without it that repair is refused and nothing is deleted. Rejects with the reason when the repair fails or `fix_id` is unknown. |
| `app_ready` | — | `AppCommand[]` — the page now listens to `app://command` (call it once the listener is live); the commands the host sent before that, oldest first. Never rejects. |

```ts
type Check = {
  id: string;            // credentials, dir.config, dir.data, dir.logs, history, sessions, workspaces,
                         // index.<workspace id> (one per workspace with a local index),
                         // deployment, session, local_work, pending_revokes
  title: string;
  status: "ok" | "warn" | "fail";
  message: string;       // for a person; never a secret
  fix_id?: string;       // credentials.tighten | credentials.move_aside | dir.tighten.{config,data,logs}
                         // | history.tighten | history.move_aside | sessions.tighten
                         // | sessions.move_aside | workspaces.drop_missing
                         // | workspaces.move_aside | revokes.retry
                         // | index.tighten:<workspace id> | index.move_aside:<workspace id>
                         // | index.rebuild:<workspace id> | index.refresh:<workspace id>
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
and turns it on again so it is built from nothing. One turned on whose last
build is more than 24 h old while its folder is not watched is a warning,
and `index.refresh` refreshes it (as `index_refresh`). One written by a newer app
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
native window follow its light/dark mode; `core:window:allow-set-focus`
lets it bring an inspector tab's own window (`panel-<tab>`) to the front.
Those are the only window permissions granted.

An inspector tab opens in a window of its own through
`window.open("about:blank#elitea-panel=<tab>")` (`<tab>`: lowercase letters
and dashes). The host creates the native window `panel-<tab>` for it (one per
tab: asking again raises it), titled after its document, its size and
position remembered by the window-state plugin. The window shows the empty
same-origin document the page fills itself; it may not navigate, and no
capability names its label, so it has no IPC of its own. Every other
`window.open` is refused. `log:allow-log` (`plugin:log|log`) lets the UI append
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
| `toggle_changes` | — | View ▸ Toggle Inspector (⌘⌥\\) |
| `back` | — | View ▸ Back (⌘[) |
| `forward` | — | View ▸ Forward (⌘]) |
| `send_to_cloud` | — | Thread ▸ Send to Cloud… (⇧⌘U): open the Send to cloud sheet of the thread on screen. |
| `open_in_chat` | — | Thread ▸ Open in Chat: open the thread on screen in the web chat. |
| `toggle_plan` | — | Thread ▸ Plan Mode (⇧⌘P): switch the composer between Act and Plan. |
| `choose_agent` | — | Thread ▸ Choose Agent… (⌘L): open the composer's agent picker. |
| `index_settings` | — | Folder ▸ Index Settings…: open the folder's index settings (inspector, Index). |
| `handoff_settings` | — | Folder ▸ Handoff Settings…: open the folder's cloud handoff settings. |
| `workspace_opened` | `{workspace_id: string}` | A folder became (or already was) a workspace through File ▸ Open Folder… (⌘O), a drop on the window, or a drop on the dock icon / "Open With" in Finder. One event per folder; the workspace is already in `workspace_list`, so the UI selects it. |
| `workspace_open_failed` | `{message: string}` | One of those folders could not be opened (the first failure; a message for a person). |
| `files_dropped` | `{paths: string[]}` | Files (not folders) were dropped on the window: their **absolute** paths, in drop order. The host does nothing else with them; the UI attaches the images among them to the composer with `attachment_add`, and a path inside a workspace can be made relative against `Workspace.path` (e.g. for `mentions`). |
| `run_diagnostics` | — | Help ▸ Run Diagnostics…: open the Doctor. Sent live, never queued (the connect screen listens without `app_ready`). |
| `signed_out` | — | The session ended on this computer outside a sign-out the page asked for (the Doctor moved the stored sign-in aside): drop what the page shows of it (the desktop shell reloads to the connect screen). Sent live, never queued. |
| `focus_turn` | `{workspace_id: string, turn_id: string}` | A running or waiting turn was chosen in the menu-bar item's menu (see below): show its folder. (A notification click still only activates the app: the notification plugin has no click callback on desktop.) |

**Activity contract.** Two places fold the agent, index and handoff streams
into "what is running now": the host (`src/activity.rs`: the menu-bar item and
the Dock) and the web feed (`widgets/desktop-shell/model/activityFeed.store.ts`:
the activity island). They follow ONE set of rules, so they cannot drift; change
both together:

- A turn is running from its first `status` with phase `resolving`, `starting`,
  `running` or `committing`, and from an `approval_request`.
- A turn ends at `done`, at an `error` event, or at a `status` with phase `done`,
  `cancelled` or `error` — a turn refused at the start sends `error` and
  `status` `error` and never `done`. (The web feed keeps a turn listed after
  that only while its commit is pending or failed.)
- An approval waits from `approval_request` until `approval_respond` answers it,
  an `approval_closed` names its `request_id` (the host closed it at its wait),
  or its turn ends. No other event closes it: a `tool_result` or a `text_delta`
  says nothing about which approval it was.
- Index progress: `Parsing <m> files` in an `index://event` message sets the
  total and resets the count; `Processed <n> files` sets the count; leaving the
  `building` state clears it. (Web: `foldProgress`, `features/workspace-index/
  model/progress.ts`; host: `ActivityState::observe_index`.)
- A cloud turn streaming into the window runs from `activity_cloud_run(running: true)` until both `running` and `waiting` are false (completion and errors both end it); it waits on the person while `waiting` is true, which counts like an approval; web: `cloudRuns` in the feed.
- A send to the cloud runs until its `handoff://event` phase is `complete`,
  `cancelled` or `error`; its bar is `done` of `total` (indeterminate when
  `total` is 0).

**Menu-bar item and Dock** (`src/activity.rs`, host-side; the page does nothing for
them). The host folds the same `agent://event`, `index://event` and
`handoff://event` streams the window gets: the menu-bar item's title is
`⚠ n` while n approvals wait, else `◐ m:ss` (the longest-running turn), else
only the icon; its menu lists the waiting approvals (tool and folder, never a
command or a path) and the running turns, then Show / Quit, and choosing a
turn shows the window and sends `focus_turn`. The Dock's progress bar follows a
send to the cloud (`done`/`total`), else the index builds (files read of files
to read; indeterminate until that is known), and is removed when they end.

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

Seven event names, all emitted to the `main` window only: `app://command`
(above), `app://context-menu` (`{request_id, id}`, the outcome of `context_menu_show`),
`agent://event`, `index://event`, `handoff://event` (Cloud handoff, above),
`process://event` (Background processes, above) and `mcp://event` (Local MCP
servers, above). The webview may listen
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
| `status` | `{phase: "resolving" \| "starting" \| "running" \| "done" \| "cancelled" \| "error", message?: string, project_instructions?: string[], model?: TurnModel, continued?: number, host_prompt?: string, note?: string, timings?: Timing[]}` — `timings` (below) are the turn's phases measured since the previous `status`; `project_instructions` (on `running` only, when any) lists the AGENTS.md paths the turn applies, in order; `model` (on the first `running`) is the model the turn runs on, `{name, reasoning_effort?, chosen, pinned}` (`chosen`: the person's choice over the agent's model; `pinned`: the agent pins its model, so no choice is offered); `continued` and `host_prompt` (on a later `running`) say the run went on past its step limit for the n-th time from the host's own prompt, which is not the person's message; `note` (on `done`) is `step_limit_reached` when the turn stopped at its limit |
| `text_delta` | `{text}` — model text as it streams |
| `thinking_delta` | `{text}` — the model's reasoning as it streams (the gateway's `reasoning_content` / `reasoning` deltas), sent next to `text_delta` as it arrives; the transcript folds it into a collapsed "Thinking" block of the step (issue 1228). Never logged |
| `thinking_end` | `{chars: number}` — a reasoning block closed (the model moved on to its answer or its tool calls, or its stream ended); `chars` is its length |
| `tool_call` | `{call_id, tool, args_summary, remote: boolean}` |
| `tool_result` | `{call_id, ok: boolean, summary, truncated: boolean}` |
| `usage` | `{model: string, step: number, prompt_tokens: number, completion_tokens: number, total_tokens: number, cached_tokens?: number, cache_write_tokens?: number, reasoning_tokens?: number, context?: ContextMeter}` — one of the turn's own model calls as the gateway reported it, sent once the call's stream ended (only when the gateway reported usage); `step` counts the turn's calls from 1. `prompt_tokens` is what that call sent, its cached reads (`prompt_tokens_details.cached_tokens`) and cache writes (`…cache_write_tokens`, Anthropic) included; `reasoning_tokens` are part of `completion_tokens` (`completion_tokens_details.reasoning_tokens`); each optional count only when non-zero. A step retried after a context overflow (below) sends the dropped call's usage too: both calls were billed. `context` (below) is that call's request occupancy as the host measured it before sending: the composer's context meter. A summary call's and a sub-agent's calls send none; they count in `usage_total` |
| `usage_total` | `{prompt: number, completion: number, cached: number, cache_write: number, reasoning: number, calls: number, subagents?: {same fields}}` — once the run ended, before `status` `done`/`error`/`cancelled`: every model call made on the turn's execution id (the agent's steps, compaction summaries and sub-agents together: what the gateway's request log holds for it); `subagents` is the sub-agents' share, when any ran. The thread history keeps it past its event cap; a thread's total is the sum of its turns' |
| `context` | `ContextMeter` (below) — the request occupancy when its `phase` changes: `compacting` as a summary call starts (the meter says "Summarizing…" while it runs), `compacted` once the step's request is the compacted one, `measured` again at the next call (or when a summary failed and the request went out uncompacted). A turn starts `measured`; calls that stay `measured` send none, their occupancy rides on `usage` |
| `approval_request` | `{request_id, kind?: "continue" \| "mcp_authorization", steps?: number, call_id?, toolkit?, server_url?, authorize_path?, tool, title, detail, command?: string[], paths?: string[], reason, source?: "workspace" \| "user" \| "default" \| …, rule?: string, justification?: string, loopback?: true, keep_alive?: true, can_remember: boolean}` — `loopback` and `keep_alive` mark a background command that may serve on localhost or that lives until the session ends (**Background processes**); `kind: "continue"` is the step limit's question (below), `kind: "mcp_authorization"` an MCP server's sign-in (**MCP sign-in**); `source` is the layer that asks, and when an approval rule asks, `rule` names it (`workspace#3`, `user#1`) and `justification` is its text (the card shows it in place of `reason`) |
| `plan_updated` | `{plan: {title, explanation?, status: "in_progress" \| "completed", steps: [{n, step, status: "pending" \| "in_progress" \| "completed" \| "cancelled"}]} \| null}` — the agent's checklist changed (the runtime planner's `update_plan`, `start_step`, `complete_step`, `delete_plan`, or the host copying an accepted plan in): the whole plan, `null` once deleted. The latest one is the plan; the thread history stores it like every event, so the todo pane replays after a restart. The plan itself lives in the session (`elitea.plan.v1:` state), so the next turn's planner reads it back |
| `plan_proposed` | `{request_id, call_id, plan}` — the agent's `exit_plan_mode`: the plan (Markdown, model-written) as a card with Accept / Decline; answered with `approval_respond` `accept` or `decline` (+ `feedback`). See **Plan mode** |
| `plan_accepted` | `{call_id}` — the person accepted the plan: the session left plan mode and the same turn goes on with every tool; the composer's plan switch turns off |
| `subagent_started` | `{agent_id, parent_call_id, ordinal: number, label?}` — the agent's `spawn_agent` call `parent_call_id` started a read-only sub-agent (**Sub-agents**, below); `ordinal` is its place among the turn's sub-agents, from 1 |
| `subagent_event` | `{agent_id, inner}` — what the sub-agent did: `inner` is `{kind: "tool_call", call_id, tool, args_summary, remote}`, `{kind: "tool_result", call_id, ok, summary, truncated}` (its own tool calls, never the turn's `tool_call`s) or `{kind: "text_delta", text}` (one model answer's text, at most 2 KiB); at most 200 per sub-agent |
| `subagent_finished` | `{agent_id, status: "completed" \| "failed" \| "budget_exhausted" \| "cancelled", steps, input_tokens, output_tokens, result?, error?}` — the sub-agent settled: its report (`result` at most 8 KiB), once per run (a `send_input` that continues it starts another); `cancelled` when it was closed or the turn ended or was cancelled before it answered |
| `process_started` | `{process_id, call_id, command, tty: boolean, sandbox, loopback: boolean}` — a `run_command` call started a background process (**Background processes**) |
| `tool_output_delta` | `{call_id, process_id, stream: "stdout" \| "pty", data, offset: number}` — its output while this turn runs: at most 20 per second per process, at most 16 KiB each, `offset` the byte offset of `data` in the process's output |
| `process_exited` | `{process_id, call_id, status: "exited" \| "killed" \| "timed_out", exit_code?}` — it ended while this turn runs (the turn's end stops the ones it does not keep) |
| `approval_closed` | `{request_id, reason: "timeout"}` — the host closed an open `approval_request` nobody answered within its wait (today: an MCP sign-in, 10 minutes); the card goes, the turn's `hitl_exchanges` record it with `decision: "timeout"`, and an `approval_respond` for it answers `approval_closed` |
| `error` | `{code, message}` |
| `done` | `{committed: boolean, commit: "pending" \| "none", execution_id?, conversation_id, message_ids: string[], changed_files: number}` — sent before the turn's commit (below): `committed` is `false` and `commit` `pending` for a turn being saved, `none` for a cancelled one |
| `commit_failed` | `{execution_id, code, message}` — after `done`: the turn's commit did not land after its retries; `turn_commit_retry` sends it again |

Order of a turn: `status` `resolving` → `status` `starting` → `status`
`running` → (`thinking_delta`, `thinking_end`, `text_delta`, `tool_call`, `approval_request`, `plan_proposed`, `plan_accepted`, `plan_updated`, `approval_closed`, `tool_result`, `usage`, `context`, `subagent_started`, `subagent_event`, `subagent_finished`, `process_started`, `tool_output_delta`, `process_exited`)* →
`usage_total` → `status` `done` | `error` → `done` → (`commit_failed`). A cancelled turn
ends `usage_total` → `status` `cancelled` → `done` with `commit: "none"`; nothing is
committed and its execution expires on the server. A run that fails after
the start is still committed, as a failed answer (`is_error`), and its
`error` event comes before `status` `error`. `done` ends the turn for the
UI, and the host holds the workspace (`workspace_busy`) until it is sent,
so the UI is busy until `done`, not until `error`.

**The commit after `done`.** `done` is sent as soon as the run ends; the
turn's commit then runs in its conversation's lane (one at a time per
conversation, in order). The next turn of that conversation starts (its
`local_turn` start, whose memory recall reads the committed conversation)
only once the previous commit settled, so it waits in `status` `starting`
while one is still being saved. A commit that fails is retried with backoff
on the same execution id and body (`409 local_turn_already_committed`
counts as saved); after the last attempt (or at once for a refusal a retry
cannot change, such as `410` or `403`) the host sends `commit_failed`
(the turn's last event) and keeps the body in memory until the app quits.
`agent_turn_status` answers the settled state: its `done` payload carries
`committed: true, commit: "committed"`, or `commit: "failed"`. A quit with a
commit still running or failed is logged; that execution expires on the
server, as before.
The read-only, concurrency-safe calls of one model step (`read_file`,
`list_tree`, `search_files`, `find_files`, `read_output`, `read_document`,
git reads, the index tools)
run together, at most 8 at once, before the step's other calls; writes,
commands and remote toolkit calls then run one at a time in the model's
order, so at most one tool `approval_request` is open at a time. Their
`tool_call` and `tool_result` events interleave: match a result to its call
by `call_id`, never by position. The commit lists the steps in the model's
call order, whatever order they ran in.
**Tool output budgets** (`elitea-local-tools` `budget`). Every local tool
result the model reads stays within a token budget (estimated as
`ceil(bytes / 4)`): `read_file` and `read_output` 12k (whole lines,
`next_offset`; a line longer than a page continues at `next_column`),
`run_command` 6k for stdout and stderr together (head and tail),
`run_tests` 6k (counts, at most 20 failures or compiler errors of at most
2 KiB each, the whole log always spilled),
`search_files` and `find_files` 8k (`truncated`), `git_diff` and
`git_log` 8k, `read_document` 12k. When a command's, a diff's, a log's or
a document's output is cut, the whole of it is kept in a file under the
workspace's data directory (`sessions/<workspace id>/out/<conversation>/`,
`0700`, files `0600`, hidden from the file tools by the deny list even when
the workspace is the home folder), and the result names it as `output_id`;
the model pages it with `read_output` (by id only, no path, and only the
thread's own outputs). Outputs are per conversation (at most 64 MiB each,
256 MiB per workspace, the oldest dropped first), outlive a policy change
and an app restart for 24 hours, and go with `thread_history_delete`; a
gone id answers `local_tools.output_expired` (the model re-runs the tool).
A `tool_result`'s `summary` is unchanged.
**Documents** (`read_document`, #1220). PDF, Word, Excel, PowerPoint,
OpenDocument, RTF, EPUB and e-mail files are read as text, page by page (a
PDF page, a slide or a sheet is one page, marked `[page N]` in the text; a
format with no pages is page 1). Arguments: `path`, `pages` (`"5"`, `"1-20"`,
`"30-"`; default all) and `offset` (a character offset into the selected
text). The result carries `page_count`, `pages` (the range the text shows),
`text`, `truncated`, `extractor`, and, when something is left,
`next_offset` (continue inside the selection with the same `pages`) and
`next_pages` (the pages the selection did not reach: the extractor's answer
is capped, and so is the selection at 512 KiB). The host does not link the
extractor (xberg turns on `serde_json/preserve_order`); the release bundle
carries it as a second binary beside the executable,
`elitea-doc-extract-sidecar` (`apps/elitea-desktop/doc-extract-sidecar`,
its own lockfile). Per document the host sends the bytes on stdin (a
16-byte header, the media type, the file; no path) to one process run under
the session's sandbox (read-only, no network, the session's deny list, an
empty environment, 130 s wall clock then the process group is killed) and
reads at most 8 MiB of JSON back. A crash, a timeout or an unreadable file
is the tool's error (`local_tools.io`, `local_tools.timeout`,
`local_tools.binary`); the host keeps running. Without the sidecar (a
`cargo run` build) only text files are read and a PDF answers
`local_tools.unsupported`; where the sandbox cannot hide the denied
credentials, documents are not extracted
(`local_tools.sandbox_unavailable`). No command or event is added.
Inside a long turn, once a model request (its messages, the system
instruction and the tool declarations) passes 60% of the model's window,
the tool results older than the newest 6 are sent to the model as short
notes naming the tool, its arguments and any `output_id`
(`elitea-agent-runtime` `context::ToolResultElision`); the session and the
committed transcript keep them whole. The window is the turn's model
limits (**Context window**, below).

**Context window** (#1217). The turn's model limits are the resolved
version's frozen `model_context_limits` when it carries them, else the
model catalogue's row for the model
(`GET /api/v2/configurations/models/{project_id}?section=llm&include_shared=true`:
`context_window`, `max_output_tokens`, `max_input_tokens`; the turn's
project's own row before a shared one), read once per app session (a
sign-out, sign-in or deployment switch reads again). Only a definitive
answer is kept: a 2xx, or a 404 (the model is not described); a catalogue
that could not answer now (a network error, 401, 403, 408, 429, a 5xx) is
asked again next turn, the turn running on the fallback meanwhile. A model
the catalogue does not describe runs on a 128k window (the elision's
default) with 4k output, marked `window_fallback: true`. The order inside a long session: the older tool
results are elided first (above); then, before every model call, the step
compaction (`elitea-agent-runtime` `context_management::StepCompaction`)
measures the provider body the host will send (messages, system
instruction, tool declarations; `ceil(bytes / 4)` tokens) and, once it
passes the window minus a reserve of 16k tokens (at most a quarter of the
window; the request's output reservation when larger), folds everything
before a cut point into a `context_summary` v1 record, keeping about the
newest 20k tokens (at most half the trigger) verbatim. A cut is at a turn's
start or at a step with no tool call open (a call and its result are never
parted). The summary is asked of the agent's own model through the same
`/llm` route and execution id (its spend is the turn's), without tools and
without prompt-cache directives, and streams nothing to the UI (the meter's
`context` event says `compacting` while it runs); it carries
the previous summary's objective and a cumulative `files_touched` entry
(`[{path, op: read|edit|write|delete}]`, in `key_facts`). A summary that
ignores the template is asked for once more without tools, then kept as a
plain-text v1 record (≤ 32 KiB). When even the kept tail does not fit, the
step starts from the summary, the last user message and the last tool
result cut short. When the summary model fails, a request that still fits
(or a `/compact` one) goes out uncompacted and the meter says `measured`;
one that does not fit fails the turn with the summary call's error. A model
refusal for a too-long request (`context_length_exceeded`, or a `length`
finish with no answer text, no tool call and fewer than 512 output tokens
and under half the request's own `max_tokens`, so an agent with a small
`max_tokens` that uses it is not an overflow) compacts and retries the
step once; a second one, or a summary that fails on that retry, fails the
turn with `context_overflow` ("… Start a new thread to continue."). An
answer whose text already streamed (`text_delta`) is never retried, so the
UI never shows a dropped answer before a new one. After a compaction the host appends
an adk `EventCompaction` marker to the turn's session after the last event
it folds, so the next turn's history starts from the summary instead of
summarising the thread again; the turn's commit carries the latest summary
as `context_summary` (the shared v1 record, never a host format; a server
that does not know the field ignores it). An agent version whose
`context_settings` say `enabled: false`, `enable_summarization: false` or
`session_compaction: false` runs without compaction; `session_compaction:
{reserve_tokens?, keep_recent_tokens?, fresh_window?}` tunes it and
`summary_instructions` guide the summary.

```ts
type ContextMeter = {
  used_tokens: number;      // the request's estimated input tokens
  window_tokens: number;    // the window the budget uses (the model's, at most 272k)
  percent: number;          // used of window, 0-100 (the meter's figure; it warns at 80)
  phase: "measured" | "compacting" | "compacted";
  compacted: boolean;       // this turn compacted
  compacted_at?: string;    // when it last did (RFC 3339, UTC)
  window_fallback: boolean; // the 128k default: the catalogue did not name the model's
};
```

It carries estimates and budget arithmetic only, never model content.
Events are not replayed: a UI that may have missed one (the window was
hidden, a cancel was answered `turn_unknown` or `turn_not_cancellable`)
asks `agent_turn_status` and takes its `done` payload as the event.

**Timings.** A `status` event carries, as `timings`, every phase of the
turn measured since the previous `status` (absent when none was):

```ts
type Timing = { phase: "resolving" | "participant" | "starting" | "first_token" | "tools" | "committing"; ms_since_turn_start: number; phase_ms: number };
```

`resolving` is the agent version's read (`resolved_version`; on a warm
turn its revalidation, beside the start), `participant` the conversation
lookup (the answering participant and UUID; on a warm turn its re-read,
beside the start), `starting` from `status` `starting` until the local turn
is started and its session ready (a seed included), `first_token` from `status` `running` to the model's first
streamed chunk, `tools` the wall time any tool call ran (overlapping calls
counted once), `committing` the commit with its retries (it runs after
`done`, so it is in the log line only, never on a `status`). `ms_since_turn_start` is
when the phase ended, from `agent_turn_start`. A phase before the first
token that takes longer than 2 s is logged as a warning, and each turn
logs one line with all of them (`local turn timings: resolving=…ms …`):
what to compare before and after a change.

**Step limit.** Each run of the agent takes at most its step limit of
model steps: the workspace's own setting (`workspace_agent_settings_set`)
when the person set one, else the agent's `meta.step_limit`, else 100; never
more than 200. The workspace setting wins because it is the person's choice
on this machine, while an agent's value is often the platform's default
(an agent created without one is stamped 25).
When a run ends because it used them (adk's own end of the run, not a
count), the host asks `approval_request {kind: "continue", title: "The agent
used its N steps", detail: <the start of the agent's last text>, steps: N,
can_remember: false}`. It is asked through the prompt only: no workspace
rule or remembered choice answers it, and `allow_always` answers once.
`allow_once` / `allow_always` continue: the same agent runs again on the same
session, within the same execution, from the host's prompt "Continue from
where you stopped." (`status` `running` with `continued`), at most 3 times a
turn. `deny`, no answer within 10 minutes, or a fourth limit end the turn
normally: what happened is committed as one turn (its `hitl_exchanges` hold
each question as kind `continue`), and `status` `done` carries
`note: "step_limit_reached"`. Cancelling while the question is open cancels
the turn.

`message_ids` are the question's and the answer's message UUIDs, in that
order: the messages the commit writes. `approval_request.can_remember` is true when
`allow_always` is remembered for this workspace (a simple command or a
file change); otherwise `allow_always` answers once and the UI does not
offer it. For `run_tests` the remembered choice is the runner kind
(`cargo`, `go`, `vitest`, …) for this workspace: a later run of that
runner, whatever its filter, within the sandbox and network it was
approved with, is not asked again (one that is destructive with the
network off still is); it is asked first when it sets `network: true`.
It is false for a `run_command` that would run unconfined,
decided per command from the sandbox chosen for it: none (a host that
allows unenforced sandboxes), Landlock alone (Linux without bubblewrap,
under partial enforcement), or a bubblewrap walk cut at its cap. Such a
command is asked every time, so a remembered choice would never apply.
Its result carries `enforcement: "partial"` or `"none"` and, for Landlock
and a cut walk, an `enforcement_note` saying what was not hidden; the
turn's `local_work` report keeps the weakest `enforcement` and its note. `can_remember` is
computed from the very precondition the host's `remember` checks; should
storing the choice still fail (the scope the person picked does not match,
or the store cannot be written), the call is approved once and a warning
logged — an approval is never turned into an error. Remote toolkit
confirmations (`confirmation_required`) never remember.
