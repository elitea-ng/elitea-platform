# Workshop lists: status and origin facets on the agents list (#1627, ADR-0051 M1)

The agents and pipelines list endpoint
(`GET /api/v2/elitea_core/applications/prompt_lib/{projectID}`,
`listApplications`) gains two optional query parameters, `status` (`draft` or
`published`) and `origin` (`own` or `forked`), and its response gains `facets`:
the number of matching items per status and per origin, which ignore the
`status` and `origin` filters themselves and honour every other filter. The web
and desktop apps use them: the status tabs are gone, and Status and Origin are
filters in the list's side panel. The web app also asks for up to 100 items per
list request (it asked for the default 20 before).

- **Who is affected:** every deployment (no migration); client builders
  (additive change, the client contract lock is updated).
- **What to do:** upgrade Main before, or together with, the web and desktop
  apps. A new web app against an older Main shows no Status or Origin filter
  (there are no `facets`) and an old Main ignores the two parameters. No new
  setting, no new permission.
- **Behaviour:** an item's status is `published` when any of its versions is,
  otherwise `draft`; an item is `forked` when its first version records the
  fork's source. Old `/agents/drafts` and `/agents/published` links open the
  list with that status filter on.
