# GitHub: `get_workflow_runs` (issue 2265)

**What changed.** The GitHub toolkit gained the SDK's `get_workflow_runs` tool (category `read`): recent
GitHub Actions runs, newest first, optionally of one workflow (an ID or a file name such as `build.yml`),
filtered by branch, status or conclusion and event, up to 100 (10 by default), of the toolkit's repository
or one named with `repo_name`. Each run is a compact object with its ID, workflow, number, event, status,
conclusion, times, branch, commit and URL. The toolkit editor lists the tool for the `github` type. A missing workflow or repository answers the SDK's `{error, message}` object; a run with an unreadable field is left out and counted in `skipped_runs`.

**Who is affected.** Agents and pipelines that use a GitHub toolkit; the run ID it returns is the input of
`get_workflow_status` and `get_workflow_logs`.

**What to do.** Nothing. Roll the Worker together with Main as usual.
