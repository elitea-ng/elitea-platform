# Object Storage Migration — Staged Implementation Plan

**Normative decision:** `elitea-docs/docs/internal/03-architecture/adrs/adr-0016-object-storage-architecture.mdx`
**Gap analysis and target contract:** `elitea-docs/docs/internal/03-architecture/cloud-native-migration/spec-artifact-storage.mdx`

This plan is written to be executed by a coding agent (Sonnet or Haiku class) one
stage at a time. Each stage is independently mergeable, has an explicit file
list, explicit acceptance criteria, and a verification command whose exit code
decides pass or fail.

> Every path, symbol, library API, and test assertion below was verified against
> the tree at commit `de3cdf1` and against the pinned module versions. Where a
> stage touches a file whose *test* pins the current behaviour, that test is
> named in the stage. Do not discover these the hard way.

## Posture: brownfield service, clean break from legacy

**Brownfield** means the constraints that bind are *Go-side*: existing packages,
tests that pin current behaviour, a generated OpenAPI surface with a conformance
gate, and first-party consumers. Every one of those is named in the stage that
touches it.

**Clean break** means legacy Python is read for intent only. Do **not**:

- reproduce a legacy response shape, field name, or serialisation quirk;
- preserve a legacy URL form because legacy accepted it;
- build tooling to read legacy data — no artifact bytes are migrated;
- implement a capability solely because legacy had one. §3.3 of the spec lists
  what is deliberately dropped, including the entire `/artifacts/s3` surface.

Design the API for its consumers. Sizes are integers, not human-formatted
strings. Errors are a typed envelope. Retention is a day count. If you find
yourself porting a Python helper's output format, stop — that is the failure
mode this posture exists to prevent.

**No consumer code is changed by this plan.** The SDK, the web UI, and the
indexer all call endpoints that will no longer exist. Each is a separately
tracked issue, reworked once the new API lands. Do not edit `elitea-sdk`,
`apps/elitea-web`, `apps/elitea-ui`, or the indexer as part of any stage here.

