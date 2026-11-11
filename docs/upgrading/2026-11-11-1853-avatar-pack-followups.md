# Agents: avatar pack follow-ups: fork, import and export carry packs, an orphan media sweep, and contract 1.49 (#1853, #1854, #1855, #1856, #1881, #1864)

- **What changed.**
  - Fork and import write `application_versions.avatar_pack`. The files the pack names are copied into the
    target project's `agent-media` bucket, and the pack's URLs and `meta.icon_meta` are rewritten to that
    project (#1853). A file that cannot be copied keeps its source URL, and the fork or import still succeeds.
  - Export writes `avatar_pack` per version and embeds the exporting project's files as `avatar_media`
    (`{"<sha256>.<ext>": "<base64>"}`). Import checks each embedded file (its name must be the digest of its
    bytes, and the bytes must pass the same check as an upload). An export file can now be up to about
    18 MiB larger per version (9 slots of 2 MiB).
  - Loop generation makes its 4 to 8 frame edits at once, at most 4 at a time, so a loop takes about as long
    as one image edit (#1854).
  - A new daily scheduled job, `agent.media.orphan.sweep.v1` (03:23), deletes `agent-media` files that no
    version, version revision, release or chat participant names and that are older than 7 days (#1855).
    References are read across every project, because a published or catalogue copy names its source
    project's files. If any project's references cannot be read, the job deletes nothing that day.
  - The participants read gets the avatar pack from the publication query, one round trip fewer (#1856).
  - Client contract **1.49** (`x-elitea-client-contract`): `ConversationParticipantPersona.avatar_pack_v1`,
    an `AgentAvatarPackV1` object. `persona.avatar_pack` is deprecated, and the server no longer sends it
    (#1881).
  - The mobile app plays animated WebP and MP4/WebM avatar states (expo-image, expo-video) and keeps the
    media as files in its caches folder (#1864, #1882, #1883).
- **Who is affected.**
  - Projects whose agents have avatar packs.
  - Web, desktop or mobile builds from contract 1.46 to 1.48 read the pack from `persona.avatar_pack`, so
    against this Main they show the agent's icon (`meta.icon_meta`, which Save keeps equal to the still)
    and the default motion, and lose only the pack's state files. Upgrade the clients with Main.
  - Generated still candidates that were never saved into a pack, and files of removed packs, are deleted
    from 7 days after this release.
- **What to do.**
  - Upgrade Main and the clients. No migration and no configuration change.
  - The mobile app needs a new native build (store or dev client) for expo-image and expo-video; an OTA
    update does not reach an older binary (the runtime version is the native fingerprint).
  - An ingress or proxy body limit in front of the import route must allow the larger export files.
