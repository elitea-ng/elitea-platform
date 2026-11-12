# One default-version rule for the web, the desktop host and the agent page (issues 1941, 1568, 1955)

- **What changed.**
  - **`GET application` embeds the default version's details.** `version_details` used to be the first listed
    version's, so an agent page opened with no version in the URL (for example after deleting the open version)
    showed `base` instead of the default. It is now the version `meta.default_version_id` names, else `base`, else the
    first listed. The web editor, the web send and the desktop host read the default by that one rule; the
    `is_default` flag is derived from the same meta and no longer takes precedence in the clients.
  - **Agent teams list: keyset paging.** `GET agent_teams` takes `after_name` and `after_id` (both or neither) as an
    alternative to `offset`. Additive; `offset` still works.
- **Who is affected.** A client that read `version_details` as "the first version" and not as "the default" (the
  bundled clients do not). Nothing to do for operators; no migration.
