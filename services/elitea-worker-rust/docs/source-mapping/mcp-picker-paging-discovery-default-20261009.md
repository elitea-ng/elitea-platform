# MCP/toolkit picker paging and standalone discovery default

Status: `complete` for both findings of the post-merge browser pass on `main`
0def77b22. Main, Web and the standalone compose file change. No Worker, contract-breaking
or migration change.

## Findings and business behaviour

1. **Picker paging.** In the pipeline/agent editor, Tools → "+ MCP" listed only the
   MCPs on the first page of `GET /api/v2/elitea_core/tools/prompt_lib/{project}?limit=20&offset=0`.
   A new MCP (`verifyauthmcp`) and several older MCPs did not appear until the user
   typed a search.
2. **Discovery default.** `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED` was set in no
   compose file. On the standalone stacks every MCP/toolkit page therefore showed
   "The tool list did not load. Try again." (`toolkit_available_tools` answered 503
   "toolkit discovery unavailable").

Behaviour kept from the current platform (EliteaUI `useDropdownData.jsx`,
`PlusChatSubmenu.jsx`; Pylon `elitea_core/utils/application_tools.py::toolkits_listing`):

- the MCP and Toolkit sections are separate lists, each paged server-side by
  offset, 20 rows per page, loading the next page on scroll near the end, with a
  bottom "Loading…" row;
- the MCP list asks the server with `mcp=true`; search is server-side (`query`)
  over name OR description;
- the typed listing never shows `type='application'` rows (agent-as-tool links).

Not ported:

- `sort_by`/`sort_order`, `toolkit_type`, `author_id`, `ids`, `search_artifact`
  and folder filters. The picker does not use them.
- Pylon's lenient `mcp` parsing (any value other than `true` meant `false`).
  Main refuses anything but `true`/`false` with 400.
- Pylon counting a string `meta.mcp = "true"` as MCP. Main follows the new Web's
  `isMcpToolkit` rule (boolean `true` only), so server and client agree.

## Cause

1. The listing endpoint had only `limit`/`offset`. The picker read one shared
   20-row cursor and split MCP from toolkit on the client. Its auto-page effect
   paged only while a section had **zero** matches. When page 1 held one or two
   MCPs, the dropdown never overflowed, the scroll trigger never fired, and every
   later MCP stayed unreachable. In the real project 31 of 55 rows were
   `application` rows, which Main returned and the Toolkit section did not filter.
   Rows that shared a name had no tie-breaker (`ORDER BY name`), so offset paging
   could skip or repeat them.
2. Discovery is composed only when the flag is `true`. Both workers serve
   `toolkit.available_tools.v1` with no gate of their own, but no compose file set
   the flag.

Found in review and fixed here: the "Create new" return path (`?newToolkitId=`)
matched the new id only against the loaded first page, so a new MCP that sorts
past row 20 was never attached. It now resolves the id directly.

## Change

