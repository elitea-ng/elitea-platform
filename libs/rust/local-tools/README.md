# elitea-local-tools

The desktop host's local tool family (ADR-0029 decision 4). This page holds
the approval defaults; the rustdoc of `approvals` and `classify` is the
reference.

## Approval defaults

The owner's decision: **do not ask for approval if it's not very
destructive.**

| Call | Default |
|---|---|
| `read_file`, `list_tree`, `search_files`, `find_files`, `read_output`, `read_document`, git reads | allowed |
| `write_file`, `edit_file`, `multi_edit`, `apply_patch` inside the workspace | allowed: the turn's checkpoint undoes them. `path_deny`, `.git` and paths outside the workspace are refused before any rule |
| `git_commit` | allowed: denied paths (`path_deny`, credentials, the desktop app's data) are never committed: left out of the given paths, and a commit of what is staged is refused, naming them, while the index holds one |
| `run_command`, `read-only` or `workspace-write` sandbox, no network, not destructive | allowed, compound commands included (`cargo test 2>&1 \| tee target/log`, `npm ci && npm test`) |
| `run_command` that is destructive (below), asks for the network or `full-access`, or would run unconfined: its sandbox cannot hide what it denies (no sandbox on a host that allows unenforced ones, Landlock alone, a bubblewrap walk cut at its cap; decided per command, from the sandbox chosen for it) | asked |
| `run_tests`, network off | as `run_command` for the test command it runs: allowed unless that command is destructive or would run unconfined |
| `run_tests` with `network: true` | asked, the first time per runner kind and workspace: "always allow" is remembered for the runner kind (`cargo`, `go`, `vitest`, …, whatever the filter), within the sandbox and network it was made for, never for a command that is destructive with the network off |

## Tests and builds (`run_tests`)

`project::detect` reads the workspace's root, its first-level folders and
the folders the turn mentions (marker files only, through the confined
`Workspace`: a denied or symlinked path is never read, and no project code
runs) and names a test command and a build check per runner: cargo
(`cargo test` / `cargo check`), go (`go test ./...` / `go vet ./...`),
vitest and jest (through the package manager the lockfile names), pytest,
Maven (`./mvnw` when present), Gradle (`./gradlew` when present), or the
package's own `test` script (mocha, ava, `node --test`, …; its output is
read as text). The profile is cached per session and
re-read when a marker file changes; the desktop's environment preamble
shows it as its `Project:` line (only when the policy allows the shell:
without it the agent cannot run those commands).