**Horizontal scaling and plane separation are standing constraints, not a
per-stage concern.** ADR-0016's whole decision rests on "any number of
`elitea-main` replicas, no PVC, no node affinity... losing a pod loses no
data and interrupts no subsequent read" — every stage must hold that
invariant, not just the ones that mention it explicitly. Concretely: no
stage may introduce state that lives only in one `elitea-main` process's
memory, or that assumes a request and a later request for the same logical
operation land on the same replica (no sticky routing, no local caches that
are the only copy of something). Any single-use, claim, or "only once"
semantic (a grant's `consumed_at`, a sweeper tick, a lease) must be enforced
by an atomic Postgres operation (`UPDATE ... WHERE <not-yet-claimed>`,
`SELECT ... FOR UPDATE`, an advisory lock) — never an in-process mutex,
`sync.Once`, or local map, all of which silently stop providing the
guarantee they appear to provide the moment a deployment runs more than one
replica, which is the normal case here, not an edge case. This also means a
periodic background job (a scheduler tick, a sweep) must either be
idempotent under concurrent execution by construction, or be started from a
scheduling mechanism that itself guarantees single-instance execution — do
not assume "there's only one process" when reasoning about a job's
correctness. Separately, ADR-0016 depends on and must stay consistent with
ADR-0014 (`adr-0014-control-output-data-plane-separation.mdx`): bulk artifact
bytes belong on the scoped transfer-grant path directly between the caller
and the object store, never proxied through a control-plane RPC/broker path,
and a reference (grant ID, object key) is never itself an authorization —
every read of a reference must re-check a current, unexpired, correctly-
scoped grant against Postgres.

---

## How to execute this plan

1. Read the stage. Read every file in its "Read first" list before editing.
2. Make only the changes the stage lists.
3. Run the stage's **Verify** command. It must exit 0.
4. Run the global gate: `task vet && task lint && task test && task build`.
5. Commit with the stage identifier in the subject, e.g. `feat(storage): S1 object store interface and typed errors`.

### Path bases — read this before touching anything

| Prefix | Relative to |
| --- | --- |
| `internal/`, `cmd/`, `migrations/`, `tests/`, `api/openapi/`, `go.mod` | `services/elitea-main/` |
| `deploy/`, `.github/`, `Taskfile.yml`, `docs/` | repository root |

Verify commands begin with `cd services/elitea-main` where they operate on the
Go module. Grep acceptance criteria are written repo-root-relative and are
therefore fully qualified — a grep against a path that does not exist exits
non-zero with no output and would otherwise pass trivially.

### Global guardrails — violating any of these fails review

- **No local filesystem in the artifact path.** No `os.Create`, `os.MkdirAll`,
  `os.OpenFile`, `os.TempDir`, `filepath.Join` on a caller-influenced path, and
  no `ParseMultipartForm` or `r.FormValue` anywhere under
  `internal/api/v2/artifacts/` or `internal/infra/storage/`. `r.FormValue` calls
  `ParseMultipartForm` internally and poisons `r.MultipartReader()`; read query
  parameters with `r.URL.Query().Get`.
- **No whole-object buffering.** No `io.ReadAll` on a request body or an object
  body. Bounded reads use `http.MaxBytesReader` or `io.LimitReader` with an
  explicit limit.
- **No default project.** Never write `if projectID == "" { projectID = "1" }`.
  A missing or non-canonical project identifier is a 400.
- **No `filepath.Join` for object keys.** It collapses `..`. Keys are validated
  and rejected, never normalised.
- **Never add `//go:build ignore` to a file to make a stage compile.** It does
  not work here: a package containing zero buildable files still fails
  `go vet` for any package that imports it, and `task vet` and `task test`
  both break. If a stage seems to need it, the staging is wrong — stop and
  re-read the sequencing note below.
- **Fail closed.** Unknown backend, missing credentials, failed authorization,
  and errored lookups all produce an error, never a permissive fallback.
- **Do not touch** `internal/infra/storage/content_server.go`,
  `postgres_content.go`, `postgres_secret_vault.go`, or any existing Go file
  in `internal/infra/storage/`. Those serve execution input content and
  configuration state, not artifacts.
- Follow `AGENTS.md`'s other guardrails. Its default of preserving public HTTP
  paths and SDK behaviour does **not** apply to `/api/v2/artifacts/*` or
  `/artifacts/s3/*`: ADR-0016 §7 and this plan's Posture section replace every
  legacy artifact path deliberately, for API-design reasons, not only to fix a
  security defect. That replacement — the full S7-S11 route redesign — is the
  explicit goal of this plan, not an exception to flag.

### Why the sequencing looks the way it does

The existing `storage.Backend` interface has callers in three handler files
(`internal/api/v2/artifacts/{s3handler,handler,pg_repo}.go`) and is implemented
by all four backend packages (`internal/infra/storage/{s3,azure,gcs,filesystem}/`,
each with a `var _ storage.Backend = (*Backend)(nil)` compile-time assertion).
Rewriting `Backend`'s method set in place breaks all seven sites immediately,
and they are not replaced until much later — six consecutive stages would fail
the global gate.

So: **S1 introduces the new interface under a new name, `ObjectStore`, in new
files (plus the small additive `ObjectInfo` edit above, which is backward
compatible). The old `Backend` interface and all its callers stay untouched and
compiling.** S3 makes each remote backend implement `ObjectStore` *alongside*
its existing `Backend` methods — the `s3`, `azure`, and `gcs` packages gain a
second interface satisfaction without losing the first. Handlers migrate one at
a time in S8, S9, S10 — `pg_repo.go` is deleted in S8, the moment its
replacement lands, not held until the end. The old `Backend` interface and the
filesystem package are deleted at the end of S10, when nothing references them
anymore.

---

## Stage map

| Stage | Title | Depends on | Risk |
| --- | --- | --- | --- |
| S1 | `ObjectStore` interface, typed errors, validated `ObjectRef` | — | Low |
| S2 | Emulator-reachable backends: Azure and GCS configuration | — | Low |
| S3 | Implement `ObjectStore` on the three remote backends | S1, S2 | Medium |
| S4 | Conformance suite and CI emulators | S2, S3 | Medium |
| S5 | Factory, fail-closed config, `main.go` wiring | S3 | Low |
| S6 | Metadata schema and repositories | S1 | Medium |
| S7 | OpenAPI contract; register route stubs; delete `oapiserver/artifacts.go` | S6 | Medium |
| S8 | Bucket-plane handlers; delete `pg_repo.go` | S5, S6, S7 | Medium |
| S9 | Object-plane handlers: download, HEAD, streaming upload, batch delete | S5, S6, S7, S8 | High |
| S10 | Retire `/artifacts/s3`; delete `Backend`/`filesystem/`; strip old `Backend` methods from `s3`/`azure`/`gcs` | S8, S9 | High |
| S11 | Production mount, auth, RBAC, public-rule removal | S10 | High |
| S12 | Limits: body caps, read and write deadlines, quota | S6, S11 | Medium |
| S13 | Project-creation bucket bootstrap | S6, S8 | Low |
| S14 | Retention ledger and sweeper | S6, S8, S9 | Medium |
| S15 | Presigned transfer grants | S3, S6, S11 | Medium |
| S16 | Native multipart upload | S3, S15 | Medium |
| S17 | Deployment: Helm, compose, secrets, workload identity | S2, S5, S12 | Medium |
| S18 | Observability: metrics, tracing, audit | S11, S14, S15 | Low |
| S19 | API conformance suite in `tests/contract/` | S10, S11, S15, S4 | Medium |
| S20 | Adjacent planes: chat attachments, icons, result-artifact attestation | S9, S11, S15; **S20c also blocked on spec §6 open question 2 being answered** | High |

S1, S2, S3, S5, S6, S7, S8, S9, S10, S11 are the critical path. S4
(conformance suite) can run any time after S2/S3 — nothing on the path to S11
depends on it, so it is not itself "critical path" in the blocking sense.

There is **no data-migration stage**. Legacy artifact bytes, `centry.storage_meta`,
and the on-disk `libcloud` tree are out of scope per ADR-0016 §8.

---

## S1 — `ObjectStore` interface, typed errors, validated `ObjectRef`

**Preconditions:** none.
**Read first:** `internal/infra/storage/storage.go`, `config.go`, `client.go`.

This stage **adds new files and makes one small, additive edit to `storage.go`**.
It does not delete `Backend`, does not remove or rename any existing field, and
does not touch any handler or backend implementation.

**Edit `internal/infra/storage/storage.go`:** add two fields to the existing
`ObjectInfo` struct — `ETag string` and `DigestSHA256 []byte`. Every current
construction of `storage.ObjectInfo{...}` in the four backend packages uses
keyed field literals (verified: `storage.ObjectInfo{Key: ..., Size: ...}`
throughout `s3/`, `azure/`, `gcs/`, `filesystem/`), so adding fields is
non-breaking — no existing call site needs to change. **Do not declare a second
`ObjectInfo` type in a new file.** `objectstore.go` and `storage.go` are the
same Go package; a duplicate type declaration is a compile error
(`ObjectInfo redeclared in this block`), not a style choice, and it would break
`go build ./...` for every stage from here through S10.

**Create `internal/infra/storage/errors.go`:**

```go
var (
    ErrNotFound           = errors.New("storage: not found")
    ErrAlreadyExists      = errors.New("storage: already exists")
    ErrAccessDenied       = errors.New("storage: access denied")
    ErrPreconditionFailed = errors.New("storage: precondition failed")
    ErrTooLarge           = errors.New("storage: object too large")
    ErrInvalidKey         = errors.New("storage: invalid object reference")
    ErrNotSupported       = errors.New("storage: operation not supported by backend")
)
```

**Create `internal/infra/storage/ref.go`:**

```go
type ObjectRef struct{ projectID, bucket, key string } // zero value invalid

func NewObjectRef(projectID, bucket, key string) (ObjectRef, error)
func NewBucketRef(projectID, bucket string) (ObjectRef, error) // key empty
func ValidateKeyPrefix(p string) error                          // laxer, see below
func (r ObjectRef) ProjectID() string
func (r ObjectRef) Bucket() string
func (r ObjectRef) Key() string
func (r ObjectRef) StorageKey(keyPrefix string) string
func (r ObjectRef) BucketPrefix(keyPrefix string) string
```

Object-key rules, all rejecting with `ErrInvalidKey` wrapped in a message naming
the offending field:

- `projectID` matches `^[1-9][0-9]{0,17}$`. No leading zeros, no sign, no dot.
- `bucket` matches `^[a-z][a-z0-9-]{1,62}$`.
- `key`: non-empty; at most 1024 bytes; `utf8.ValidString`; no rune below 0x20
  and no 0x7f; no leading `/`; no `//`; no segment equal to `.` or `..`; no
  trailing `/`.

`ValidateKeyPrefix` is **deliberately laxer** and applies only to list prefixes:
it permits the empty string and permits a trailing `/`, because the SDK forces
every list prefix to end in `/` (`prefix if prefix.endswith('/') else f"{prefix}/"`).
It still rejects `..`, a leading `/`, `//`, control characters, and anything
over 1024 bytes. An object key and a list prefix are **not** the same type of
value; do not reuse one validator for both.

`StorageKey` returns, with `keyPrefix` omitted entirely when empty:

```
{keyPrefix}/p/{projectID}/b/{bucket}/o/{key}
```

`BucketPrefix` returns the same string truncated after `/o/`, for use as a list
prefix.

**Create `internal/infra/storage/objectstore.go`** with the new interface and
its supporting types — **everything except `ObjectInfo`**, which now lives in
`storage.go` per the edit above. Every type the interface references must be
declared somewhere in the package — an agent that omits one produces a file
with undefined identifiers.

```go
type UploadID string
type Part struct{ Number int32; ETag string; Size int64 }
type BatchError struct{ Key string; Err error }

type PutOptions struct {
    ContentType   string
    ContentLength int64 // -1 when unknown
    Metadata      map[string]string
}

type ByteRange struct{ Start, End int64 } // End -1 means "to end of object"

type ListQuery struct {
    Bucket            ObjectRef // bucket ref; its Key() is always empty
    KeyPrefix         string    // validated by ValidateKeyPrefix; may end in "/"
    Delimiter         string
    MaxKeys           int32     // 0 means backend default; hard cap 1000
    ContinuationToken string
}

type ListPage struct {
    Objects               []ObjectInfo
    CommonPrefixes        []string
    IsTruncated           bool
    NextContinuationToken string
}

type BatchResult struct {
    Deleted []string
    Failed  []BatchError
}

type Capabilities struct {
    Presign         bool
    NativeMultipart bool
    ServerSideCopy  bool
}

type ObjectStore interface {
    Put(ctx context.Context, ref ObjectRef, body io.Reader, opts PutOptions) (ObjectInfo, error)
    Get(ctx context.Context, ref ObjectRef, rng *ByteRange) (io.ReadCloser, ObjectInfo, error)
    Stat(ctx context.Context, ref ObjectRef) (ObjectInfo, error)
    Delete(ctx context.Context, ref ObjectRef) error
    DeleteBatch(ctx context.Context, refs []ObjectRef) (BatchResult, error)
    List(ctx context.Context, q ListQuery) (ListPage, error)

    PresignGet(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
    PresignPut(ctx context.Context, ref ObjectRef, ttl time.Duration, opts PutOptions) (string, error)

    StartMultipart(ctx context.Context, ref ObjectRef, opts PutOptions) (UploadID, error)
    PresignPart(ctx context.Context, ref ObjectRef, id UploadID, part int32, ttl time.Duration) (string, error)
    CompleteMultipart(ctx context.Context, ref ObjectRef, id UploadID, parts []Part) (ObjectInfo, error)
    AbortMultipart(ctx context.Context, ref ObjectRef, id UploadID) error

    Capabilities() Capabilities
}
```

**Semantic rules every implementation must honour**, stated here so the
conformance suite has one source of truth:

- `Delete` is **idempotent**: a missing key returns nil, never `ErrNotFound`.
- The not-found mapping to `ErrNotFound` applies to `Get` and `Stat` only.
- `List` returns exactly one page per call.
- An unsupported operation returns `ErrNotSupported`, never a panic.

**Acceptance criteria:**

- `ref_test.go` is table-driven and rejects, at minimum: `../x`, `a/../b`,
  leading `/`, `//`, trailing `/`, empty key, a 1025-byte key, a key containing
  `\x00` and one containing `\x7f`, invalid UTF-8, `projectID` values `0`, `01`,
  `-1`, `1.2`, and empty, and bucket names `A`, `1abc`, `a`, `a_b`, and a
  64-character name. Every rejection satisfies `errors.Is(err, ErrInvalidKey)`.
- `ValidateKeyPrefix` **accepts** `""`, `folder/`, and `a/b/`, and rejects
  `../`, `/a`, `a//b`.
- Valid refs produce the exact expected `StorageKey` with and without a prefix.
- The tree still builds: `storage.go`'s `ObjectInfo` gained two fields; nothing
  else changed and nothing was deleted.

**Verify:** `cd services/elitea-main && go test ./internal/infra/storage/ -race -run 'ObjectRef|KeyPrefix|StorageKey' -v && go build ./...`

---

## S2 — Emulator-reachable backends

**Preconditions:** none. May run in parallel with S1.

**Why first:** the Azure backend hardcodes `https://%s.blob.core.windows.net/`
and accepts only shared-key credentials; the GCS backend ignores
`cfg.CredentialsFile` and sets no endpoint. Until both are configurable, Azurite
and fake-gcs-server are unreachable and S4 cannot be written.

**Add the missing module first** — `azidentity` is only an indirect entry in
`go.sum` today, so the first import fails under `-mod=readonly`:

```bash
cd services/elitea-main && go get github.com/Azure/azure-sdk-for-go/sdk/azidentity@v1.13.1 && go mod tidy
```

That version requires `azcore v1.20.0`, below the pinned `v1.22.0`, so nothing
else moves.

**Edit `internal/infra/storage/azure/backend.go`:**

- Add `Endpoint string` to `Config`. When set, use it verbatim as the service
  URL; otherwise derive `https://{account}.blob.core.windows.net/`.
- When `Key` is empty, use `azidentity.NewDefaultAzureCredential` with
  `azblob.NewClient(serviceURL, cred, opts)`, enabling workload identity. When
  `Key` is set, keep the shared-key path.
- **Record on the Config which credential path was taken.** Presigning depends
  on it: `blob.Client.GetSASURL` returns `MissingSharedKeyCredential` when there
  is no shared key. A token-credential client must mint a **user-delegation**
  SAS instead (`ServiceClient().GetUserDelegationCredential` then
  `sas.BlobSignatureValues.SignWithUserDelegation`), and Azurite does not issue
  user-delegation keys. S3 uses this flag for `Capabilities().Presign`.

**Edit `internal/infra/storage/gcs/backend.go`:**

- Add `Endpoint string` to `Config`.
- `Endpoint` and `CredentialsFile` are **mutually exclusive**. Setting both is a
  hard construction error inside the client library —
  `options.WithoutAuthentication is incompatible with any option that provides
  credentials`. Reject the combination at config time with an error naming both
  environment variables, rather than letting the client fail at startup.
- When `CredentialsFile` is non-empty: append `option.WithCredentialsFile`.
- When `Endpoint` is non-empty and `CredentialsFile` is empty: append
  `option.WithEndpoint(cfg.Endpoint)` and `option.WithoutAuthentication()`.
- **`GCS_ENDPOINT` must be a full base URL including the `/storage/v1/` path**,
  for example `http://localhost:4443/storage/v1/`. `option.WithEndpoint` is a
  complete base-URL override, not a host override; only the
  `STORAGE_EMULATOR_HOST` code path appends `storage/v1/` for you. A bare host
  value silently routes every call to the wrong path and 404s on every
  `Stat`, `List`, and `Delete`. Document this in the `Config` field comment.

**Edit `internal/infra/storage/config.go`:** add `AzureEndpoint` from
`AZURE_STORAGE_ENDPOINT` and `GCSEndpoint` from `GCS_ENDPOINT`.

**Acceptance criteria:** both `New` functions accept an endpoint; neither
hardcodes a public-cloud hostname when one is supplied; the GCS constructor
returns an error when both `Endpoint` and `CredentialsFile` are set, with a
message naming both variables.

**Verify:** `cd services/elitea-main && go build ./internal/infra/storage/... && go test ./internal/infra/storage/gcs/ -run Config`

---

## S3 — Implement `ObjectStore` on the three remote backends

**Preconditions:** S1, S2.

Each of `s3`, `azure`, `gcs` gains an `ObjectStore` implementation. **Keep the
existing `Backend` methods in place** — handlers still call them until S10. Add
the new methods to the same type where names do not collide, and add a small
adapter type where they do.

Add `var _ storage.ObjectStore = (*Backend)(nil)` to each package.

**Add the AWS uploader at the one compatible version:**

```bash
cd services/elitea-main && go get github.com/aws/aws-sdk-go-v2/feature/s3/manager@v1.22.34
```

`@latest` upgrades 19 modules including the core SDK and smithy-go — do not use
it. `v1.22.34` is a much smaller bump: it moves `service/s3` from `v1.105.0` to
`v1.105.2` plus six same-family modules by one patch each (`config`,
`credentials`, `service/{signin,sso,ssooidc,sts}`), and leaves the core SDK
(`aws-sdk-go-v2`) and `smithy-go` untouched. Run `go mod tidy` and inspect
`git diff go.mod` before committing — the exact patch set can drift as new
releases land — and record what actually moved in the commit message. The
module carries a deprecation notice pointing at `feature/s3/transfermanager`;
note in the same commit message why the successor is not being used yet.

### S3 backend

- **Route every non-seekable body through `manager.Uploader`, not only
  unknown-length ones.** Against an `http://` endpoint, *any* non-seekable body
  fails before the network with `failed to compute payload hash: failed to seek
  body to start, request stream is not seekable` — the SigV4 middleware's
  `RewindStream`, not the checksum middleware. Setting
  `RequestChecksumCalculation: WhenRequired` does not help. Since S4 runs
  RustFS over plain HTTP and S9 passes a `*multipart.Part` straight into `Put`,
  every
  upload test would otherwise fail. Use the plain `PutObject` path only when the
  body implements `io.ReadSeeker`.
- Remove `isErrType`. Use `errors.As` with `*types.NoSuchKey`,
  `*types.NoSuchBucket`, and `smithy.APIError` code matching.
- `DeleteBatch` chunks keys into groups of at most 1000 and **leaves
  `Delete.Quiet` nil** — the field is `*bool` and `Quiet: false` does not
  compile; omitting it selects verbose mode, which is what populates
  `Deleted[]`. Report every `Errors[]` entry in `BatchResult.Failed`.
- `List` honours `MaxKeys`, `Delimiter`, and `ContinuationToken`, returning
  `CommonPrefixes` and `NextContinuationToken` from the provider response.
- When `AccessKey` is empty, let `awsconfig.LoadDefaultConfig` resolve IRSA,
  instance, and environment credentials — do not force static credentials.
- `Get` with a non-nil range sets the `Range` header and expects 206.
- Presigning uses `s3.NewPresignClient` with `PresignGetObject` /
  `PresignPutObject`. It works with a custom `BaseEndpoint` and `UsePathStyle`.
  Note that `ContentType` on `PutObjectInput` is **not** signed, so a presigned
  PUT does not enforce media type — S15 verifies it on commit.
- **`CompleteMultipartUpload` requires parts in strictly ascending
  `PartNumber` order — confirmed against real RustFS, which rejects an
  out-of-order list with `InvalidPartOrder: Part numbers must be strictly
  increasing`.** Sort the caller's `[]Part` by `Number` before building the
  request; do not assume the caller already sorted it (S16's future HTTP
  handler has no reason to guarantee that, and neither does this stage's own
  conformance suite, S4, which deliberately completes with parts reversed to
  catch exactly this).

### Azure backend

- `Put` sets `UploadStreamOptions{BlockSize: 8 << 20, Concurrency: 4}`. The
  library defaults of a 1 MiB block and concurrency 1 cap an object at 50000
  blocks × 1 MiB ≈ 48.8 GiB and serialise every upload. 8 MiB raises the ceiling
  to about 390 GiB.
- `List` obtains a container client via
  `client.ServiceClient().NewContainerClient(cfg.ContainerName)`. The hierarchy
  pager exists **only** on `container.Client`; the top-level `azblob.Client`
  exposes only the flat pager. Use `NewListBlobsHierarchyPager(delimiter, opts)`
  when `Delimiter` is set and `NewListBlobsFlatPager` otherwise; set `Marker`
  from `ContinuationToken`, call `NextPage` exactly once, and return
  `*resp.NextMarker` as `NextContinuationToken`. The `CommonPrefixes` equivalent
  is `page.Segment.BlobPrefixes`.
- Map errors with `bloberror.HasCode`: `BlobNotFound` and `ContainerNotFound` to
  `ErrNotFound` on `Get`/`Stat`; `AuthenticationFailed`, `AuthorizationFailure`,
  and `AuthorizationPermissionMismatch` to `ErrAccessDenied`.
- `Delete` swallows `BlobNotFound` and `ContainerNotFound` and returns nil.

### GCS backend

- `Get` issues a single call: `NewRangeReader(ctx, off, len)` when `rng != nil`,
  `NewReader` otherwise. Read attributes from `reader.Attrs`. Remove the separate
  `Attrs` call and its TOCTOU window. **`ObjectInfo.ETag` is empty on this
  path** — `ReaderObjectAttrs` does not carry one. Only `Stat` and `List`
  populate `ETag`, from `ObjectAttrs.Etag`. Handlers must not require it on the
  `Get` path.
- `Put` sets `w.ChunkSize` explicitly (8 MiB) and `w.ContentType`; on `io.Copy`
  error it calls `w.Close()` and returns the copy error, otherwise it returns
  the `Close` error.
- `List` honours `Query.Delimiter` and `Query.Prefix` and paginates via
  `iterator.NewPager`, returning `CommonPrefixes` from `attrs.Prefix` entries.
- Map `gcstorage.ErrObjectNotExist` and `ErrBucketNotExist` to `ErrNotFound` on
  `Get`/`Stat`. **`Delete` must explicitly check
  `errors.Is(err, gcstorage.ErrObjectNotExist)` and return nil** — the provider
  returns an error for a missing key, unlike S3.
- The four multipart methods return `ErrNotSupported`. The pinned client library
  has no multipart upload API: no upload identifier, no part list, no per-part
  signing. Do not attempt to emulate one.

### Capabilities must be honest

```go
// s3
Capabilities{Presign: true, NativeMultipart: true, ServerSideCopy: true}
// azure
Capabilities{Presign: cfg.HasSharedKey, NativeMultipart: true, ServerSideCopy: true}
// gcs
Capabilities{Presign: cfg.HasSigningMaterial, NativeMultipart: false, ServerSideCopy: true}
```

`Capabilities` exists precisely so a handler can fall back to the streaming
facade rather than failing. Reporting `true` uniformly defeats its purpose and
makes the conformance suite unpassable.

**Acceptance criteria:**

- `var _ storage.ObjectStore = (*Backend)(nil)` compiles in all three packages.
- `Capabilities()` matches the constructed configuration — a table test asserts
  Azure with no key reports `Presign: false`, and GCS always reports
  `NativeMultipart: false`.
- `grep -rn 'io.ReadAll' services/elitea-main/internal/infra/storage/{s3,azure,gcs}/` finds
  nothing. **Scoped to the three backend packages this stage writes, not the
  whole `internal/infra/storage/` tree** — that tree also contains pre-existing,
  out-of-scope code this stage does not touch, e.g. `content_server.go`'s
  deliberate, documented, small bounded `io.ReadAll` on a validation slice. The
  wider grep this stage originally specified fails against code no
  implementation of this stage could ever satisfy.
- The tree still builds: the old `Backend` methods and their callers are intact.

**Verify:** `cd services/elitea-main && go build ./... && go vet ./internal/infra/storage/...`

---

## S4 — Conformance suite and CI emulators

**Preconditions:** S2, S3.

**Create `internal/infra/storage/conformance/`** — one table-driven suite run
against each backend its environment provides. Each backend skips with `t.Skip`
when its emulator variables are absent, so the suite is green on a laptop with
no containers and exhaustive in CI.

Cases 1 to 11 run against every backend. Cases 12 to 14 are **gated on
`Capabilities()`** and skip with a recorded reason when the backend reports the
feature as unavailable.

1. `Put` then `Stat` returns the same size and content type.
2. `Get` on a missing key satisfies `errors.Is(err, ErrNotFound)`.
3. `Delete` on a missing key returns **nil** on every backend.
4. `List` with a key prefix returns only matching keys.
5. `List` with `Delimiter: "/"` and a prefix ending in `/` returns
   `CommonPrefixes` and no keys below the delimiter.
6. `List` with `MaxKeys: 2` over 5 objects returns `IsTruncated: true` and a
   continuation token yielding the remainder with no duplicates and no gaps.
7. `Get` with `ByteRange{10, 19}` returns exactly 10 bytes matching the source.
8. `DeleteBatch` over 1500 keys deletes all of them and reports no failures.
9. `DeleteBatch` including 3 missing keys succeeds for the present ones.
10. A 5 MiB object round-trips with a matching SHA-256, not merely a matching
    length.
11. A zero-byte object round-trips.
12. *(gated on `Presign`)* `PresignGet` produces a URL an unauthenticated
    `http.Get` can fetch, and that returns 403 after a 2-second TTL elapses.
13. *(gated on `NativeMultipart`)* `StartMultipart` / `PresignPart` /
    `CompleteMultipart` over two 5 MiB parts yields an object whose digest
    matches the concatenation.
14. *(gated on `NativeMultipart`)* `AbortMultipart` leaves no listable object.

Plus one case asserting that a backend reporting `NativeMultipart: false`
returns `ErrNotSupported` from all four multipart methods.

**Emulator notes that determine whether this stage works at all:**

- **RustFS**, not MinIO — Elitea's chosen S3-compatible service for local dev,
  CI, and self-hosted deployment (see S17). RustFS is a real, independent
  S3-API-compatible object store (Apache 2.0, `github.com/rustfs/rustfs`); it
  is not a code fork or wrapper of MinIO and requires no special-casing in the
  `storage/s3` backend — it is exercised through the exact same code path as
  AWS S3, addressed by `S3_ENDPOINT_URL` like any other S3-compatible endpoint.
  Verified against the project's own `docker-compose-simple.yml` and container
  docs as of 2026-08: official image `rustfs/rustfs:latest`; S3 API on `9000`,
  console on `9001`; server-side credentials set via `RUSTFS_ACCESS_KEY` /
  `RUSTFS_SECRET_KEY` (these configure the RustFS server's root identity — pass
  the same values through as Elitea's client-side `S3_ACCESS_KEY` /
  `S3_SECRET_KEY` when pointing the S3 backend at it); the container runs as a
  non-root user, UID/GID `10001:10001`, and every mounted data path must be
  writable by that UID or the container fails to start. Run over plain HTTP
  with `S3_FORCE_PATH_STYLE=true` — RustFS is accessed by host:port rather than
  a DNS-based virtual-hosted bucket subdomain, so path-style is the safe
  default; confirm against the current compose file, since RustFS is a young,
  fast-moving project (public beta, first released mid-2025) and exact
  defaults can shift between releases. **Copy the actual service definition
  from `rustfs/rustfs`'s own `docker-compose-simple.yml` rather than
  hand-rolling one** — re-verify the image tag and env var names against that
  file at implementation time.
- **Azurite**: shared key only. It does not issue user-delegation keys, so run
  the Azure leg with `AZURE_STORAGE_KEY` set, or the whole leg skips (not just
  case 12 — every operation needs a credential Azurite accepts, and shared key
  is the only one it does). **The `mcr.microsoft.com/azure-storage/azurite:latest`
  image, confirmed as of 2026-08, rejects every request from the pinned
  `azblob` SDK version with `400 InvalidHeaderValue` / "The API version ... is
  not supported by Azurite" unless started with `--skipApiVersionCheck`** —
  confirmed by actually running the SDK against a live container, not
  documentation. Pass it as a command-line argument to the `azurite-blob`
  entrypoint.
- **fake-gcs-server**: listens on 4443 with HTTPS by default — pass `-scheme http`
  or the connection fails regardless of the endpoint path. Set `GCS_ENDPOINT`
  to the full `http://localhost:4443/storage/v1/` form. Case 12 will skip for
  GCS unless a fake service-account key is injected via
  `option.WithCredentialsJSON` so `SignedURL` can sign locally; recording the
  skip is acceptable.

**Create `deploy/docker-compose.storage-test.yml`** with the three emulators,
and add a step to the `test` job in `.github/workflows/ci-go.yml` (its only Go
test job — there is no separate "storage" job) that starts it before the
tests. A `services:` block is also acceptable if it proves
workable in the runner; the acceptance criterion is that the suite runs and
passes in CI, not the mechanism.

**Edit `deploy/docker-compose.yml`:** add a `rustfs` service and a
bucket-create init container so `task up` gives a working local object store.

**Acceptance criteria:** every non-gated case passes against all three backends
in CI; gated skips print their reason; the suite skips cleanly with no emulator.

**Verify:** `cd services/elitea-main && go test ./internal/infra/storage/conformance/ -race -v`

---

## S5 — Factory and wiring

**Preconditions:** S3. **If S4 has also already run** (it's listed as
running any time after S2/S3, independently of S5), **this stage breaks
`internal/infra/storage/conformance/conformance_test.go`'s build.** S4's
suite calls the old flat `storage.ConfigFromEnv()` (no arguments, no error
return) to read `S3_ENDPOINT_URL` and the rest for all three backends at
once; this stage's rewrite makes it `ConfigFromEnv(lookup) (Config, error)`,
single-backend-selecting via a required `STORAGE_BACKEND`. Do not route the
conformance suite through the new `ConfigFromEnv` to fix this — it is a
different consumer with a different job (stand up all three backends side by
side against three emulators in one run, not select and validate one for
production) and forcing it through would mean inventing a fake
`STORAGE_BACKEND` override per subtest and requiring `STORAGE_CONTAINER` to
be set just to satisfy validation the suite doesn't otherwise need. Instead,
edit the suite's three `setupS3`/`setupAzure`/`setupGCS` functions to read
`os.Getenv` directly for the handful of variables they need — they mostly
already did per-backend presence checks individually; this only removes
their shared call to the now-incompatible `ConfigFromEnv()`.

**Rewrite `internal/infra/storage/config.go`:**

```go
type Config struct {
    Backend   string // "s3" | "azure" | "gcs" — required, env STORAGE_BACKEND
    Container string // physical bucket or container name — required, env STORAGE_CONTAINER
    KeyPrefix string // optional, empty by default, env STORAGE_KEY_PREFIX
    S3    S3Config
    Azure AzureConfig
    GCS   GCSConfig
}

func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error)
```

Take a lookup function, matching the convention in `cmd/elitea-main/*_config.go`.
Return an error — never a default — when `STORAGE_BACKEND` is unset, empty, or
not one of the three; when `STORAGE_CONTAINER` is unset; when the selected
backend's required credentials are absent; or when GCS has both an endpoint and
a credentials file. `ARTIFACTS_DATA_DIR` is no longer read. `STORAGE_CONTAINER`
is the single physical bucket/container name used regardless of backend — the
per-backend `Config` structs (`S3Config`, `AzureConfig`, `GCSConfig`) take it as
their container/bucket field; there is no separate per-backend container-name
env var (the old prototype's `AZURE_STORAGE_CONTAINER` and `GCS_BUCKET` are
retired along with the rest of `config.go`).

**Use the lookup function, not `os.Getenv`, as a matter of convention — not
because CI enforces it yet.** `scripts/env-drift-check.sh` fails when code
reads a variable via `os.Getenv("X")` that the chart neither sets nor
allowlists, but it is invoked only from `.github/workflows/ci-gateway.yml`,
whose `paths:` trigger is scoped to `services/elitea-llm-gateway/**` — it will
not run at all on a PR touching only `services/elitea-main`. There is no
`golangci-lint` rule banning `os.Getenv` either. So nothing in this repo's CI
actually blocks a stray `os.Getenv` here today; follow the lookup-function
convention anyway, since S17 (which owns the Helm chart) runs later and the
check may be widened to cover `elitea-main` before then.

**The factory does NOT live in `internal/infra/storage/`.** `s3`, `azure`, and
`gcs` each already import package `storage` (for `ObjectRef`, `ObjectInfo`,
`PutOptions`, and the rest of S1's types — required by S3's
`var _ storage.ObjectStore = (*Backend)(nil)` assertions). A factory inside
package `storage` that constructs `s3.Backend` / `azure.Backend` / `gcs.Backend`
would need `storage` to import `s3`, `azure`, and `gcs` — `storage → s3 →
storage` is a direct Go import cycle, not a style choice. This is exactly the
constraint the file this stage deletes documents: `internal/infra/storage/client.go`
says the factory "lives in `cmd/elitea-main/main.go` to avoid import cycles
(backends import this package for the interface types; this package cannot
import them back)". That was correct. Keep it true.

**Delete `internal/infra/storage/client.go`** — supersede its comment with the
real thing, in the place it already said the real thing belongs.

**Create `cmd/elitea-main/storage_factory.go`** (package `main`, which can
safely import `internal/infra/storage`, `internal/infra/storage/s3`,
`internal/infra/storage/azure`, and `internal/infra/storage/gcs` — none of
those import `main`, so there is no cycle):

```go
func newObjectStore(ctx context.Context, cfg storage.Config) (storage.ObjectStore, error)
```

A `switch` over `cfg.Backend` calling `s3.New`, `azure.New`, or `gcs.New` with
the matching sub-config, and an error for anything else. Unexported — it has
exactly one caller, `main()`.

**Edit `cmd/elitea-main/main.go`:**

- Call `storage.ConfigFromEnv(os.LookupEnv)` and the new `newObjectStore`.
- On error, log and exit non-zero. The service does not start without a working
  object store.
- Add a startup readiness probe issuing one `Stat` against a sentinel key,
  tolerating `ErrNotFound` but not `ErrAccessDenied` or a transport error.
- Add an `ObjectStore` field to `RouterConfig` and assign it. **Do not assume
  assigning the existing `Storage` field puts the backend on the request path** —
  at the time this stage was written, it was read only inside
  `newProductionRouter` (then `newPrototypeCompatibilityRouter`), reached only
  when `prototypeCompatibilityRequested(cfg)` held; `main.go` didn't set any
  triggering field yet, so it never reached that branch. (#243 later deleted
  that predicate and the separate branch it gated — `NewRouter` now always
  builds via `newProductionRouter` — but the underlying caution still
  applies: assigning `ObjectStore` alone doesn't mount routes. S11 is what
  actually mounts the routes.)

**Acceptance criteria:**

- A table-driven test asserts `ConfigFromEnv` errors for: unset backend,
  `STORAGE_BACKEND=filesystem`, `STORAGE_BACKEND=nonsense`, `s3` with no
  container, `azure` with no account, `gcs` with both endpoint and credentials
  file.
- A separate table-driven test in `cmd/elitea-main/storage_factory_test.go`
  asserts `newObjectStore` dispatches to the right constructor per
  `cfg.Backend` and errors on an unrecognised one. It does not need real
  network reachability — asserting the returned concrete type (or the error for
  the unknown case) is enough, **for `s3` and `azure`**. `gcs.New` dials for
  live GCP default credentials at construction time unless S2's
  `Endpoint`/`WithoutAuthentication` support already landed (S5 depends on S3,
  which depends on S2, so by the time this stage runs it always has) — if
  you're implementing out of order for any reason, the `gcs` leg of this test
  needs S2's constructor to assert a concrete type instead of tolerating a
  credential error.
- `grep -n 'ARTIFACTS_DATA_DIR' services/elitea-main/internal/infra/storage/config.go` finds
  nothing. **Scoped to the one file this stage rewrites — not the whole
  `services/elitea-main/` tree.** `internal/api/router_security_test.go` also
  reads `ARTIFACTS_DATA_DIR` (it builds a filesystem-backed fixture for the
  legacy `/artifacts/s3` proxy) and legitimately keeps doing so until S10
  deletes that proxy and repoints the fixture at S8's fake store — S10's own
  text says so explicitly. A tree-wide grep here is unsatisfiable at this
  stage regardless of implementation quality; do not widen it.
- `grep -n 'os.Getenv' services/elitea-main/internal/infra/storage/config.go services/elitea-main/cmd/elitea-main/storage_factory.go` finds nothing.
  (Scoped to the two files this stage writes — the wider directories contain
  pre-existing, out-of-scope `os.Getenv` calls, e.g. in
  `postgres_secret_vault_integration_test.go` and `main.go`'s unrelated
  `AUTH_DEV_MODE` lookup, which this stage does not touch and a broader grep
  would never let pass.)
- `go build ./...` exits 0 (this is the actual test for the import-cycle
  constraint above — a cycle fails the build with `import cycle not allowed`,
  not a grep).

**Verify:** `cd services/elitea-main && go build ./... && go test ./internal/infra/storage/... ./cmd/elitea-main/... -race`

---

## S6 — Metadata schema and repositories

**Preconditions:** S1.

**Create `services/elitea-main/migrations/shared/0057_artifact_storage.sql`.**
The head is `0056_agent_execution_output.sql`, so `0057` is free.

**There is no registration file.** `LoadManifest` discovers migrations from the
`//go:embed shared/*.sql tenant/*.sql` in `migrations/embed.go`. But there **is**
a pinned head assertion that will fail: bump
`internal/infra/db/migrate/manifest_test.go:97` from
`require.EqualValues(t, 56, Head(shared))` to `57`. Forgetting this reds
`task test`.

Four tables. `BIGINT GENERATED ALWAYS AS IDENTITY` matches the house style
already used in `migrations/shared/`.

```sql
CREATE SCHEMA IF NOT EXISTS elitea_storage;

CREATE TABLE elitea_storage.buckets (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id      BIGINT      NOT NULL,
    name            TEXT        NOT NULL,
    display_name    TEXT        NOT NULL DEFAULT '',
    bucket_type     TEXT        NOT NULL DEFAULT 'local',
    is_pinned       BOOLEAN     NOT NULL DEFAULT FALSE,
    tags            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    retention_days  INTEGER,
    expires_at      TIMESTAMPTZ,
    notified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    CONSTRAINT buckets_name_valid CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
    CONSTRAINT buckets_type_valid CHECK (bucket_type IN ('system','autogenerated','local'))
);
CREATE UNIQUE INDEX buckets_project_name_uniq
    ON elitea_storage.buckets (project_id, name) WHERE deleted_at IS NULL;

CREATE TABLE elitea_storage.objects (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bucket_id      BIGINT      NOT NULL REFERENCES elitea_storage.buckets(id) ON DELETE CASCADE,
    key            TEXT        NOT NULL,
    byte_length    BIGINT      NOT NULL,
    media_type     TEXT        NOT NULL DEFAULT 'application/octet-stream',
    digest_alg     TEXT,
    digest         BYTEA,
    classification TEXT        NOT NULL DEFAULT 'internal',
    scan_state     TEXT        NOT NULL DEFAULT 'not_scanned',
    expires_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT objects_key_len CHECK (octet_length(key) BETWEEN 1 AND 1024)
);
CREATE UNIQUE INDEX objects_bucket_key_uniq ON elitea_storage.objects (bucket_id, key);
CREATE INDEX objects_expiry ON elitea_storage.objects (expires_at) WHERE expires_at IS NOT NULL;

-- Required by S12 (quota) and S20a (attachment bucket name).
-- retention_default_days and retention_max_days are deliberately distinct:
-- the first is what a bucket gets when the caller omits a value, the second
-- is the ceiling S8's PATCH validates a caller-supplied value against. They
-- are not interchangeable — see S8.
CREATE TABLE elitea_storage.project_storage_policy (
    project_id             BIGINT PRIMARY KEY,
    max_object_bytes       BIGINT,
    max_total_bytes        BIGINT,
    retention_default_days INTEGER,
    retention_max_days     INTEGER, -- null means unlimited
    attachment_bucket      TEXT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Required by S15 (presigned grants) and S20c (attestation commit).
CREATE TABLE elitea_storage.transfer_grants (
    id           UUID        PRIMARY KEY,
    project_id   BIGINT      NOT NULL,
    bucket_id    BIGINT      NOT NULL REFERENCES elitea_storage.buckets(id) ON DELETE CASCADE,
    key          TEXT        NOT NULL,
    method       TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT '',
    max_bytes    BIGINT      NOT NULL,
    digest_alg   TEXT,
    digest       BYTEA,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT grants_method_valid CHECK (method IN ('GET','PUT'))
);
CREATE INDEX grants_expiry ON elitea_storage.transfer_grants (expires_at);
```

**Create `internal/infra/db/repos/artifact_buckets.go` and
`artifact_objects.go`** following the construction and error conventions of the
neighbouring repositories — read `internal/infra/db/repos/index_ingest_results.go`
first for the house style.

Methods: `ListBuckets`, `GetBucket`, `CreateBucket` (mapping unique violation
`23505` to `ErrAlreadyExists`), `UpdateBucketRetention`, `SetBucketPinned`,
`UpdateBucketTags`, `SoftDeleteBucket`, `UpsertObject`, `ListObjects`,
`DeleteObjects`, `SumBucketBytes`, and `CountBucketObjects` — the last two are
single aggregate queries (`SUM`/`COUNT`), never a full object listing. S8's
bucket response needs both, and `UpdateBucketTags` closes a real gap: S19's
conformance suite exercises "patch (pin, tags, retention)" end to end, but
without this method there is no way to persist a tag update at all — say so
explicitly in S8 too, when you get there, not just here.

**Also add `GetProjectStoragePolicy(ctx, projectID) (ProjectStoragePolicy, error)`**,
reading the `project_storage_policy` row (a missing row means every field is
unlimited/default — do not error). S8's retention-limit check and S12's quota
check both need to read this row and no earlier stage provides a way to; there
is no other repository method anywhere in this plan that fetches it.

**And add the four methods S14's sweeper needs**, none of which the methods
above can compose into, because a background sweeper has no request-bound
`projectID` to scope by and this plan defines no cross-project enumeration
method otherwise: `ListExpiredObjects(ctx, olderThan time.Time, limit int32) ([]ObjectRow, error)`
(bounded batch, backed by the `objects_expiry` index above — this is the query
that index exists for, and until now nothing used it),
`DeleteObjectRows(ctx, ids []int64) error`,
`ListBucketsNeedingExpiryNotice(ctx, within time.Duration, limit int32) ([]BucketRow, error)`
(buckets with `expires_at` inside the window and `notified_at` still null),
and `MarkBucketNotified(ctx, bucketID int64) error`.

**And add `SumProjectBytes(ctx, projectID) (int64, error)`**, the method S12's
project-quota check needs. `SumBucketBytes` (above) is deliberately
single-bucket — it feeds one bucket's `size_bytes` response field in S8 — and
does not compose into a project total; joining across every bucket owned by
a project is a different query (`SUM(size_bytes) ... JOIN buckets ON
buckets.id = objects.bucket_id WHERE buckets.project_id = $1 AND buckets.
deleted_at IS NULL`), not a loop over `SumBucketBytes` calls. S12 is the only
caller; without this method S12's `project_storage_policy.max_total_bytes`
check has nothing to compare against.

**Acceptance criteria:** every method has a `*_postgres_integration_test.go`
following the existing pattern. Unique-violation and no-rows paths map to the S1
sentinels. **Every test function in this package is named with a `TestArtifact`
prefix** (e.g. `TestArtifactBucketsRepositoryRejectsDuplicateName`,
`TestArtifactCountBucketObjectsExcludesSoftDeleted`) — **this is a hard
requirement, not a suggestion.** The existing repositories in this directory
are named by behavior (`TestListPendingIndexIngestIDsIsCapabilityAndStreamScoped`
and similar — none of the 317 test functions in this package follow a
domain-noun-prefix convention), and this stage's own Verify command filters on
`-run Artifact`, which matches against the Go function name only, not the file
name. A correctly implemented, correctly named-by-behavior test that happens
not to contain the literal substring "Artifact" is silently excluded from the
run — not reported as a skip, simply absent from the output — while the
command still exits 0. Follow the neighbouring repositories' construction and
error style; do not follow their naming style for these specific functions.

**Note on the verify command:** every `*_postgres_integration_test.go` in that
package calls `newPostgresIntegrationPool`, which **skips** when the integration
database URL is unset, and `ci-go.yml` sets no such variable. A bare `go test`
therefore exits 0 with every new test skipped. Either add a `postgres:16`
service and the DSN to the ci-go test job as part of this stage, or accept that
the suite is local-only and gate on the explicit run below.

**Do not pipe `go test` through `tee` and then check the pipeline's exit
code — it lies.** This shell has `pipefail` off by default (confirmed:
`false | tee /tmp/x; echo $?` prints `0`), so `go test ... | tee log && grep ...`
reports success from `tee`'s exit code, not `go test`'s — a single genuinely
failing test among a dozen passing ones is invisible to this command.
Capture `go test`'s own exit code before piping:

**Verify:** `cd services/elitea-main && go test ./internal/infra/db/repos/ -race -run TestArtifact -v > /tmp/s6.log 2>&1; rc=$?; cat /tmp/s6.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s6.log && ! grep -q -- '--- SKIP' /tmp/s6.log`

---

## S7 — OpenAPI contract: define the new artifact API

**Preconditions:** S6.
**Read first:** `api/openapi/v2.yaml`, `internal/api/router.go` (the
`/artifacts` block, ~line 672), `internal/api/oapiserver/artifacts.go`,
`internal/api/oapiserver/server.go`.

**This stage must land before any route change.**
`internal/api/oapiserver/conformance_test.go` asserts the forward direction:
every `operationId` in `api/openapi/v2.yaml` must resolve to a route registered
by `api.NewRouter(buildFullSurfaceConfig())`, and it pins floors of 75 spec
operations and 280 router routes. The spec currently declares 78 operations, 9
of them artifact operations: `bucketList`, `createBucket`, `editBucket`,
`updateBucketPin`, `deleteBucket`, `artifactList`, `createArtifact`,
`deleteArtifacts`, `deleteArtifact`.

Changing routes without updating the spec fails the forward assertion. Removing
operations without replacing them drops the count below the floor. Both break
`task test`.

**Replace the 9 legacy artifact operations with the 13 operations of the new
contract**, so the count rises rather than falls:

| Method | Path | operationId |
| --- | --- | --- |
| GET | `/artifacts/buckets/{projectID}` | `listBuckets` |
| POST | `/artifacts/buckets/{projectID}` | `createBucket` |
| GET | `/artifacts/buckets/{projectID}/{bucket}` | `getBucket` |
| PATCH | `/artifacts/buckets/{projectID}/{bucket}` | `updateBucket` |
| DELETE | `/artifacts/buckets/{projectID}/{bucket}` | `deleteBucket` |
| GET | `/artifacts/objects/{projectID}/{bucket}` | `listObjects` |
| POST | `/artifacts/objects/{projectID}/{bucket}` | `uploadObject` |
| POST | `/artifacts/objects/{projectID}/{bucket}:batchDelete` | `batchDeleteObjects` |
| GET | `/artifacts/objects/{projectID}/{bucket}/{key}` | `downloadObject` |
| HEAD | `/artifacts/objects/{projectID}/{bucket}/{key}` | `statObject` |
| DELETE | `/artifacts/objects/{projectID}/{bucket}/{key}` | `deleteObject` |
| POST | `/artifacts/grants/{projectID}/{bucket}` | `createTransferGrant` |
| POST | `/artifacts/grants/{projectID}/{grantID}:commit` | `commitTransferGrant` |

The old paths — `/artifacts/buckets/default/{project_id}`,
`/artifacts/artifacts/default/{project_id}/{bucket}`, `/artifacts/artifact/...`,
and everything under `/artifacts/s3` — are removed from the spec entirely. The
`default/` segment was a Pylon mode selector with exactly one value; it carries
no information and does not survive.

Schema rules, applied uniformly:

- Sizes are `integer` with `format: int64`, in bytes. Never a formatted string.
- Timestamps are `string` with `format: date-time`.
- Retention is `retention_days`, an integer, nullable for "no expiry".
- Every non-2xx response uses one shared `Error` schema:
  `{"error": {"code": "...", "message": "...", "details": {}}}` with `code` an
  enum of `NotFound`, `AlreadyExists`, `AccessDenied`, `InvalidArgument`,
  `InvalidKey`, `TooLarge`, `QuotaExceeded`, `PreconditionFailed`,
  `NotImplemented`, `Internal`.
- `listObjects` returns `{"objects": [...], "common_prefixes": [...], "next_cursor": "..."}`
  with `next_cursor` omitted when exhausted.

Regenerate with `task openapi`. Confirm `internal/api/generated/api.gen.go`
updates — this is the only file it touches (`Taskfile.yml`'s `openapi` task
runs one `oapi-codegen` invocation against that one output path).
**`apps/elitea-web/src/shared/api/endpoints.manifest.json` is not, in the
current codebase, mechanically regenerated from `v2.yaml` by any command —
confirmed by reading it and `apps/elitea-web/scripts/check-endpoint-manifest.mjs`.**
It is a hand-maintained, append-only manifest with its own CI enforcement
(R-A5) and an explicit "never remove or renumber another unit's entries;
append only" convention, owned by the separate UI reimplementation program.
Leave it untouched — this is squarely "do not edit web UI source to match,"
the same boundary this paragraph already draws, just via a different
mechanism than "regenerating it" implies.

**The conformance test checks one direction only, and editing the spec alone
fails it.** `every_spec_operation_resolves_to_a_route` requires every spec
`operationId` to resolve to *some* registered router pattern — it does not
require the reverse. So after this edit, the 13 new paths have no route to
resolve to until something registers them, and the test fails with all 13
unresolved. **This stage must also register a minimal stub route for each of
the 13 new paths**, so the shape resolves; the *behaviour* is filled in by
S8/S9. Do this in `internal/api/router.go`, inside
`newProductionRouter`: replace the existing
`r.Route("/artifacts", func(r chi.Router) { ... })` block (currently ~11
registrations built on `v2artifacts.NewInMemoryHandler()`, around line 670)
with the 13 new paths from the table above, each pointing at one shared
placeholder:

```go
func notImplementedArtifact(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusNotImplemented)
    _, _ = w.Write([]byte(`{"error":{"code":"NotImplemented","message":"pending S8/S9"}}`))
}
```

**Register the three key-bearing routes** (`downloadObject`, `statObject`,
`deleteObject`) **as a chi wildcard, not the literal `{key}` from the table.**
chi v5.1.0 has no multi-segment named-param syntax — a literal `{key}` matches
only one path segment and 404s on any key containing `/`, which S1's own key
grammar explicitly allows. Register these three as
`r.Get("/artifacts/objects/{projectID}/{bucket}/*", notImplementedArtifact)`
(and `r.Head`, `r.Delete` for the other two) instead. This is safe for the
conformance check specifically: `segmentsMatch` in
`internal/api/oapiserver/conformance.go` treats a **trailing** `*` as matching
any remainder of the spec path, unconditionally — confirmed by reading
`conformance.go:308-339`, which returns `true` the instant it reaches a
trailing `*` in the router pattern, before comparing further segments. So a
router pattern ending in `/*` still resolves the spec's `.../{key}` operation.
The other ten paths use ordinary `{projectID}`/`{bucket}`/`{grantID}` chi
params, matching the table's placeholders directly.

This removes the `artifactHandler := v2artifacts.NewInMemoryHandler()` line
and its 11 call sites from `router.go`. `NewInMemoryHandler` and `memRepo`
themselves stay — `internal/api/v2/artifacts/handler_test.go` still calls
`NewInMemoryHandler()` at seven places, so they remain live until S8 deletes
them there, per S8's own instructions. S8 and S9 do not add new registrations
at these paths and do not need to touch the wildcard pattern again; they
**replace** `notImplementedArtifact` at each path with the real handler,
editing the same 13 lines this stage adds.

**Also delete `internal/api/oapiserver/artifacts.go` here, not in S10.**
Regenerating `api.gen.go` from the new 13-operation spec removes the generated
param types for the 9 retired operations (`BucketListParams`,
`ArtifactListParams`, `EditBucketParams`, `DeleteBucketParams`,
`UpdateBucketPinParams`, `DeleteArtifactParams`, `DeleteArtifactsParams`).
`internal/api/oapiserver/artifacts.go` is a hand-written `ServerInterface`
implementation typed directly against those seven structs. Leave it in place
and `task openapi` produces a spec that compiles fine, but the *file* no longer
does — seven `undefined: generated.XxxParams` errors, breaking `go build ./...`
for this stage and, transitively, `task vet`/`task test`/`task build` for S7,
S8, and S9, since nothing between here and the old S10 deletion point fixes it.
This is the exact "N consecutive stages fail the global gate" failure mode the
`Backend`/`ObjectStore` split earlier in this plan exists to avoid — it just
wasn't applied to this file.

Deleting it here is safe and does not depend on S8 or S9: `oapiserver.Server`
is never wired into production routing (`oapiserver.New`/`Mount`/`Handler` are
referenced only by `internal/api/oapiserver/conformance_test.go` and
`conformance_matcher_test.go`); `internal/api/oapiserver/mount.go` is 19 lines
and registers nothing artifact-specific, so it needs no edit; and
`var _ generated.ServerInterface = (*Server)(nil)` (`server.go:68`) stays
satisfied through the embedded `generated.Unimplemented` (`server.go:17`),
which supplies a 501 stub for every operation `artifacts.go` used to implement.
Also drop the now-dead `artifactsDir` field and `ArtifactsDir` config field
from `server.go` (currently at roughly lines 30, 45, 49–51, 64) in this same
edit. **S10 no longer has this item on its list** — it was moved here.

**Delete the two now-orphaned response schemas**, `S3BucketListResponse` and
`S3ObjectListResponse` (`components/schemas`), referenced only by the old
`bucketList`/`artifactList` paths this stage removes (`$ref` at the old
`/artifacts/s3` and `/artifacts/s3/{bucket}` blocks). Nothing else points at
them. Their description prose names `internal/api/v2/artifacts/s3handler.go`,
which contains the literal substring `artifacts/s3` — leaving them in place
makes an unanchored `grep 'artifacts/s3' api/openapi/v2.yaml` still match after
a fully correct edit, failing the Verify command on complete, correct work.
Deleting them removes the last `artifacts/s3` text from the file. The Verify
grep below is also anchored to the `paths:` keys as defense in depth.

**"The two" undercounts it — replacing all 9 old operations with the 13 new
ones orphans eight more schemas, not just these two.** `Bucket`,
`BucketCreateRequest`, `BucketCreatedResponse`, `BucketUpdateRequest`,
`BucketPinRequest`, `BucketDeletedResponse`, `ArtifactUploadResponse`, and
`ArtifactDeletedResponse` are each referenced only by one of the 9 retired
operations too (confirmed by grep — nothing else in the file points at any
of them). Delete all ten, not two; the "nothing else points at them"
justification above applies identically to the other eight. The new schema
block reuses the name `Bucket` for a differently-shaped replacement, which is
safe only because the old one is deleted in the same edit — leaving both
would be a duplicate-key YAML error, not a merge.

**Acceptance criteria:** the spec declares at least 75 operations after the
swap; `go build ./...` and `go test ./internal/api/oapiserver/ -race` pass —
genuinely, because all 13 new paths now resolve to the stub and
`oapiserver/artifacts.go` no longer references a removed type; no `/artifacts/s3`
path key remains in `v2.yaml`; every route the stub replaced now returns 501
with the typed error envelope instead of 404.

**Verify:** `cd services/elitea-main && task openapi && go build ./... && go test ./internal/api/oapiserver/ -race && ! grep -qE '^  /artifacts/s3' api/openapi/v2.yaml`

---

## S8 — Bucket-plane handlers

**Preconditions:** S5, S6, S7.
**Read first:** `internal/api/v2/artifacts/handler.go`, `pg_repo.go`,
`handler_test.go`, and the 13-path stub block S7 added to
`newProductionRouter` in `internal/api/router.go`.

Rewrite the bucket half of `internal/api/v2/artifacts/handler.go` against the S6
repositories and the S1 `ObjectStore`, serving the S7 contract.

**Delete in this stage, because their replacements land here:**
`internal/api/v2/artifacts/pg_repo.go` (it queries `centry.storage_meta`, which
no Go migration creates and which is not migrated), `NewInMemoryHandler`, and
`memRepo`.

**Wire the five bucket-plane routes.** S7 already registered
`GET/POST /artifacts/buckets/{projectID}` and
`GET/PATCH/DELETE /artifacts/buckets/{projectID}/{bucket}` against
`notImplementedArtifact`. **Replace that placeholder at those five lines** with
`NewHandler(realRepo, realStore).{ListBuckets,CreateBucket,GetBucket,UpdateBucket,DeleteBucket}`
— do not add a second registration at the same path; chi panics on that. The
object-plane paths stay on the S7 stub until S9.

**`NewInMemoryHandler` has one remaining live caller after S7's edit:**
`internal/api/v2/artifacts/handler_test.go`, at seven places. Replace each with
`NewHandler(fakeRepo, fakeStore)` over two in-test fakes added in this stage:
`internal/api/v2/artifacts/fake_store_test.go` (an in-memory `ObjectStore`) and
`internal/api/v2/artifacts/fake_repo_test.go` (an in-memory implementation of
the S6 repository interface — nothing before this stage builds one; `fakeRepo`
is not a pre-existing fixture).

Response bodies:

```jsonc
// GET /artifacts/buckets/{projectID}
{"buckets": [{"name": "reports", "type": "system", "is_pinned": false,
              "tags": {}, "retention_days": 365, "expires_at": "…",
              "size_bytes": 10485760, "object_count": 42,
              "created_at": "…", "updated_at": "…"}]}

// POST, GET one, PATCH  -> the single bucket object above
// DELETE                -> 204, no body
```

`size_bytes` and `object_count` come from `SumBucketBytes` and
`CountBucketObjects` (S6) — both single-aggregate queries, never a full object
listing. Both fields are integers; formatting is the client's job.

**`PATCH` accepts `is_pinned`, `retention_days`, and `tags`, all optional and
independently settable.** Route `tags` through S6's `UpdateBucketTags` — S19's
conformance suite exercises "patch (pin, tags, retention)" as one combined
capability, and there is no method anywhere else in this plan that persists a
tag update; this is the only place it happens.

**Retention is an explicit `retention_days` integer.** Do not port legacy's
`(expiration_measure, expiration_value)` pair or its `relativedelta` calendar
arithmetic. A client that wants "one year" sends `365`. Read the limit via S6's
`GetProjectStoragePolicy(ctx, projectID).RetentionMaxDays` — **not**
`retention_default_days`, which is what applies when the caller omits a value,
not a ceiling on what they may request; the two columns exist separately in S6
precisely because they mean different things. Reject with 403 and
`code: QuotaExceeded` when `retention_days` exceeds `RetentionMaxDays`, where a
null `RetentionMaxDays` (or a missing policy row) means unlimited.

`POST` validates the bucket name against `^[a-z][a-z0-9-]{1,62}$` and rejects a
duplicate with 409 and `code: AlreadyExists`. **Do not silently normalise** —
legacy lowercased and stripped underscores, which quietly created a different
bucket than the caller asked for. Reject with 400 and `code: InvalidArgument`,
naming the rule.

Deleting a bucket deletes its objects via `ObjectStore.DeleteBatch` over a
prefix listing, then soft-deletes the metadata row.

**Acceptance criteria:** handler tests assert the exact JSON keys above for
every route; a bucket name needing normalisation is rejected, not normalised;
a retention value above the project limit produces 403 with the typed code;
`size_bytes` is an integer in every response.

**Verify:** `cd services/elitea-main && go test ./internal/api/v2/artifacts/ ./internal/api/ -race`

---

## S9 — Object-plane handlers

**Preconditions:** S5, S6, S7, **S8** — this stage's fake store and the
`NewHandler(fakeRepo, fakeStore)` constructor its own acceptance tests depend
on are introduced by S8, not created here.

This stage closes the highest-impact gap: object download and stat are
implemented in every backend and routed nowhere.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/artifacts/objects/{projectID}/{bucket}` | List. `prefix`, `delimiter`, `limit`, `cursor` |
| POST | `/artifacts/objects/{projectID}/{bucket}` | Streaming multipart upload, field `file` |
| POST | `/artifacts/objects/{projectID}/{bucket}:batchDelete` | Body `{"keys": [...]}`, per-key results |
| GET | `/artifacts/objects/{projectID}/{bucket}/*` | Streams bytes, `Range` supported |
| HEAD | `/artifacts/objects/{projectID}/{bucket}/*` | Metadata as headers |
| DELETE | `/artifacts/objects/{projectID}/{bucket}/*` | Delete one object |

**The key is the chi wildcard, not a named param.** chi v5.1.0 has no
`{name...}` syntax; a literal `{key}` matches only one path segment and 404s
the instant a key contains an internal `/`. Since S1's own key grammar
explicitly allows multi-segment keys and folder listing is a required
capability, this is not an edge case — it is the common case. S7 already
registered all six of these paths against the `notImplementedArtifact` stub,
with the three key-bearing routes on the correct `/*` wildcard pattern (not the
table's `{key}` placeholder) for exactly this reason. **Replace the stub at
each of the six lines with the real handler — do not add a second registration
at the same path** (chi panics on that) and do not change the pattern again.
Extract the key with `strings.TrimPrefix(chi.URLParam(r, "*"), "/")`, exactly
the pattern already used by the routes this stage retires
(`internal/api/v2/artifacts/{handler,s3handler}.go` both do this today).

Response bodies:

```jsonc
// list
{"objects": [{"key": "a/b.png", "size_bytes": 1024, "media_type": "image/png",
              "etag": "…", "modified_at": "…"}],
 "common_prefixes": ["a/"],
 "next_cursor": "…"}            // omitted when exhausted

// upload -> 201
{"key": "…", "size_bytes": 0, "media_type": "…", "etag": "…", "created_at": "…"}

// batch delete -> 200
{"deleted": ["a", "b"], "failed": [{"key": "c", "code": "AccessDenied", "message": "…"}]}

// single delete -> 204, no body
```

**Upload must stream.** Use `r.MultipartReader()` and pass the `file` part
directly as the `io.Reader` to `ObjectStore.Put`. `ParseMultipartForm` spills to
`os.TempDir` above its memory limit and is prohibited by ADR-0016. Read query
parameters with `r.URL.Query().Get` — **never `r.FormValue`**, which calls
`ParseMultipartForm` internally and makes `MultipartReader` return
`http: multipart handled by ParseMultipartForm`.

The server derives the object key from the logical bucket and the supplied
display name. A caller never supplies a storage path or a physical bucket.
Overwrite behaviour is an explicit `?overwrite=true|false` query parameter,
defaulting to `false`; a collision with `overwrite=false` returns 409 and
`code: AlreadyExists`.

**Do not read the display name via `Part.FileName()`.** Per RFC 7578, Go's
implementation runs it through `filepath.Base`, silently truncating a
multi-segment name like `a/b/c.png` to `c.png` — caught empirically by this
stage's own key-with-slash acceptance test, which failed against the
uploaded key until fixed. Parse `Content-Disposition` directly
(`mime.ParseMediaType(part.Header.Get("Content-Disposition"))` and read the
`filename` param) to recover the raw value. This does not reopen a
traversal hole: `storage.NewObjectRef`'s `validateKey` (S1) is the actual
guard against `..` segments, leading/trailing slashes, and empty segments —
the same class of input `FileName`'s stripping exists to block — and it
still runs on whatever key this handler passes it.

**Download** streams with `io.Copy`, sets `Content-Type` from stored metadata
falling back to extension detection, sets `Content-Length`, and supports `Range`
by passing a `ByteRange` to `ObjectStore.Get` and replying 206.

**Batch delete** takes a JSON body, not repeated query parameters, and always
returns a per-key result array. An empty `keys` array is 400 with
`code: InvalidArgument` — it never means "delete the bucket", which is what
legacy did.

**Do not emit a `file_modified` event from the server.** That event is
dispatched by the SDK's artifact tool, not by Elitea's HTTP layer. Preserving
or re-specifying it belongs to the SDK rework issue.

**Acceptance criteria:**

- `grep -rn 'ParseMultipartForm\|io.ReadAll\|FormValue' services/elitea-main/internal/api/v2/artifacts/ --include='*.go' | grep -v _test.go`
  finds nothing. Scoped to non-test files: S8's fake in-memory `ObjectStore`
  (`fake_store_test.go`) legitimately buffers with `io.ReadAll` in its own
  `Put` — it is a test double standing in for a real backend, not the
  handler code this criterion is about, and the unscoped grep would flag it
  as a false positive.
- A streaming test proves the handler does not buffer: set
  `t.Setenv("TMPDIR", t.TempDir())` and assert that directory is empty after a
  40 MiB upload; and drive the request body from an `io.Pipe` while the fake
  store records `time.Now()` on its first `Read`, asserting that timestamp
  precedes the final write to the pipe. Do **not** use `runtime.ReadMemStats` —
  GC timing and `-race` make the delta meaningless, and the test body is
  allocated in the same process.
- Download, HEAD, and range tests pass against the fake store, using the
  `NewHandler(fakeRepo, fakeStore)` constructor and fake store S8 introduces.
- **A key containing an internal `/` round-trips through the real chi router**,
  not just the fake store — e.g. upload to `a/b/c.png`, then GET, HEAD, and
  DELETE the same key through the mounted route and confirm each resolves the
  full key rather than 404ing on the first segment. This is the case the
  `/*` wildcard fix above exists to cover.
- A key containing `..` returns 400 with `code: InvalidKey`.

**Verify:** `cd services/elitea-main && go test ./internal/api/v2/artifacts/ -race`

---

## S10 — Retire `/artifacts/s3`, and the deletions

**Preconditions:** S8, S9.
**Read first:** `internal/infra/storage/storage.go`,
`internal/infra/storage/{s3,azure,gcs}/backend.go`,
`internal/api/router_security_test.go`,
`internal/api/oapiserver/conformance_matcher_test.go`.

Everything in this stage is deletion. There is no S3-proxy rewrite: ADR-0016 §7
retires the surface rather than porting it.

**Delete:**

- `internal/api/v2/artifacts/s3handler.go` and its tests.
- The prototype `/artifacts/s3/*` registrations in `internal/api/router.go`
  (the five `r.Get`/`r.Put`/`r.Delete` calls near line 241) and
  `mountArtifactS3Routes` together with its block in `router_security_test.go`.
- `internal/infra/storage/filesystem/` (whole directory).
- The old `Backend` interface and `BucketInfo` in `internal/infra/storage/storage.go`.

`internal/api/oapiserver/artifacts.go` and the dead `artifactsDir`/`ArtifactsDir`
fields in `server.go` are **not this stage's job** — S7 already deleted them,
because leaving them until here breaks S7/S8/S9's own builds. If you find them
still present, S7 was not implemented correctly; fix it there, not here.

**Also strip the old `Backend` method set from the three surviving backends.**
S3 kept `s3`, `azure`, and `gcs`'s original `Backend`-shaped methods
(`ListBuckets`, `CreateBucket`, `DeleteBucket`, `RenameBucket`, and the
old-signature `ListObjects`/`GetObject`/`PutObject`/`DeleteObject`/`StatObject`)
and their `var _ storage.Backend = (*Backend)(nil)` assertions *alongside* the
new `ObjectStore` methods, deliberately, so handlers could keep calling the old
ones through S9. Deleting `Backend`/`BucketInfo` from `storage.go` above, on its
own, breaks all three: every one of those old methods and the compile-time
assertion still references `storage.Backend` and `storage.BucketInfo`, which no
longer exist. This is not hypothetical — `go build ./...` immediately after
removing `Backend`/`BucketInfo` fails with `undefined: storage.Backend` and
`undefined: storage.BucketInfo` in all three files. Read and edit
`internal/infra/storage/{s3,azure,gcs}/backend.go` in this stage: delete the
old `Backend`-shaped methods and the `var _ storage.Backend = (*Backend)(nil)`
line from each, keeping only the `ObjectStore` methods S3 added and the
`var _ storage.ObjectStore = (*Backend)(nil)` assertion. (`filesystem/`'s
identical breakage is moot — that whole package is deleted above.)

**`internal/api/router_security_test.go` needs care.** It imports the filesystem
package for its fixture and it contains the traversal test. Repoint the fixture
at the S8 fake store, and **keep the traversal test, retargeted at the new
object routes**: the current test covers only the percent-encoded `%2e%2e` form,
which incidentally fails; add the raw `..` form, which actually escapes, and
assert 400 with `code: InvalidKey` now that the handler constructs `ObjectRef`
before any store call.

**`internal/api/oapiserver/conformance_matcher_test.go` also needs a one-line
fix, unrelated to conformance itself.** `TestMatcherProbeE_MidPatternWildcardDoesNotSwallow`
hardcodes its positive control against the literal route
`/artifacts/s3/{bucket}/deep/object/key` — a route this stage deletes. Repoint
that probe (and its assertion message) at the S7-registered
`/api/v2/artifacts/objects/{projectID}/{bucket}/*` instead; it exercises the
identical trailing-wildcard behavior and is, if anything, a better example
since it is the actual reason this plan uses that pattern. Left unfixed, this
test fails the moment the `/artifacts/s3/*` registrations are gone —
`go test ./internal/api/oapiserver/...` genuinely reds — even though the file
has nothing to do with artifact storage.

Deleting the S3 surface also deletes the only reason to build SigV4, the S3
credential API, the XML dialect, and the sub-resource dispatch table. If a stage
ever seems to need one of those, the surface has been reintroduced by mistake.

**Acceptance criteria:**

- `grep -rn 'infra/storage/filesystem' --include='*.go' services/` finds nothing.
- `grep -rn 'storage_meta\|bucket_metadata' --include='*.go' services/` finds nothing.
- `grep -rn 'NewInMemoryHandler\|memRepo\|S3Handler\|mountArtifactS3Routes' --include='*.go' services/` finds nothing.
- `grep -rn 'artifacts/s3' services/elitea-main/internal/api/router.go services/elitea-main/internal/api/router_security_test.go services/elitea-main/internal/api/v2/artifacts/ services/elitea-main/internal/api/oapiserver/` finds nothing.
  **Scoped deliberately to what this stage owns.** `internal/api/main_public_rules.go`,
  `main_public_rules_test.go`, and `production_router_test.go` also contain the
  literal string `artifacts/s3` at this point — removing it there is S11's job,
  paired with a specific test re-anchor, and S11 runs strictly after this
  stage. A broader `grep -rn 'artifacts/s3' services/elitea-main/` is
  **unsatisfiable at the end of S10 as a standalone check** — it can only pass
  once S11 has also run. Do not widen this grep; check S11's own acceptance
  criteria for that file instead. (`.yaml` is excluded for the same reason S7
  already cleared `/artifacts/s3` from `v2.yaml`.)
- `grep -rn 'storage\.Backend\b\|storage\.BucketInfo\b' services/elitea-main/internal/infra/storage/` finds nothing.
- A raw `..` key returns 400, not 200 and not 404.

**Verify:** `task test && task build`

---

## S11 — Production mount, auth, RBAC

**Preconditions:** S10.
**Read first:** `internal/api/router.go`, `production_router.go`,
`main_public_rules.go`, `main_public_rules_test.go`, `router_security_test.go`,
`internal/api/oapiserver/conformance_test.go`.

**Extract a single `mountArtifactRoutes(r chi.Router, deps ArtifactDeps)` and
call it from both `newProductionRouter` and `production_router.go`'s
`NewRouter`.** (#243 later deleted `NewRouter`'s own inline build — it was
dead code, unreachable in every real deployment — and made it delegate to
`newProductionRouter` unconditionally, so `mountArtifactRoutes` now has a
single call site inside `newProductionRouter`.) Registering only in
production breaks the oapiserver conformance test, which walks the prototype
router. One shared mount function satisfies both.

- Apply `apimw.Auth` plus `apimw.RequireResolvedPermissionsForProject` to
  **every** route. The current `/api/v2/artifacts/*` routes have authentication
  but no permission check at all.
- Permission mapping, using the existing
  `configuration.artifacts.artifacts.{view,create,edit,delete}` catalog:
  GET and HEAD to `view`, POST upload and bucket create to `create`, PATCH to
  `edit`, DELETE and `:batchDelete` to `delete`, grant creation to `create`.
  The `edit` permission is easy to miss — legacy uses it for retention and pin
  changes, and wiring those to `create` or `delete` grants or denies the wrong
  principals.
- **Remove** `^/artifacts/s3/.*$` from `internal/api/main_public_rules.go`, and
  **in the same commit** delete the `current.artifacts.s3` entry from the `want`
  table in `main_public_rules_test.go` and re-anchor
  `TestCurrentMainRoutePublicRulesReturnsDetachedCatalog` on the new index-0
  rule, `current.admin_ui.assets`. Both tests iterate by index and fatal on a
  length mismatch, so removing the rule alone reds `task test`. No artifact
  route is ForwardAuth-exempt; the surface that justified the exemption is gone.
- Delete `TestLegacyS3CompatibilityRoutesAreNotProductionMounted`, which pins
  the dead-code state, and replace it with a test asserting the new routes
  **are** mounted and **do** require authentication and permission.
- **Do not let the artifact routes inherit the shadow middleware.**
  `internal/api/shadow/middleware.go` wraps `http.ResponseWriter` without an
  `Unwrap` method and buffers the whole response into a `bytes.Buffer`. It is
  installed on the entire `/api/v2` group in the prototype router. Mount the
  artifact routes outside it, or the download path buffers every object in
  memory and `ResponseController` deadlines stop working.

**Acceptance criteria:**

- Every artifact route returns 401 unauthenticated and 403 with a valid
  principal lacking the permission. The 11 routes with a real handler by this
  point (5 from S8, 6 from S9) additionally return 2xx with the right
  permission. **The two grant routes are excluded from the 2xx check** — S15,
  which implements them, hasn't run yet, so they correctly still return 501
  regardless of authorization outcome; only their 401/403 gating is asserted
  here.
- A PATCH with only `create` permission returns 403; with `edit` it succeeds.
- A request for project 8's object with a principal scoped to project 7 returns
  403 — tested end-to-end through the router, not at the store layer.
- `grep -rn 'artifacts/s3' services/elitea-main/internal/api/` finds nothing.

**Verify:** `cd services/elitea-main && go test ./internal/api/... -race`

---

## S12 — Limits

**Preconditions:** S6, S11.

- Wrap every upload route body in `http.MaxBytesReader`, limit sourced from
  `elitea_storage.project_storage_policy.max_object_bytes` falling back to
  `ARTIFACT_MAX_OBJECT_BYTES` (default 150 MiB, matching the legacy
  vault-sourced `chat_max_file_upload_size_mb`).
- **Replace the global `ReadTimeout: 10 * time.Second` with
  `ReadHeaderTimeout: 10 * time.Second`** and set per-request body deadlines via
  `http.ResponseController.SetReadDeadline` in the upload handlers. `ReadTimeout`
  bounds reading the entire request including the body, so it is the real upload
  ceiling today — well below any size limit.
- **Do the same for the write side.** `WriteTimeout: 120 * time.Second` on the
  same lines caps every *download* at 120 seconds regardless of object size,
  which S9 has just made a live route. Set per-request write deadlines with
  `http.ResponseController.SetWriteDeadline` on the download route
  (`GET /artifacts/objects/{projectID}/{bucket}/*`, S9). There is no S3-GET
  route by this stage — S10 already retired `/artifacts/s3`.
- `ResponseController` walks `Unwrap()`. The OTel middleware's recorder
  implements it; the shadow middleware's does not — which is why S11 keeps the
  artifact routes off the shadow path.
- Enforce the project quota on the write path against
  `project_storage_policy.max_total_bytes` compared with S6's
  `SumProjectBytes(ctx, projectID)` — **not** `SumBucketBytes`, which is
  single-bucket and cannot be summed across a project by looping (see S6) —
  returning 413.

**Prerequisite gap found while implementing the bullet above, not stated
anywhere in S8/S9's text: nothing ever wrote to `elitea_storage.objects`.**
S9's object-plane handlers call `ObjectStore.Put`/`Delete`/`DeleteBatch`
directly against the physical backend and never call S6's
`UpsertObject`/`DeleteObjects` against the metadata table those aggregates
read from. `SumBucketBytes`, `SumProjectBytes`, and `CountBucketObjects` —
S8's own `size_bytes`/`object_count` bucket fields, not just this stage's
quota check — have been silently returning zero for every real upload since
S9 shipped; nothing in S8 or S9's acceptance criteria caught it because
their fakes never modeled the metadata table as the write path's
responsibility. A `SumProjectBytes` comparison against a total that never
moves off zero is not a partial implementation of this bullet, it is a
no-op with no way to ever return 413. Fix in this stage, not a follow-up:
`UploadObject` calls `UpsertObject` after a successful `Put` (using the
resulting `ObjectInfo`, since the exact byte length isn't known until the
stream is fully read — this is also why the quota check itself runs after
the write and rolls back on violation, not before); `DeleteObject` and
`BatchDeleteObjects` call `DeleteObjects` for whatever the physical delete
actually removed.

**The `http.Server{ReadTimeout, WriteTimeout}` edit lives in `cmd/elitea-main/main.go`
(`package main`), which nothing under `internal/api/v2/artifacts` can import —
not just by convention, but as a genuine Go import cycle
(`artifacts → ... → cmd/elitea-main → internal/api → internal/api/v2/artifacts`).
This stage's own Verify command, as scoped, cannot exercise that edit at all**
— a fully correct-looking `-run 'Limit|Quota|Deadline'` pass in that package
proves nothing about whether `ReadTimeout`/`WriteTimeout` were actually
touched. Extract the server construction into a small, testable, same-package
helper — `func newHTTPServer(handler http.Handler) *http.Server` in
`cmd/elitea-main/` — and add `cmd/elitea-main/http_server_test.go` asserting
`ReadHeaderTimeout` is set and `ReadTimeout`/`WriteTimeout` are zero. This is
the only way the ceiling change is checked by anything, since no later stage
touches `cmd/elitea-main` either.

**Known, deliberately deferred gap: `DeleteBucket`'s cascade
(`deleteAllObjects`, S8) deletes every physical object via
`ObjectStore.DeleteBatch` but never calls the metadata-layer `DeleteObjects`
for the same keys, orphaning their `elitea_storage.objects` rows.** This is
not a quota-correctness bug — every aggregate query this stage and S8 rely
on (`SumProjectBytes`, `SumBucketBytes`, `CountBucketObjects`) joins through
`elitea_storage.buckets` and filters `deleted_at IS NULL`, so a soft-deleted
bucket's orphaned object rows are already excluded regardless of whether
they still physically exist. It is a real data-hygiene gap (unbounded row
growth) worth closing alongside S14's sweeper, which already touches
expired-row cleanup — not this stage's job, since it doesn't affect
anything S12 needs to be correct.

**Acceptance criteria:** a 200 MiB upload returns 413 with a JSON error body,
not a truncated stream or a connection reset; a 100 MiB upload over a 30-second
body succeeds; a write that would push a project's `SumProjectBytes` total
past `max_total_bytes` returns 413 even though the individual object is under
`max_object_bytes` — this is a materially different code path from the
per-object cap and needs its own test, not just a bullet in the requirements;
`newHTTPServer`'s returned config has `ReadHeaderTimeout` set and
`ReadTimeout`/`WriteTimeout` unset — this is what actually stands in for
"a 300-second download completes," since nothing in this stage's own test
scope can drive a real 300-second HTTP round trip.

Name every new test `TestArtifactLimit*` — the current filter
(`-run 'Limit|Quota|Deadline|Timeout'`) collides with a genuinely unrelated,
already-passing test, `cmd/elitea-main/main_test.go`'s
`TestServeApplicationReturnsAtOneSharedDrainDeadline` (a graceful-shutdown
test, matched via the substring "Deadline"), so the bare command reports a
full pass today with zero S12 code written. A distinctive prefix removes both
that collision and the separate zero-match risk.

**Verify:**
```
cd services/elitea-main && go test ./internal/api/v2/artifacts/ ./cmd/elitea-main/... -race -run TestArtifactLimit -v > /tmp/s12.log 2>&1; rc=$?; cat /tmp/s12.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s12.log && ! grep -q -- '--- SKIP' /tmp/s12.log
```

---

## S13 — Project-creation bucket bootstrap function (unwired)

**Preconditions:** S6, S8.

Legacy creates `reports` and `tasks` with `bucket_type='system'` on project
creation and deletes both on project deletion. Without this, every project
created by the Go service has no system buckets — and the legacy on-disk tree
confirms both exist for every project.

**There is no project-creation or project-deletion path to hook, anywhere in
this Go service, today.** Verified: no `CreateProject`/`DeleteProject`
handler exists (`internal/api/v2/projects/handler.go` implements only
`GetProject`, `GetCurrentProjectList`, `GroupList`, `PutProjectGroups`); no
`internal/application/projects`-shaped package exists at all; and the
`projects` table itself has no `CREATE TABLE` in any migration under this
service — it is owned and written by something outside `elitea-main`
entirely. S6 and S8 build no such hook either. This stage's original framing
("hook the project-creation path") describes a mechanism that does not exist
in this codebase, and no stage before it creates one.

**Scope this stage down accordingly: build the bootstrap and teardown logic
as an idempotent, directly callable, fully-tested function — do not claim it
is wired into production, because it cannot be.**

```go
func BootstrapProjectBuckets(ctx context.Context, projectID string) error
func TeardownProjectBuckets(ctx context.Context, projectID string) error
```

`BootstrapProjectBuckets` creates `reports` and `tasks` with
`bucket_type='system'`, idempotently (a second call is a no-op, not an
error). `TeardownProjectBuckets` soft-deletes both and purges their objects.
Unit-test both directly against the S6 repository — no HTTP layer, no
project-lifecycle integration, because none exists to integrate with.

**Leave an explicit, visible gap, not a silent one.** Add a code comment on
`BootstrapProjectBuckets` stating it is not called from anywhere yet, and
record the open question this raises: an owner needs to identify the actual
project-creation and project-deletion trigger — a legacy webhook, an event
this service should subscribe to, a database trigger, or something not yet
designed — before this function does anything in a running system. Until
that's answered, every project created after this migration ships has no
system buckets, exactly as today.

**Acceptance criteria:** `BootstrapProjectBuckets` called twice for the same
project creates exactly one `reports` and one `tasks` bucket, both
`bucket_type='system'`; `TeardownProjectBuckets` removes both and leaves no
objects. **The criterion is scoped to the function's own behavior in
isolation** — this stage does not and cannot assert "a newly created project
lists exactly reports and tasks" in the running service, because nothing
creates a project through this service yet. **Name every test function in
this package with a `TestArtifact` prefix** (e.g.
`TestArtifactBootstrapCreatesReportsAndTasksIdempotently`) — see the naming
rule in S6, which this stage's Verify command relies on for the same reason.

**Verify:**
```
cd services/elitea-main && go test ./internal/application/artifactbootstrap/... -race -run TestArtifact -v > /tmp/s13.log 2>&1; rc=$?; cat /tmp/s13.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s13.log && ! grep -q -- '--- SKIP' /tmp/s13.log
```

**Defect found and fixed during implementation, empirically confirmed against a
real Postgres database:** the first-draft `purgeObjects` returned immediately
on any `ObjectStore.DeleteBatch` failure (a top-level error, or any
`result.Failed` entries) without ever calling `Repository.DeleteObjects` for
the objects that *did* delete in that same call. Confirmed with a
reproduction test forcing one of two keys to fail: the successfully-deleted
object's physical bytes were gone, but its `elitea_storage.objects` metadata
row survived — and unlike the already-documented, already-deferred
`DeleteBucket`-cascade gap (S12), the bucket here stays active (not
soft-deleted) on a partial failure, so that orphaned row keeps counting
toward the project's quota indefinitely, and a retry can never rediscover the
key because `List` no longer returns it. Fixed by calling
`Repository.DeleteObjects` with `BatchResult.Deleted` unconditionally before
inspecting `batchErr`/`Failed`. Regression test:
`TestArtifactTeardownPreservesMetadataConsistencyOnPartialDeleteFailure`.

**Second defect found and fixed:** `parseProjectID` accepted project-ID
strings (`"007"`, `"+9001"`, a 19-digit `int64`) that
`storage.NewBucketRef`'s `projectIDPattern` regex rejects.
`BootstrapProjectBuckets` would happily create real bucket rows for such an
ID; `TeardownProjectBuckets` would then permanently fail to build a
`BucketRef` for the identical string, with no way to ever tear the buckets
back down through this package. Fixed by having `parseProjectID` delegate to
`storage.NewBucketRef` for validation instead of duplicating (and drifting
from) its rule. Regression coverage added to
`TestArtifactBootstrapRejectsInvalidProjectID`.

Both were caught by a Workflow adversarial-review pass (plan-compliance +
bug-hunt + test-integrity reviewers) run before committing, not by the
original test suite — the bug-hunt reviewer reproduced the first defect with
a mutation test before any fix existed. A third finding from the same pass
(the required "not called from anywhere yet" comment lived on the
`Bootstrapper` struct rather than on `BootstrapProjectBuckets` itself, as
this section asks for by name) was also corrected; a fourth
(`fakeObjectStore.List` never paginated, leaving `purgeObjects`'s
continuation-token loop completely untested) was closed by adding
`pageLimit`/`failKeys` test-only knobs and
`TestArtifactTeardownPurgesObjectsAcrossMultiplePages`.

---

## S14 — Retention ledger and sweeper

**Preconditions:** S6, S8, **S9** — stamping `expires_at` on written objects
means editing the upload path, which S9 introduces; it does not exist after S8
alone.
**Read first:** `internal/api/v2/artifacts/handler.go` (bucket create/retention
update, S8) and the object-write path (S9); the S6-created
`internal/infra/db/repos/artifact_{buckets,objects}.go`;
`internal/runtimecomposition/index_schedule_due_work*.go` and
`index_runtime.go` for the sweeper *shape*; **and
`internal/runtimecomposition/composition.go`, specifically the
`schedulingapp.Registry`/`newPublisherSet` chain around line 985** — this is
where the pattern files' shape actually gets registered into the running
scheduler, and neither pattern file references it. Skipping this file is why
an otherwise-correct sweeper never runs in production.

This stage has four distinct pieces of work. Each needs its own acceptance
check — a Verify command scoped to one package, as below, structurally cannot
exercise all four:

1. **Stamp `expires_at`.** On bucket create and retention update, compute
   `expires_at` for the bucket (edit in `internal/api/v2/artifacts/handler.go`,
   S8's package). Stamp `expires_at` on objects written into it (edit in the
   S9 upload path, same package). Neither edit touches
   `internal/runtimecomposition` at all.
2. **The sweeper `Handler`.** Follow the shape in
   `index_schedule_due_work*.go`/`index_runtime.go`: selects
   expired objects in bounded batches via S6's new `ListExpiredObjects`,
   deletes them through `ObjectStore.DeleteBatch`, and removes the metadata
   rows via `DeleteObjectRows` in the same transaction boundary as the delete
   acknowledgement.
3. **Wire the sweeper into production — the pattern files alone do not do
   this.** `schedulingapp.Job{...}` is registered exactly once in the whole
   codebase, inside `composition.go`'s `New(...)`, accumulated into
   `publisherRoot` via repeated `newPublisherSet(...)` calls. Add the
   retention-sweeper `Job` to that same chain. Without this edit, a fully
   correct `Handler` compiles, unit-tests green, and never runs — objects past
   `expires_at` are never deleted and the notification never fires, in a
   running service, indefinitely. This is the same class of gap S15 already
   flags for its own routes ("no other stage catches the gap... a
   handler-level Verify command can pass while the route itself is still
   unwired") — the identical failure mode, one layer down, in the scheduler
   instead of the router.
4. **The notification.** A real, wired notification mechanism already exists —
   don't invent a new one and don't just log. `internal/infra/db/repos/
   index_schedule_notification.go`'s `InsertCurrentIndexScheduleNotification`
   writes a row into `centry.notifications` (`uuid`, `is_seen`, `project_id`,
   `user_id`, `meta`, `event_type`), which `internal/api/v2/notifications/
   events.go` already streams to the browser over the existing
   `notifications_notify` Socket.IO event — no new transport is needed, only a
   new writer following that file's construction pattern (a new sqlc query, a
   new `event_type` value, e.g. `'artifact_bucket_expiring'`). Emit one insert
   per bucket with one day of `expires_at` remaining, sourced from S6's
   `ListBucketsNeedingExpiryNotice`, deduplicated by calling
   `MarkBucketNotified` once sent.
   **This table's `user_id` column is `NOT NULL`, and every existing read path
   is scoped `WHERE user_id = ...` — there is no project-wide broadcast row.**
   `elitea_storage.buckets` (S6) has no owning-user column, only `project_id`,
   and this plan defines no project→member-list lookup anywhere a bucket
   notification could iterate over. Do not invent one. Pick one of: (a) look
   up the bucket-creating user if S8's create handler is changed to record
   one (a schema change this plan does not currently make), or (b) find and
   reuse an existing project-membership enumeration elsewhere in the
   codebase if one exists, and if you cannot find one, treat this as an open
   question for a human owner and land the sweeper/repository-method work
   (items 1-3, 5) without the notification insert rather than guessing at a
   `user_id`.
5. **Multipart-abort lifecycle rule.** Provider-native lifecycle rules are
   **not** used for per-bucket retention (see item 2). Set one coarse
   provider-native rule aborting incomplete multipart uploads after 7 days —
   this is a one-time backend/factory-level configuration call (S3's package),
   not sweeper logic, and belongs in whichever of `s3`/`azure`/`gcs`
   `New(...)` already runs at startup.

**Acceptance criteria:**

- An object with `expires_at` in the past is gone from both the object store
  and the metadata table after one sweeper tick.
- If the `user_id` open question above is resolved, a `centry.notifications`
  row with `event_type = 'artifact_bucket_expiring'` is inserted exactly once
  per bucket per expiry cycle, and is visible through the existing
  `internal/api/v2/notifications/events.go` stream — not just that
  `MarkBucketNotified` was called, which a no-op stub can also satisfy. If the
  open question is left unresolved for a human owner, this criterion is
  dropped for this landing and the open question is recorded instead of
  silently skipped.
- **A composition-level test asserts the sweeper `Job` is actually present in
  `composition.go`'s registry** — not just that the standalone `Handler`
  passes its own unit test. This is the check that catches item 3 being
  skipped.
- A newly created bucket with `retention_days: 30` has `expires_at` set on
  itself and on every object subsequently uploaded into it — checked through
  the real S8/S9 handlers, not by calling the sweeper's repository methods
  directly.
- The configured backend reports an active multipart-abort lifecycle rule
  after startup.
- **Every new test function in this stage's scope is named with a
  `TestArtifactRetention` prefix.** A bare `-run Retention` collides with
  three genuine, unrelated, already-passing tests in
  `internal/runtimecomposition` (`TestExecutionReplayRetentionJanitor*`, about
  execution-history replay, nothing to do with artifact storage) — confirmed:
  that filter alone reports a clean PASS today, before any S14 code exists.

**Verify:** both halves must be guarded — a bare `go test` at the end of the
chain exits 0 on zero matches just like the first half would, and the second
half is what covers 4 of this stage's 5 work items (everything except item 1).
```bash
cd services/elitea-main && \
  go test ./internal/runtimecomposition/ -race -run TestArtifactRetention -v > /tmp/s14a.log 2>&1; rc=$?; cat /tmp/s14a.log; \
  test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s14a.log && ! grep -q -- '--- SKIP' /tmp/s14a.log && \
  go test ./internal/api/v2/artifacts/... ./internal/infra/storage/... -race -run TestArtifactRetention -v > /tmp/s14b.log 2>&1; rc=$?; cat /tmp/s14b.log; \
  test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s14b.log && ! grep -q -- '--- SKIP' /tmp/s14b.log
```

**S6 already anticipated every repository method items 2 and 4 need**
(`ListExpiredObjects`, `DeleteObjectRows`, `ListBucketsNeedingExpiryNotice`,
`MarkBucketNotified` all already existed, each already documenting "the
retention sweeper (S14) is this method's only caller"). One gap remained:
`ObjectRow` carries only `bucket_id`, not `project_id`/bucket name, so a
sweep across every project (no request-bound scope) had no way to build a
physical `ObjectRef` for an expired object. Added `GetArtifactBucketByID` (a
new sqlc query + `ArtifactBucketsRepository.GetBucketByID`) to resolve it.

**The sweeper Handler follows the `schedulingapp.Job` pattern
(`currentIndexScheduleDueWork`'s shape), not the simpler self-ticking
`publisherRunner` pattern `execution_replay_retention.go` also uses in this
codebase** — both are real, pre-existing patterns; the plan's explicit
"Follow the shape in `index_schedule_due_work*.go`" instruction
picks the former. It's registered into the **same** registry/scheduler
instance the index-scheduling job already uses (a second `schedulingapp.Job`
argument to the same `schedulingapp.NewRegistry` call), not an independent
scheduler with its own instance ID/lease/pool — the plan's "add the
retention-sweeper Job to that same chain" wording. **Consequence, stated
plainly:** the sweeper only runs when `IndexSchedulingEnabled` (and by
extension `IndexIngestDispatchEnabled` and the whole optional
`runtimecomposition` runtime) is enabled. A deployment running artifact
storage without that optional runtime gets no automatic sweeping — the
S8/S9 upload/retention paths still stamp `expires_at` correctly regardless,
but nothing deletes past it. Building an independent scheduler subsystem
just for this stage was judged out of proportion; flagged here as an open
question for a human owner rather than silently accepted.

**Composition-level test, honestly scoped.** No test anywhere in this
codebase invokes `runtimecomposition.New(...)`'s full dependency graph
end-to-end (Redis, gRPC TLS, signing keys, five Postgres pools — confirmed
by grep, none exists). Building one from scratch for this one assertion was
judged disproportionate. Instead, the index-job and retention-job
`schedulingapp.Job{...}` construction was extracted into
`scheduledJobs(...)` and the `schedulingapp.NewRegistry(...)` call into
`scheduledJobRegistry(...)` (`internal/runtimecomposition/
scheduled_job_registry.go`) — `New()` calls both, and
`TestArtifactRetentionScheduledJobsIncludesBothIndexAndSweepJobs` calls the
exact same functions directly, asserting via the newly-exported
`Registry.RegisteredJobs()` that both job IDs are present. This catches "a
correctly implemented, unit-tested Handler that never reaches the registry"
— exactly the gap class this stage's plan text calls out — provided `New()`
keeps routing through these two functions rather than reconstructing the
job list inline again. It does not catch "`New()` stops calling
`scheduledJobs()` at all"; a full end-to-end test would, but at a cost this
stage's scope did not justify.

**The `user_id` open question (S14 item 4) is resolved via
`centry.project.owner_id`** — option (b) from the plan text ("find and
reuse an existing project-membership enumeration"), not option (a) (S8's
`CreateBucket` was not changed to record a creating user; that remains a
schema change this plan does not make). New query
`GetArtifactBucketOwningProjectUserID` reads `centry.project.owner_id`
directly; no new schema. This notifies the project's single owner, not
every project member (`auth_core__project_user_role` would give the full
member list, if that's ever wanted instead).

**`UpdateArtifactBucketRetention` now resets `notified_at` to `NULL`** —
found while implementing the notification path: without this, a bucket
already notified once, whose retention is later extended (a new
`expires_at`), would never be eligible for a fresh notification, since
`ListArtifactBucketsNeedingExpiryNotice`'s `notified_at IS NULL` filter
would permanently exclude it. The notification UUID is derived from
`(bucketID, expiresAt)`, not `bucketID` alone, for the same reason: stable
across a retried tick within one cycle, distinct across cycles, so a
renotification under a new `expires_at` doesn't silently collide with (and
get dropped by) the earlier notification's `ON CONFLICT (uuid) DO NOTHING`.

**Multipart-abort lifecycle rule (item 5): S3 and GCS get the call; Azure
does not, deliberately.** `PutBucketLifecycleConfiguration`
(S3)/`Bucket.Update(Lifecycle)` (GCS) are **not** called from `New()` —
first-draft did that, then broke `cmd/elitea-main`'s
`TestArtifactNewObjectStoreDispatchesPerBackend`, whose GCS subtest's own
comment ("this asserts a concrete type rather than tolerating a credential
error") documents a deliberate pre-existing invariant: `New()` must stay a
cheap, non-network-dialing constructor. Extracted into a
`ConfigureRetentionLifecycle(ctx) error` method on each backend instead,
called explicitly from `main.go` right after `objectStoreReadinessProbe`
via an optional-interface check (`azure.Backend` doesn't implement it, so
`main.go` skips it via a type assertion, not a special case). Confirmed
empirically: RustFS both accepts and round-trips the S3 rule (a live
`GetBucketLifecycleConfiguration` after `ConfigureRetentionLifecycle`
reports it — see `s3/lifecycle_test.go`); fake-gcs-server accepts the GCS
call without erroring but does **not** persist it
(`Bucket.Attrs().Lifecycle` stays empty afterward) — a known emulator
limitation, not evidence against real GCS, and not independently
verifiable in this stack. Azure gets no call at all: the data-plane SDK
already used here has none (lifecycle policies are an Azure Resource
Manager/management-plane resource, a different SDK needing a subscription
ID, resource group, and storage-account resource ID `azure.Config` does not
carry), and Azurite exposes no ARM endpoint — architecturally untestable
here, not just an emulator gap. Azure's own default ~7-day
uncommitted-block garbage collection already delivers the same outcome
with zero explicit configuration, which is why this was judged not worth
the added configuration surface.

**Out-of-scope discovery, not fixed here:** running `internal/infra/db/repos`'s
Postgres integration tests against a real database (not part of this
stage's own surface) surfaced 7 pre-existing failures unrelated to this
plan — confirmed via `git log` that the affected files were last touched by
unrelated indexing/CI commits. Root cause for at least one:
`configuration_validation_postgres_integration_test.go`'s `centry.project`
stub declares `create_success`/`suspended` `NOT NULL` with no `DEFAULT`,
but `TestTenantBoundariesRejectMissingLegacySchema` inserts a row omitting
both columns. Invisible in CI since `ci-go.yml` never sets
`ELITEA_TEST_DATABASE_URL` or runs a Postgres service for this job, so
these tests always skip in the pipeline. Flagged as a follow-up task rather
than fixed inline — unrelated to artifact storage. (Fixed in a later,
separate commit, `8041a1f`, alongside wiring a real Postgres service into
`ci-go.yml` so this package's integration tests stop skipping in CI —
`9b60898`. Neither commit is otherwise part of this plan's stage sequence.)

**Horizontal-scaling audit (added after S16, prompted by an explicit
request to verify ADR-0016's "any number of `elitea-main` replicas"
requirement holds):** confirmed the sweeper is safe under multiple
concurrently-running replicas, and that this was never something S14 had
to build — it piggybacks on the exact same `schedulingapp.Registry`/`Runner`
the pre-existing index-scan job already uses
(`internal/runtimecomposition/scheduled_job_registry.go`), and that shared
framework's cross-replica correctness boundary is Postgres-native: occurrence
claims use `FOR UPDATE OF occurrence SKIP LOCKED` plus a fenced
`lease_epoch`/`claim_fence` UPDATE
(`internal/db/queries/scheduled_occurrences.sql`,
`internal/infra/db/repos/schedule_occurrences.go`), not an in-process lock
of any kind — see `internal/application/scheduling/runner.go`'s own doc
comment: "Every Main replica may run it: PostgreSQL planning and occurrence
leases provide logical ownership and takeover." `MaxParallel:1`/`PageSize:1`
per tick means only one occurrence is guaranteed exclusive per replica-tick;
if the sweeper falls behind, two different due occurrences of the same job
could in principle be claimed by two different replicas concurrently — but
the sweep's own business logic tolerates this by construction, not by
avoiding it: `sweepExpiredObjects`'s delete calls are idempotent no-ops on
an already-deleted row, and `notifyExpiringBuckets`'s
`NotifyBucketExpiring` derives a deterministic UUID from
`(bucketID, expiresAt)` with `ON CONFLICT (uuid) DO NOTHING`
(`artifact_retention_notification.go`), so a duplicate concurrent
notification insert produces zero rows rather than a duplicate delivery.
No code change needed; recorded here since it was checked, not assumed.

---

## S15 — Presigned transfer grants

**Preconditions:** S3, S6, S11.

**Wire the two grant routes.** S7 registered
`POST /artifacts/grants/{projectID}/{bucket}` (`createTransferGrant`) and
`POST /artifacts/grants/{projectID}/{grantID}:commit` (`commitTransferGrant`)
against the `notImplementedArtifact` stub in `internal/api/router.go`, and no
stage before this one replaces them — S8 and S9 explicitly touched only their
own 5 and 6 lines. **Replace the placeholder at those two lines with the real
handlers** — do not add a second registration at the same path (chi panics on
that). Without this, the two endpoints stay permanently 501 in production; no
other stage catches the gap, since this package (`internal/api/v2/artifacts`,
`package artifacts`) cannot reach `internal/api/router.go` (`package api`)
from its own tests, so a handler-level Verify command can pass while the route
itself is still unwired. Add a small router-level test in `internal/api/`
(e.g. `internal/api/grant_routes_test.go`) that builds the router and asserts
both grant paths resolve to something other than the stub — this is the only
way to actually exercise the replacement, since `internal/api/v2/artifacts`'s
own tests exercise the handler directly and never touch the mounted route.

Add `POST /api/v2/artifacts/grants/{projectID}/{bucket}` returning a
short-lived presigned URL bound to a server-derived key, persisted in
`elitea_storage.transfer_grants`. **The request body carries the caller's
expected `digest_alg`/`digest` (and content type and size) for the object it
is about to upload** — this is not optional decoration. The grant row's
`digest`/`digest_alg` columns exist so commit has something to check the
uploaded bytes *against*; nothing else in this plan populates them, so if the
caller doesn't supply an expected digest at grant-creation time, commit has
no digest to compare and the "mismatched digest returns 409" criterion below
is unimplementable. Reject a grant request with no digest when the caller
declares one is required (media types where integrity matters); allow it to
be optional for cases where digest verification genuinely isn't needed.

The grant response carries the URL, the required method, the expiry, and the
required content type and maximum size. It does **not** carry the physical
bucket, the backend URL, or any credential. The caller may request a display
name and a logical bucket; the server chooses the key.

`POST /api/v2/artifacts/grants/{projectID}/{grantID}:commit` verifies size,
digest, and media type against the grant row before the object becomes
listable, then stamps `consumed_at`. Media type verification at commit is
**mandatory**, not defensive: `ContentType` is not part of the signed
presigned-PUT payload, so the URL alone does not enforce it.

**Digest verification cannot be done with `Stat`/`HeadObject` alone — this is
not an implementation shortcut to find, it is a real property of presigned
uploads that must be designed around.** Confirmed empirically against a real
RustFS instance: a presigned PUT followed by `HeadObject` returns an `ETag`
that is the object's content-MD5 (not SHA-256, and not even the same
algorithm S1's `ObjectInfo.DigestSHA256` names), and `ChecksumSHA256` is
`nil` — including with `ChecksumMode: ENABLED` and via
`GetObjectAttributes`. No cheap provider-side call yields a SHA-256 for an
object that landed via presigned PUT, on any backend. **The commit handler
must call `Get` on the object and stream it through a SHA-256 hasher to learn
the real digest — there is no shortcut.** This costs one full read of the
object from the store to `elitea-main` at commit time. That is a real,
deliberate cost of verifying integrity on a path designed to keep bulk bytes
off `elitea-main` during upload; it is not paid during the upload itself, and
it is bounded to once per commit, not once per read. If that cost is judged
unacceptable for large objects, the alternative is to relax commit to
size-and-media-type-only and drop digest verification — but that is a
material change to what ADR-0016 promises ("Go verifies size, digest, and
media type on commit") and should be a deliberate decision, not a silent
downgrade because the digest check turned out to be inconvenient to
implement.

Default TTL 15 minutes, maximum 60. `PUT` grants are single-use.

**Fall back to the streaming facade when `Capabilities().Presign` is false** —
an Azure client using workload identity, or a GCS client with no signing
material. Return the facade URL, not an error.

**Acceptance criteria:** a presigned `PUT` followed by a commit produces a
listable object; a commit with a mismatched digest returns 409 and the object is
deleted; a commit with a mismatched media type returns 409; an expired grant URL
returns 403 from the provider; a second commit on the same grant returns 409;
**both grant endpoints resolve through the real chi router**, not just the
handler under test — a request to the mounted route returns something other
than 501 with `code: NotImplemented`, matching the pattern S9 already requires
for its own routes. **Name every new test function with a
`TestArtifactGrant` prefix.** Both `-run Grant` target packages
(`internal/api/v2/artifacts` and `internal/api`) currently have zero tests
matching that substring, so an unimplemented S15 also reports
`[no tests to run]`, exit 0 — indistinguishable from success without a
required-match guard.

**Verify:**
```bash
cd services/elitea-main && \
  go test ./internal/api/v2/artifacts/ -race -run TestArtifactGrant -v > /tmp/s15a.log 2>&1; rc=$?; cat /tmp/s15a.log; \
  test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s15a.log && \
  go test ./internal/api/ -race -run TestArtifactGrant -v > /tmp/s15b.log 2>&1; rc=$?; cat /tmp/s15b.log; \
  test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s15b.log
```

**Design decisions made during implementation, not fully pinned down by the
plan text above:** the grant's storage key is always the grant ID itself
(never a caller-derived name) — this is what lets `looksLikeGrantID` reject
an obviously-malformed `grantID` path parameter before it ever reaches a
`::uuid`-casting query. `display_name` is accepted per the schema but has no
effect for exactly that reason. TTL is fixed at 15 minutes with no
per-request override, since `CreateTransferGrantRequest` has no TTL field —
the plan's "maximum 60" is dead text with nothing to cap. Only `sha256` is
accepted as `digest_alg`; commit streams the object through a hasher exactly
once (`io.Copy` into `sha256.New()`), matching the plan's explicit
"there is no shortcut" instruction. Any commit-time rejection (size, digest,
media type, quota) deletes the uploaded object outright rather than leaving
it orphaned for the S14 sweeper to eventually collect — a failed grant commit
should not require a sweep cycle to clean up. A grant is always an upload:
`method` accepts `PUT` only. `GET` was accepted at first and is now refused
with 400, because a download grant could never work — the key a grant
reserves is a newly generated grant id, no request field names an existing
object, and the presigned download URL therefore addressed an empty key and
answered 404 every time. Download an object through
`GET /artifacts/objects/{projectID}/{bucket}/{key}` instead.
`CommitTransferGrant` still rejects a non-`PUT` grant with 400, for rows
written before that change.
`DigestMismatch` and `MediaTypeMismatch` were added as new `Error.code` enum
values — 409 was already used for `AlreadyExists`, but ADR-0016 and this
plan both treat digest and media-type failures as distinct, typed outcomes a
caller should be able to switch on, not a generic conflict.

**Two blocking defects found and fixed by a Workflow adversarial-review pass
(plan-compliance + bug-hunt + test-integrity reviewers) run before
committing, not by the original test suite:**

1. The first-draft media-type check was `info.ContentType != "" &&
   info.ContentType != grant.ContentType` — the `!= ""` guard let a backend
   that reports no `ContentType` at all silently bypass verification
   entirely, contradicting this section's own explicit "mandatory, not
   defensive" requirement one paragraph up. One reviewer reproduced it with a
   throwaway test before any fix existed. Fixed by removing the guard: an
   empty backend-reported `ContentType` now correctly mismatches against any
   non-empty `grant.ContentType` and returns 409. Regression test:
   `TestArtifactGrantCommitWithEmptyBackendContentTypeReturns409`. Fixing
   this also required updating `internal/api`'s router-level test doubles
   (`alwaysSucceedsArtifactStore.Get` and
   `alwaysSucceedsArtifactRepo.GetTransferGrant` in
   `artifact_rbac_test_doubles_test.go`), which had previously relied on the
   now-removed bypass to make their fixed grant/store pairing agree.
2. `CommitTransferGrant` never checked S12's project storage quota
   (`GetProjectStoragePolicy`/`SumProjectBytes`/`max_total_bytes`) — a
   transfer grant could commit an object that pushed a project's total past
   its configured limit, a control `UploadObject` already enforces on the
   exact same metadata. Fixed by adding the identical
   check-after-write/roll-back-on-violation block `UploadObject` already
   uses (`objects.go`), placed after `UpsertObject` and before
   `MarkTransferGrantConsumed`. Regression test:
   `TestArtifactGrantCommitExceedingProjectQuotaReturns413AndRollsBack`.

Both regression tests were confirmed to actually catch their bug (not just
pass vacuously) by temporarily reverting each fix in isolation and observing
the corresponding test fail, then restoring the fix.

**Minor findings from the same pass, also fixed:** `store.Delete`'s error was
silently discarded on every rejection path, risking an untracked orphan
object invisible to the S14 sweeper if the physical delete itself failed —
folded into a new `rejectCommit` helper that surfaces a failed cleanup
delete's error in the response's `details` field instead of swallowing it.
The full object was always read and hashed before the `max_bytes` check ran,
even though `info.Size` is available immediately after `Get` — added an
early size short-circuit before `io.Copy`. The OpenAPI spec's
`commitTransferGrant` responses omitted 412 and 413 entirely and its 409
description inaccurately implied it covered size mismatches — corrected to
list 412 (expired grant) and 413 (`max_bytes` or quota) separately, with 409
scoped to digest/media-type mismatch and already-consumed. Added table-driven
coverage for `parseGrantDigest`'s hex-decode-failure and wrong-length
branches, and for `looksLikeGrantID`'s per-character dash-position and
hex-charset checks — both had only ever been exercised by their "obviously
malformed" case, not their specific failure branches.

**Known, accepted gap:** the Postgres integration tests for
`ArtifactTransferGrantsRepository` exercise create/fetch/scope/single-use
against a real database but never a grant row whose `bucket_id` violates the
`buckets` foreign key or whose `method` violates
`grants_method_valid` — both are enforced by the schema (migration 0057) and
by the handler's own validation before a grant is ever created, so a
constraint-violation path only exists as defense-in-depth against a bug that
would already be caught elsewhere.

---

## S16 — Native multipart upload

**Preconditions:** S3, S15.

Expose the four multipart operations through the grant API for objects above
64 MiB, **gated on `Capabilities().NativeMultipart`**. Part state lives in the
provider, never in Redis and never in process memory.

GCS reports `NativeMultipart: false` and must fall back to the streaming facade.
Do not emulate multipart on top of resumable writes.

**Do not port the legacy design.** Legacy stored parts in Redis with a 24-hour
TTL, fell back to a process-local dictionary when Redis was unavailable,
concatenated the whole object in memory on completion, and recorded `uploadId`
ownership without ever checking it — permitting cross-tenant part theft.
Ownership must be verified on every part and completion call.

**Acceptance criteria:** a 2-part 10 MiB upload against S3 and Azure completes
with a matching digest; the same request against GCS transparently uses the
facade; a part call with another project's grant returns 403. That is three
required, distinct scenarios. **Name every new test function with a
`TestArtifactMultipart` prefix.** `-run Multipart` alone matches zero tests on
the unmodified tree (`internal/api/v2/artifacts` has no existing test
containing that substring) — so an unimplemented S16 also reports
`ok ... [no tests to run]`, exit 0, indistinguishable from success.

A bare "at least one PASS" check is not enough here either: a suite that runs
only one of the three scenarios and legitimately skips or drops the other two
(e.g. only the GCS-facade test, if the S3/Azure cases are accidentally gated
behind an emulator flag) would still satisfy "at least one match." Require at
least 3 `--- PASS` lines, not just a nonzero count.

**Verify:**
```
cd services/elitea-main && go test ./internal/api/v2/artifacts/ -race -run TestArtifactMultipart -v > /tmp/s16.log 2>&1; rc=$?; cat /tmp/s16.log; test $rc -eq 0 && test "$(grep -c -- '--- PASS' /tmp/s16.log)" -ge 3
```

**Plan defect found during implementation:** unlike S15's own section (which
explicitly warns "this package cannot reach `internal/api/router.go`... a
handler-level Verify command can pass while the route itself is still
unwired" and requires a router-level test as an acceptance criterion), this
S16 section named no such requirement and the Verify command above never
runs `internal/api` at all — the exact failure mode S15's text warns about,
un-guarded here. Closed the same way S15 was: added
`TestArtifactMultipartRoutesResolveThroughRealHandlerWhenConfigured` in
`internal/api/router_security_test.go`, proving all three new routes
resolve through the real handler chain (not the S7 stub), even though
nothing in this stage's own literal Verify command would have caught its
absence.

**Design decisions made during implementation, not fully pinned down by the
plan text above:** "expose the four multipart operations through the grant
API" was read as folding Start into the *existing* `CreateTransferGrant`
endpoint (same URL, response shape changes based on whether `upload_id` or
`url` comes back) rather than adding a fourth new endpoint — three new
routes were added (part presign, complete, abort), not four, matching
storage.ObjectStore's own four multipart methods 1:1 once Start is counted
against the endpoint it reuses. `multipartThreshold` (64 MiB) is a Go
constant, not project-configurable — the plan names the number but not a
policy override, and `project_storage_policy` has no matching column to
source one from. `maxPartNumber` (10000) is S3's own limit, used as a
shared validation ceiling for all backends since the plan names no number
of its own and GCS never reaches this code path at all
(`Capabilities().NativeMultipart` is always false).

The plan's own acceptance criterion — "a part call with another project's
grant returns 403" — is a deliberate departure from S15's own
`CommitTransferGrant`, which collapses "wrong project" and "grant does not
exist" into a single 404 via a project-scoped SQL `WHERE` clause. S16's
multipart continuation endpoints (`PresignUploadPart`,
`CompleteMultipartUpload`, `AbortMultipartUpload`) instead fetch the grant
by ID alone (`GetTransferGrantByID`, unscoped) and compare `ProjectID`
explicitly in the handler, so a real-but-foreign grant ID gets 403
(AccessDenied) while a genuinely unknown ID still gets 404 — exactly the
distinction the plan's own acceptance criterion requires and S15's shape
cannot make. This 403 check was extended to `AbortMultipartUpload` too,
beyond what the plan's acceptance-criteria sentence names literally ("a
part call") — the plan's body text ("ownership must be verified on every
part and completion call") plus ordinary security-consistency reasoning
argued for treating abort identically, since an unauthenticated-for-this-grant
caller aborting someone else's in-progress upload is exactly the kind of
cross-tenant interference the check exists to prevent.

`PresignUploadPart`/`CompleteMultipartUpload` both reject an expired grant
(412); `AbortMultipartUpload` deliberately does not — cleanup of an
abandoned session must not be blocked by the same TTL that blocks new
uploads, or an expired-but-still-open provider-side multipart session would
have no way to be released through this API before S14's lifecycle-rule GC
eventually reaps it. A part-presign URL's TTL is bounded by the parent
grant's own remaining lifetime (`time.Until(grant.ExpiresAt)`), not a fresh
`defaultGrantTTL` on every call — otherwise a caller could keep refreshing
part URLs indefinitely past the grant's own 15-minute boundary.
`AbortMultipartUpload` marks a grant terminal by reusing the same
`consumed_at` column a successful commit uses, rather than adding a
dedicated `aborted_at` column — every other endpoint treats "consumed" as
simply "no longer usable," and this stage found no place in the codebase
that reads `consumed_at` and assumes it specifically means "successfully
committed" (S14's sweeper works off `elitea_storage.objects`, not
`transfer_grants`, and is unaffected either way).

**Horizontal-scaling and control/data-plane-separation check** (prompted by
an explicit request mid-stage to verify this plan still meets
`elitea-docs/docs/internal/03-architecture/adrs/adr-0016-object-storage-architecture.mdx`'s
"any number of `elitea-main` replicas... losing a pod loses no data and
interrupts no subsequent read" requirement and
`elitea-docs/docs/internal/03-architecture/adrs/adr-0014-control-output-data-plane-separation.mdx`'s
plane separation): confirmed clean on both counts. No multipart state lives
in `elitea-main` process memory or in Redis — the only durable state is the
`upload_id` string in `elitea_storage.transfer_grants` (Postgres) plus the
session the provider itself holds; any pod can service any request for a
given grant, with no session affinity requirement, exactly like S15's
existing grants. This is also why the blocking concurrency fix below had to
be a Postgres-level atomic claim (`UPDATE ... WHERE consumed_at IS NULL`)
rather than an in-process mutex or `sync.Once` — the latter would silently
stop working the moment a deployment ran more than one replica, which is
the normal case per ADR-0016, not an edge case. Bulk part bytes never
transit `elitea-main` at all (client uploads each part directly to the
provider via the presigned URL `PresignUploadPart` returns) except at
`CompleteMultipartUpload`'s own verification read — the same
already-accepted, already-documented cost `CommitTransferGrant` (S15) pays,
not a new one.

**One blocking defect found and fixed by a Workflow adversarial-review pass
(plan-compliance + bug-hunt + test-integrity reviewers, each independently
verified) run before committing:** `CompleteMultipartUpload` and
`AbortMultipartUpload`'s first-draft shape both claimed the grant
(`MarkTransferGrantConsumed`) only *after* calling their own store method
(`store.CompleteMultipart` / `store.AbortMultipart`) — mirroring
`CommitTransferGrant`'s existing, already-shipped ordering. Two concurrent
same-project requests (a client retry racing a cancel, for example) could
both observe `consumed_at IS NULL` before either wrote, then race on which
backend call landed first: if abort won, it could delete the very parts
complete was in the middle of assembling, silently and permanently
discarding an otherwise fully, legitimately uploaded transfer, with no way
to recover it (the grant would already read as consumed by whichever side
won the metadata write). Fixed by claiming the grant *before* calling
either store method in both handlers, making the two mutually exclusive:
whichever request wins the claim proceeds; the loser gets a clean 409
without the store ever being touched. `AbortMultipartUpload`'s original
"treat a lost race as a tolerated no-op, return 204 anyway" handling was
itself part of the bug (it could report success on an abort that a
concurrent, legitimate completion had already beaten) and was replaced with
a hard 409 on a lost claim.

This fix has a real, documented trade-off: a `CompleteMultipartUpload` that
fails verification (digest/media-type/quota, inside the shared
`finalizeGrantCommit`) is no longer retriable in place the way
`CommitTransferGrant` is — the grant is already consumed by the time
verification runs. A caller whose large upload fails verification must
start a new grant. Judged acceptable given multipart exists specifically
for objects above the 64 MiB threshold, where closing the concurrent-abort
race matters more than in-place retry convenience after a failed check.

Regression coverage:
`TestArtifactMultipartConcurrentCompleteAndAbortAreMutuallyExclusive`
(`internal/api/v2/artifacts/multipart_test.go`) reproduces the race
deterministically — not via real goroutines, which are inherently
timing-dependent — using new `fakeStore` hooks
(`beforeCompleteMultipart`/`beforeAbortMultipart`) that synchronously make
a second, "concurrent" request land exactly inside the window a genuine
race would occupy. The initial version of this test was itself flawed: it
pre-set `ConsumedAt` on the grant before issuing the request, which is
rejected by `requireOwnedMultipartGrant`'s ownership check before the
ordering-dependent code is ever reached, regardless of which ordering is
under test — it passed identically against both the buggy and fixed code
and proved nothing. The hook-based version was confirmed, by temporarily
reverting the fix, to actually fail against the pre-fix ordering (a
concurrent complete corrupting an in-flight abort's parts, and vice versa)
before being confirmed to pass against the fix.

**Minor defect found and fixed:** `CreateTransferGrant`'s native-multipart
branch calls `store.StartMultipart` (creating real, billed backend-side
state) *before* persisting the grant row. If the subsequent database insert
failed, the caller received a 500 with no `grant_id`/`upload_id` to ever
reference or clean up that session with — an orphaned multipart upload with
no compensating rollback, unlike `rejectCommit`'s established
cleanup-on-failure convention elsewhere in this package. Fixed by calling
`store.AbortMultipart` as best-effort compensation when the metadata write
fails after a successful `StartMultipart`; if that compensating call itself
fails too, S14's own incomplete-multipart-upload lifecycle rule (S3/GCS;
Azure documented gap — see S14) eventually reclaims it, matching this
package's existing tolerance for bounded, non-permanent leaks elsewhere.

**Nits found and fixed:** a `time.Until(grant.ExpiresAt)` computed after a
database round trip that followed the handler's own expiry check could, in
principle, read as non-positive despite that check having just passed
(`PresignUploadPart`) — added a second, cheap guard immediately before the
value is used. The exact 64 MiB boundary (`max_bytes == 67108864`) had no
dedicated test, only values well clear of it on both sides — added
`TestArtifactMultipartExactlyAtThresholdStaysSingleShot`. The sqlc compiler
baseline (`internal/db/schema/artifact_storage_baseline.sql`) declared
`upload_id` mid-table while the real migration
(`0058_artifact_multipart.sql`) appends it via `ALTER TABLE`, making it
physically last in the deployed table — no functional effect (every query
names its columns explicitly; nothing here does `SELECT *`), but corrected
for legibility so the two files describe the same physical layout, not
just the same logical one. A test's "parts deliberately out of order"
comment overclaimed what it proved — the ordering guarantee actually comes
from the store layer (both `fakeStore` and the real S3 backend re-sort by
part number internally), not the handler — corrected to state what the
test actually demonstrates. `fakeStore.CompleteMultipart` never validated
a part's ETag against what `simulateMultipartPartUpload` issued, so no test
anywhere exercised a wrong-ETag rejection even though
`CompleteMultipartUpload` is the first place in this codebase an
externally-supplied ETag reaches `storage.ObjectStore.CompleteMultipart` —
added the check to the fake (modeling real S3/Azure `InvalidPart`-style
behavior) plus a regression test.

**Known, accepted gap, inherited from S15:** as with
`ArtifactTransferGrantsRepository`'s existing Postgres integration tests,
S16's own addition
(`TestArtifactGrantRepositoryUploadIDRoundTripsAndGetByIDIsUnscopedAgainstRealPostgres`)
does not exercise the `bucket_id` foreign key or `grants_method_valid`
CHECK constraint directly — both are enforced by the schema and by the
handler's own validation before a grant is ever created, so a
constraint-violation path only exists as defense-in-depth against a bug
that would already be caught elsewhere.

---

## S17 — Deployment

**Preconditions:** S2, S5, S12 — the env var names this stage wires into the
chart come from all three: `S3_ENDPOINT_URL`/credentials from S5,
`AZURE_STORAGE_ENDPOINT`/`GCS_ENDPOINT` from S2, `ARTIFACT_MAX_OBJECT_BYTES`
from S12. Confirm all three actually landed before starting — this stage is
naturally one of the last to run, not "right after S5."

- `deploy/helm/elitea-main/values.yaml` and `templates/deployment.yaml`: add
  the storage environment block. **Render `S3_ACCESS_KEY`, `S3_SECRET_KEY`,
  and `AZURE_STORAGE_KEY` as individual `env:` entries with
  `valueFrom.secretKeyRef`, not a chart-managed `envFrom: secretRef` gated
  behind a `create` toggle.** Both shapes are common Helm idiom, but only the
  former renders the literal key names `helm template` output — and this
  stage's own Verify command greps for exactly that. A `create`-gated
  `envFrom` secret defaults to off and never spells out `S3_ACCESS_KEY` in
  rendered output at all, so the Verify command fails even on an otherwise
  reasonable implementation. The chart currently delivers only
  `envFrom: configMapRef` and has no secret path of either shape.
- `deploy/helm/elitea-main/templates/serviceaccount.yaml`: add an `annotations`
  block so IRSA and Azure Workload Identity can be configured. It has none
  today, which is why credentials must be static.
- Confirm no `volumes`, `volumeMounts`, `persistentVolumeClaim`, or `emptyDir`
  appear for artifact data. There are none — keep it that way, and the HPA
  scaling from 2 to 10 replicas becomes correct rather than accidentally so.
- Remove the storage variables from
  `deploy/helm/elitea-main/env-drift-allowlist.txt` now that they are wired, and
  add the new names introduced by S2, S5, and S12.
- Remove `ARTIFACTS_DATA_DIR` and the artifact bind mount from
  `deploy/docker-compose.staging.yml`.
- Add a Helm chart for `pylon-indexer` and `elitea-worker-python`, or record
  explicitly that they are out of scope for R1.

A bare `grep -q 'S3_ACCESS_KEY'` only proves the literal string appears
*somewhere* in the rendered output — it passes just as well for a hardcoded
plaintext `value:` as for a real `valueFrom.secretKeyRef`, and it never checks
`S3_SECRET_KEY` or `AZURE_STORAGE_KEY` at all. Check all three names are each
followed by a `secretKeyRef` within the next few lines, not merely present.

**Verify:**
```bash
helm lint deploy/helm/elitea-main && \
helm template deploy/helm/elitea-main > /tmp/rendered.yaml && \
for VAR in S3_ACCESS_KEY S3_SECRET_KEY AZURE_STORAGE_KEY; do \
  grep -A3 -- "name: $VAR" /tmp/rendered.yaml | grep -q 'secretKeyRef' || { echo "missing valueFrom.secretKeyRef for $VAR"; exit 1; }; \
done && \
! grep -qE 'persistentVolumeClaim|emptyDir' /tmp/rendered.yaml
```

**Design decisions made during implementation:** the three storage secrets
(`S3_ACCESS_KEY`, `S3_SECRET_KEY`, `AZURE_STORAGE_KEY`) are rendered via a
new `secrets:` map in `values.yaml`, mirroring
`deploy/helm/elitea-llm-gateway`'s already-established convention exactly
(same map shape, same `valueFrom.secretKeyRef` rendering loop in
`deployment.yaml`) rather than inventing a new pattern — this is the first
time elitea-main's own chart uses that convention; the pre-existing
plaintext-forbidden secrets (`APPLICATION_SECRET_KEY`, `OIDC_CLIENT_SECRET`,
etc.) remain allowlisted only, out of scope for this stage. All three
default to `optional: true`, since only one backend is ever active per
`STORAGE_BACKEND`: `AZURE_STORAGE_KEY` is simply irrelevant when
`STORAGE_BACKEND=s3`, and either backend can run credential-less via
workload identity (the new `serviceAccount.annotations` knob) instead of a
static key. `GOOGLE_APPLICATION_CREDENTIALS` was deliberately left
allowlisted rather than wired as a fourth chart secret — GCS's intended
production path is GKE Workload Identity (needing no credentials file at
all), and forcing every deployment to provision a credentials-file Secret
would be wiring the non-default path as if it were required.

**Pylon-indexer / elitea-worker-python Helm charts: recorded out of scope
for R1**, per the bullet's own "or record explicitly" alternative. Both
have a `Containerfile` (confirmed:
`services/pylon-indexer/Containerfile`, `services/elitea-worker-python/Containerfile`)
but no chart today. Building production-grade charts for two Python
services with their own env/secret/health-check surfaces is materially
larger than this stage's remaining bullets and outside this plan's actual
scope (a Go artifact-storage migration, not a general infra uplift) —
tracked as a separate, unscoped follow-up, not silently dropped.

**Defects found and fixed while empirically verifying this stage** (running
the actual downstream consumer of `env-drift-allowlist.txt`,
`services/elitea-llm-gateway/scripts/env-drift-check.sh`, rather than
assuming the allowlist edit alone was sufficient) — both in shared
infrastructure also used by `elitea-llm-gateway`'s own CI, not storage-specific,
but directly blocking trust in this stage's own verification:

1. The script's `_test.go` exclusion was silently a no-op: three
   `grep -rhoE ...` extraction calls used `-h` (suppress filename), and the
   *next* pipeline stage filtered `grep -v '_test.go'` — by the time that
   filter ran, the filename it was supposed to match against was already
   gone. Every env var read only inside an integration test file (e.g.
   `ELITEA_TEST_DATABASE_URL`) was misreported as a hard, production-breaking
   FAIL. Fixed by dropping `-h`; each `sed` step's existing greedy `.*\("`
   prefix-strip already discards the now-preserved `path/to/file.go:` prefix
   in the same step, so no other pipeline stage needed to change.
2. None of the script's three read-detection patterns (`os.Getenv(`, a
   `*Or(` helper, or a `fooEnv = "X"` const) could see
   `storage.ConfigFromEnv`'s `lookup("X")`-parameter indirection (S1's
   injectable-lookup design, used for every storage env var) — every var
   read that way was misreported as "chart sets it but code never reads it."
   Added a fourth extraction pattern for `lookup\("[A-Z][A-Z0-9_]+"\)`, WARN-tier
   only (same treatment as the existing indirect-const pattern), since a
   regex cannot tell a required call from an optional one for this shape.

Both were real, live bugs in a script gating CI (`ci-gateway.yml`) with no
storage connection at all — found only because this stage's own Verify
philosophy is "run the actual consumer, don't assume the input file is
correct." Fixing them dropped `bash env-drift-check.sh`'s elitea-main
result from 9–12 FAILs to 2 (see below), and turned ~20 previously-invisible
WARN-tier findings visible for the first time.

**Also discovered, not a script bug:** `env-drift-check.sh`'s elitea-main
coverage was reachable only through `ci-gateway.yml`, whose own path trigger
is scoped to `services/elitea-llm-gateway/**` — meaning it never fires for
an elitea-main-only or a chart-only change, despite the script's own header
comment claiming "it just also covers elitea-main now." In practice this
meant the check had never actually run against elitea-main in this
engagement's own CI history. Fixed by adding a dedicated `env-drift` job to
`ci-go.yml` (which does trigger on `services/elitea-main/**` and, newly,
`deploy/helm/elitea-main/**`), running the script's single-target form
against elitea-main specifically rather than duplicating the gateway's own
check.

**Two genuinely pre-existing, storage-unrelated FAILs surfaced once the
above were fixed**, both trivial and both fixed rather than left red (a
freshly-wired, immediately-red CI gate is worse than not wiring it):
`BRAND_PACK_PATH` (`internal/api/router.go`) was never wired into the
chart at all — added to `values.yaml`'s `env` map.
`ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA` (`cmd/elitea-main/main.go`) is a
dev-only bootstrap flag that should never be an exposed production chart
knob — added to the allowlist with justification instead of wiring it.
`bash services/elitea-llm-gateway/scripts/env-drift-check.sh` now exits 0
(passed) for both services; the remaining ~20 WARN-tier findings (vars read
with a code default but no chart override knob — `ELITEA_RUNTIME_ENABLED`,
`REDIS_URL`, and others, all pre-existing and unrelated to storage) do not
fail CI and were left as visible, now-honest debt rather than silently
re-hidden — recorded in `services/elitea-llm-gateway/DECISIONS.md`.

**Two minor findings from a Workflow adversarial-review pass, both fixed:**

1. `optional: true` on the three storage secrets, combined with the S3/Azure
   backends' existing (pre-S17) fallback to their cloud SDK's own ambient
   credential chain (`LoadDefaultConfig` / `NewDefaultAzureCredential` —
   see S2/S3), means an operator who forgets *both* the Secret above *and*
   `serviceAccount.annotations` doesn't get a clear startup failure — on a
   cluster whose nodes already carry some IAM role/managed identity with
   storage access (common for node-level logging/monitoring DaemonSets),
   the pod silently starts and runs under whatever ambient identity it
   inherited. Not a logic bug this stage introduced (the backends'
   fallback behavior predates S17 and is required for IRSA/Workload
   Identity to work at all) but the chart's own comment didn't warn that
   "credential-less" can mean "credentialed by whatever the node has"
   rather than "fails until configured." Fixed by expanding the `secrets:`
   block's comment in `values.yaml` with this exact caveat.
2. The new `env-drift` job wasn't in `build`'s `needs:` list, unlike
   `sqlc`/`lint`/`test`. `needs:` only controls scheduling, not merge
   enforcement (that's entirely a branch-protection setting outside this
   repo's files, and applies identically to the pre-existing three jobs
   too) — so this wasn't a regression specific to the new job, but adding
   it to `needs:` costs nothing and keeps the four gating jobs visually
   and schedule-wise consistent.

---

## S18 — Observability

**Preconditions:** S11, **S15** — auditing "every grant issuance" requires the
grant-creation code S15 adds; it does not exist right after S11. **S14** — the
byte-usage gauge below is sourced from S14's sweeper tick, which is a sibling
branch off S6/S8/S9, not something S11 or S15 transitively guarantees.

Instrument every `ObjectStore` method with the existing OTel setup — read
`internal/api/middleware/otel.go` for the house pattern (its actual test names
are `TestOtelMiddleware_SetsStatusCode` / `TestOtelMiddleware_DefaultStatus200` —
behavior-named, no "Metrics" substring; do not assume you can grep for that
word in existing code and find the convention this stage should follow).
Operation duration histogram, error counter by sentinel type, bytes-in and
bytes-out counters, all labelled by backend and operation but **never** by
project identifier or key, which would explode cardinality. Add a per-project
byte-usage gauge sourced from the metadata table on the sweeper tick (S14), not
per request.

**There is no audit-record mechanism anywhere in this codebase to hook —
verified exhaustively.** `internal/domain/admin/types.go` declares
`AuditEntry`/`AuditListResponse` types with zero writers and zero consumers
anywhere else in the tree. The existing `AuditTraces`/`AuditTraceHeatmap`
handlers (`internal/api/v2/eliteacore/handler.go`) are hardcoded stubs
returning an empty list — not real persistence to extend. No migration
creates an audit table. The only other "audit" code in the repository
(`cmd/cutover-ctl/audit.go`) is an unrelated static-analysis CLI for a
different migration. **Do not invent a bespoke audit mechanism for this stage
alone.** Emit a structured `slog` line (`operation`, `bucket`, `key`,
`project_id`, `principal`, `outcome`) for every delete and every grant
issuance instead, and record this as an explicit open question: an owner
needs to decide whether artifact deletes and grants belong in a real,
queryable audit trail — and if so, whether that's the same
`AuditEntry`/`AuditListResponse` shape already declared and unused, or
something new. This is the same treatment S13 gives its own missing
integration point.

**Name every new test function with a `TestArtifactObservability` prefix.**
`-run Metrics` alone matches nothing today (`[no tests to run]`, exit 0 —
already hollow before any code exists) and, worse, would silently exclude any
correctly-implemented but behaviorally-named tracing or audit-logging test
even once one Metrics-named test exists to make the bare command pass.

**Acceptance criteria** — four distinct, separately testable behaviors, so "at
least one PASS" is not enough (a suite that only exercises one of them, e.g.
only the histogram, would still satisfy a bare nonzero-match check):
1. An `ObjectStore` call records the operation-duration histogram and the
   error counter (on a sentinel error), labelled by backend and operation
   only — assert no `project_id`/key label exists on either.
2. A write/read records the bytes-in/bytes-out counters.
3. The per-project byte-usage gauge updates after a sweeper tick (S14), not
   on a per-request path — assert it does *not* move between ticks.
4. A delete and a grant issuance each emit the structured `slog` line
   (`operation`, `bucket`, `key`, `project_id`, `principal`, `outcome`).

**Verify:**
```
cd services/elitea-main && go test ./internal/infra/storage/... -race -run TestArtifactObservability -v > /tmp/s18.log 2>&1; rc=$?; cat /tmp/s18.log; test $rc -eq 0 && test "$(grep -c -- '--- PASS' /tmp/s18.log)" -ge 4
```

**Implemented as a decorator, not scattered call sites.** `internal/infra/storage/instrumented.go`'s
`instrumentedStore` implements every `ObjectStore` method, wrapping a real
backend and recording duration/error/bytes metrics plus the delete audit log
at the one choke point every Put/Get/Delete/... call passes through —
`cmd/elitea-main/storage_factory.go`'s `newObjectStore` now always returns
`storage.Instrument(store, cfg.Backend)`, so "instrument every method" and
"log every delete" are true by construction, not by remembering to call
something at each of the 13 `ObjectStore` methods' call sites. A mirrored
`errors.Unwrap`-style `Unwrap() ObjectStore` method lets tests (and any
future caller) reach the concrete backend through the wrapper.

**Verify-scope-vs-acceptance-criteria gap, same pattern as S14/S16.** The
literal Verify command only exercises `internal/infra/storage/...`, but 2 of
the 4 acceptance criteria concern code that has to live elsewhere: the
per-project gauge is sourced from the S14 sweeper's tick
(`internal/runtimecomposition`), and "every grant issuance" is emitted from
the S15/S16 grants handler (`internal/api/v2/artifacts`). Resolved the same
way S14's sweeper resolved its own version of this gap: centralized the two
testable primitives — `storage.RecordProjectByteUsage` and `storage.LogAudit`
— as small, exported, directly-testable functions in `internal/infra/storage`
itself, called from both real sites (`artifact_retention_sweep.go`'s new
`updateProjectByteUsageGauges`, `grants.go`'s post-`CreateTransferGrant` call).
Both the shared primitives (tested in `internal/infra/storage`, satisfying the
literal Verify command) and their real call sites (tested in their own
packages' existing suites, which still pass) have coverage.

**OTel test harness: the global delegation "only once" constraint.**
`observability.go`'s instruments are package-level vars created in `init()`,
against whatever `MeterProvider` is the OTel global default at that point (a
no-op stub — this codebase constructs no explicit provider anywhere,
following `internal/api/middleware/otel.go`'s existing house pattern).
Read `go.opentelemetry.io/otel/internal/global/state.go` directly to confirm
the test-time re-plumbing actually works: `otel.SetMeterProvider` delegates
every previously-created instrument to the provider passed on its *first*
call only (guarded by a package-level `sync.Once` — a second call in the
same test binary changes what a future `otel.Meter(...)` returns but does
**not** re-delegate the already-delegated instruments). So
`internal/infra/storage/observability_test.go`'s `setupTestMeterProvider`
installs one `sdkmetric.NewMeterProvider(sdkmetric.WithReader(...))` exactly
once, guarded by its own `sync.Once`, shared across every
`TestArtifactObservability*` test in the package. Individual tests disambiguate
their own recorded data points within that shared `ManualReader` state by
attribute value — a unique `backend` string (`t.Name()`) for the per-operation
instruments, a unique `project_id` constant for the gauge — rather than by
isolated readers, since isolated readers are exactly what the "only once"
constraint rules out.

**New sqlc query empirically verified against real Postgres, not just the
sweeper's fakes.** `ListArtifactProjectIDsWithBuckets` (used by the sweeper's
`updateProjectByteUsageGauges` to know which projects to gauge) has both a
fake-repo unit test (`artifact_retention_sweep_test.go`, exercising the
sweeper's own call pattern) and a real-Postgres integration test
(`TestArtifactBucketsRepositoryListsProjectIDsWithBuckets`, new in
`internal/infra/db/repos/artifact_buckets_postgres_integration_test.go`),
covering the `DISTINCT project_id ... WHERE deleted_at IS NULL` query itself:
one project with two buckets returns once, a project whose only bucket is
soft-deleted is excluded, a project with no buckets never appears — run
against a throwaway `pgvector/pgvector:0.8.5-pg18-trixie` container (matching
`ci-go.yml`'s `integration` job image at the time; superseded by #1359, which
removed pgvector: that job and every Postgres integration run now use plain
`postgres:18`) via
`ELITEA_TEST_DATABASE_URL`, migrations applied through the existing
`applyPostgresIntegrationMigrations` helper, database torn down in
`t.Cleanup`.

**Per-user direction: proceed through the remaining stages (S19, S20a/b/c)
without waiting for a per-stage go-ahead, then run one comprehensive
multi-agent adversarial review across the whole feature once it's fully
implemented, rather than the per-stage Workflow review S15-S17 each got.**
S18 itself still got its own full local verify sweep (`gofmt`, `go build`,
`go vet`, `task lint`, the literal Verify command above, full workspace
`task test`, plus the Postgres-integration run above) before being committed
— only the *adversarial* review pass moved to the end.

---

## S19 — API conformance suite

**Preconditions:** S10, S11, **S15** — grant/presign coverage below needs the
real handlers S15 wires in, not the S7 stub. **S4** — this stage needs the
RustFS/Azurite/fake-gcs-server emulator wiring S4 builds, and S4 is deferrable
relative to S1-S11 (it has no critical-path dependents until now), so check it
was actually done before starting this stage rather than assuming it was.
**Read first:** `tests/contract/contract_test.go` (the existing `TestMain` and
its `CONTRACT_AUTH_TOKEN` gate), `.github/workflows/ci-contract.yml`.

`services/elitea-main/tests/contract/` and `.github/workflows/ci-contract.yml`
already exist and run weekly. The workflow accepts a `legacy_url` input and was
built to diff Go against the legacy service — **that comparison is no longer
meaningful** and the input should be removed. Repurpose the harness as a
self-contained conformance suite driving the new artifact API against a running
Go service plus a RustFS emulator (S4).

**`package contract`'s existing `TestMain` (`tests/contract/contract_test.go`)
gates the entire package shut by default, and this stage's Verify command
cannot detect it — this is not the same bug the `TestArtifact`-naming rule
below fixes, it is a different, deeper one.** `TestMain` reads
`CONTRACT_AUTH_TOKEN` and calls `os.Exit(0)` **before `m.Run()`** when it's
unset — which is the default locally, and is exactly what
`ci-contract.yml`'s PR-triggered `compile` job does (it deliberately sets no
token, by design, so the legacy-parity fixtures merely compile on every PR
without needing secrets; only the weekly `schedule`/`workflow_dispatch` job
sets the real token from `secrets.CONTRACT_AUTH_TOKEN`). Any new
`TestArtifact*` test added to this same package inherits that gate — it is
never reached, at all, regardless of its name. **You must restructure
`TestMain` as part of this stage**, not merely add tests alongside it: gate
only the legacy-parity comparison tests on `CONTRACT_AUTH_TOKEN`, and let the
new self-contained `TestArtifact*` suite run unconditionally, skipping
per-test via `t.Skip` only when S4's RustFS environment variables are absent
— the same pattern S4 already uses for its own emulator-gated cases, not a
new one. Then edit `ci-contract.yml`'s PR-triggered `compile` job to actually
start the RustFS emulator (mirroring what S4 already wires into `ci-go.yml`)
so the new suite runs on every PR, not only in the weekly cron.

Cover, end to end through the real router with real auth:

- Bucket create, get, list, patch (pin, tags, retention), delete.
- Bucket name rejection: uppercase, leading digit, underscore, 63-plus characters.
- Retention above the project limit returns 403 with `code: QuotaExceeded`.
- Upload, download, HEAD, delete, and range read of a 5 MiB object with a
  digest check on the round trip.
- Upload with `overwrite=false` onto an existing key returns 409.
- List with `prefix` and `delimiter`, asserting `common_prefixes` and that
  `next_cursor` paginates without duplicates or gaps.
- Batch delete with a mix of present and absent keys, asserting the per-key
  result array; an empty `keys` array returns 400.
- Every error code in the envelope enum is reachable by some request, and no
  response body is plain text.
- Grant creation followed by a presigned upload and commit produces a listable
  object; a commit with a mismatched digest or media type returns 409.
- A key containing `..`, a leading `/`, or a control character returns 400 with
  `code: InvalidKey`.
- Cross-project access returns 403.

**Acceptance criteria:** the suite fails if any response is not the documented
shape; `ci-contract.yml` no longer references a legacy base URL. **Name every
test function with a `TestArtifact` prefix**, for the same reason S6 requires
it: `-run Artifact` matches Go function names, not file names or behavior, and
a correctly-implemented test named by what it checks rather than by
containing the literal word "Artifact" is silently excluded from this
command's run with no failure or skip reported.

The "Cover, end to end" list above names 11 distinct scenarios. A bare
"at least one PASS" check cannot tell a fully-run suite from one where most
of those scenarios are `t.Skip`-ed because S4's RustFS environment variables
are only partially set in the run environment — the restructured `TestMain`
makes that skip legitimate in isolation, but a Verify command that can't
distinguish "1 passed, 10 skipped" from "11 passed" is not actually verifying
this stage's coverage. Require at least 11 `--- PASS` lines, not just a
nonzero count — a real implementation with table-driven subtests (e.g. the
4-way bucket-name-rejection case) will clear this floor comfortably.

**Verify:**
```
cd services/elitea-main && go test ./tests/contract/ -race -run TestArtifact -v > /tmp/s19.log 2>&1; rc=$?; cat /tmp/s19.log; test $rc -eq 0 && test "$(grep -c -- '--- PASS' /tmp/s19.log)" -ge 11 && ! grep -q 'CONTRACT_AUTH_TOKEN not set' /tmp/s19.log
```

**TestMain restructured as a two-gate, not one.** `tests/contract/contract_test.go`'s
old `TestMain` called `os.Exit(0)` before `m.Run()` whenever `CONTRACT_AUTH_TOKEN`
was unset — the default everywhere except `ci-contract.yml`'s weekly/dispatch
job — which would have silently gated the new self-contained `TestArtifact*`
suite shut too, on every PR, with no failure or skip reported at all (exit 0
for the wrong reason, the same class of hollow-verify this plan calls out
repeatedly). Fixed by converting the 8 legacy-parity tests
(`TestApplicationsList`, `TestHealthz`, `TestPredict`, ...) to each call a new
`requireLegacyParityCredentials(t)` helper (`t.Skip` when `authToken == ""`)
instead, and moving `setupArtifactSuite()`'s build/teardown into `TestMain`
itself, wrapping `m.Run()` — the right place for whole-binary setup/teardown
shared across many independent top-level test functions, as opposed to
per-test `t.Cleanup`.

**Harness: real router, real Postgres, real S3-compatible backend — built
once, shared by every `TestArtifact*` test.** `tests/contract/artifact_harness_test.go`
builds an ephemeral Postgres database (the same `CREATE DATABASE`-per-run +
embedded-shared-migrations pattern every Postgres-integration package in this
repo reimplements, no shared testutil exists — see
`internal/runtimecomposition/artifact_retention_sweep_postgres_integration_test.go`,
the closest existing precedent, reused almost verbatim including its
`centry.project`/`centry.notifications` stub) and a real S3 backend against
RustFS (the same emulator + bucket-self-provisioning pattern
`internal/infra/storage/conformance`'s `setupS3` already established),
wrapped in S18's `storage.Instrument(...)` — the real production construction
path, not a bypass of it. `internal/api.NewRouter(api.RouterConfig{Pool,
ObjectStore, ArtifactPermissionResolver})` with `AUTH_DEV_MODE=true` builds
the actual production router: every request goes through the real
auth/RBAC/handler chain exactly like `internal/api/router_security_test.go`'s
S11 tests do, via a package-local `artifactPermissiveResolver` that grants
every artifact permission unconditionally — S19 is testing the artifact API's
own contract (response shapes, status codes, the error envelope), not RBAC
enforcement, which S11's router-level tests already cover exhaustively for
every one of these routes. Wrapped in `httptest.NewServer(router)` so tests
drive it with real HTTP round trips, matching the "against a running Go
service" framing literally. Every test gets its own project ID
(`nextArtifactProjectID`, seeded from `time.Now().UnixMilli()` — not a fixed
literal, and not `UnixNano()` either: a project ID must match
`^[1-9][0-9]{0,17}$`, at most 18 digits, and `UnixNano()` is already 19 as of
2026) so 11 independent scenarios can safely share one Postgres database and
one physical RustFS bucket without cross-test interference, without paying
for 11 separate ephemeral databases.

**Two real, previously-undetected production defects found — both fixed,
both in the codebase since S3/S9, both invisible to the existing S4
conformance suite until this stage actually asserted on the thing they
break.** `s3.Backend.Put` and `azure.Backend.Put`'s non-seekable-body path —
the one every real HTTP multipart-form upload actually takes, since S9's
`UploadObject` always passes a `*multipart.Part` with `ContentLength: -1` —
always returned `ObjectInfo{Size: 0}`. Neither SDK's streaming-upload
response carries a size/content-length field to read it from
(`transfermanager.UploadObjectOutput` and `blockblob.CommitBlockListResponse`
were both read directly to confirm neither has one), and nothing populated
`Size` from the actual bytes transferred. In production, this meant every
object ever uploaded through the real HTTP upload path silently reported
`size_bytes: 0` to the caller **and** recorded `byte_length: 0` into
`elitea_storage.objects` (`UpsertObject` reads `info.Size` directly) —
zeroing every size aggregate (`SumBucketBytes`, `SumProjectBytes`, S12's
quota checks, S18's own per-project gauge) for real uploads, the same class
of defect S12 found in the metadata-*write* path itself, this time in the
size *value* written down that path. The existing S4 conformance suite has a
case exactly named for this (`NonSeekableBodyRoundTripsMatchingDigest`) but
only ever asserted round-trip digest correctness via a fresh `Get` call
afterward — `Get`'s `ObjectInfo.Size` comes from an independent, already-correct
backend response (a real `Content-Length`/`GetObject` response), so that
assertion passed regardless of what `Put` itself returned, and never caught
this. Fixed both backends with a `countingReader` wrapping the streamed body
before it reaches the SDK's uploader, reporting the actual bytes read as
`Size`; strengthened the existing conformance case to assert on `Put`'s own
returned `Size`, not just the later round trip, closing the coverage gap
that let this hide. GCS's `Put` was already correct — `storage.Writer.Attrs().Size`
is populated by the GCS client's own internal accounting.

**One acceptance-criterion case relocated, not dropped: the control-character
key-rejection scenario cannot be driven through the upload path.** A raw
control byte (`\x01`) embedded in a multipart `Content-Disposition` filename
fails Go's own `mime.ParseMediaType` before `UploadObject` ever reaches
`storage.NewObjectRef` — surfacing as `InvalidArgument` (the
Content-Disposition parse failure), a different, earlier, equally-defensive
400, not the `InvalidKey` this case is meant to exercise. The `..` and
leading-slash cases both reach `storage.NewObjectRef` fine through the same
upload path (`mime.ParseMediaType` tolerates those bytes) and were left as
originally designed. The control-character case moved to
`TestArtifactKeyRejection`'s download-path subtest instead — a raw URL path
with no MIME header parsing in between, reaching `validateKey` directly and
returning the expected `InvalidKey`.

**Media-type-mismatch commit rejection verified empirically, not assumed
unreachable.** `s3.Backend.PresignPut` signs `Content-Type` as part of the
presigned URL when the grant declares one, which could plausibly mean an
S3-compatible backend rejects any PUT whose actual `Content-Type` header
doesn't match what was signed — closing off the one realistic way to
reproduce `finalizeGrantCommit`'s "uploaded object's media type does not
match the grant" check (`MediaTypeMismatch`) through the presigned-URL path
at all. Rather than assume, `TestArtifactGrantCommitMediaTypeMismatchReturns409`
sends the presigned PUT with no `Content-Type` header at all — the exact
"out-of-band presigned PUT" scenario `finalizeGrantCommit`'s own doc comment
names — with a `t.Skip` fallback (not a hard failure) documenting the
alternative if the backend had rejected it. Run against real RustFS: the PUT
succeeds, the stored object reports an empty `Content-Type`, and the commit
correctly 409s with `MediaTypeMismatch` — confirmed reachable, not a
documented gap.

**`legacy_url` removed from `ci-contract.yml`'s `workflow_dispatch` input**,
per this stage's own instruction that diffing against a specific legacy
instance is "no longer meaningful." Narrowly scoped: only the *input* is
removed (the weekly/dispatch `contract` job's `CONTRACT_LEGACY_URL` env var
now reads a fixed default instead of an overridable input) — the
legacy-parity comparison tests themselves (`TestApplicationsList`,
`TestSkillsList`, ..., none of them artifact-related) are untouched;
retiring that whole mechanism for unrelated endpoints is out of this
stage's scope. The `compile` job (previously "Compile & Skip-Run," now
"Compile & Run") gained the same Postgres service container +
`docker-compose.storage-test.yml` RustFS/Azurite/fake-gcs-server emulator
steps `ci-go.yml`'s own `test` job already has, so the new `TestArtifact*`
suite actually runs — not just compiles-and-skips — on every PR touching
`tests/contract/**` or `api/openapi/**`.

**Per-stage adversarial Workflow review deferred to one comprehensive pass
after S20, per the same explicit user direction S18 recorded** — this
stage still got its own full local verify sweep (`gofmt`, `go build`,
`go vet`, `task lint`, the literal Verify command above, a strengthened
run of the S4 conformance suite confirming both `Put` fixes, and a full
workspace `task test` with every emulator + a real Postgres attached, 103
packages, zero failures) before being committed.

---

## S20 — Adjacent planes

**Preconditions:** S9, S11, **S15** — S20c needs the grant and commit machinery.
**S20c is additionally blocked on spec-artifact-storage.mdx §6 open question 2**
("does the attestation plane share this `ObjectStore`?") being answered by an
owner — that gate is prose, not a stage number, so check it explicitly before
starting S20c even once S9/S11/S15 are done.

Three separate pieces of work, each with its own design step. Do not merge them.

**S20a — Chat attachments.** Go's `AddAttachments` takes a JSON body and writes
`chat_conversations.meta`; legacy is a multipart upload writing bytes to a
bucket plus `chat_messages_attachment` rows. These are different resource types.
Implement the byte path against the S9 handlers, port the 5 MiB chunked-upload
contract (`file_id`, `chunk_index`, `total_chunks`, `file_name`), and source the
per-project limits from `elitea_storage.project_storage_policy` — legacy reads
them from vault secrets at request time: 150 MB total, 150 MB per file, 3 MB per
image, 365-day retention, and the attachment bucket name from
`default_attachment_bucket`. A cutover that flips this route before the byte
path exists silently drops attachment bytes. Name every new test
`TestArtifactAttachment*` — `-run Attachment` alone would also match any
pre-existing `chat_conversations`-meta attachment tests that aren't about the
byte path.
*Verify:*
```
cd services/elitea-main && go test ./internal/api/v2/... -race -run TestArtifactAttachment -v > /tmp/s20a.log 2>&1; rc=$?; cat /tmp/s20a.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s20a.log && ! grep -q -- '--- SKIP' /tmp/s20a.log
```
(Minor, non-blocking: `grep -qc` combines `-q`, which exits after the first
match, with `-c`, whose count is then never read by anything — harmless
here since the intent was only ever "at least one PASS," which `-q` alone
already gives, but noted for the same reason every other stage's Verify
quirks get noted.)

**Local-disk chunk buffering does not port — Postgres does, per this plan's
own ADR-0016 standing constraint.** Legacy disk-buffers each chunk under
`{tempdir}/elitea_chunks/{file_id}/chunk_{index:06d}` and detects
completeness by counting files on disk. Direct-porting that to elitea-main
would silently break under any deployment with more than one replica and no
sticky session (the norm per ADR-0016, not an edge case) — chunk N and
chunk N+1 of the same upload can land on two different pods, and the second
pod's local disk has never seen the first chunk. `migrations/shared/
0059_attachment_chunks.sql` makes Postgres the shared buffer instead: one
row per chunk (`elitea_storage.attachment_chunks`, PK `(project_id,
conversation_id, file_id, chunk_index)`), `COUNT(*)` in place of a directory
listing, a retried `chunk_index` overwriting (`ON CONFLICT DO UPDATE`, not
`DO NOTHING`) rather than double-counting — the same idempotent-per-index
behavior legacy gets for free from filesystem semantics. No claim/lock
guards the merge step: if two concurrent requests both observe "all chunks
present" (the race window is the last two chunks of one upload landing on
different replicas near-simultaneously), both read the identical committed
rows, both merge and `Put` the identical bytes to the identical
deterministic key (last-writer-wins, not a torn write), and both call
`DeleteChunks` (idempotent — the second call just deletes zero rows).
Redundant work on that rare race, never corrupted output — accepted rather
than adding a lock, the same trade-off S14's sweeper already makes for its
own duplicate-tick idempotency.

**Bytes go through the S6/S9 metadata tables, not a bare `ObjectStore.Put`
with no row.** A simpler design would skip `elitea_storage.buckets`/
`objects` entirely and just `Put` bytes at a fixed key — rejected because it
would silently drop two things legacy has that this plan's earlier stages
already built machinery for: S14's retention sweep (legacy's
`chat_bucket_retention_days`, 365 days by default) never applies to an
object with no metadata row and no stamped `expires_at`, and the object's
bytes never count toward `SumProjectBytes`/S12's quota. Reusing
`ArtifactBucketsRepository`/`ArtifactObjectsRepository` for a
project-scoped, lazily-created `"system"`-type bucket (name from
`elitea_storage.project_storage_policy.attachment_bucket`, default
`chat-attachments`) gets both back at the cost of one extra get-or-create
round trip per upload.

**Import cycle discovered, and why it points at an unexported local
interface rather than a repos-package refactor.** `internal/infra/db/repos`
already has its own `conversations.go` (pre-S20a, implementing
`conversations.Repository` for `chat_conversations`/`chat_participants`/etc.
by returning `conversations.Conversation`/`conversations.ListResponse`
wire-shape DTOs directly — the `Repository` interface's own return types
force this direction). `internal/api/v2/conversations` importing
`internal/infra/db/repos` directly for S20a's byte path would therefore be
`repos → conversations → repos`, an import cycle Go rejects outright — found
immediately on the first `go build`, not by inspection. Fixed the only way
that doesn't touch the pre-existing direction: `attachments.go` defines its
own `AttachmentStore` interface using plain types only (no `repos.*` types
anywhere in its signature), and `internal/api/attachment_store.go` — which,
unlike `conversations`, can import both packages freely — supplies the
concrete `attachmentRepoAdapter` wrapping the real
`ArtifactBucketsRepository`/`ArtifactObjectsRepository`/
`AttachmentChunksRepository` and translating between the two type
vocabularies. `router.go` wires it: `v2convs.NewHandler(cfg.ConvsRepo).
WithPool(cfg.Pool).WithObjectStore(cfg.ObjectStore).
WithAttachmentStore(newAttachmentStore(cfg.Pool))` — the first `WithPool`
call also newly wires a pool into `convHandler` at all; it had never been
called before S20a (`grants.go`'s equivalent registration for other
handlers already did this, `conversations` had not), so three of its own
pre-existing pool-dependent methods (unrelated to attachments) start
receiving a real pool for the first time as an incidental side effect of
this stage's own wiring need.

**The three now-pool-fed methods, investigated and left unfixed as a
deliberate, acknowledged scope boundary — not silently carried forward.**
`List`, `PostMessage`, and `UpdateEntitySettings` (`handler.go`, pre-dating
this migration entirely) each build raw SQL with `fmt.Sprintf("p_%s",
projectID)` for the caller's tenant schema, then `%q`-interpolate that
string directly into the query text (`fmt.Sprintf(`SELECT ... FROM
%q.chat_conversations ...`, s)`) rather than using a parameterized
identifier — `%q` produces a Go-escaped string literal, not a
Postgres-escaped quoted identifier, so a `projectID` containing a literal
`"` is not neutralized the way it would be by proper identifier quoting.
None of the three validates `projectID`'s format before this point (the
route itself registers `{projectID}` with no regex constraint), so this is
a latent SQL-injection-shaped gap in the *identifier* position of these
three queries. It predates S20a and this migration; S20a's only actual
change is that `cfg.Pool` now reaches it when it never did before.
Confirmed, not assumed, before deciding not to fix it: this whole route
group is gated on `if cfg.ConvsRepo != nil` (`router.go`), and
`internal/runtimecomposition/composition.go` never sets `RouterConfig.ConvsRepo`
— grepped, zero hits — so it stays exactly as unreachable from real
production traffic today as it was before this stage, matching the
established "production composition never sets these fields" prototype-compat
convention `TestArtifactStubRoutesReturn501WithTypedEnvelope`'s own doc
comment describes for the artifact stub routes. Fixing the underlying raw-SQL
pattern in three chat handlers is out of scope for a storage migration plan —
it is pre-existing legacy-compat code this plan does not otherwise touch, and
a change there carries its own regression risk this plan's own tests cannot
cover. Left as an open question for a human owner: if `ConvsRepo` is ever
wired into real production composition, `projectID` must be validated as
strictly numeric — matching `storage.NewBucketRef`'s own
`^[1-9][0-9]{0,17}$` convention — before it reaches any of these three query
sites, either via a `{projectID:[0-9]+}` chi route constraint or an
explicit check at the top of each handler.

**One legacy DB write deliberately not ported, flagged as an open question
— same treatment S13/S14/S18 give their own external-ownership gaps.**
Legacy also inserts a `chat_messages_attachment` row (schema
`c.POSTGRES_TENANT_SCHEMA`, a polymorphic child of `message_items`) so a
chat message renders its attachments inline. That table is absent from this
service's current migration baseline
(`internal/db/schema/agent_chat_baseline.sql` has
`chat_conversations`/`chat_message_items`/`chat_messages_text`/
`chat_messages_context` — no `chat_messages_attachment`) — and unlike
`centry.project`/`centry.notifications` (externally-owned tables this
service already references with an established "stub it in integration
tests, never migrate it" convention), there is no existing "current
baseline" doc establishing who owns this table's DDL today. Inventing a
migration for it risks colliding with whatever process — legacy's own
Alembic history, almost certainly — already owns it in the real shared
database. Left for a human owner to answer: does Go take over migration
ownership of `chat_messages_attachment`, is it provisioned by a shared
process this service should simply start writing to, or does the chat UI's
attachment rendering move onto `elitea_storage.objects` as a later,
separate, consumer-side change (out of this plan's scope per "Any change to
consumer code")? Until answered, an uploaded attachment's bytes are durable
and downloadable through the generic artifact API, but do not yet appear
inline in the chat transcript the way a legacy attachment does.

**A live wire-contract discrepancy found in legacy itself, not ported.**
Legacy's Pydantic contract names the overwrite flag `overwrite_attachments`,
but both call sites in the actual frontend hook
(`attachmentUpload.hook.ts`'s `uploadSmallFile`/`uploadChunk`) send the
field as `overwrite` instead — meaning `overwrite_attachments` silently
never receives a value from any real client today, and legacy's own
default (`False`, i.e. duplicate-filename rejection) is what actually
governs in practice regardless of what a caller intends. This stage's Go
implementation does not read either field at all: `finalizeAttachment`
always upserts via `elitea_storage.objects`' own `UpsertObject` (an
`ON CONFLICT ... DO UPDATE`, not a create-only insert), so a same-key
re-upload always succeeds and overwrites — closer to "always overwrite"
than to either of legacy's two candidate flag names' `False` default.
Recorded here rather than silently reproduced, since porting a bug that was
itself already dead code in every real caller would only be faithful to
something no client actually exercises.

**Full verify sweep green:** `gofmt`, `go build`, `go vet`, `task lint`, the
literal Verify command above (10 `--- PASS`, 0 `--- SKIP`, handler-level
against in-memory fakes — no infra dependency, matching the Verify
command's own "must not SKIP" requirement), a new real-Postgres integration
test for `AttachmentChunksRepository` (retry-overwrites-not-doubles,
per-`(project,conversation,file)` isolation for a client-chosen `file_id`),
and a full workspace `task test` with every emulator + a real Postgres
attached (103 packages, zero failures).

**S20b — Icons.** `UploadIcon` writes to `ICONS_DATA_DIR` and returns
`/icons/{projectID}/{filename}`, but nothing serves `/icons/*` — no route, no
Traefik rule, no volume. Every uploaded icon is currently unreachable. Move
icons onto the object store using the S9 object plane with a reserved system
bucket, and serve them through the download route. **Edit the real,
production-mounted handler — `internal/api/v2/eliteacore/handler.go`'s
`UploadIcon`, registered at `internal/api/router.go:619-620` as
`POST /upload_icon/prompt_lib/{projectID}` — not
`internal/api/oapiserver/misc.go`'s `UploadApplicationIcon`.** That second
function looks like the same feature (it also returns an icon-upload URL) but
belongs to `oapiserver.Server`, which S7 already establishes is never wired
into production routing — it exists only for the package's own conformance
tests. Editing it would compile, pass its own package's tests, and leave the
real, reachable-but-broken icon path completely untouched. Once icon bytes
are written through the object store instead of `ICONS_DATA_DIR`, the
pre-existing `TestUploadIconStoresFileWithinConfiguredRoot`,
`TestUploadIconRejectsTraversalAndSymlinkEscape`, and
`TestDeleteIconIsConfinedToConfiguredRoot` are testing a code path (filesystem
path confinement) that no longer exists — delete them as part of this stage
rather than leaving them green-but-meaningless against dead code. Name the
replacement tests `TestArtifactIcon*` — `-run Icon` alone would keep matching
those three tests until they're actually removed, hiding whether the removal
happened.
*Verify:*
```
cd services/elitea-main && go test ./internal/api/v2/eliteacore/ ./internal/api/oapiserver/ -race -run TestArtifactIcon -v > /tmp/s20b.log 2>&1; rc=$?; cat /tmp/s20b.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s20b.log && ! grep -q -- '--- SKIP' /tmp/s20b.log
```

**No `elitea_storage.buckets`/`objects` metadata row for icons, unlike S20a's
attachments.** A deliberate asymmetry, not an oversight: icons have no
retention or per-project-quota requirement this plan names anywhere (unlike
chat attachments' legacy `chat_bucket_retention_days`), so there is nothing
S14's sweeper or S12's quota check would need to see. `iconBucket` is a bare
`storage.ObjectStore.Put`/`Get`/`Delete` against a fixed `"icons"` bucket
name (not per-project-policy-configurable like S20a's `attachment_bucket`
field) keyed `{projectID}/{filename}` via `ObjectRef`'s own project
namespacing — no Postgres involvement at all beyond what `UploadIcon`/
`DeleteIcon` already did (clearing `icon_meta` off `application_versions`).

**`DownloadIcon` is a standalone function, not a `*Handler` method — the
auth-group scoping forced this, not style preference.** `coreHandler` (the
real `eliteacore.Handler` instance `UploadIcon`/`DeleteIcon` are methods on)
is constructed *inside* `newProductionRouter`'s
`r.Group(func(r chi.Router) { r.Use(apimw.Auth(...)); ... })` closure — a
Go closure-scoped local variable, not reachable outside it. The route this
stage adds must sit *outside* that closure, alongside the two sibling
static-icon routes (`/app/application_icon/*`, `/app/application_tool_icon/*`)
already registered unauthenticated a few lines earlier in the same
function, for the same reason: a browser `<img src="/icons/1/abc.png">`
request carries no `Authorization` header, so gating this route behind the
session/token auth middleware would make every uploaded icon unreachable
again — the exact bug this stage exists to fix, reintroduced a different
way. `eliteacore.DownloadIcon(store storage.ObjectStore) http.HandlerFunc`
takes only the one dependency it needs and is constructed directly from
`cfg.ObjectStore`, entirely independent of `coreHandler`.

**Project ID validation quietly tightened as a side effect of switching to
`storage.ObjectRef`.** The pre-existing `validIconPathSegment` check (any
non-empty string without `/`, `\`, or a null byte, ≤255 chars) was written
for a local-disk directory name and never required `projectID` to be
numeric. `storage.NewObjectRef` requires `^[1-9][0-9]{0,17}$` — stricter,
and now the authoritative check (kept `validIconPathSegment` in place too,
as a cheap early-reject; `NewObjectRef`'s is what actually gates storage
access). A non-numeric project ID was never a realistic case in practice
(every other call site in this codebase already assumes a numeric project
ID — S8/S9/S15/S20a all `strconv.ParseInt` it), so this is treated as a
tightening that happens to fall out of the migration, not a scope item in
its own right — flagged here rather than silently absorbed.

**`UploadIcon`'s "no file" branch deliberately checks `h.store == nil` after
the `FormFile` lookup, not before.** The pre-existing, still-relevant
`TestUploadIcon` sends no file part at all and expects `{"ok": true, "url":
""}` regardless of whether storage is configured — a first draft that
checked `h.store` first broke it, caught immediately by running the
existing suite before writing anything new. `DeleteIcon` similarly stays
unconditionally `204` on any outcome (missing icon, unconfigured store,
invalid path segment) — matching its exact pre-S20b behavior, not
introducing a new error path for what was always a best-effort operation.

**Full verify sweep green:** `gofmt`, `go build`, `go vet`, `task lint`, the
literal Verify command above (7 `--- PASS`, 0 `--- SKIP`, handler-level
against an in-memory fake — no infra dependency), the full pre-existing
`internal/api/v2/eliteacore` suite (confirming `TestUploadIcon`/
`TestDefaultIcons`/the three `ProjectIcon` stubs/etc. all still pass
unchanged), and a full workspace `task test` with every emulator + a real
Postgres attached (103 packages, zero failures).

**Swarm review across S18–S20b: 48-agent adversarial pass, every finding
addressed.** Per the standing instruction to run a swarm review after full
implementation and fix everything it finds, a `Workflow`-orchestrated review
ran across the four just-completed stage groups (S18/S19/S20a/S20b): 2
lenses (correctness/bug-hunt, plan-compliance+test-integrity) × 4 stage
groups = 8 review agents, surfacing 20 candidate findings; each went through
2 independent adversarial verifiers (confirmed only if both agreed), 40
verify agents, 19 of 20 confirmed. Two were blocking, fixed first:

- **Stored XSS in icon serving (S20b).** `DownloadIcon` derived
  `Content-Type` from the backend's stored `ObjectInfo.ContentType` (an
  unauthenticated, public route — a browser `<img src>` carries no auth
  header), so an uploaded file with an HTML/SVG-script payload and a
  browser-sniffable or attacker-chosen content type could execute script in
  the app's own origin. Fixed at both the write path (`allowedIconExtensions`
  allowlist gating `safeIconExtension`, restricting storage to genuine image
  extensions) and the read path (`Content-Type` now derived only from the
  allowlisted extension, never the stored value; `X-Content-Type-Options:
  nosniff` added). Regression test:
  `TestArtifactIconUploadRejectsNonImageExtensionAndDownloadSetsNosniff`.
- **Unbounded chunk-storage growth (S20a).** Nothing capped `total_chunks`
  on a chunked attachment upload before storing chunks, and nothing ever
  reclaimed an abandoned session's partial chunks — an attacker or a client
  bug could grow `elitea_storage.attachment_chunks` without bound. Fixed
  with `maxAttachmentChunks()` (rejects `total_chunks` above the limit
  before any chunk is stored, 400) and a new `DeleteStaleAttachmentChunks`
  sqlc query wired into S14's existing retention sweeper as an optional
  `WithAttachmentChunks` dependency (`artifactRetentionStaleChunkTTL = 12h`).

Moderate/minor findings, all fixed:

- **S18's `instrumented.go` `DeleteBatch` audit had two bugs**, both in
  `LogAudit`'s per-key attribution: a bucket-collision misattribution (keys
  from different buckets folded into one lookup) and a silent omission (a
  key absent from `result.Deleted` never got an audit line at all, including
  on partial failure). Rewritten to iterate `refs` directly and emit exactly
  one audit line per requested ref, defaulting to `"failure"` when absent
  from `Deleted`. Regression test:
  `TestArtifactObservabilityInstrumentedStoreDeleteBatchAuditsEveryRequestedKey`.
- **S18 test-coverage gaps (3), closed.** `TestArtifactObservabilityInstrumentedStoreEmitsAuditThroughTheRealWrapper`
  (the existing suite asserted audit *content* but never proved it flowed
  through the real `instrumentedStore.Delete` wrapper, not a bypassed
  helper); a new cross-package OTel `ManualReader` harness in
  `internal/runtimecomposition` (`setupArtifactRetentionGaugeReader`,
  mirroring `observability_test.go`'s own pattern) backing
  `TestArtifactRetentionSweepRecordsTheActualByteTotalNotJustCallPresence`
  (proves the gauge shows the real `SumProjectBytes` value, not a swapped
  argument that happened to also be an `int64`); and
  `TestArtifactGrantCreateEmitsAuditLogOnSuccess` (asserts real `slog`
  output from `CreateTransferGrant`'s success path, not just that the call
  compiles).
- **S20a: chunk-merge buffer over-allocation.** The merge step
  pre-allocated `len(chunks) * attachmentMaxChunkBytes` regardless of each
  chunk's actual size; changed to sum actual `len(c.Bytes)` across fetched
  chunks.
- **S20a: attachment size/retention limits ignored the project storage
  policy.** `finalizeAttachment` now calls `AttachmentPolicy` once and uses
  its `max_object_bytes`/`retention_default_days` as overrides
  (policy-first, env-fallback-second), matching `objects.go`'s S12
  precedent — previously only the env-var defaults applied, regardless of
  a project's configured policy. Regression test:
  `TestArtifactAttachmentUsesProjectStoragePolicyFileLimit`.
- **S20a: chunk-merge test only proved arrival-order merge, not
  index-order.** `TestArtifactAttachmentChunkedUploadReceivesThenMerges`
  sent chunk 1 before chunk 0 in the underlying fix, and the merge step
  reorders by `chunk_index` rather than assuming request arrival order
  already matches it.
- **S20a's `router.go` `WithPool` reactivating three pre-existing,
  unrelated pool-dependent methods** — investigated and documented rather
  than fixed, as its own paragraph above under S20a explains (confirmed
  unreachable from real production traffic: `composition.go` never sets
  `RouterConfig.ConvsRepo`).

**Bugs the fix pass itself introduced, and caught by rerunning the suite
against real infra afterward — not by inspection.** Two of `tests/contract`'s
pre-existing helpers, `decodeArtifactJSON` (read body, no close) and
`requireArtifactStatus` (check status, no body read), were unified into one
`requireArtifactJSON` that reads-and-closes exactly once, closing an HTTP
connection leak (the old `requireArtifactStatus` never closed a successful
response's body) and a lost-diagnostic-body bug (the old
decode-then-status call order left a status-mismatch failure message
reading an already-drained body). This refactor itself broke three call
sites that read a response's *raw* bytes after a status check
(`TestArtifactObjectUploadDownloadHeadDeleteRangeRead`'s full-object and
range-read downloads, `TestArtifactGrantPresignedUploadAndCommitRoundTrip`'s
downloaded-object check) — each now called the new `requireArtifactJSON`
for the status check first, which drains and closes the body, then tried to
`io.ReadAll` the same already-closed body for the digest/content
comparison, either erroring outright (`http: read on closed response body`)
or, where the read error was discarded, silently comparing against an empty
byte slice. Only surfaced by actually running `go test ./tests/contract/...`
against the real Postgres + RustFS stack afterward — the package still
compiled and `go vet` stayed silent, since nothing about a double body read
is a static-analysis error. Fixed with a third helper,
`requireArtifactBody`, for exactly this raw-bytes-after-status shape (read,
close, check status, return bytes — one call, one read).

**S19's `TestArtifactErrorEnvelopeCoversRemainingCodes` acceptance
criterion — "every error code in the envelope enum is reachable by some
request" — closed for `NotImplemented`/`Internal`, the two codes it did not
yet cover.** Both are unreachable through `tests/contract`'s own harness:
its one real backend (RustFS/S3) never returns `storage.ErrNotSupported`
for any operation the harness exercises, and forcing a genuine Postgres
failure without corrupting the shared suite's own database would need a
second, disposable harness. Investigated instead of built from scratch:
`internal/api/artifact_stub_routes_test.go` (S7/S8/S9) already drives both
codes through the real router in a white-box `RouterConfig`-level suite —
`TestArtifactStubRoutesReturn501WithTypedEnvelope` already asserts
`NotImplemented` for all 13 stub routes, and
`TestArtifactBucketRoutesWireToRealHandlerWhenConfigured` already drives a
request into the real handler against an unreachable pool (port 1,
connection refused), landing on `Internal` — but only asserted the code was
*not* still the S7 stub, never pinning the exact code. Added one new test,
`TestArtifactRouteReturnsInternalCodeWhenBackendFails`, targeting
`ListBuckets` specifically (the one bucket-plane route with no request body
to decode first, so nothing else can 400 before the pool is ever touched)
and asserting `code == "Internal"` explicitly — closing the gap without
building a second ephemeral-database harness for one assertion.

**Full verification after every fix above:** `gofmt -l` (clean on every file
this fix pass touched — pre-existing, unrelated formatting debt in ~27
other files, none of them touched here, is left as-is), `go build ./...`,
`go vet ./...`, `task lint` (0 issues), `task vet`, `task build`, the S18
and S19 Verify commands re-run directly against real Postgres + RustFS +
Azurite + fake-gcs-server, and a full workspace `go test -race -count=1
./...` with every emulator and a real Postgres attached — every package
`ok`, zero `FAIL`.

**S20c — Result-artifact attestation.** `elitea_runtime.index_result_artifacts`
is documented as owned by "the future artifact upload data plane".
`ArtifactVerifier` is a required production dependency, `VerifyDurable` returns
`ErrIndexIngestArtifactUnavailable` on no rows, and **there is zero non-test
insert into the table** — so any index-ingest result carrying a result artifact
cannot settle today. Implement the writer: a grant, commit, and verify path on
the S15 machinery, honouring the existing 1 MiB and `application/json` policy,
sharing the `ObjectStore` adapter but keeping its own metadata and settlement
semantics per ADR-0014. Requires an answer to open question 2 in the spec.

This stage builds a writer with **no wired caller** — same shape as S13.
`ArtifactVerifier`'s production implementation lives in
`internal/application/output/`, called from the index-ingest completion path,
but nothing in that path today constructs a grant or commits bytes to the
object store first; the codebase has no worker- or provider-facing surface
that a result-producing caller (a skill run, a pipeline step,
`services/elitea-worker-python`, or elsewhere) could call to obtain a grant,
stream the artifact bytes, and commit before index-ingest tries to verify it.
Implement `CreateArtifactGrant` / `CommitArtifact` / `ResolveArtifact` per
ADR-0014 as isolated, tested functions on the S15 grant machinery, and leave
them unwired from any producer. Do not invent a call site — record as an open
question for a human owner: who builds the producer-side caller (is
`services/elitea-worker-python` in scope, or a Go-side skill/pipeline runner?),
and whether that's a new stage or belongs to the worker's own tracked issue.
Name every new test `TestIndexIngestArtifact*` so the Verify command actually
matches something — `-run IndexIngestArtifact` alone matches zero tests today
(there is nothing in the tree that satisfies that filter), so the bare command
would exit 0 for the wrong reason: no tests found, not tests passed.
*Verify:*
```
cd services/elitea-main && go test ./internal/application/output/ ./internal/infra/db/repos/ -race -run TestIndexIngestArtifact -v > /tmp/s20c.log 2>&1; rc=$?; cat /tmp/s20c.log; test $rc -eq 0 && grep -qc -- '--- PASS' /tmp/s20c.log && ! grep -q -- '--- SKIP' /tmp/s20c.log
```

**Open question 2 answered before implementation, via explicit user
direction:** yes, the attestation plane shares this `ObjectStore` — S20c is
implemented on the existing S15 grant machinery
(`ArtifactTransferGrantsRepository`, `elitea_storage.transfer_grants`) and
`storage.ObjectStore`, per the plan's own default framing above, rather than
inventing a second grant table or object-store adapter.

**Package split mirrors the existing ArtifactVerifier/VerifyDurable
pair.** `internal/application/output/index_artifact_writer.go` defines the
domain contract only — `ArtifactGrant`, `CreateArtifactGrantRequest`,
`CommitArtifactRequest`, and the `ArtifactGrantIssuer` interface (`Create
ArtifactGrant`/`CommitArtifact`/`ResolveArtifact`) — the same layering
`ArtifactVerifier` already uses one file up. `internal/infra/db/repos/
index_result_artifact_writer.go`'s `IndexResultArtifactWriter` is the
concrete implementation, composing `*ArtifactTransferGrantsRepository`,
`*ArtifactBucketsRepository`, and `storage.ObjectStore` — the same three
dependencies `internal/api/v2/artifacts/grants.go`'s `CreateTransferGrant`/
`CommitTransferGrant` already use, just orchestrated as plain Go methods
instead of HTTP handlers, since this stage has no route to mount. ADR-0014
actually lists `CreateArtifactGrant`/`CommitArtifact`/`ResolveArtifact`
among the *runtime control service's* stable gRPC methods (mTLS unary
Protocol Buffers) — this stage deliberately stops at the isolated
application-layer logic those RPCs would eventually call, not the RPC
methods themselves: `AGENTS.md`'s "no gRPC" guardrail for this codebase's
general application/repository layers, and the existing
`internal/transport/runtimegrpc/control` package (which has no reference to
any of these three names) is where that transport wiring would live, as a
separate, later stage.

**`storage_record_id` is the S15 grant's own id, reused rather than
inventing a second identifier.** `DurableIndexArtifact.StorageRecordID` is
documented as "opaque... never a data-plane URI or token" — a fresh UUIDv4
grant id already satisfies that, and `elitea_storage.transfer_grants` rows
are never deleted (grepped for a delete path; none exists — only S14's own
lifecycle rule reclaims *backend-side* incomplete multipart sessions, never
metadata rows), so `GetTransferGrantByID(storageRecordID)` durably resolves
bucket/key for `ResolveArtifact` indefinitely. The bucket itself is a fixed
per-project system bucket (`index-artifacts`), auto-created on first use —
the same `RequireAttachmentBucket`/`iconBucket` convention S20a/S20b
already established, again because no project-creation hook exists
anywhere in this service for a writer to depend on.

**Real bug found running the Verify command, not by inspection: a CHECK
constraint the schema already had.**
`index_result_artifact_verified_order CHECK (bytes_verified_at >=
metadata_created_at)` — `bytes_verified_at` is caller-supplied, but
`metadata_created_at` only had a `DEFAULT clock_timestamp()`, evaluated
server-side at INSERT time. A Go-computed `time.Now()` stamped before the
INSERT statement runs is always microseconds earlier than that DEFAULT,
so every real commit's insert failed the CHECK (SQLSTATE 23514) — this
never showed up in `go build`/`go vet`, only in the real-Postgres
integration test. Fixed by setting `metadata_created_at` explicitly to the
same Go-computed value as `bytes_verified_at` in `InsertIndexResultArtifact`
(`internal/db/queries/runtime_index_ingest.sql`), guaranteeing equality
rather than relying on two independently-evaluated timestamps to land in
the right order.

**Duplicate-identity commit handling: retry vs. conflict, distinguished
in Go, not SQL.** `InsertIndexResultArtifact` has no `ON CONFLICT` clause on
purpose — a `(artifact_id, immutable_version)` primary-key collision can
only be reached *after* `MarkTransferGrantConsumed` already succeeded on
the grant this call is committing (an exact replay of the *same* grant
always fails earlier, at `grant.ConsumedAt != nil`), so a collision here
is always a *different*, fresh grant claiming the same declared artifact
identity — either a legitimate producer retry (a prior grant/commit round
trip the caller never observed the result of) or a genuine identity reuse
bug. `IndexResultArtifactWriter.reconcileDuplicateCommit` loads the
existing durable row and compares every field the newly-verified upload
produced against it: an exact match returns the existing row as a
successful idempotent no-op; any mismatch returns the new
`ErrArtifactGrantConflict` sentinel, kept distinct from
`ErrIndexIngestArtifactMismatch` (which fires earlier, for an upload that
disagrees with its own grant) so a caller can tell "this commit's bytes
were wrong" apart from "this identity was already committed as something
else." Both branches leave the just-uploaded, now-orphaned object in the
bucket — a bounded, non-permanent leak, the same tolerance this plan's own
`rejectCommit` (S15) and multipart-abort-failure (S16) precedents already
accept, since nothing outside a caller ever observes it and nothing in
this bucket is tracked by S14's `elitea_storage.objects`-based sweeper
regardless.

**Test proves genuine integration with the pre-existing read side, not
just the new write side in isolation.** Every other `index_ingest_results`
Postgres integration test in this repo seeds its artifact row with a raw
`INSERT` (`seedPostgresIndexArtifactAttestation`) — necessary before this
stage, since nothing produced a real row any other way.
`TestIndexIngestArtifactGrantCommitResolveAndIngestRoundTrip` does not call
that helper at all: it drives a real `CreateArtifactGrant` → real presigned
PUT against RustFS → real `CommitArtifact`, then feeds the exact same
`IndexIngestFrame` straight into the already-shipped, unmodified
`IndexIngestService.IngestIndex` (`ArtifactVerifier.VerifyDurable` reading
back precisely what this stage's writer just committed) and asserts it
settles — proving this stage's write path and S16-era's existing read path
actually agree on the wire, not just independently pass their own tests.
Also covers: an exact retry through a *fresh* grant is idempotent (same
`StorageRecordID` returned, no second durable row); a fresh grant reusing
the same artifact identity with genuinely different content returns
`ErrArtifactGrantConflict`; and `ResolveArtifact`'s returned URL, fetched
for real, serves back the exact bytes that were committed.

**Full verify sweep green:** `gofmt`, `go build`, `go vet`, `task lint` (one
staticcheck finding — an unnecessary embedded-field selector in the new
test file — fixed), the literal Verify command above, and a full workspace
`go test -race ./...` with every emulator + a real Postgres attached, zero
failures.

**Still open, exactly as this stage's own text requires — not answered
here:** who builds the producer-side caller for `CreateArtifactGrant`/
`CommitArtifact` (`services/elitea-worker-python`, a Go-side skill/pipeline
runner, or elsewhere), and whether wiring it — along with the
`CreateArtifactGrant`/`CommitArtifact`/`ResolveArtifact` gRPC methods
ADR-0014 lists on the runtime control service — is a new stage of this plan
or belongs to the worker's own tracked issue. `IndexResultArtifactWriter`
has no caller anywhere in this codebase, by design, matching S13's own
precedent.

**S20c-scoped swarm review: 4 agents (2 review lenses + 2 independent
adversarial verifiers), 4 findings, both verifiers confirmed all 4, all 4
fixed.** Smaller than the 48-agent S18-S20b pass, scaled to this stage's own
size (5 files, ~650 lines) — 2 review lenses (correctness/bug-hunt,
plan-compliance+test-integrity) read the stage in full plus every
cross-check file (`index_ingest_results.go`, `grants.go`,
`artifact_transfer_grants.go`), one of them additionally ran the plan's own
literal Verify command live and empirically committed a 10 MiB `image/png`
artifact against the running emulator stack to *prove* one finding rather
than infer it; 2 independent verifiers then read the live code
(not the findings' own quoted snippets) and tried to refute each — both
returned `refuted: false` on all 4, all fixed before commit:

- **Doc-comment overclaim.** `CommitArtifactRequest`'s comment claimed
  re-supplying identity at commit time lets `CommitArtifact` "detect a
  caller that tries to commit a grant against a *different* artifact
  identity" — true for only 3 of the 9 relevant fields
  (`MediaType`/`ByteLength`/`Digest`, the only ones the shared
  `elitea_storage.transfer_grants` row has room for); `ArtifactID`,
  `ImmutableVersion`, `ExecutionID`, `Generation`, `TenantID`,
  `ProjectionProjectID`, `CommandID`, `Classification` were never
  cross-checked. Fixed the comment to state exactly what is and is not
  checked, and flagged the real, still-open gap this exposes (a caller
  that mixes up which `CommitArtifactRequest` identity belongs to which
  grant ID is not caught) as a must-close-before-wiring item for whoever
  eventually connects a producer — not something this pass attempted to
  close by itself, since the only real fix (binding an identity digest to
  the grant) would mean extending the shared S15 grant schema, a
  larger-blast-radius change than this already-long pass should make
  hastily at the very end of the plan.
- **No grant-provenance check (fixed for real).** `CommitArtifact` never
  checked `grant.Method == "PUT"` or that the resolved bucket was
  `index-artifacts` — unlike S15's own `CommitTransferGrant`
  (`grants.go:382-385`), which does check `Method`. Since
  `elitea_storage.transfer_grants` is shared with S15's general-purpose
  REST grant API, a grant minted there for an unrelated bucket (with
  caller-controlled `content_type`/`max_bytes`/`digest`) could be
  "laundered" through this writer's `CommitArtifact`. Added both checks;
  regression tests exercise a real GET-method grant and a real
  different-bucket grant, each correctly rejected.
- **Missing cheap pre-hash size check (fixed for real).** `CommitArtifact`
  streamed and SHA-256-hashed the entire object before ever checking its
  size, unlike S15's `finalizeGrantCommit` (`grants.go:437-440`), which
  checks `info.Size > grant.MaxBytes` immediately after `Get` — a
  presigned PUT URL enforces no server-side size cap, so an oversized
  upload forced a full read+hash before rejection. Added the same
  early-exit check S15 uses (kept the post-hash check too, matching S15's
  own "optimization, not a replacement" reasoning for keeping both).
- **1 MiB/`application/json` policy unenforced at write time (fixed for
  real).** The plan's own text ("honouring the existing 1 MiB and
  `application/json` policy") was not honored anywhere — `CreateArtifactGrant`
  never referenced `IndexIngestOutputPolicy`; the only check that ran was
  `IndexArtifactReference.Validate()`'s generic non-zero-length/parseable-MIME-type
  check. Empirically proven exploitable by the reviewer (a 10 MiB
  `image/png` grant succeeded cleanly). `NewIndexResultArtifactWriter` now
  takes the same `IndexIngestOutputPolicy`
  `NewIndexIngestResultsRepository` already validates against — reused
  directly rather than a second, independently-configured policy type, so
  a future caller wiring both cannot let them drift — and
  `CreateArtifactGrant` rejects a mismatched media type or an oversized
  artifact before ever minting a grant.

Four new regression tests
(`TestIndexIngestArtifactGrantCommitResolveAndIngestRoundTrip`'s own new
subtests) prove each fix against the real Postgres+RustFS stack. Full
resweep after fixing: `gofmt`, `go build`, `go vet`, `task lint` (0
issues), the literal Verify command above, and a full workspace `go test
-race ./...` with every emulator + a real Postgres attached — zero
failures.

**Plan complete: S1-S20c, all 21 stages (20 numbered plus S20's three-part
split), implemented, adversarially reviewed, and tested** — the swarm
review's own findings across both passes (S18-S20b's 48-agent pass and
S20c's own 4-agent pass) are fixed and regression-tested, fulfilling the
standing instruction to run a swarm review after full implementation and
fix everything it finds.

---

## Explicitly out of scope

Do not implement, and reject review requests that add them without a new ADR:

- **The entire `/artifacts/s3` surface**, and everything that existed only to
  serve it: AWS SigV4 verification, the customer-facing S3 credential API and
  its key format, S3 XML response bodies, the `?format=json` mode,
  `move_objects`, S3 Select, and the `?acl` / `?tagging` / `?lifecycle` /
  `?versioning` / `?delete` sub-resources.
- **Any migration of legacy artifact data**: the on-disk `libcloud` tree,
  `centry.storage_meta`, legacy bucket tags, dual-write, read-through shims, or
  a reverse migration.
- **Any change to consumer code.** `elitea-sdk`, `apps/elitea-web`,
  `apps/elitea-ui`, and the indexer are each a separate tracked issue.
- Bucket rename. It never existed in legacy; the Go prototype invented it and
  dropped retention doing so.
- Per-project bring-your-own-S3 credentials. Threaded through every legacy
  endpoint but a no-op in every deployment.
- `bucket_permissions` enforcement, until open question 1 in the spec is
  answered.
- Virus scanning and quarantine. Required by `spec-security-verification` but
  needs its own design and an external scanner dependency.
- Cross-region replication, object versioning, object lock, and *provider-native*
  soft delete/undelete (Azure Blob soft delete, GCS soft-delete). This is a
  different thing from the S6 `buckets.deleted_at` metadata tombstone that
  `SoftDeleteBucket` writes in S8/S13 — that one is in scope; it is an internal
  DB flag, not a provider recovery feature, and nothing here forbids it.

## Reference

- Decision: `elitea-docs/docs/internal/03-architecture/adrs/adr-0016-object-storage-architecture.mdx`
- Contract and current-state detail: `elitea-docs/docs/internal/03-architecture/cloud-native-migration/spec-artifact-storage.mdx`
- Consumer rework: tracked as issues in `EliteaAI/elitea_issues`
