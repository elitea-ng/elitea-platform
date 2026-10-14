# GitHub effects and the rest of the artifact tools (2026-10-13)

The Rust worker served 20 of the `github` toolkit's 44 SDK tools (reads only) and 4 of the
`artifact` toolkit's 16. This change serves 37 of 44 and 10 of 16, read against the
worker-pinned SDK `b5113a1` (`services/elitea-worker-python/elitea-sdk.lock.json`). What is
left is the six index tools of each toolkit (the index toolset serves five of them, Main owns
`index_data`) and one GitHub tool that cannot be ported (below).

The per-tool capability is `supported_tools.github` (37 names) and `supported_tools.artifact`
(10 names) in
`services/elitea-main/internal/runtimecomposition/current_rust_worker_toolkit_capability_snapshot.json`;
`sdk_conformance::assert_sdk_conformance` fails if either list drifts from what the family
serves, and every served tool passes the SDK schema gate with no exemption.

## GitHub

Code: `libs/rust/agent-runtime/src/toolkits/families/github/{writes,project_writes,patch}.rs`
(new), `tools.rs`, `client.rs`, `workflow_runs.rs`, `projects.rs`; the shared REST client in
`libs/rust/connectors/src/github/client.rs` gains `send_effect`/`execute_effect` and the
`Conflict` and `UnknownOutcome` failure codes.

