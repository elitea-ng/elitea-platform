# Code recovery through same-consumer pending delivery

## Trigger and behavior

A Worker restart leaves its command in the Redis pending entry list.
The cross-consumer idle floor is 60 seconds. Each Main claim or renewal selects at most 30 seconds.
A 60-second Code runtime can expire before this conservative reclaim path admits checkpoint recovery.

The intake now scans entries already owned by its configured consumer.
The scan uses `XREADGROUP` with a validated entry cursor and a bounded count.
It does not claim another consumer's entries, acknowledge a command, or grant execution authority.
Main still authorizes every exact signed redelivery.

## Owners and bounds

- `transport/redis_streams.rs` owns the exact consumer command and bounded response parser.
- `transport/redis_generation.rs` owns connection replacement without implicit replay.
- `execution/redis_delivery.rs` owns the scan cursor, scheduling, capacity, and local duplicate suppression.
- Existing command processors own signed verification, Main claims, checkpoint recovery, and terminal retirement.

The scan starts at process intake and reads at most one page per existing reclaim interval.
A short or empty page starts another scan. A deferred entry remains pending and can appear in that scan.
An active local entry cannot create another local processing owner.
Deleted payloads advance the cursor without creating a command or acknowledgment.
Malformed payloads stop that transport generation through its existing error path.

Own pending reads select only the configured group, consumer, and stream.
They can reset idle time for this consumer's entries. They cannot refresh another consumer's entries.
New intake and due cross-consumer reclaim remain separate scheduled turns.
Stop closes admission and retains the existing bounded task drain.

## Unchanged authority

The cross-consumer minimum idle remains 60 seconds.
Main leases, fence checks, execution identities, job deadlines, and sandbox dispatch rules remain unchanged.
No database schema, grant format, API, or image profile changes.

Main's `internal/application/execution/claims.go` bounds a positive, whole-millisecond TTL to 30,000 ms.
Its `internal/infra/db/repos/claims.go` evaluates lease liveness with PostgreSQL `clock_timestamp()`.
An unexpired claim belonging to a different workload session returns `RETRY_LATER_NOACK`.
Expiry permits the existing exact claim transition; a renewal extends expiry from the current database clock
only while the original execution, generation, command, workload, session, producer, attempt, epoch, and fence match.
The Worker scan does not calculate a takeover time or assume the lease expires 30 seconds after initial admission.
It periodically resubmits the original signed delivery to the existing claim checks.
`RetryLaterNoAck` retains the delivery without acknowledgment, so a later scan can retry after the actual lease expires.

## Evidence boundary

The restart receipt records the original retained JavaScript runtime and the replacement Main claim.
It also records deadline failure before successful platform recovery.
This change addresses same-consumer restart delivery. A changed consumer retains the original reclaim behavior.
A larger backlog still obeys existing capacity and page limits.

Focused controlled-clock tests cover startup, repeated deferral, fair scheduling, duplicate admission, capacity, and Stop.
Transport fixtures cover exact consumer selection, cursor order, missing payloads, and malformed responses.
Native tests do not prove Redis server behavior or deployed restart recovery.
The root operator owns those acceptance checks.

The final focused native run passed 45 tests with no failures or ignored tests:
17 Redis delivery tests, 6 Redis generation tests, and 22 Redis stream/parser tests.
All three commands used `cargo test --locked --offline -j1 --lib` with their owning selectors
and `--test-threads=2`. Strict `cargo clippy --locked --offline -j1 --all-targets --all-features -- -D warnings`
passed. Scoped Rust formatting and whitespace checks passed.
The private receipt packet retains the initial fixture compilation failure and the original Stop fixture
count-barrier failure. The Stop fixture now waits for its exact heartbeat event and retains its original drain assertions.
No Linux, live Redis, Main database, or runtime restart acceptance was run for this patch.

Graph errors currently lose the node class through the generic `agent.legacy` conversion.
That diagnostic correction is separate from this transport change.