| Path | Change |
| --- | --- |
| `services/elitea-main/internal/api/v2/toolkits/handler.go` | `InstanceListFilter`, `parseInstanceListFilter` (optional `mcp` = `true`/`false`, `query` ≤ 128 runes, valid UTF-8, no NUL; otherwise 400), `instanceWhere`, `mcpToolkitPredicate`, `likeLiteral`, `pgRepo.ListToolkitInstances` (filtered COUNT and page, `ORDER BY name, id`). With `mcp` present the typed listing also drops `application` rows. Without `mcp` the raw listing is unchanged for its other callers. `ListToolkits` delegates with an empty filter. |
| `services/elitea-main/internal/api/v2/toolkits/*_test.go`, `internal/api/v2/mcp/internal_toolkits_execute_test.go` | Fakes implement `ListToolkitInstances`. New unit and real-PostgreSQL tests. |
| `services/elitea-main/api/openapi/v2.yaml`, `client-contract/v1.lock.json`, `internal/api/generated/api.gen.go` | `mcp` and `query` parameters on `listToolkitInstances` (additive lock update, regenerated server code). |
| `apps/elitea-web/src/shared/api/generated/**` | Orval regeneration for the two parameters only. |
| `apps/elitea-web/src/features/agents/api/useToolkitInstancePager.ts` (new) | One server-filtered infinite query per section (`mcp`, trimmed `query`); `fetchMore` does nothing while a fetch is in flight. |
| `apps/elitea-web/src/features/agents/ui/ToolMenuSections.tsx` | Each section owns its pager. It keeps paging while fewer than a page of items is visible. Client guards drop `application` rows and keep the name-or-description match. |
| `apps/elitea-web/src/features/agents/ui/ToolMenu.tsx` | "Create new" return resolves `newToolkitId` by `GET /tool/prompt_lib/{project}/{id}` and attaches by the row's own `isMcpToolkit`. |
| `apps/elitea-web/src/features/agents/ui/ToolMenuDropdown.tsx` | The Loading row sits below the listed rows while the next page loads. |
| `deploy/docker-compose.standalone-full.yml` | `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED: "${ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED:-true}"`. |
| `services/elitea-main/internal/runtimecomposition/toolkit_discovery_compose_test.go` (new) | Reads the compose files and feeds the elitea-main env to the real config parser. |
| `apps/elitea-web/src/entities/toolkit/api/toolkitToolsApi.ts`, `shared/ui/ToolListError/ToolListError.tsx`, `shared/i18n/en.json` and the ToolkitForm, TestToolSettings, LoopToolSelect, BaseToolNode and deprecated ToolNode consumers | Exact 503 `{"error":"toolkit discovery unavailable"}` → `isDiscoveryDisabled` → inline info message without Retry. |
| `apps/elitea-web/e2e/streaming/chat.mcp.spec.ts` | Comment and assertion text only. |

## Discovery decision

Discovery is **enabled in the standalone compose stacks**, and Web also gains a
**readable message** for deployments that keep it off.

- Standalone-full already runs elitea-main with the runtime, a worker dispatch
  plane and the object store, so discovery was the one missing flag. Both
  workers serve `toolkit.available_tools.v1`, and `toolkit_route.go` picks the
  stream for each: index-ingest for Python, agent for Rust. The rust-agent
  overlay inherits the value. An operator can still set it to `false`.
- Helm keeps it opt-in (`main.runtime.toolkitDiscovery.enabled=false`; chart
  untouched). Such installs used to show a Retry that could never succeed. They
  now get "Tool discovery is turned off on this deployment. Ask an administrator
  to enable it." The match is on status 503 **and** the exact body, so a
  transient 503 on the same route keeps the generic message and Retry.
- `docker-compose.e2e-standalone.yml` (the PR e2e job) does not run the runtime
  and is unchanged. The only discovery-gated journey
  (`e2e/journeys/toolkits/toolkits.lifecycle.spec.ts:476`) runs there and keeps
  skipping. Workflows that use standalone-full (nightly real-LLM, deepwiki real
  engine, the manual live-toolkit lane and other standalone jobs) now have
  discovery on. No workflow file changed.

## Tests

Go, from `services/elitea-main`. `ELITEA_TEST_DATABASE_URL` pointed at a
throwaway `pgvector/pgvector:pg16` container, started for the run and removed after.

- `go test -race -count=1 -p 1` over `internal/api/v2/toolkits/...`,
  `internal/api/v2/drafts/...`, `internal/api/v2/mcp/...`,
  `internal/infra/storage/...`, `internal/runtimecomposition/...` and
  `internal/api/clientcontract/...`: 10 packages ok, 0 fail.
