# Mobile: an agent's avatar pack in chat (#1835, ADR-0050 decision 6)

No operator action. No migration, no new route, no new setting.

- Client contract 1.46 (additive): `getAgentMedia`
  (`GET /api/v2/agent_media/{project}/{sha256}.{ext}`) is tagged `client`. It
  stays root-mounted with no credential, like `/icons`, so the X-Client-Version
  gate never judges it and it never answers 426. The description of
  `persona.avatar_pack` on a participant now says what #1619 already serves
  there: the version's pack as an object. The lock keeps the property typed as a
  string, because a type change would break the 1.x contract.
- The mobile app shows an agent's avatar pack in a chat: the still, and a state's
  own still image in place of the default motion. It reads the pack only from a
  workspace whose discovery says `client_contract` 1.46 or later; on an older
  one it shows `meta.icon_meta` as before. Pack media is downloaded with no
  credential from the workspace origin only, redirects refused, at most 2 MiB.
