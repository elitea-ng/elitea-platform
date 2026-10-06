# Confluence indexing parity harness

This directory is test-only. It freezes the current
`elitea-sdk==0.8.30` Confluence indexing behavior. Its baseline lock is
independent of the target worker lock so a target SDK upgrade cannot silently
rewrite the current-product evidence. It is current-baseline characterization,
not standalone-worker parity closure.

The harness crosses the following real boundaries:

- fake Confluence and LiteLLM services run as independent HTTP servers;
- all model calls use the pinned SDK's real `EliteAClient`, including its
  `/llm/v1` paths, bearer/project headers, tokenization, and retry policy;
- the current SDK source is imported from the exact worker lock revision;
- the database profile writes through `langchain-postgres` to a real
  PostgreSQL/PgVector server and inspects the resulting rows directly;
- real Poppler and Tesseract executables process deterministic PDF, populated
  OCR, and empty-OCR fixtures in the mandatory CI profile;
- the Go control-plane test serializes the production command-bus envelope
  and asserts that image, base64, prompt, and attachment bytes are absent.

It does not claim that the standalone worker already has end-to-end Confluence
parity. The worker currently exposes the real SDK operation only behind its
claimed execution, protected input-content, workload-identity, and output
protocol. There is no production composition seam for injecting this harness's
Confluence/LiteLLM endpoints or initialized `EliteAClient`. The target half is
therefore an explicit skip until that prerequisite exists.

## Pinned current baseline

The harness accepts only the artifact recorded in
`current_pylon_sdk_baseline.json`:

```text
version: 0.8.30
revision: 48c51a16634a9924f6c5d5313c3bacedb0b5b56b
```

Create a detached baseline SDK worktree without changing the SDK checkout:

```bash
git -C ../elitea-sdk worktree add --detach \
  /tmp/elitea-sdk-0.8.30 \
  48c51a16634a9924f6c5d5313c3bacedb0b5b56b
```

## Fast HTTP and source-parity profile

Run from `services/elitea-main` with an environment containing the current SDK
indexing extras:

```bash
ELITEA_CURRENT_SDK_ROOT=/tmp/elitea-sdk-0.8.30 \
DYLD_LIBRARY_PATH=/opt/homebrew/opt/cairo/lib:$DYLD_LIBRARY_PATH \
python -m pytest tests/parity/confluence_indexing \
  -m "not pgvector"
```

The source HTTP fault matrix covers `401`, `403`, `429`, and `5xx`. LiteLLM
authentication and `403`, `429`, `500`, and `503` failures are exercised
through the real SDK clients for both chat and embeddings. Current attachment
download behavior is deliberately asserted as observed: a non-200 download is
turned into an indexable dependent error document rather than a skipped item.
That is parity evidence, not an endorsement of the behavior.

The retry fixture returns `503` once and then `200`; the pinned SDK must make
exactly two page requests and preserve the successful page contract. A separate
PgVector test freezes the exhausted-source-failure contract: the only persisted
row is `index_meta`, its history is `created -> failed`, counts remain zero, and
the emitted status events are `in_progress -> failed`.

## Disposable PostgreSQL/PgVector profile

`run_pgvector.sh` starts an isolated `pgvector/pgvector:0.8.1-pg18` container when
`ELITEA_CONFLUENCE_PARITY_PGVECTOR_URL` is not already supplied. It removes only
the uniquely named container it creates.

```bash
ELITEA_CURRENT_SDK_ROOT=/tmp/elitea-sdk-0.8.30 \
DYLD_LIBRARY_PATH=/opt/homebrew/opt/cairo/lib:$DYLD_LIBRARY_PATH \
./tests/parity/confluence_indexing/run_pgvector.sh
```

The golden successful outcome is:

- one parent Confluence page;
- two dependent documents (`notes.txt` and `diagram.png`);
- `parent_id=page-1` on both dependent rows and
  `dependent_docs=att-text;att-image` on the parent;
- three persisted non-metadata chunks;
- one SDK-visible indexed document, because current `docs_count` counts base
  documents rather than dependent documents;
- two vision calls for the single image, with the two distinct current prompts;
- embedding requests for both metadata and content rows;
- terminal `completed` index metadata with exact counts and history;
- the exact ordered 15-event contract: 13 `thinking_step` events plus
  `in_progress` and `completed` status events, including every current
  `message`, `tool_name`, `toolkit`, count, and status field (only generated
  UUID/timestamps are normalized);
- the observed Confluence stats shape (`items_processed=1`,
  `total_fetched=0`, `total_skipped=0`);
- no source, image, base64, prompt, credential, or result bytes in the command-bus
  control envelope.

The production command-bus envelope serializer test hashes a deterministic 62 MiB
data fixture:
ten 3 MiB image payloads plus one 32 MiB payload. Only the immutable bundle
reference, byte length, and digest may enter the command; every payload canary
and representative base64/prompt/result fragment must be absent.

This proves only that the production command serializer is reference-only. It
does **not** yet prove the complete admission -> durable bundle persistence ->
JetStream publish -> standalone-worker consumption -> output-streaming path under
the 62 MiB workload, and it does not close issue #5681 by itself.

## Required full profile

This profile is required before claiming Confluence parity, but it is not
currently mounted as a mandatory repository CI job. A valid execution:

- checks out revision
  `48c51a16634a9924f6c5d5313c3bacedb0b5b56b` and validates its Git archive
  digest against `current_pylon_sdk_baseline.json`;
- runs against `pgvector/pgvector:0.8.1-pg18`;
- installs and verifies Poppler and Tesseract;
- runs the complete HTTP, model-client, binary-media, ordered-event, and real
  PgVector characterization suite.

`pytesseract==0.3.13` is supplied through the standalone worker's
`indexing-current` profile. A future mandatory job must have no ad hoc OCR
install: import and real Tesseract tests must fail when the declared production
dependency profile drifts from the current Confluence loader requirements.

## Baseline transition evidence

The complete HTTP and PgVector golden matrix was rerun when the worker lock
moved from the provisional SDK 0.8.26 target to the current Pylon Indexer
0.8.30 baseline. No covered observable contract drifted. In particular, 0.8.30
made two vision requests for the one image because parent augmentation and
dependent attachment extraction use different prompts.

The worker lock has since moved to 0.8.53. Import/profile verification and
focused indexing contracts are not a substitute for a separate target run
against these immutable current goldens. Until that transition run passes,
0.8.30 remains the current behavioral authority and 0.8.53 remains the
admitted target artifact, not proven full Confluence parity.

## Explicit remaining prerequisites

Two tests are intentionally skipped rather than presenting mocked success:

- standalone-worker parity needs production composition that can claim a real
  execution, resolve protected input content/workload identity, initialize the
  SDK client, and bind the fixture Confluence/LiteLLM endpoints;
- Stop parity needs the real orchestration boundary because current
  `index_data` is synchronous and exposes no cancellation input. Today Stop is
  Pylon task cancellation plus index-meta reconciliation, not an SDK operation.

The production 62 MiB cross-process path is also an explicit residual. The
current test cannot honestly replace durable admission, real JetStream delivery,
worker claim/consumption, and streamed output with an in-process fake.

## Evidence boundary

The fake services, fixed fixtures, request recorder, and database assertions are
deterministic. They do not use production credentials or external networks.
The PgVector test skips with the exact missing environment prerequisite rather
than replacing the database with an in-memory fake.
