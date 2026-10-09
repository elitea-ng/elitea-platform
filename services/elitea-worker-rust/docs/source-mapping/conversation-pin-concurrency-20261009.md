# Conversation pin concurrency

PR elitea-ng/elitea-platform#1168, branch `fix/conversation-pin-deadlock`. It was first based on `origin/main` at `7ac0eaf9` and then rebased onto
`b4ffbe33f`. Evidence was collected on 2026-10-08 and 2026-10-09, and the PR was opened on 2026-10-09.

**Scope after the rebase.** While this branch was being prepared, #1163 (`f084c75b4`) landed on `main` with the Pin
half of the fix: `FOR SHARE OF c` became `FOR NO KEY UPDATE OF c`. That closes the CI failure (Pin vs Pin). **Unpin
was still unfixed on `main`.** It locked the pin row before the conversation row, so it could still deadlock against
a Delete or a Pin. This PR fixes Unpin and adds the tests and this record. For Pin, it only adds one comment sentence
that states the lock order shared by all three writers.

The CI job **Go (PG18)** failed intermittently in
`services/elitea-main/internal/api/v2/folders/pin_routes_postgres_integration_test.go`
`TestPublicPinRoutesConcurrentUpsertsKeepOneSharedRow`, reporting "concurrent pin returned HTTP 500". It failed on
`main` at `604a49e1` (#1139) and `fcf86c31`, and on PR #1144 run 37803287273. Other runs passed.

## Business behaviour

**Kept from the current platform:** a conversation has one shared pin per project, visible to every member who can
see the conversation. Any authorized member can pin or unpin it, and repeating either action succeeds.

**Unchanged contract of the new platform:**
- one `centry.social_pins` row per `(entity, project_id, entity_id)`, recording the last pinner;
- the conversation's `sync_at` is stamped only when the pin state changes (a new pin or a removed pin), never on a
  repeated pin, and `updated_at` is never touched;
- a 404 for a conversation that is deleted or that the caller cannot see;
- chat authority is checked inside the effect transaction;
- a Delete that races a Pin never leaves an orphan pin (the earlier F3 fix on #1139).

**Not ported:** nothing. The current platform has no equivalent locking, and an aborted transaction surfacing as a
500 is not behaviour worth keeping.

## Root cause

The failure was verified on real PostgreSQL 18 before any change:
- 50 runs of the folders test: 2 failed runs and 14 HTTP 500s;
- the server logged 14 `deadlock detected` (SQLSTATE 40P01);
- in every cycle, the pin `INSERT … ON CONFLICT` was waiting while the stamp `UPDATE … sync_at` held the conflicting
  lock.

| Pair | Interleaving before the fix |
|---|---|
| Pin vs Pin (the CI failure) | Both take `FOR SHARE` on the conversation, and the two locks are compatible. T1 inserts the pin row without committing. T2's `ON CONFLICT` waits for T1. T1's stamp `UPDATE` needs `FOR NO KEY UPDATE`, which conflicts with T2's `FOR SHARE`. That is a deadlock. |
| Unpin vs Pin, pin row present | Unpin deletes the pin row (locking it), then its stamp waits on Pin's `FOR SHARE`. Pin waits on the pin row Unpin holds. That is a deadlock. |
| Unpin vs Delete | Unpin read the conversation **without a lock**, deleted the pin row, then waited on the conversation for its stamp. Delete holds the conversation (deleted) and waits on the pin row. That is a deadlock. |

Unpin was the only writer that took the pin row before the conversation row. Delete
(`services/elitea-main/internal/infra/db/repos/conversations.go:878`, pin removal at `:982`) and Pin lock the
conversation first. A trace of every other `social_pins` writer and every `chat_conversations` locker found no
further inversion:
- project deletion touches `social_pins` and `centry.project` only;
- agent admission, node recovery and participant removal lock the conversation but never `social_pins`;
- the FK child inserts take `FOR KEY SHARE`, which does not conflict with `FOR NO KEY UPDATE`.

## Changes

| Path | Change |
|---|---|
| `services/elitea-main/internal/infra/db/repos/social_pins.go:117` | **Already on `main` through #1163.** Pin locks the conversation `FOR NO KEY UPDATE OF c` (was `FOR SHARE OF c`). This is the same lock the stamp needs, taken before the pin row. Concurrent Deletes are still blocked, so the orphan-pin protection is unchanged. This PR adds only the shared-order sentence (`:113-114`). |
| `services/elitea-main/internal/infra/db/repos/social_pins.go:186` | **This PR.** Unpin's `visible` CTE takes `FOR NO KEY UPDATE OF c`. The pin `DELETE` is guarded by `EXISTS (SELECT 1 FROM visible)`, so it cannot lock the pin row before the conversation is locked (comment `:181-184`). |

Every writer now locks the **conversation row first and the pin row second**. No retry loop was added: the
deadlock cannot occur, so there is nothing to retry.

## Tests

New file: `services/elitea-main/internal/infra/db/repos/conversation_pin_concurrency_postgres_integration_test.go`.

| Test | What it proves | On `7ac0eaf9` (neither half) | On `main` `b4ffbe33f` (Pin only, #1163) | This PR |
|---|---|---|---|---|
| `TestConcurrentConversationPinsAndUnpinsNeverDeadlock` (`:100`) | 8 rounds × 3 phases × 12 concurrent workers (`pinRaceWorkers`, `:35`). Concurrent pins leave **1** row and **exactly 1** stamp. Concurrent unpins leave **0** rows and **exactly 1** stamp. Interleaved pins and unpins all succeed and leave at most 1 row. Stamps are counted by a test-only trigger on `sync_at` in the isolated database. | FAIL: `deadlock detected (SQLSTATE 40P01)` | PASS 5/5 (the Unpin-vs-Pin cycle remains possible but was not hit) | PASS |
| `TestAnUnpinRacingTheConversationDeleteIsNotADeadlock` (`:161`) | A **forced** interleaving: a held transaction deletes the conversation the way Delete does, Unpin is confirmed blocked through `pg_stat_activity`, then the pin is removed and the transaction commits. Unpin must answer 404, neither side is aborted, and no pin row is left. | FAIL: Unpin returned 40P01 | **FAIL 5/5**: Unpin returned 40P01 | PASS |

**Mutation check** (on `7ac0eaf9`, before the rebase). Each half of the fix was reverted on its own:
- Unpin without the lock: the forced test failed 3 out of 3 times.
- Pin back on `FOR SHARE`: the stress test failed 3 out of 3 times, through its interleaved phase.

**Repeated runs after the fix, PG18 (`pgvector/pgvector:0.8.1-pg18-trixie`, `deadlock_timeout=100ms`):**
- the 5 repo pin tests (the 2 new ones plus `TestAPinRacingTheConversationDeleteLeavesNoOrphanPin`,
  `TestConversationPinReachesTheOtherDeviceThroughTheDelta` and `TestDeletingAPinnedConversationIsOneDeletedTombstone`)
  at `-count=20`: all pass;
- `TestPublicPinRoutes*` (folders) at `-count=50`: all pass;
- **0** new deadlocks in the server log.

**`go test -race` on the touched and dependent packages, on the branch rebased onto `b4ffbe33f`:**

One run, `-timeout 60m`, real PG18. "Tests" are top-level tests; "incl. subtests" also counts subtests.

| Package | Tests passed | Incl. subtests | Skipped | Failed |
|---|---|---|---|---|
| `internal/infra/db/repos` | 918 | 1,669 | 10 | 0 |
| `internal/api/v2/folders` | 36 | 38 | 0 | 0 |
| `internal/api/v2/social` | 47 | 115 | 0 | 0 |
| `internal/api/v2/eliteacore` | 356 | 651 | 0 | 0 |
| `internal/api/v2/conversations` | 162 | 204 | 0 | 0 |

0 data races. After the rebase, the repo pin tests at `-count=20` and the folders `TestPublicPinRoutes*` at
`-count=50` passed again, with 0 new server deadlocks.

Earlier runs before the rebase:
1. The first `-race` run hit Go's default 10-minute test timeout.
2. A run with `-timeout 60m` failed one test, `TestCorpusOnlySchemaSupportsFoldersAndSelectedConversations`, with 20 s
   context deadlines while the machine was loaded by about 10 other local stacks. That test does not touch pins, and
   it passes alone in about 3 s on both this branch and `origin/main`.
3. A third run passed with 0 failures and 0 races.

The 10 `repos` skips are pre-existing and each needs an environment this run did not provide:
- `TestPostgresNATSServiceBackedVisibilityRepair` (secured NATS);
- `TestCompiledSnapshotPostgresLifecycle` (`ELITEA_COMPILED_SNAPSHOT_PG_REQUIRED`);
- `TestEditorEmptyStopPostgres`, `TestEditorLifecyclePostgres`, `…TracePagination` and `…TraceReceiptIdentity`
  (`ELITEA_EDITOR_TEST_DATABASE_URL`);
- `TestPostgresServiceBackedCommandBusOutageBacklogReliability`;
- `TestIndexIngestArtifactGrantCommitResolveAndIngestRoundTrip` (S3);
- `TestPostgresPgvectorSameTargetSerializationAcrossInstalledSDKProcess` (installed SDK);
- `TestCurrentSocialAuthorsPostgresCompatibility` (`ELITEA_SOCIAL_AUTHORS_TEST_DATABASE_URL`).

**Other checks:**
- `go vet ./...`: clean, both before and after the rebase.
- `gofmt -l`: clean for the touched files. It flags one existing file, `analytics_tools_postgres_integration_test.go`,
  which is also unformatted on `main` and was not changed here.
- `golangci-lint`: not installed locally. This is an open item, not a pass; CI runs it.

## Performance

**Budget:** no added round trips, statements or bytes, and no new hot-path work.

**Result:**
- Statement count is unchanged. Each Pin and Unpin is one statement, plus one stamp `UPDATE` only on a state change,
  inside one transaction. Only the lock strength changed.
- Pinners and unpinners of the **same** conversation now run one after another rather than in parallel. Each held the
  same row lock for its stamp anyway, so the extra wait is limited to a repeated, no-op pin or unpin.
- Measured: 296 contended calls on one conversation (288 in the stress rounds plus 8 resets) completed in 0.78 s
  under `-race`, about 2.6 ms per call when run one after another.
- Browser burst: 120 requests in 10 rounds of 12 concurrent calls, all 200. The run 2 burst took 101 ms; the browser
  allows 6 connections per host, so at most 6 calls overlap.

**Accepted trade-off (code-review finding 1):** a no-op Unpin now waits behind an in-flight conversation writer, for
example agent admission holding `FOR UPDATE`. Before, it returned without a lock. Pin already waited the same way.
Both stay inside the existing 5 s budget (`social_pins.go:81`, `:163`).

## Durability

**Crash windows:**
- a Main crash, or a lost database connection, between the pin statement and the commit;
- the same between the stamp and the commit.

**Recovery rule:**
- one transaction (`pgx.BeginFunc`, `social_pins.go:118`, `:191`) holds the pin row, the stamp and the locks;
- PostgreSQL rolls back the uncommitted transaction and releases its locks, so neither a pin without a stamp nor a
  stamp without a pin can persist;
- the caller can repeat the request: the upsert keeps a single row, and the stamp follows the state change only.

Proven by: the stamp-count assertions, and the existing delete-race test.

No migration, persisted format, checkpoint or event changes.

## Resilience

- **Bounded in time:** every call keeps its 5 s context deadline (`social_pins.go:81`, `:163`). Lock waits are
  cancelled by that deadline instead of being broken by PostgreSQL's deadlock detector.
- **Errors keep their type:** 404 for a gone or invisible conversation (forced-race test) and 503 when storage is not
  configured (existing `TestPublicPinRoutesReportStorageFailure`). A 40P01 abort can no longer come from these three
  writers.
- **No retry loop:** the brief's less-preferred option was not needed, so nothing repeats work.
- **Code-review finding 2:** Unpin's lock order relies on the `DELETE`'s dependency on `visible`. A forced test guards
  it, and the comment at `social_pins.go:181` states the invariant for future edits.
- **Code-review finding 3:** the pure-pin phase of the stress test does not always reproduce the pin/pin deadlock on
  its own. The interleaved phase does, together with the mutation check and the HTTP test. Recorded, not changed.

## Security

The `security-review` pass found no issues: none met the >80% confidence bar.

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Trust boundaries and identity | No change | Identity is still `auth.UserFromContext` (`validateSocialPin`). |
| Object-level authorization | Yes, unchanged | `chatauthority.Load`, and the `Predicate` inside the effect statement, are untouched. After a lock wait, READ COMMITTED rechecks the row the same way for `FOR SHARE` and `FOR NO KEY UPDATE`. A deleted conversation gives a 404 (forced test). Negative authorization is covered by the existing `TestPublicPinRoutesShareCanonicalRowsAndEnforceChatAuthority`. |
| Input, parsing, amplification | No | No new input or parsing. Ids are still validated by `socialPinID` and `tenantschema.Valid`. |
| Injection and construction | Yes, unchanged | The only SQL change is a constant locking clause. The schema is still `tenantschema.Quote`, and all values are bound parameters. |
| Egress and SSRF | No | No outbound calls. |
| Secrets | No | None added. A secret-pattern scan of the staged diff was empty, and this record holds no credentials. |
| Supply chain | No change | No dependency change. `govulncheck` (local Go 1.26.5) reports 7 reachable standard-library vulnerabilities (GO-2026-5026, -5972, -6088, -6089, -6090, -6091, -6218). The list is **identical on `origin/main`**, so all are pre-existing; they are fixed by a newer Go patch release. |

**Privilege:** the new lock needs `UPDATE` on `chat_conversations`, which the stamp already needs, so the role needs no
new grant.

## Recovery guarantees (touched component × phase)

A conversation pin is not an execution phase (P01–P17 in `../recovery-guarantees.md`), so that matrix has no row to
update. These rows cover the pin API:

| Component × phase | Before | After | Enforced by | Proven by |
|---|---|---|---|---|
| Main × concurrent pin, unpin or delete on one conversation | **F**: an untyped 500 when PostgreSQL aborted the deadlock victim. Pin vs Pin was closed on `main` by #1163; Unpin vs Delete and Unpin vs Pin were still **F** on `b4ffbe33f`. | **I**: every call succeeds, there is one shared row, and one stamp per state change | `social_pins.go:117` (#1163), `:186` (this PR) | `TestConcurrentConversationPinsAndUnpinsNeverDeadlock`, `TestAnUnpinRacingTheConversationDeleteIsNotADeadlock`, folders `-count=50` |
| Main × crash or restart mid-pin | **I** | **I** (unchanged): the transaction rolls back, and repeating the request gives one row | `social_pins.go:118`, `:191` | Transaction atomicity; the stamp-count tests |
| PostgreSQL × restart mid-pin | **I** | **I** (unchanged): the uncommitted transaction is discarded and the client repeats it | the same transaction | Not fault-injected (an existing platform gap, G-PG-01) |
| Main × lock wait past 5 s | **F** | **F** (unchanged): the context deadline ends the call and nothing is committed | `social_pins.go:81`, `:163` | Not separately tested |
| Web/browser × reload after a pin or unpin | **R** | **R**: the list is re-read from the authoritative row | grouped folder list | Browser evidence below |

## Browser evidence

**Stack:** a private local standalone stack, `elitea-pindl` at `http://pindl.localhost:18160`. Real backend, no
response mocks.
- Started with `deploy/scripts/standalone-stack.sh` and overlays that pin image tags and give the stack its own host.
  It uses an explicit `10.231.60.0/24` network, because the Docker address pools were exhausted by other stacks.
- **Main**, two builds of `ghcr.io/elitea-ng/elitea-main:pindl-20261008`:
  - **Run 1 (2026-10-08):** `sha256:378b12a72f0ccff52a8b32aa04432914fe1bbede3f3f4782a459b380eed3bc98`, built from
    this branch before the rebase (`7ac0eaf9` plus both halves of the fix).
  - **Run 2 (2026-10-09):** `sha256:a29a9f05f17401c01b48bdbbac1a8a7d1ac275f96734b694c1852e29fbbfaa0d`, built from the
    branch rebased onto `b4ffbe33f`, which is what this PR merges. The stack was re-run with `up`, so the newer
    migrations were applied and Main was recreated.
- **Web:** the locally built image `ghcr.io/elitea-ng/elitea-web:dtfx-20261008` =
  `sha256:241e61287dc23e38cfa8c0cdd23ada154c64534f349424b017e35f93a12fe2b5`. Web is unchanged by this PR.
- All other services reuse the same locally built `dtfx-20261008` images.
- Signed in as the seeded test identity `e2e-chat@autotest.local` through the stack's OIDC mock, which asks for a
  subject only. Personal project `90107`.

**Fixtures:**
- The conversation was created **through the UI**: chat `1`, "pin deadlock browser check", answered by the offline
  mock model from `seed-llm`.
- `seed` and `seed-llm` are the stack script's own seeders.
- The database was read only to verify results and was never written to.

| Step | Observation |
|---|---|
| Tab B opened on chat 1, then tab A pins from the row menu ("Pin on top") | `POST /api/v2/social/pin/prompt_lib/90107/conversation/1` → 200. One `social_pins` row (user 7). `sync_at` was stamped at the pin time, and `updated_at` is unchanged. |
| Tab B **reload** | The conversation shows in the pinned area with the pin icon, and the date groups are empty. The grouped list (`GET …/folder/prompt_lib/90107?grouped=true`) returns `pinned=[1]`. |
| A burst from the page, using the page's own session | 10 rounds of 12 concurrent calls, mixing pins and unpins over both route families (`social` and `elitea_core`): 72 `POST` 200 and 48 `DELETE` 200, **0 non-200**. Afterwards there is exactly 1 pin row. |
| Tab B unpins from the row menu ("Unpin") | `DELETE /api/v2/social/pin/prompt_lib/90107/conversation/1` → 200. 0 pin rows, and `sync_at` was stamped again. |
| Tab A **reload** | The conversation is back under "Today", and the grouped list returns `pinned=[]`. |
| Logs (run 1) | PostgreSQL logged 0 `deadlock` or 40P01 entries. Main logged 0 HTTP 5xx entries. |
| Run 2: tab A pins, tab B **reloads** | `POST …/social/pin/…/conversation/1` → 200. Tab B shows the conversation pinned, and the grouped list returns `pinned=[1]`. |
| Run 2: burst from tab B | The same 10 × 12 mix: 72 `POST` 200 and 48 `DELETE` 200, **0 non-200**, in 101 ms. |
| Run 2: tab B unpins, tab A **reloads** | `DELETE …/social/pin/…/conversation/1` → 200. Tab A shows the conversation under "Yesterday" (the date had rolled over), and the grouped list returns `pinned=[]`. 0 pin rows; `sync_at` was stamped at the unpin time (`2026-10-09 07:13:36Z`), and `updated_at` is still the original `2026-10-08 19:22:30`. |
| Logs (run 2) | PostgreSQL logged 0 `deadlock` or 40P01 entries. The recreated Main logged 0 HTTP 5xx entries. |

Not done in the browser: an unpin racing a delete. The forced real-PostgreSQL test covers it, because a click
cannot reproduce that timing.

## Open limits and follow-ups

1. `golangci-lint` was not run locally (not installed). It runs in CI.
2. `TestPublicPinRoutesConcurrentUpsertsKeepOneSharedRow` is still a probabilistic test; the forced and stress repo
   tests are the deterministic guard.
3. The pre-existing standard-library `govulncheck` findings need a Go patch bump. That is separate dependency hygiene.
4. `analytics_tools_postgres_integration_test.go` is not `gofmt`-clean on `main`. Pre-existing and outside this
   change.
