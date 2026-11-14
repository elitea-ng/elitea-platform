# Folders for the agents, pipelines, skills, toolkits and MCPs lists (#2048)

Main has five new operations under
`/api/v2/elitea_core/entity_folders/prompt_lib/{projectID}/{kind}` (`kind` is
`agent`, `pipeline`, `skill`, `toolkit` or `mcp`): `listEntityFolders`,
`createEntityFolder`, `updateEntityFolder`, `deleteEntityFolder` and
`moveEntityToFolder`. The agents and pipelines list endpoint now also honours
`folder_id` (before, any value was a 400). The web and desktop apps use them.

- **Who is affected:** every deployment (one tenant migration); client builders
  (additive change).
- **What to do:** **run the migrations** (tenant `0245_project_entity_folders.sql`),
  and upgrade Main before, or together with, the web and desktop apps; an app
  that calls the new routes against an older Main gets 404. No new setting, no new
  permission.
- **Data:** two tenant tables, `project_folders` (one row per folder, with its
  `entity_kind`, name, `position`, creator) and `project_folder_items` (one row
  per placed entity, primary key `(entity_kind, entity_id)`, so an entity is in at
  most one folder; a composite foreign key keeps an item in a folder of its own
  kind). Deleting a folder cascades to its item rows only; entities are never
  touched. They are not the chat folders (`chat_conversation_folders`), which stay
  per user.
- **Behaviour:** folders are project-wide, shared with every member. A name is
  unique per list, ignoring case (409 on a clash). Order is by `position`
  descending, a new folder goes one 1,000,000 above the highest; an update with
  `neighbor_above_id` / `neighbor_below_id` wins over `position` and the server
  rebalances when the neighbours are adjacent.
- **Permissions:** the routes reuse each list's own: list to read (agents and
  pipelines `models.applications.applications.list`, skills
  `models.applications.skills.list`, toolkits and MCPs
  `models.applications.tools.list`); create to make a folder (`…applications.create`,
  `…skills.create`, `…tools.create`); update to rename, reorder, delete and move
  (`models.applications.application.update`, `…skills.update`, `…tool.update`).
  Nothing to seed.
- **Not the access folders:** the optional `entity_folders` / `social_folder_items` /
  `folder_access_overrides` tables of a deployment that ran the social plugin are
  a separate, read-only access overlay and are not touched.
