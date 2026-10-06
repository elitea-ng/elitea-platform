# Graph receipt PostgreSQL CI admission

## Source mapping

The existing PostgreSQL 18 CI job owns its disposable database service.
`src/state/postgres_checkpointer_tests/graph_receipts.rs` requires explicit admission before it creates isolated databases.
`src/state/postgres_checkpointer_tests.rs` owns database creation, checkpoint migration, and cleanup.
`src/state/postgres_checkpointer.rs` retains the existing production checkpoint behavior.

## Failure and correction

The CI test step supplies `ELITEA_TEST_DATABASE_URL` without the two required graph receipt admission flags.
Both graph receipt tests fail at the required-mode assertion before they connect to PostgreSQL.

The same CI test step now sets `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`.
Its existing PostgreSQL 18 service, loopback URL, and `postgres` administrator database satisfy the remaining guards.
No test guard, ignore attribute, production checkpoint path, or dependency changes.

## Required coverage

The existing begin/pause/complete test verifies immutable receipts, exact replay, replacement takeover, and stale-writer refusal.
The existing append test verifies atomic latest-parent comparison and an unchanged checkpoint frontier.
Both tests remain unignored in the ordinary CI test command.
Each test creates a uniquely named `elitea_rust_cp_` database.
The existing cleanup owner attempts to remove these test databases.

Run the focused group against an explicitly owned disposable PostgreSQL 18 server:

```sh
ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1 \
ELITEA_TEST_DATABASE_GUARD=disposable-pg18 \
ELITEA_TEST_DATABASE_URL='<loopback URL for the disposable postgres database>' \
cargo test --locked --all-features -j1 --lib \
  state::postgres_checkpointer_tests::graph_receipts:: -- --nocapture
```

## Focused verification

The current-source-pinned macOS test binary passes both required tests against a fresh PostgreSQL 18 container.
The direct exit is zero: two passed, zero failed, and zero ignored.
The run uses required mode and the disposable database guard.
It requires no compiler or stack override.
The fixture has one CPU and a 768 MiB memory limit.

Root verifies the fixture container's exact identity and owner before removing it.
The fixture stop exits zero, and container removal is verified.
Per-database cleanup is not separately queried.
No shared service or live product database changes.

The replacement Linux CI result remains required.
This focused PostgreSQL proof does not establish deployed runtime acceptance.
