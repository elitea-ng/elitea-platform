# Drag and drop into project folders, item order, and personal pins (#1628)

Builds on the project folders of #2048 (`2026-11-14-2048-entity-folders.md`);
there is no second folder model.

- **Who is affected:** every deployment (one tenant migration); client builders
  (additive change).
- **What to do:** **run the migrations** (tenant
  `0249_project_folder_item_order_and_pins.sql`), and upgrade Main before, or
  together with, the web and desktop apps. An app that calls the new pin route
  against an older Main gets 404. No new setting, no new permission.
- **Data:** `project_folder_items.position` (integer, default 0): the entity's
  place inside its folder, sorted descending like the folders and the chat
  folders. Existing rows keep 0 and read in entity-id order, as before. A new
  table `project_entity_pins (user_id, entity_kind, entity_id)` holds each
  member's own pins. `centry.social_pins` is not reused: it holds one shared pin
  per project and entity, is owned by the legacy schema and feeds the shared
  `is_pinned` flag of configuration lists, so a private pin there would pin for
  the whole project.
- **API:** `moveEntityToFolder` accepts optional `neighbor_above_id` and
  `neighbor_below_id` (entity ids in the target folder; both absent puts the
  entity at the top, a neighbour outside the folder is a 400, and the server
  respaces the folder when the neighbours are adjacent integers).
  `listEntityFolders` also answers `pinned_ids` (the caller's own pins).
  New `setEntityPin` (`PUT .../{kind}/pins/{entityID}`, body `{"pinned": bool}`).
  The agents and pipelines list accepts `pinned=true`.
- **Permissions:** unchanged for folders and moves (the list's update
  permission). A pin is private and needs only the list's list permission.
- **Audit:** every write under `/api/v2/elitea_core/entity_folders/` is now
  recorded in `centry.audit_events` (the surface is audited), and a move carries
  the entity type, entity id and target folder. Reads are not recorded.
