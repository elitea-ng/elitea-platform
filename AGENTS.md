# Elitea Platform

## Scope and maturity

This repository is the greenfield replatform prototype for Elitea. The target is
one Go modular monolith for the current `pylon_main` and `pylon_auth` product
domains, plus independently scalable Python workers for LangChain/LangGraph
execution and indexing. The candidate architecture and approval state are
indexed in `../elitea-docs/docs/internal/03-architecture/adrs/index.mdx`;
implementation gates are in
`../elitea-docs/docs/internal/03-architecture/cloud-native-migration/implementation-readiness.mdx`.
Identity/tenant, trace projection and cutover behavior are specified in the
three `spec-*.mdx` pages beside that readiness register.
Only `Approved` ADRs are final. `In Review` records constrain prototype work but
still require owner approval; code here is not evidence that a decision is final.

Treat the repository as pre-production until the documented compatibility,
security, tenant-isolation, migration, and failure-mode gates pass. Do not copy
prototype shortcuts into new code merely to preserve current behavior.

## Repository map

| Path | Ownership |
| --- | --- |
| `services/elitea-main/` | Go API, auth, control plane, persistence, and browser streaming |
| `apps/elitea-ui/` | EliteaUI Git submodule; make UI changes in its owning repository |
| `libs/go/` | Shared Go libraries; keep dependencies narrow and consumer-driven |
| `libs/proto/` | Versioned cross-language contracts and generated-code inputs |
| `deploy/` | Local compose and Kubernetes packaging |
| `docs/` | Prototype-local implementation notes; architecture decisions belong in `elitea-docs` |

## Architecture boundaries

- Preserve public HTTP paths, legitimate-user outcomes, SDK behavior, and
  required realtime behavior during the drop-in migration. Documented security
  corrections intentionally break header trust, implicit admin, unscoped or
  unsafe token/session behavior, open redirects, raw errors and cross-tenant
  access; compatibility never requires preserving a vulnerability.
- Do not preserve Pylon plugin loading, Arbiter pickle payloads, global service
  locators, internal Python RPC aliases, or hot-installed dependencies as target
  architecture.
- Use Redis Streams for durable asynchronous worker commands and events. Use an
  authenticated mTLS HTTP/JSON API for identity, credential redemption,
  provider catalogs, artifact grants, health, and capability discovery. Do not
  introduce gRPC until profiling proves it solves a measured control-plane
  bottleneck.
- Durable messages contain identifiers, immutable revisions, encrypted
  references and non-sensitive bounded metadata. Plaintext prompts, repository
  content, tool payloads, secret values, bearer credentials, arbitrary
  serialized objects and raw internal errors do not enter shared Redis streams.
- Sign the exact serialized envelope body bytes and verify before decode; never
  call protobuf canonical. At-least-once exact redelivery is valid and must
  reach durable inbox/claim idempotency.
- PostgreSQL is the source of truth for execution state. Redis is transport, not
  the workflow database. Consumers must be idempotent and generation-fenced.
- Go owns product data and migrations. Python owns LangGraph checkpoints and
  execution-local state only; vector stores and LiteLLM keep explicit separate
  ownership.
- `provider_worker` is split: Go owns provider descriptors, authorization,
  policy, routing, health, and grants; Python owns the LangChain adapter and
  provider invocation lifecycle.
- Execution grants activate only after claim and bind to command, generation,
  claim attempt, lease epoch and workload identity. Provider egress is enforced
  outside the worker process; a Go package interface alone is not a security
  isolation boundary.

## Working agreements

- Before editing, run `git rev-parse --show-toplevel` and `git status --short`.
  Preserve unrelated changes and submodule state.
- Prefer explicit constructor dependencies and domain interfaces. Do not add a
  new service locator, shared mutable registry, or package named after a retired
  framework abstraction.
- Add or change a cross-language contract in `libs/proto/` before depending on
  an unversioned map or event name. Reserve removed protobuf field numbers and
  run compatibility checks once Buf configuration is present.
- Keep API errors typed and safe for callers. Log internal causes with trace and
  execution identifiers; never return raw `err.Error()` across a trust boundary.
- Enforce authentication, project authorization, and tenant context before
  mounting product, admin, storage, shadow, or cutover handlers.
- Use transaction-local tenant selection and repository-owned versioned
  migrations. Never return a pooled connection after releasing it or use
  session-scoped tenant state across requests.
- Do not add credentials, private URLs, local `.env` values, or development
  defaults that act as secrets. TLS verification is mandatory outside an
  explicitly isolated test.
- Do not edit generated files, caches, the UI submodule, or tracked binaries.
  Build outputs such as `services/elitea-main/elitea-main` and `cutover-ctl`
  should not be updated or newly committed.

## Language (ASD-STE100)

Write all agent-authored text in ASD-STE100 Simplified Technical English. This
rule applies to GitHub issues, pull-request bodies, review comments, commit
messages, and documentation changes in this repository.

- Write short sentences: maximum 20 words for an instruction, maximum 25 words
  for a description.
- Give one instruction per sentence. Use the active voice and the present tense.
- Start each instruction with the verb.
- Use one approved term for one thing. Do not switch between synonyms for the
  same object.
- Keep paragraphs short: maximum 6 sentences.

Code identifiers, file paths, command lines, log excerpts, and quoted output
are exempt from these rules.

## Verification

Start with targeted checks, then expand in proportion to the change:

```bash
task test
task vet
task lint
task build
```

Read the exit status of `task test` directly. Do not pipe it through `grep`,
`tail` or `head` and then read `$?`. A pipeline reports the status of its LAST
element, and that has produced several false green reports here.

`task test` runs `scripts/go/workspace-run.sh test`. The script runs every
workspace module and prints a summary. Read the summary:

- A `FAIL` line names the module that failed. The run continues past it, so a
  later `PASS` line does not cancel it.
- A `NOT COVERED` line names a Go module on disk that this command did not
  open. `services/elitea-llm-gateway` is always there. It sits outside
  `go.work`, it builds with `GOWORK=off`, and `ci-gateway.yml` runs it. A green
  `task test` is not evidence about that module.
- A `workspace-run skip ledger` line gives the number of tests that skipped
  themselves, and names each one with its reason. `go test` prints `ok` for a
  package whose tests all called `t.Skip`, so a suite that ran and a suite
  that did nothing look the same without this list. Eighteen suites did
  exactly that (#423). A skipped test gave you no result. Read the list before
  you call a run green.
  - `DECLARED` means `scripts/go/declared-skips.txt` states why the test
    cannot run in this environment.
  - `UNDECLARED` means nothing states why. Give the test what it needs, or add
    the reason to that file. Set `ELITEA_REQUIRE_DECLARED_SKIPS=1` to make an
    undeclared skip fail the run, as CI does.

For Go-only work, run from `services/elitea-main/`:

```bash
go test ./...
go vet ./...
```

For deployment changes, also run `task helm:lint` and render the affected chart.
For protocol or compatibility work, add fixtures covering exact statuses,
headers, nested/null JSON, auth and tenant isolation, database effects,
idempotency, retries, ordering, reconnect, HITL, and rollback. A skipped
credential-dependent contract suite is not a passing compatibility gate.

## Delivery

Use Conventional Commits. Keep architecture-document changes in `elitea-docs`
and implementation changes here linked by the relevant ADR/specification. Pull
requests must state the compatibility surface changed, security assumptions,
failure modes exercised, and checks that were run or intentionally skipped.
