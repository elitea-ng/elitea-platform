# GitLab: merge request tools, issue filters and a structured issue list (issue 2265)

**What changed.** The GitLab toolkit gained the SDK's `list_merge_requests` and `get_merge_request`
(compact merge request metadata without diffs; both `read` category). `get_issues` now takes the SDK's
filters (`state`, `labels`, `author_username`, created and updated windows, `page`, `per_page`,
`fetch_all`) and returns a list of compact issue objects (title, number, state, labels, author, created
and updated time) in place of the text `Found N issues:` followed by a list. `get_issue` comments now
carry `created_at`. The toolkit editor lists the new tools and shows the new `get_issues` arguments.

**Who is affected.** Agents and pipelines that use a GitLab toolkit. A prompt or a pipeline step that
parsed the old `get_issues` text answer sees a JSON list instead; the SDK changed it the same way. An
empty result is now `[]` where it was `No open issues available`.

**What to do.** Nothing for operators. Authors who matched the old text should read the list instead.
