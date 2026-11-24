# Import a set of agents as a team; imports link their toolkits (#3010)

**What changed.**

- New route `POST /api/v2/elitea_core/team_import/prompt_lib/{project_id}`
  (operation `importTeam`). The body is the import document
  (`applications`, `toolkits`, `skills`) plus `team`: `name`, `description`,
  `floor_policy`, `lead` and `members`, the agents named by their
  `import_uuid`. One transaction creates every agent, version, toolkit,
  skill and link, then the saved team; any failure keeps nothing. Before it
  writes, it refuses a taken agent name (`409 agent_name_taken`, compared
  without case), a taken team name (`409 team_name_taken`), a version tool
  reference to a toolkit the document does not carry (`422 toolkit_missing`)
  and an invalid team (`422 team_invalid`). A version tool reference to a
  sub-agent (a toolkit of type `application`) whose agent is not in the
  document is refused as `toolkit_missing` too. Agent names are compared as
  they will be stored (cut to the name limit). Team imports of one project
  run one at a time (a transaction-scoped advisory lock), so two cannot both
  pass the name check. It needs both
  `models.applications.export_import.import` and `models.teams.create`.
- The import route (`import_wizard` and `export_import` POST) now imports the
  top-level `toolkits` and `skills` arrays of an `{applications, ...}` body,
  the shape the export writes and the web Import sends. They were ignored, so
  every imported agent with a toolkit answered `207` "unable to link tools".
  Such an import now answers `201` and links the toolkit. A bare entity array
  is read as before.
- `GET .../export_import/prompt_lib/{project}/{id}?follow_version_ids=<ids>`
  now keeps only the named versions without `fork=true` too (every named
  version, in version order; `404` when none matches). It used to be
  honoured only for a fork, so Export of the open version wrote every
  version. A value with no numeric id keeps every version, as before.
- Web: Workshop › Agents › Import reads a zip with a `team.yaml` as a team,
  and the Teams list has **Export** on each row (the same zip). Desktop ships
  the web bundle, so it has both. Mobile has no Workshop import.

**Who is affected.** Clients that posted `{applications, toolkits}` and
relied on the toolkits being ignored now get them imported. Clients that
exported with `follow_version_ids` and no `fork` now get one version.

**Rollback.** The previous release has no team route, ignores the top-level
toolkits and skills again, and exports every version. Teams and agents made
by a team import stay; nothing is migrated.
