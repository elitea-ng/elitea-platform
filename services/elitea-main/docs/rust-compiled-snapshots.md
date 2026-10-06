# Rust compiled snapshots

This feature reuses one immutable compiled executable for an exact prepared request.
Package bundles remain separate from compiled snapshots.
The feature stays disabled by default.
Runtime acceptance and performance measurements remain separate gates.

## Operator authority

Set `ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_ENABLED=true` only after worker and supervisor assembly passes acceptance.
Enable the existing agent dispatch and exact sandbox audience configuration first.
Set every file and quota value in the table.
Reject partial configuration while the feature is disabled.

| Variable suffix after `ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_` | Required value |
| --- | --- |
| `PROFILES_FILE` | Absolute canonical release profile file path |
| `PROFILES_SHA256` | Lowercase SHA-256 of the exact profile file bytes |
| `GLOBAL_ENTRIES` | 1..100000 |
| `GLOBAL_BYTES` | 1..1099511627776 |
| `TENANT_ENTRIES` | 1..GLOBAL_ENTRIES, counted across all tenant projects |
| `TENANT_BYTES` | 1..GLOBAL_BYTES, counted across all tenant projects |
| `PUBLISHING_TTL_SECONDS` | 1..300 |
| `READY_TTL_SECONDS` | 1..86400 |

Set `ELITEA_RUST_COMPILED_AGENTSTATE_DSN_FILE` to the canonical private operator DSN file.
Use the same original agentstate database as the supervisor ledger.
Do not place DSN contents in a ConfigMap or environment value.
Do not use the Main business database for this pool.
The constructor opens at most four connections and closes the pool after runtime shutdown.
Main checks the exact embedded agentstate migration head at startup.
The existing migrator still accepts `AGENTSTATE_DATABASE_URL`.
Main does not apply migrations at startup.

Mount both files in the existing runtime material directory.
Keep every parent directory under operator ownership.
Reject symlinks, non-regular files, mutable public profiles, and public private DSN files.
Bound the profile file at 1 MiB and the private DSN file at 16 KiB.

## Immutable release profiles

Write exact canonical JSON with revision 1 and 1..64 profiles.
Each profile contains `binding` and `dependency_bundle_sha256` fields.
Use the exact version 1 runner binding fields and field order.
Use an empty dependency bundle root for the built-in vendor profile.
Bind a retained Cargo profile to its exact native bundle root.
Do not copy test fixture hashes into release configuration.

The trusted release profile pins both images, platform, target, policy, Cargo inputs, vendor, toolchain, adapter, wrapper, and flags.
Tenant, project, source, and prepared request fields bind each request separately.
The complete prepared request includes the exact input and timeout.
A caller cannot add a release profile or attest its own compiler.

## Main composition and lifecycle

The optional control request uses a 1 MiB plus 80 KiB frame limit.
Other control requests keep their existing 64 KiB limit.
The feature uses the existing private mTLS content listener and ObjectStore.
Read and publication use distinct revision 4 grants.
Native package and execution grants cannot authorize compiled publication.

The compiler records its descriptor and export proof under the original fenced lease.
The compiler can release that lease before initial publication authorization.
Executable staging requires the publisher's current live original runtime lease.
The immutable export epoch remains separate from later publication lease epochs.
Staged rows cannot authorize reads or execution.
Ready publication requires the exact successful original compile receipt and confirmed runtime cleanup.
File upload alone cannot make a row ready.

The existing replay maintenance loop runs one bounded cache sweep per pass.
Each sweep claims at most 32 rows and uses a 30-second deadline.
Quota includes publishing, ready, and evicting rows.
Failed deletion retains quota until a fenced retry removes both fixed objects.
The original compile receipt remains retained while its index row exists.
Recorded original execution recovery does not require a fresh cache lookup.
Expiry cannot create a new execution identity after dispatch.

## Isolated PostgreSQL fixture

Run `bash scripts/contract/check_compiled_snapshots_postgres.sh` against a new empty disposable database.
Set `ELITEA_COMPILED_SNAPSHOT_PG_FIXTURE_DSN` to its literal loopback URL with an explicit port and `sslmode=disable`.
Use a database name with the `elitea_compiled_fixture_` prefix.
Set `ELITEA_COMPILED_SNAPSHOT_PG_DISPOSABLE_ACK=isolated_fixture_only`.
The script requires fixture material and fails on omission.
The owning `compiled-snapshots-postgres` CI job provides these values.
The ordinary local test skips explicitly when fixture material is absent.
The test creates no database and drops no database.
It applies only the exact owning agentstate migration history inside the guarded empty fixture.
The operator or CI service owns disposal.

The fixture covers concurrent quotas, staged visibility, successful publication, cleanup, crash recovery, expiry, row locks, and receipt retention.
Fixture callbacks model fixed object verification and deletion.
ObjectStore and mTLS tests check their own content and authority boundaries separately.
Real Linux supervisor authority and full product acceptance remain required before enablement.
