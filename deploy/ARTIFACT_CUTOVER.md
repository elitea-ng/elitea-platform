# Artifact cutover — the current platform's object store to the Go one

Issue #337. This runbook moves the artifact plane of the mixed (hybrid)
deployment from the current platform to `elitea-main`. It is a **route change
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

## 2. What this repository already changed

- `deploy/centry-hybrid/pov-compose.yml` sets `ELITEA_ARTIFACTS_ENABLED: "true"`
  and the `STORAGE_*` / `S3_*` variables, and adds `runtime-artifacts` (the
  object store) with `runtime-artifacts-bucket-init` (which creates the
  container).
- `deploy/centry-hybrid/traefik/index-routes.yml` has one artifact router,
  `go-artifacts`, with no `Host` guard, resolving to `elitea-main`. It covers
  both the browser surface (`/api/v2/artifacts/...`) and the root-mounted
  `/artifacts/s3/...` surface the pinned SDK speaks. Before this change the S3
  half went to pylon behind a `Host(elitea-gateway)` guard and the browser half
  fell through `base.yml`'s `PathPrefix("/")` catch-all to pylon.
- `services/elitea-main/tests/deployedge/edge_artifact_owner_test.go` holds the
  two together. It finds artifact routers by their RULES, not by name, and it
  fails when the file declares none — an absent router is a silent fall-through
  to pylon, not an unrouted path.

`deploy/centry-hybrid/compose.sh config` asserts the same pair in the rendered
Compose model and refuses the retired router name.

## 3. The procedure

### 3.1 Before the flip

1. **Inventory the source.** On the host that runs `pylon_main`, the libcloud
   root is `<centry>/pylon_main/libcloud/storage` (the container path
   `/data/libcloud/storage` under the `./pylon_main:/data` mount).

   ```bash
   deploy/centry-hybrid/scripts/migrate-artifacts.sh \
     --source <centry>/pylon_main/libcloud/storage \
     --alias hybrid --dry-run
   ```

   `--dry-run` copies nothing. It prints one line per object — the decoded
   source path and the target key — and a count. Read the REFUSED lines: a
   bucket whose name is not `^[a-z][a-z0-9-]{1,62}$` cannot be addressed
   through the Go API at all (`bucketPattern` in `ref.go`), so rename it in the
   current platform before the cutover rather than copying objects nobody can
   then read.

2. **Point `mc` at the target.**

   ```bash
   mc alias set hybrid http://127.0.0.1:9000 elitea <secret>
   ```

   `<secret>` is `RUSTFS_SECRET_KEY` on the `runtime-artifacts` service. Use the
   published address of your own store if it is not the compose one.

### 3.2 The copy

```bash
deploy/centry-hybrid/scripts/migrate-artifacts.sh \
  --source <centry>/pylon_main/libcloud/storage \
  --alias hybrid \
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

Nothing to flip by hand: the flag and the routes are in the tree. Bring the
stack up and the new routing is live.

```bash
deploy/centry-hybrid/compose.sh config   # validates the model, refuses drift
deploy/centry-hybrid/compose.sh up
```

`compose.sh up` force-recreates `auth_gateway` on purpose, because the route
file is a bind mount and an editor's atomic replace leaves a running container
attached to the previous inode.

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
to work. `edge_artifact_owner_test.go` fails on that combination, which is the
point of holding the two statements in one gate.

To reverse the cutover:

1. Revert both files together (`git revert` of the commit that changed them, or
   set `ELITEA_ARTIFACTS_ENABLED: "false"` **and** restore the
   `runtime-worker-current-artifacts` router with `service: current-main`).
2. `deploy/centry-hybrid/compose.sh up`.

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
