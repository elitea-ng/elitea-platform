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
| `write_file`, `edit_file`, `apply_patch` inside the workspace | allowed: the turn's checkpoint undoes them. `path_deny`, `.git` and paths outside the workspace are refused before any rule |
| `git_commit` | allowed: denied paths (`path_deny`, credentials, the desktop app's data) are never committed: left out of the given paths, and a commit of what is staged is refused, naming them, while the index holds one |
| `run_command`, `read-only` or `workspace-write` sandbox, no network, not destructive | allowed, compound commands included (`cargo test 2>&1 \| tee target/log`, `npm ci && npm test`) |
| `run_command` that is destructive (below), asks for the network or `full-access`, or would run unconfined: its sandbox cannot hide what it denies (no sandbox on a host that allows unenforced ones, Landlock alone, a bubblewrap walk cut at its cap; decided per command, from the sandbox chosen for it) | asked |

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

## Output budgets and spill files

Every result stays within a per-call budget of estimated tokens
(`ceil(bytes / 4)`; `budget::Budgets`, which the host may set in
`SessionConfig::budgets`):

| Tool | Default | Over budget |
|---|---|---|
| `read_file`, `read_output` | 12k tokens | stops at the last whole line; `truncated`, `next_offset`. A single line longer than a page is cut and continues at `next_offset` plus `next_column` (a byte offset in the line, passed back as `column`), so every byte pages |
| `run_command` | 6k tokens, stdout and stderr together | each stream keeps its head and tail; the whole output (up to 4 MiB per stream) is spilled: `output_id`, `stdout_bytes`, `stderr_bytes` |
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