| SDK tool | Rust | Answer | Notes |
|---|---|---|---|
| `set_active_branch` | effect | `Active branch set to 'x'` / `Branch 'x' not found in repository` | Local state only (the branch the reads default to, for this invocation); classified as an effect like GitLab's. An external `repo_name` is refused as in the SDK. |
| `create_branch` | effect | `Branch 'x' created successfully, and set as current active branch.` | From the active branch. A taken name gets the first free `_vN` found from `git/matching-refs`, not up to 1000 one-request tries; a lost race moves to the next. |
| `delete_branch` | effect | the SDK's texts | `main`, `master`, the base branch (case-insensitive) and, without `force`, the active branch are refused before any request; a forced delete of the active branch resets it to the base. |
| `create_file` | effect | `Created file p` | Never replaces: an existing file (or a directory) answers `File p already exists. Use update_file instead.` and nothing is sent. `filepath` copies the bytes of a platform object through the Office host (8 MiB cap). The base branch is protected. |
| `update_file` | effect | `Updated file p` | Reads the file with its sha, applies the OLD/NEW pairs (all or nothing, the family's shared editor), writes with that sha. A file that changed meanwhile is a conflict, not a lost update. |
| `delete_file` | effect | `Deleted file p` | Sends the sha it read. A directory is refused. |
| `create_pull_request` | effect | `Pull request created successfully! PR #n, URL: u` | `base` defaults to the base branch. GitHub's own reason (for example "A pull request already exists") rides in the failure. |
| `create_issue` / `update_issue` / `comment_on_issue` | effect | the SDK's texts | `update_issue` is ONE request; labels and assignees replace the current ones when given, an explicit `[]` clears them (the answer says so), and leaving them out keeps them. |
| `trigger_workflow` | effect | the SDK's result object | Resolves the workflow by id or file name, then dispatches by id. |
| `apply_git_patch` / `apply_git_patch_from_file` | effect | the SDK's result object (`success`, `applied_changes`, `failed_changes`, counts, `message`) | See "Patches". The file form reads the patch from a platform bucket (1 MiB cap). |
| `create_issue_on_project` / `update_issue_on_project` | effect | the SDK's texts | See "Project boards". |
| `get_workflow_logs` | read | the SDK's job-details object | Run, jobs, steps and each job's `logs_url`. The log archive is never downloaded (GitHub answers with a redirect to a storage host the egress guard does not admit); the `note` says so. |
| `search_project_issues` | read | the project snapshot of `list_project_issues` plus item bodies | The pinned SDK declares no `search_query` variable in its query, so it returns the first items unfiltered. This applies GitHub's native `items(query:)` filter. |

Not ported: `generic_github_api_call`. It calls any method of a `PyGithub` object by name and
returns `raw_data`; the contract is the Python library's surface, so a port would be a different
tool under the same name. The same reasoning is on record for BigQuery's `execute`.

### Classification and confirmation

Every effect has `is_read_only() == false` and `is_concurrency_safe() == false`; the reads keep
both true. That is the only classification the family does: whether an effect needs a person's
approval is the operator's `toolkit_security.sensitive_tools` list, matched by name, exactly as
for GitLab, Bitbucket and Azure DevOps Repos (`ToolAdmissionPolicy::sensitive_tool`). The direct
path runs an effect once under the node-recovery journal (`direct-tool-effects-design.md`); a
call whose answer is lost is recorded as an uncertain effect and is never re-dispatched.

### Failures

A failure is the tool's RESULT, one sentence per cause, because the admission wrapper redacts a
tool's `AdkError` to its category (`invocation::sanitize_tool_error`), which would make a 403 and
a 409 read the same. The cause comes from the status:

| GitHub | Sentence says |
|---|---|
| 401 | credentials rejected; do not retry |
| 403 | refused: no write permission or protected branch; nothing changed |
| 403/429 with a rate-limit header | rate limited; nothing changed; retry later |
| 404 | repository, branch, file, issue or workflow not found, or not visible |
| 409 | conflict: target changed or exists; read again; nothing changed |
| 400/422 | rejected as invalid, with GitHub's own `message` and `errors[].message` (bounded) |
| 5xx, timeout, lost body | UNKNOWN OUTCOME: sent, answer lost; may or may not have been made; check before retrying |
| no connection, egress refused | nothing was sent |
| GitHub App credentials | cannot write; use a token or username and password |

### Patches

The diff reader follows the SDK's (`_parse_git_patch`); four places differ because the SDK's
reading of a valid diff corrupts the file: hunks are checked against the file (the SDK applies
them blind, so a patch made for other content deletes and inserts at the wrong lines), a blank
line inside a hunk is an empty context line, `\ No newline at end of file` marks the previous
line instead of ending the hunk and dropping what follows, and a created file ends with a
newline unless the diff says otherwise.

`apply_git_patch` reads and checks EVERY file first and writes nothing if any file does not
match (`success: false`, `nothing was changed`). It then commits file by file, in patch order
(the SDK's one commit per file, with its `<message> - Create|Update|Delete|Rename <path>`
messages). If a write fails the rest are not tried and the answer lists `Failed to apply ...` and
`Not attempted: ...`; a rename creates the new file before it removes the old one, so a failure
leaves a copy, never a loss. The SDK applied the files that fit and listed the rest; this one
does not write anything to a branch it knows the patch does not fit.

### Project boards

`create_issue_on_project` is three requests GitHub cannot make atomic: a draft item on the board,
its conversion to an issue, the field values. Every document is fixed text with variables (the SDK
interpolates ids and values into mutation strings). If the conversion fails the answer names the
draft item that is left on the board. If a field update fails the answer lists what was set, the
field that failed with its sentence, and the fields not tried (the SDK stops at the first failed
field and reports its error text as the list of updated fields). A date that cannot be read
(`YYYY-MM-DD`, optionally `THH:MM[:SS]`) is reported as an invalid field value; the SDK's
`dateutil` fallback writes the current time. The board is looked up among the repository's first
20 projects (the SDK's 10) by fields only; the issue is looked up DIRECTLY by repository and number
(`repository(owner,name){issue(number){projectItems}}`, one request, then the item of this project;
the same number can exist in two repositories). `issue_repo` names the repository, default the
board's; if it is not there and `issue_repo` was absent the answer says so and asks for it; the board
is never scanned. The lookups are reads (a failure is an outage, nothing changed);
a mutation's error is a definite refusal only when every error is typed (not found, no permission,
rate limit, unprocessable) and no data came with it; untyped errors, other types, `INTERNAL`,
partial data, a lost answer or a 5xx are an unknown outcome. A
conversion whose answer is lost says it may have applied and to check the board before retrying.

### Deliberate differences, in one place

1. `update_issue` leaves labels and assignees alone when they are not passed (owner decision). The
   SDK's schema text says an empty list removes them, but its code removes them whenever they are
   absent, so closing an issue stripped them. An explicit `[]` still clears (the answer says so).
   With no field given at all it is refused instead of sending an empty PATCH.
2. No silent overwrite: `create_file` never replaces; `update_file`, `delete_file` and the patch
   send the sha they read.
3. `create_branch` finds a free name from the matching refs. A branch, tag or ref name, and every
   path in a patch, with a `.`, `..` or empty segment is refused before any request (it would
   address another ref or file in the URL). Integer arguments are read as pydantic reads them
   (`"5"` is 5; a non-number is "must be a number"). The GitHub HTTP client is built without
   protocol-level retries (`CLIENT_POLICY.disable_retries`), as ado, bitbucket and gitlab are; a
   retried POST is a second effect. So is the Office host's (`OFFICE_POLICY`), which carries object
   writes and bucket creation. A full ref (`refs/heads/x`) is refused where a branch name is
   expected. A CRLF patch applies (the CR is not part of a path), and a patch that touches one path
   twice (two diffs, a rename whose target is modified) is refused whole.
4. `apply_git_patch` is checked before it writes (above).
5. `get_workflow_logs` returns job details only; `search_project_issues` filters.
6. Active-branch state lives on the invocation's client, as the SDK's lives on its wrapper.
7. Texts the SDK returns as strings for failures of GitHub itself (`Failed to create pull request:
   ...`) are the per-cause sentences above.

## Artifact

Code: `libs/rust/agent-runtime/src/toolkits/families/artifact/extra_tools.rs` (new), `tools.rs`;
`OfficeToolHost::create_bucket` (`families/office_host.rs`) and its worker implementation
`services/elitea-worker-rust/src/transport/office_host.rs`; the shared line-edit helper
`families/gitlab_org/edit.rs::apply_pairs`.

| SDK tool | Rust | Notes |
|---|---|---|
| `read_multiple_files` | read | Files in order, each a string, the `content_too_large` object for a file over the per-file cap, or `Error reading file: <cause>`; a cumulative 200,000-character budget, after which the rest are skipped with the SDK's notice and not fetched. A leading `/` names the bucket (`/bucket/file`). At most 32 files. |
| `grep_file` | read | The artifact wrapper's `Found N match(es) in p:` layout (not the repository families'), case-insensitive; an invalid regular expression finds nothing, as in the SDK. Stops at 2,000 matches or 200,000 characters and says how many it did not show. |
| `edit_file` | effect | OLD/NEW pairs applied one by one with the SDK's exact and tolerant matching; a pair that does not match or matches twice is reported and the rest still go in (`Successfully applied N edit(s) ... WARNING: ...`); nothing is written when none applies. Writes the whole file back (65,536-character write cap; the real size is in the refusal). It lists the file's size and modified time before reading it and again just before writing (two small listings, one full read) and refuses if either changed or the file appeared or vanished; with no modified time it reads the file again and compares content. The platform's text routes carry no version or conditional write, so the race after the last check remains, and a same-size rewrite inside one timestamp tick goes unseen by the marker. |
| `append_data` | effect | `\n` + data after non-empty content; answers the SDK's JSON text. A missing file is created only when the platform says it does not exist: the SDK creates it on ANY failed download, so a refused bucket or an outage overwrote the file. `.docx` (HTML/Markdown append) and other binary types are refused with a sentence. Same guard as `edit_file` (a file that appeared or changed meanwhile is not overwritten). |
| `get_file_metadata` | read | The SDK's `file_metadata` object (type, extension, size, `read_limits`, `instruction_for_readFile`, line count and hints for text); per-type structure (sheets, pages, images) is not available and the `notes` field says so. Errors are the SDK's `error` object. |
| `create_new_bucket` | effect | Name sanitized as the SDK does (`_` to `-`, lower case); `expiration_measure`/`expiration_value` become `retention_days` (days 1, weeks 7, months 30, years 365). Created through the platform's bucket API (`POST /api/v2/artifacts/buckets/{project}`) under the turn's own credential, by the same Office host that moves bytes for `pptx`. A host without one (the desktop) answers that it cannot create buckets. |

