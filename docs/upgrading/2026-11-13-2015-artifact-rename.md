# Rename a file in Artifacts (#2015)

Main has a new operation, `POST /api/v2/artifacts/objects/{projectID}/{bucket}:move`
(`moveObject`), that renames or moves one object inside its bucket. The web and
desktop Artifacts table now has a **Rename** action that calls it.

- **Who is affected:** clients of the artifact API; client builders (additive
  change, client contract 1.54).
- **What to do:** upgrade Main before, or together with, the web and desktop
  apps; an app that calls `:move` against an older Main gets 404/405. No
  migration, no new setting.
- **Behaviour:** body `{"from": "<key>", "to": "<key>", "overwrite": false}`.
  The target name is first claimed in the metadata table (insert-if-absent, so
  two racing moves or uploads cannot both win), the bytes are copied, the target
  row is written with the source's `created_at`, `expires_at` and digest (a
  rename does not restart retention), then the source row and bytes are removed.
  A failure rolls back only what the call created; with `overwrite` the existing
  target is never deleted on failure. A source blob that cannot be deleted after
  its row is gone is logged, not failed. `to`
  follows the upload's key rules and the system-bucket rule; an existing target
  is `409 AlreadyExists` unless `overwrite` is true; a missing source is 404;
  `from == to` is 400. It needs the bucket's write access and BOTH the artifacts
  create and delete permissions. It is audited (`object_moved` audit line) and
  emits an `artifact.moved` event. The metadata row (size accounting) moves with
  the object. Backends with server-side copy (S3, Azure) copy inside the store;
  others stream the bytes through Main, capped at the project's maximum object
  size (413 `TooLarge` above it) and cancelled with the request.
