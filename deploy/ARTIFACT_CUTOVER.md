# Artifact cutover — the current platform's object store to the Go one

Issue #337. This runbook moves the artifact plane of a deployment that still
runs the legacy platform to `elitea-main`. It is a **route change
plus a one-time data copy**, not a configuration switch: the two stores are
different kinds of store, and nothing can read both.

Machines here use **podman**: `podman compose`, not `docker compose`.

## 1. What is actually different about the two stores

| | current platform | `elitea-main` |
| --- | --- | --- |
| engine | Apache libcloud, `LOCAL` driver | S3 / Azure Blob / GCS |
| location | a directory on `pylon_main`'s own disk | an object-store endpoint |
| bucket name on disk | url-safe base64 of `p--<projectID>.<bucket>` | — |
| object name on disk | url-safe base64 of the key | — |
| object address | — | `[<prefix>/]p/<projectID>/b/<bucket>/o/<key>` in ONE container |
| bucket metadata | a row in `StorageMeta`, keyed by the same base64 | a row in the artifacts bucket table |

Sources, so this table can be re-checked rather than believed:
`legacy/plugins/shared/tools/storage_engines/libcloud.py` (the encoder, the
`p--<id>.` prefix and the `StorageMeta` row), the deployed
`configs/pylon_main/shared.yml` (`storage_engine: libcloud`,
`storage_libcloud_driver: LOCAL`, `storage_libcloud_encoder: base64`,
`key: /data/libcloud/storage`), and
`services/elitea-main/internal/infra/storage/ref.go` (`ObjectRef.StorageKey`).

**There is no in-place option.** `internal/infra/storage` has no filesystem
backend, and ADR-0016 §8 records that decision. `storage.ConfigFromEnv` accepts
`s3`, `azure` or `gcs` and refuses anything else with an error rather than a
default, so a deployment cannot be pointed at the old directory by mistake
either.

## 2. What this repository provides

- `deploy/scripts/migrate-artifacts.sh` copies the objects (section 3.2).
- `elitea-main` serves the artifact plane when `ELITEA_ARTIFACTS_ENABLED` is
  `"true"` and the `STORAGE_*` variables name an object store. Every stack in
  this repository (Helm, `deploy/docker-compose.standalone-full.yml`) already
  does.

The mixed `deploy/centry-hybrid` stack that this runbook was first written
against is retired (see `docs/UPGRADING.md`). On your own edge, route both the
browser surface (`/api/v2/artifacts/...`) and the root-mounted
`/artifacts/s3/...` surface the pinned SDK speaks to `elitea-main`, and do not
leave either half falling through to pylon.

## 3. The procedure

### 3.1 Before the flip

1. **Inventory the source.** On the host that runs `pylon_main`, the libcloud
   root is `<centry>/pylon_main/libcloud/storage` (the container path
   `/data/libcloud/storage` under the `./pylon_main:/data` mount).

   ```bash
   deploy/scripts/migrate-artifacts.sh \
     --source <centry>/pylon_main/libcloud/storage \
     --alias target --dry-run
   ```

   `--dry-run` copies nothing. It prints one line per object — the decoded
   source path and the target key — and a count. Read the REFUSED lines: a
   bucket whose name is not `^[a-z][a-z0-9-]{1,62}$` cannot be addressed
   through the Go API at all (`bucketPattern` in `ref.go`), so rename it in the
   current platform before the cutover rather than copying objects nobody can
   then read.

2. **Point `rc` at the target.**

   ```bash
    rc alias set target http://127.0.0.1:9000 elitea <secret>
   ```

   `<secret>` is the secret key of the target object store (`RUSTFS_SECRET_KEY` on a RustFS store). Use the
   published address of your own store if it is not the compose one.

### 3.2 The copy

```bash
deploy/scripts/migrate-artifacts.sh \
  --source <centry>/pylon_main/libcloud/storage \
  --alias target \
  --container elitea-artifacts
```

The script **deletes nothing from the source**. The source tree is the rollback.

It ends with a count comparison — objects walked against objects the target
lists — and exits non-zero when the target holds fewer. A per-object success
line does not answer "are they all there"; that comparison does.

### 3.3 Create the bucket rows

The copy writes OBJECTS. `elitea-main` resolves a bucket through its own
database before it touches the store (`requireS3Bucket`,
`internal/api/v2/artifacts/s3.go`), so an object under a bucket with no row
answers `NoSuchBucket` even though the bytes are present.

Create one row per bucket the dry run listed, through the public API — the same
surface a user would use, which also writes the owner, the retention and the
permissions the row needs:

```bash
curl -sS -X POST "$BASE/api/v2/artifacts/buckets/$PROJECT_ID" \
  -H "Authorization: Bearer $PAT" \
  -H 'Content-Type: application/json' \
  -d '{"name":"reports"}'
```

The migration script deliberately does not do this. It would have to invent
values the API computes.

### 3.4 Flip

Switch the artifact routes on your edge to `elitea-main` and set
`ELITEA_ARTIFACTS_ENABLED: "true"` on it, in one change. Recreate the edge
container if its route file is a bind mount: an editor's atomic replace leaves
a running container attached to the previous inode.

### 3.5 Prove it

1. **The index run.** Start one index run and confirm pylon serves no artifact
   request during it:

   ```bash
   podman compose ... logs --since 10m pylon_main | grep -E '/artifacts/(s3|buckets)' || echo "no artifact request reached pylon"
   ```

   `grep` finding nothing is the pass. Read the log for the run's window, not
   the whole file: an entry from before the flip is not a failure.

2. **The browser round trip.** Upload an artifact in the UI, then download it.
   Both calls must appear in `elitea-main`'s log and in neither pylon log.

## 4. Rollback

**Rollback is both edits, not one.** Setting `ELITEA_ARTIFACTS_ENABLED` back to
`"false"` without reverting `go-artifacts` leaves every artifact path resolving
to a service that composes no object store, which answers 404 on paths that used
to work.

To reverse the cutover:

1. Revert both statements together: set `ELITEA_ARTIFACTS_ENABLED: "false"`
   **and** route the artifact paths back to the legacy platform.
2. Recreate the edge and `elitea-main`.

Objects written to the Go store after the flip do **not** travel back. The
source tree is untouched, so every object that existed before the flip is still
readable through the current platform; the ones created after it are readable
only through `elitea-main`. That is the accepted loss of a rollback, and it is
the reason to rehearse the flip rather than discover it.

## 5. What this does not cover

- **Presigned URLs and multipart uploads** issued before the flip do not survive
  it. They name the old host.
- **`StorageMeta` retention rows** are not copied. Retention is re-expressed by
  the bucket rows created in 3.3; the Go store applies its own lifecycle
  configuration (`configureObjectStoreRetentionLifecycle` at boot).
- **The standalone stack** needs none of this. It has always used the Go
  artifacts.