Failures are results with one sentence per cause (refused bucket, no such file, content refused,
over the write cap, platform unreachable). For an effect, "platform unreachable" says it is not
known whether the change was made; the first four tools' write and delete now say the same.

Not served: the six index tools (the index toolset serves five).

## Gaps

* `generic_github_api_call` (above).
* The GitHub log archive text; `get_workflow_logs` has job details only.
* `edit_file`, `grep_file` and `append_data` work on the 200,000-character read cap and the
  65,536-character write cap of the platform's text routes; the SDK reads whole files for editing.
* The artifact `read_file`-style structure (sheets, pages, embedded images) is not in
  `get_file_metadata`.
* Live proof: nothing here has run against github.com or a live platform. The bucket route is
  reached with the same credential the object routes use; whether the artifact permission model
  lets that credential create buckets is unproven until a live run.

## Tests

`libs/rust/agent-runtime/src/toolkits/github_writes_tests.rs` (a scripted fake GitHub behind the
real client and request builder; a request the script does not expect fails the test),
`artifact_extra_tests.rs` (an in-memory bucket behind `PlatformWriter`), `github_tests.rs` (the SDK
schema gate over all 37 tools), `families/github/patch.rs` and `project_writes.rs` unit tests, and
the worker's `transport/office_host_tests.rs` (bucket creation, its causes, an invalid name never
sent) and `toolkits/artifact_tests.rs`.