`run_tests {command?, filter?, cwd?, timeout_seconds? (1..1800), network?}`
runs the detected command for `cwd` with the runner's structured output
switched on (`cargo --message-format=json`, `go test -json`, vitest
`--reporter=json`, jest `--json`, pytest `--junitxml` into the session's
`TMPDIR`; Maven and Gradle reports are read from the report folders of the
run's folder and of its modules up to three levels down, only those this
run wrote). A `cwd` inside a member of the root's runner narrows the run
to it rather than running the whole workspace: cargo `-p <crate>`, go in
that folder, vitest, jest and pytest with the folder as a path, Maven
`-pl`, Gradle `-p`. A given `command` must be one runner invocation; without
`cwd` it runs in the detected project's folder when exactly one runner of
its kind was found, else at the root. Maven and Gradle commands may run
only building and testing goals and tasks (every one of them: `mvn test
deploy` and `gradle test publish` are refused), and a package manager only
the scripts `test`, `test:<name>`, `check`, `typecheck` and `lint` that the
folder's `package.json` declares (refused when the script, or its
`pre`/`post` script, publishes or deploys). A `filter` is refused for a
command that runs no tests (a build check, `go vet`, `npm run lint`). It
runs exactly as `run_command` runs a command: the OS sandbox in
`workspace-write`, the network off unless asked, the deny list, the same
rules and checkpoint. Go's, Gradle's and Maven's caches (`GOCACHE`,
`GRADLE_USER_HOME`, `-Dmaven.repo.local` in `MAVEN_OPTS`) default to one
cache root kept across sessions, `<data_dir>/build-cache/{go,gradle,m2}`
(writable in the sandbox, which cannot write the home's), unless the host
passes its own. A cargo run's artifact records are left out of the
captured output, so that they cannot push the test results out of it. The
answer:

```json
{"kind": "cargo", "command": "cargo test --message-format=json", "cwd": ".",
 "exit_code": 101, "passed": 41, "failed": 1, "skipped": 0,
 "failures": [{"name": "tests::breaks", "file": "src/lib.rs", "line": 14,
               "message": "assertion `left == right` failed …"}],
 "output_id": "out-…", "truncated": false}
```

A build that fails answers `build_failed: true` and `errors` (the
compiler's messages at their file and line) instead. Output no parser
recognises comes back as its head and tail (`parsed: false`). An offline
run that failed the way a dependency fetch fails answers
`needs_network: true` and a hint to retry with `network: true`. The full
log pages with `read_output` (`output_id`; a pytest junit report as
`report_id`). Messages are runner output, framed as tool output, never
instructions. The cloud executor offers the same tool and schema; both
drop it with `run_command` when the policy turns the shell off, and plan
mode refuses it.

## The session's deny list

One list per session (`deny::DenyList`), built when it opens: the
credentials under the home, the host app's directories (those it resolved,
and the defaults under the identifier it passes), and the session's data
directory. Each entry is denied as given and as resolved (a symlinked
`~/.config` included), without regard to case where the file system folds
it. Every tool reads it: the OS sandbox of each command, the host's git,
`git_diff` and `git_commit`, and the in-process file tools, walkers and the
local index (`read_file`, `read_document`, `list_tree`, `search_files`,
`find_files` refuse or leave out what it covers, as for `path_deny`, wherever the
workspace sits).

## Finding and searching

* `find_files` lists files by name: a glob (`src/**/*.rs`; a glob without
  `/` matches the file name at any depth, one with `/` the path under
  `path` or from the workspace root; a leading `!` keeps the files that do
  not match), newest first, or with `fuzzy: true` a name fragment ranked
  by nucleo's path scorer (`nucleo-matcher`, MPL-2.0, linked unmodified).
  Names only: nothing is opened. Its glob filters what the walk yields, so
  `.gitignore` always wins over it.
* `search_files` searches text like `rg`: `output_mode` `content` (lines,
  with `context` / `context_before` / `context_after` up to 20 lines,
  each entry `{path, line, text, kind: match|context}`), `files` (paths)
  or `count` (matches per file); `multiline` lets a pattern span lines,
  `max_per_file` caps one file, and one file stops at the call's
  `max_results`. Files are searched a batch at a time on several threads
  but taken in the serial walker's (path) order up to the limit, so the
  answer, a truncated one too, does not depend on thread timing. Its
  `glob` is an override as `rg -g` takes it (unchanged from before
  `find_files`): rooted at the workspace root, `!` excludes, and it can
  name a gitignored file the walk reaches.
* Both walk as `list_tree` does (`files::walker`, which the desktop's
  local index lists with, no glob): `.gitignore` honoured, `.git`, `path_deny` and the
  session's deny list pruned in the walker's `filter_entry`, never
  filtered after, and symlinks never followed. `search_files` opens each
  file through the workspace (`O_NOFOLLOW`). The desktop index's
  `search_workspace` is a ranked search over the index; these are exact
  name and text matches over the folder as it is now.

## Edits

Every edit goes through the workspace (`O_NOFOLLOW`, temp file and
`renameat`) and the read ledger: a file must have been read, and one
changed since is refused, whatever the format.

* `apply_patch` takes a unified diff (`git diff`) or the Codex envelope
  GPT-5 and codex-family models write (`*** Begin Patch`, `*** Add File:`,
  `*** Update File:` with `@@ <anchor>` chunks, `*** Move to:`,
  `*** Delete File:`, `*** End of File`, `*** End Patch`; a heredoc around
  it is unwrapped). Envelope paths are workspace-relative: an absolute
  path, `~` or any `..` step (even one that resolves back inside, like
  `src/../secrets/x`) is refused, and `path_deny`, the deny list and `.git`
  are checked for every file before anything is written. An add over an
  existing file needs it read, as `write_file` does; a move onto an
  existing file is refused. Every file applies or none does.
* Chunks are located with Codex's `seek_sequence` ladder: exact, then
  trailing whitespace ignored (`trim_end`), then both ends (`trim`), then
  typographic dashes, quotes and spaces read as ASCII (`unicode`). The
  result's `files[].matches` names the pass per chunk and a `note` counts
  the fuzzy ones: a fuzzy match is never silent, and a fuzzy pass that
  finds more than one position fails with the candidates' line numbers.
* A unified diff's hunk is placed exactly first, searching outward from
  the header's line numbers; only when its context matches exactly
  nowhere is the same fuzzy ladder tried (after the previous hunk), with
  the same uniqueness rule and the same report.
* A chunk with only `+` lines goes right after its `@@ <anchor>` line;
  without an anchor it appends at the end of the file (Codex's rule).
  Only `\n` and `\r\n` end a line (a lone `\r` is content). Unchanged
  lines keep their endings; a file's final line without a newline stays
  so unless the patch replaces it or adds lines after it.
* Line endings for new lines, in every format: the nearest replaced line's
  ending where there is one, else the file's majority ending (CRLF only
  when more lines end in `\r\n` than `\n`; a tie or none is LF).

* `multi_edit` takes `{path, edits: [{old_string, new_string,
  replace_all?}]}` (1 to 50): every `old_string` is matched against the
  original file, must be unique unless `replace_all`, and must not overlap
  another; a failure names the edit (`edits[2]: …`, `edits[0] and
  edits[3] overlap`) and nothing is written. One freshness check, one
  atomic write. Same approval row as `edit_file` (`ToolKind::EditFile`).
* `edit_file` and `multi_edit` match exactly first; when an `old_string`
  is nowhere exactly, a whitespace-normalised match (CRLF read as LF,
  trailing whitespace ignored, one uniform indentation shift, whole lines
  only) is accepted only when unique (or with `replace_all`), re-indents
  `new_string` by the same shift (refused, rather than flattened, when a
  `new_string` line has less indentation than a shift removes) and is
  reported (`match` /
  `matches[]`: `exact` or `normalised`). A replacement takes the line
  ending of the text it replaces.
* `edit_file`, `multi_edit` and `apply_patch` results carry `snippet`: the
  new text 3 lines around each change, numbered like `read_file`, gaps
  marked `...` (per file, `path:` first, for `apply_patch`), at most 4 KiB
  per call (`snippet_truncated: true` when cut).

* The edit tools a turn is offered follow the model family
  (`EditProfile`, in `elitea-agent-runtime`'s `edit_profile` so the cloud
  executor shares it): `StrReplace` (the default; Claude, Gemini, Qwen,
  others) offers `write_file`, `edit_file`, `multi_edit` and
  `apply_patch` described as a unified diff first; `Patch` (`gpt-5*`,
  `*codex*`, `o3*`, `o4*`) offers `write_file` and `apply_patch` described
  as the Codex envelope. The agent's `meta.edit_format` (`auto`,
  `str_replace`, `patch`) overrides it. `session::tools_for(profile)` and
  the executor's `workspace_tools::workspace_tools_for(profile)` give the
  same names, descriptions and schemas per profile (the workspace
  runner's contract test checks both); the host sets it per turn with
  `LocalSession::set_edit_profile`.

The envelope parser and the ladder are adapted from OpenAI Codex
(Apache-2.0); see `NOTICE`.

## Output budgets and spill files

Every result stays within a per-call budget of estimated tokens
(`ceil(bytes / 4)`; `budget::Budgets`, which the host may set in
`SessionConfig::budgets`):

| Tool | Default | Over budget |
|---|---|---|
| `read_file`, `read_output` | 12k tokens | stops at the last whole line; `truncated`, `next_offset`. A single line longer than a page is cut and continues at `next_offset` plus `next_column` (a byte offset in the line, passed back as `column`), so every byte pages |
| `run_command` | 6k tokens, stdout and stderr together | each stream keeps its head and tail; the whole output (up to 4 MiB per stream) is spilled: `output_id`, `stdout_bytes`, `stderr_bytes` |
| `run_tests` | 6k tokens (the `run_command` budget) | counts always; at most 20 failures (or compiler errors), each message at most 2 KiB, first come first kept within the budget, the rest counted in `more_failures` / `more_errors`; the whole log (stdout, then stderr) always spilled: `output_id` |
| `search_files`, `find_files` | 8k tokens | stops early; `truncated` |
| `git_diff`, `git_log` | 8k tokens | the head; the whole output spilled: `output_id` |
| `read_document` | 12k tokens | the head; the whole text spilled: `output_id` |

Spill files live under the host's data directory,
`<data_dir>/sessions/<session id>/out/<conversation>/<output_id>.txt`
with a line index beside each (`<output_id>.idx`, so `read_output` reads
only the page it shows). The data directory is on the session's deny list,
so a workspace that contains it (a home folder) still spills and the file
tools still cannot read, list, find or search the spill files. Each
directory is `0700`, each file created `0600` with
`O_CREAT | O_EXCL | O_NOFOLLOW`; a directory or file that is a symlink,
another user's, or open to group or others is refused.

Outputs belong to a conversation (`LocalSession::set_conversation`, the
directory a digest of its id): `read_output` takes only an id (`out-` and
16 hex digits) the calling conversation was given, never a path. A
conversation keeps at most 64 MiB and a session 256 MiB, the oldest
dropped first (a conversation over its own cap drops its own oldest). They
outlive a session reopen (a policy change) and an app restart for 24
hours, then go (swept when a session opens and on every spill);
`budget::forget_conversation_outputs` removes a conversation's at once
(the desktop calls it when a thread's local history is deleted). An id
that is gone answers `local_tools.output_expired`: re-run the tool. Spill
content comes from tools that already honour `path_deny` and the deny
list.

## What counts as destructive

Every segment of a command is classified, including those inside `&&`,
`|`, `;`, `$(…)`, backticks, `<(…)`, `sh -c`/`bash -c`, `eval` and
wrappers such as `env`, `timeout` and `xargs`.

| Class | Asked | Not asked |
|---|---|---|
| Deletes | `rm -r`/`-R`/`--recursive`, `rm` with a glob, more than 10 paths or none (`xargs rm`), `find … -delete`, `find … -exec rm`, `rmdir -p`, `git clean` (not `-n`), `shred`, `truncate` | `rm file.txt`, `rmdir dir`, `unlink f` |
| Git history and discards | `push` (any), `reset --hard`/`--merge`, `checkout -- <paths>`, `checkout .`, `checkout -f`, `restore` of the working tree, `switch -f`, `rebase`, `filter-branch`, `filter-repo`, `branch -D`/`-M`/`-f`, `tag -d`/`-f`, `stash drop`/`clear`, `reflog expire`, `gc --prune…`, `prune`, `update-ref -d`, `git -c alias.…` | `status`, `diff`, `log`, `add`, `commit`, `fetch`, `checkout -b`, `restore --staged`, `stash`, `branch -d` |
| Publish and deploy | `npm`/`pnpm`/`yarn`/`bun publish`, `cargo publish`, `twine upload`, `gem push`, `docker`/`podman` other than read verbs and `build`, `kubectl`, `helm`, `terraform`/`tofu`, `pulumi`, `gh` other than read or plan verbs; every `aws`, `gcloud`, `az`, `doctl`, `fly`, `vercel`, `netlify`, `heroku`, `firebase`, `wrangler`, `rclone` | `kubectl get`, `helm template`, `terraform plan`, `pulumi preview`, `gh pr view`, `docker build` |
| Dependencies | `npm`/`pnpm`/`yarn`/`bun install`/`add`/`update`, `-g`, `cargo install`/`add`, `pip install`, `uv add`/`sync`, `poetry add`, `gem install`, `go install`/`get`, `brew`/`apt`/`dnf` other than read verbs | `npm ci`, `npm test`, `cargo build`, `go build` |
| Privilege and system | `sudo`, `su`, `doas`, `pkexec`, `chmod`/`chown -R`, `dd`, `mkfs*`, `diskutil`, `fdisk`, `parted`, `mount`, `kill -9`, `killall`, `pkill`, `launchctl`, `systemctl`, `crontab` other than `-l`, `osascript` | `kill 1234`, `chmod +x script.sh` |
| Shell constructs | `sh -c`/`eval` whose script is destructive or unreadable, a download piped or substituted into a shell (`curl … \| sh`, `bash <(curl …)`), a redirect or `tee` writing outside the workspace (`echo hi > /etc/x`) | `ls > out.txt`, `echo "$(git rev-parse HEAD)" > rev.txt` |

## What tightens the defaults

In order, each beating everything below it:

1. **Policy** (`local_work`): local work or the shell off, a sandbox wider
   than `max_sandbox_mode`, network the policy denies, a command outside a
   non-empty `command_allow`, a `command_deny` match, a `path_deny` path:
   denied. `command_deny` is **advisory**, not a security boundary: it sees
   what the model wrote, not what a script, build tool or interpreter
   starts. The sandbox and the approval confine commands.
2. **Plan mode**: read-only tools only.
3. **Workspace rules** (kept in the host's data directory, never in the
   workspace): deny beats ask beats allow. A deny or an ask matches any
   segment and any path; an allow matches the command itself and every
   path, and vouches for a compound command only when it is not
   destructive.
4. **Remembered choices** (`approve_always`): a command choice is the
   resolved program plus an argv prefix, for the sandbox mode and network
   setting it was made with (never a wider one); a file choice is the exact
   paths, or a glob the person confirmed, never the whole tool. Compound
   commands and commits are never remembered.