- Targeted run (`TestToolkitInstanceListFiltersAgainstPostgres`,
  `TestStandaloneCompose*`, `TestListToolkits*`): 59 PASS lines including
  subtests, 0 FAIL, 0 SKIP.
  - `instances_test.go:103` `TestListToolkitsPassesTheMCPAndQueryFilterThrough` (6 cases).
  - `instances_test.go:138` `TestListToolkitsRefusesABadFilterBeforeTheRepository` (7 cases; repository never called).
  - `instances_test.go:170` `TestListToolkitsAcceptsAQueryAtExactlyTheLimit` (128 runes ASCII and multi-byte; 129 refused).
  - `repository_integration_test.go:364` `TestToolkitInstanceListFiltersAgainstPostgres` (25 subtests). Fixture: 25 non-MCP rows sorting first; MCPs of all three shapes; duplicate names; near-misses (`mcpxctx`, string `"true"`, `false`, NULL meta); 4 `application` rows; names with `%`, `_` and `\`. For every filter at page sizes 7 and 20 it asserts each row appears once, no extras appear, and `total` equals the filtered count. It also asserts name-then-id order and that the raw listing still returns `application` rows.
  - `toolkit_discovery_compose_test.go:59` `TestStandaloneComposeEnablesToolkitDiscoveryByDefault` and `:82` `TestStandaloneComposeToolkitDiscoveryComposesForBothWorkers` (Python and Rust subtests through `ConfigFromEnv` and `storage.ConfigFromEnv`).
- Broader `./internal/api/...` without a database (agent run): 63 packages ok, 0 fail. The DB-backed suites skip there, as declared.
- Mutation checks: removing the NULL-safe `COALESCE` failed 4 integration subtests. Removing the compose line or flipping its default failed both compose tests.

Web, from `apps/elitea-web`:

- `npx vitest run --config vitest.config.ts --project node src/features/agents src/entities/toolkit src/shared/ui/ToolListError src/features/toolkits/ui src/features/pipelines/ui/select src/features/pipelines/ui/nodes`: 241 files, 2473 tests, all pass.
- `npm run typecheck` and `npm run lint`: clean. `check-budgets`, `check-dead-code`, `check-layer-cycle`, `check-generated-client`, `check-endpoint-manifest`, `check-contract-coverage`: pass. `i18n-backfill`: no missing keys.
- New or changed cases in `src/features/agents/ui/ToolMenu.test.tsx`:
  - `:741` the sections ask for `mcp=false` / `mcp=true`;
  - `:758` all 25 MCPs behind 30 toolkits are reached by scroll, each once;
  - `:785` the regression: paging continues without scroll while the list is shorter than a page;
  - `:810` debounced server-side `query`;
  - `:838` no `application` row;
  - `:856` Loading row below the rows;
  - `:399`, `:421`, `:448`, `:467` the return path: attached, 404, non-numeric, and an MCP not on page one attached as MCP.
- Discovery message tests:
  - `toolkitToolsApi.test.tsx:29-65`: exact 503 body, other body, 500, non-JSON, success;
  - `ToolListError.test.tsx:43`;
  - `ToolkitForm.test.tsx:379`;
  - `TestToolSettings.test.tsx:155`;
  - `LoopToolSelect.test.tsx:82`.
- Mutation checks: reverting the auto-page rule fails `:785`; dropping `mcp` from the request fails 3 tests; two matcher mutations fail the hook and TestToolSettings tests.

## Performance

- Mechanism:
  - each request is still two statements, COUNT plus one page, with the same WHERE and bound arguments (`handler.go:1750`, `:1763`);
  - named bounds: limit 20/100 and `query` ≤ 128 runes (`handler.go:1131-1134`);
  - Web fetches one 20-row page per section (`useToolkitInstancePager.ts:16`, `:51`) and never runs two fetches at once for one section (`:82`).
- Before vs after: the picker used to page the whole listing on the client to find one type. It now reads only rows of its own type. On mount there are two first-page requests (one per section) instead of one shared one.
- Proving test: `ToolMenu.test.tsx:758` asserts offsets 0 and 20 only, each once, for 25 MCPs. `:650` asserts offset paging with a constant limit.
- Measured on the browser stack: opening the editor sent exactly `offset=0&mcp=false` and `offset=0&mcp=true`; scrolling the MCP list sent one `offset=20&mcp=true`.

## Durability

- Mechanism: read-only listing. The page order is total (`ORDER BY name, id`,
  `handler.go:1763`), so a retried or resumed offset page repeats nothing and
  skips nothing. The return-path attach is the existing association insert with
  `ON CONFLICT (entity_version_id, tool_id, entity_type) DO NOTHING`
  (`handler.go:1496`), so re-sending it is harmless.
- Proving test: `repository_integration_test.go:364` (each row once across pages, duplicate names included).
- Measured: after reload, both attachments came back from the server, and the DB rows match (see browser evidence).

## Resilience

- Mechanism:
  - strict parameters: `mcp` accepts only `true` or `false`, and `query` is bounded, valid UTF-8 and NUL-free; anything else is a readable 400 before any DB read (`handler.go:1152-1173`);
  - a short or empty page stops paging even if `total` disagrees (`useToolkitInstancePager.ts`, `getNextPageParam`);
  - the return path does not attach on a 404, fetch error or non-numeric id, clears the URL, and is not retried in a loop (`ToolMenu.tsx:273-280`);
  - discovery-off is distinguished from a transient 503 by status plus exact body (`toolkitToolsApi.ts:84-90`).
- Proving test: `instances_test.go:138`, `:170`; `ToolMenu.test.tsx:421`, `:448`; `toolkitToolsApi.test.tsx:38-56`.
- Measured: `?mcp=yes` on the stack → 400 `{"error":"mcp must be true or false"}`.

## Security

`rules/security.md` categories:

- **Trust boundaries and identity:** not applicable. No new identity source; the route keeps the session/token middleware.
- **Authorization:** applies. No new route; the filters run behind the existing `toolkitGate("models.applications.tools.list")` (`router.go:3121-3122`). The tenant comes only from `{projectID}` through `tenantschema.Quote` (digits only, quoted identifier). The filters only narrow rows the caller could already list. Existing proof: `router_elitea_core_project_scope_test.go:175`. No new authz surface needs a new matrix.
- **Input, parsing and amplification:** applies. Input is bounded before use (`handler.go:1152-1173`). There is no structured parser and nothing expands.
- **Injection and construction:** applies.
  - SQL: values are bound parameters only. WHERE fragments are constants; only placeholder numbers are concatenated.
  - LIKE: wildcards escaped by `likeLiteral` (`handler.go:1711`) with `ESCAPE '\'`.
  - Proof: `repository_integration_test.go:364` (`%`, `_`, `\` match literally).
  - Web renders text through React; no HTML sinks were added.
- **Egress and SSRF:** not applicable to the code change. The compose default turns on an existing, already-gated route that dispatches to the worker; no new outbound path.
- **Secrets:** applies. `settings` stays redacted on every listing path (`handler.go:1788`; existing `per_type_redaction_test.go:61` passes). `query` matches only name and description, never settings. No secrets in the diff (scanned before commit).
- **Supply chain:** no dependency change. Scanner results are the same as `main` 0def77b22:
  - `govulncheck`: the same 19 Go standard-library advisories on both;
  - `npm audit`: 7 pre-existing (2 low, 1 moderate, 4 high);
  - `cargo deny`: not applicable (no Worker change).
- `security-review` pass on the branch diff: no finding at or above the reporting threshold. `code-review` (high): 5 findings. Three were fixed (the return path and two comments). Two were kept and documented:
  - the new 400 body is not in the operation's declared 400 schema, because the additive-only client contract refused a `oneOf`; it is documented in the operation description;
  - COUNT and the page are two statements, as before; Web stops on a short page.

## Recovery guarantees

| Component × phase | Class | Note |
| --- | --- | --- |
| Main × toolkit listing (read, admission of the editor) | I | Read-only and stateless. A Main restart mid-request fails that request; the client re-requests the same offset, and the total order makes the retry return the same rows. Code `handler.go:1740-1790`; test `repository_integration_test.go:364`. |
| Web/browser × picker paging | I | Pages are server reads keyed by `(mcp, query, offset)`; a reload rebuilds the list from page 0. Code `useToolkitInstancePager.ts:48-84`; test `ToolMenu.test.tsx:758`. |
| Web/browser × "Create new" return attach | I | The id is resolved by GET and attached with the idempotent association insert (`handler.go:1496`). A reload before the URL is cleared repeats a harmless attach. Code `ToolMenu.tsx:264-293`; test `ToolMenu.test.tsx:467`. |
| Main × tool discovery (standalone) | unchanged | The existing discovery runtime (`toolkit-discovery-main.md`) is now composed by default on standalone. Its guarantees are that mapping's, not changed here. |

## Real-browser evidence

- Stack: my own standalone stack `elitea-mcppicker` (front door
  `http://localhost:18220/app/`), built from this branch. It uses
  `deploy/docker-compose.standalone-full.yml` and
  `deploy/docker-compose.standalone-rust-agent.yml` with the real-model dump from
  `main` 0def77b22 restored. The shared verify stack was not touched.
- Images:
  - elitea-main `ghcr.io/elitea-ng/elitea-main:mcppicker-verify`, image `sha256:5fee46b1da4f…`;
  - elitea-web `ghcr.io/elitea-ng/elitea-web:mcppicker-verify`, image `sha256:e5e58c726cc1…`;
  - worker `elitea-worker-rust:main-0def77b22-verify` (unchanged).
- Image identity check: the binary extracted with `docker create` + `docker cp`
  contains the branch-only string `mcp must be true or false` once; the
  `main-0def77b22-verify` binary contains it zero times. The web bundle contains
  the new discovery message.
- The elitea-main container env showed `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED=true`
  from the compose default alone (no override sets it).
- Signed in through the stack's OIDC mock as its test admin. No response mocks.
  Project 2, which holds 24 MCPs once the fixtures below are added.
- Steps:
  1. Created pipeline 160 "MCP picker proof 20261009" in the UI. Tools → "+ MCP"
     sent `offset=0&mcp=true` (and `offset=0&mcp=false` for Toolkit), and listed MCPs only.
  2. Mouse-wheel scroll to the end of the MCP list → `offset=20&mcp=true`. Page-2
     MCPs (`rust_oauth_emulator_*`, `rust-compaction-records`, `zero argument gate5`)
     appeared with no typing: 23 MCPs, once each.
  3. Clicked "zero argument gate5" → attached. Reload → still attached; DB
     `entity_tool_mapping` row 117 (tool 95 → entity 160, version 186).
  4. "+ Toolkit" listed 17 toolkits and none of the 29 `application` rows.
  5. Typed `rehearsal` → `offset=0&mcp=true&query=rehearsal` → one MCP matched by
     its description only.
  6. "Create new" from the MCP picker → created Remote MCP `verifyauthmcp`
     (id 116) in the UI. Its page loaded tools `echo` and `reverse`
     (`toolkit_available_tools/.../116` → 200): discovery works on standalone.
  7. The creation page does not yet return to `return_url` (pre-existing; see
     follow-ups). The inbound half was exercised by opening
     `/pipelines/latest/160?newToolkitId=116&mcp="true"`: `GET tool/prompt_lib/2/116`
     → 200, `PATCH` → 201, URL params cleared. id 116 sorts on page 2 of the MCP list.
     Reload → both MCPs attached; DB row 118 (tool 116 → entity 160, version 186).
  8. Recreated elitea-main with `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED=false`
     and opened MCP 116 → 503. The form showed the inline info "Tool discovery is
     turned off on this deployment. Ask an administrator to enable it.", with no
     Retry button (`toolkit-form-tool-list-error`). Then restored the default.
- Note: the browser profile is shared with other local stacks on `localhost`
  (cookies ignore the port), so the session was reset twice by another stack's
  login (`session_unknown` in Main's log). I signed in again. This is not related
  to this change.

## Fixtures

- Pipeline 160 and MCP 116 were created through the UI.
- 15 MCP rows ("pager mcp 01".."15", description "paging fixture 20261009") were
  created through the product API (`POST /api/v2/elitea_core/tools/prompt_lib/2`,
  same session), to push MCPs past one page.
- No direct database writes. All of them live in my own stack's restored database,
  which is torn down after this PR.

## Follow-ups

- The toolkit/MCP creation page should honour `return_url` and append
  `?newToolkitId=` so the round trip completes on its own. The picker's inbound
  half is ready and proven.
- The Toolkits and MCPs list pages (`features/toolkits/lib/hooks/useLoadToolkits.ts`)
  still read the raw listing, with no server type filter and with `application`
  rows. They can adopt `mcp=` now.
- On an unsaved Remote MCP form, "Tools 0/N" lists the project's MCP instance
  names. This is the type-discovery fallback noted in `toolkit-instance-picker.md`;
  it is pre-existing.
- Confirm on the next rehearsal stack built from `main` with this change.
