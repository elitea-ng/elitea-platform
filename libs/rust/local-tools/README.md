# elitea-local-tools

The desktop host's local tool family (ADR-0029 decision 4). This page holds
the approval defaults; the rustdoc of `approvals` and `classify` is the
reference.

## Approval defaults

The owner's decision: **do not ask for approval if it's not very
destructive.**

| Call | Default |
|---|---|
| `read_file`, `list_tree`, `search_files`, `read_document`, git reads | allowed |
| `write_file`, `edit_file`, `apply_patch` inside the workspace | allowed: the turn's checkpoint undoes them. `path_deny`, `.git` and paths outside the workspace are refused before any rule |
| `git_commit` | allowed: denied paths (`path_deny`, credentials, the desktop app's data) are never committed: left out of the given paths, and a commit of what is staged is refused, naming them, while the index holds one |
| `run_command`, `read-only` or `workspace-write` sandbox, no network, not destructive | allowed, compound commands included (`cargo test 2>&1 \| tee target/log`, `npm ci && npm test`) |
| `run_command` that is destructive (below), asks for the network or `full-access`, or may run unconfined (the host allows unenforced sandboxes, or only Landlock is available and the host allows partial enforcement: credentials and the desktop app's data are not hidden there) | asked |

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
