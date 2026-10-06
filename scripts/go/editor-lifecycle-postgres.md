# Editor Test PostgreSQL acceptance

The `editor-lifecycle` job owns these tests in `ci-go.yml`.
It uses an isolated PostgreSQL 18 service.
The service creates `elitea_it_editor_lifecycle_20261002` on `127.0.0.1:15444`.
The job supplies only `ELITEA_EDITOR_TEST_DATABASE_URL`.
The fixture rejects other database names, listeners, connection fallbacks, and PostgreSQL versions.
It verifies empty database provenance, normal bootstrap, migrations, and both migration heads.

Run `bash scripts/go/editor-lifecycle-postgres.sh` with the explicit fixture URL.
The script clears the three product and package-harness database variables.
It sets `ELITEA_REQUIRE_EDITOR_POSTGRES_TEST=true` and limits Go concurrency to two.
Required mode fails when the explicit URL is absent.
Optional local runs retain the existing omission skip.

The runner selects `TestEditorLifecycle` tests and the exact `TestEditorEmptyStopPostgres` fixture.
It enables the race detector and records Go JSON events.
The JSON gate requires all thirteen acceptance scenarios and both fixture parents to pass exactly once.
Any failure, skip, missing scenario, or missing package result fails the job.
The summary reports nine lifecycle scenarios and four empty Stop scenarios separately.
The runner stores compiler and download diagnostics in a separate stderr log.
The JSON gate reads only Go test events.
The job uploads both logs and the measured summary.

The broad workspace job retains its PostgreSQL 16 harness.
Its editor skips refer to the separate required job in the skip ledger.
Those skips do not count as acceptance.
The dedicated job must pass for this workflow to pass.

Seven scenarios verify atomic scope, immutable identity, saved versions, normal lists, competing admission, pagination, and paused recovery.
Two scenarios verify receipt-bound traces, concurrent regeneration refusal, and 57-row trace pagination.
Four empty Stop scenarios verify these behaviors:

- Preserve the original canceled receipt through fresh History and replay.
- Preserve actor, project, and generation fences.
- Keep ordinary empty Stop deletion and durable replay behavior.
- Deny Stop for successful terminal runs without a pause.

The tests use production admission, terminal persistence, and trace projection.
They do not run workers, product databases, browser acceptance, or runtime cleanup.
