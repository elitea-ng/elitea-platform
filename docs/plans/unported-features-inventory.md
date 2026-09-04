# Inventory: what is not ported, not wired, or only reachable by hand

**Status:** proposed · **Created:** 2026-08-30 · **Scope:** elitea-main, elitea-llm-gateway,
elitea-worker-*, admin SPA, elitea-web

**Baseline:** measured against upstream `EliteaUI` @ `20b23c42` (2026-08-28),
read from the standalone clone. The submodule `apps/elitea-ui` is **still pinned
at `a55f36cf` (2026-07-08), 774 commits behind**, and moving it is a scoped task
of its own — see Wave 0.1. The staleness is not cosmetic: a first pass of this
document taken against the old pin concluded "route parity is done". It is not.
Re-run the comparison whenever the pin moves.

## What this document is

An inventory of everything the platform still cannot do through its own product
surfaces, plus a port plan. Three questions separate the buckets, and mixing them
is what has made previous "what's left" lists unusable:

1. **Does the capability exist in code?** If not → *unported*.
2. **If it exists, is it switched on in a default install?** If not → *built, off*.
3. **If it is on, can a user reach it without SQL?** If not → *no surface*.

A fourth bucket, **deliberate non-goals**, is listed last so nobody schedules
work against a promise the target architecture retired on purpose.

Evidence for every row is a file path in this repository. Rows marked
*unverified* are inferred from a doc comment rather than from a run.

---

## A. Bootstrap and operations — done by SQL today

These are the steps `deploy/scripts/standalone-stack.sh` and
`apps/elitea-web/scripts/e2e-stack.sh` perform with `psql`, that no product
surface performs. A greenfield install is not usable until someone runs them by
hand. This is the highest-value bucket: each row blocks a real deployment.

| # | Gap | Evidence | Why it matters |
|---|---|---|---|
| A1 | ~~No writer anywhere~~ **WRONG.** A writer exists and is default-enabled: `deploy/helm/elitea/templates/worker/runtime-session-job.yaml` (pre-install/PreSync hook, added by #598) derives the SPIFFE id from the cert, upserts, and re-verifies with the same three-column conjunction the verifier uses. **My grep filtered to `.go/.sql/.rs` and could not see shell inside a YAML template.** The real gaps are narrower: the session TTL is 90 days with **no renewer** (only another `helm upgrade`), there is no product write path (no revocation, rotation or second worker without SQL), and the externally-hosted-worker topology `guards.yaml:22-28` blesses has no writer at all | `templates/worker/runtime-session-job.yaml:195`, `values.yaml:1966,1986` | A cluster not upgraded for 90 days goes dark with **the exact signature of never having been provisioned** |
| ~~A2~~ | **DONE (226b6e1e).** Conclusion right, cause wrong: OIDC *does* consume `initial_global_admins` — but only on the `browserauth` plane, which `production_router.go:103-105` does **not mount** when OIDC is configured ("OIDC wins the browser prefix"). The plane that IS mounted, `internal/api/v2/auth/`, provisions through `resolveProvisionedUser` and assigns no role at all. Note the ref-shape mismatch: v2/auth writes `oidc:`/`saml:`-prefixed refs, the Form plane matches bare ones | `production_router.go:103-105`, `v2/auth/oidc.go:499-522,593-596` | Every runtime-enabled OIDC deployment lands here, since `main.go:1003` refuses the runtime without production auth |
| ~~A3~~ | **DONE.** `seed-llm` now writes through `POST/PUT /api/v2/configurations`, with two named SQL exceptions and a DEBUG-trap assertion that refuses any other `psql`. LLM credential + model catalogue seeding. The standalone script writes 8 `p_N.configuration` rows directly. | `standalone-stack.sh:457-601, 738-803` | The admin surfaces exist, but the write path they need is behind `ELITEA_CONFIGURATIONS_MUTATION_ENABLED` (see B1). Without it: *"the requested model is not in the project's catalog"* |
| ~~A11~~ | **DONE.** The admin Audit Trail was a complete surface over a table nothing writes. `centry.audit_events` has readers — three Go files (`v2/eliteacore/audit.go`, `audit_query.go`, `project_activity.go`, plus `v2/analytics/costs.go`) and three SPA pages (`AuditTrail.tsx`, `ProjectUserActivity.tsx`, `ScheduleHistoryDrawer.tsx`) — and its only writers are the E2E seeder and integration tests. The generated spec says it outright: "READ-ONLY from this service. Never emitted today" | `api.gen.go:1073`, `001_initial.sql:149-182` | Same shape as A1: four working queries and a full UI over an empty table. On a Go-only deployment the audit trail and heatmap are **permanently empty**, and nothing reports it |
| ~~A7~~ | **DONE (62182c61) — implemented, not refused.** Project icon selection was a silent no-op end to end. `CreateProjectIcon` parses the multipart form, **discards it**, and returns a fabricated URL; `DeleteProjectIcon` returns 204 and does nothing; `UpdateProjectInfo` reads only `body["name"]` while the web client PUTs `{icon_meta}` and gets `{"ok":true}`. No writer for the `project_icon` configuration row exists in the Go stack | `v2/eliteacore/handler.go:3575-3596, 238-249` | A third silent-wrong-data bug, on the WRITE side. Turning `ELITEA_PROJECT_INFO_ENABLED` on does not fix it — `icon_meta` stays null forever |
| A8 | **DONE for the OIDC/SAML plane (226b6e1e); the Form plane still needs the seed script.** A new SSO user could not complete a chat turn until they personally created a PAT. `LocalIssuer` "never creates, rotates, or stores a PAT" — it re-signs an existing active one. The only writer is the user-driven `POST /api/v2/auth/token/`. With no row the worker's actor-token issuance fails with a message naming a *database stage* | `authsvc/pat_issuer.go:21-22,43-50`, `v2/auth/tokens.go:174` | Needs no psql, so it is not bucket-A — but it is a first-run cliff with a misleading diagnostic, and it blocks the Wave 1.4 gate |
| ~~A9~~ | **DONE (edc51f02).** The runtime Redis server certificate had the wrong SAN for Kubernetes. `gen-runtime-certs.sh:135` issues `redis-server` with `DNS:runtime-redis` (the *compose* service name), while the chart's Service is `elitea-runtime-redis` and `values-standalone.yaml` dials that name. `values.yaml:2143-2145` even asserts the Service name is "the only SAN on redis-server.crt" — it is not | `deploy/scripts/gen-runtime-certs.sh:135` | **Following values-standalone.yaml's own instructions produces a TLS handshake failure.** One-line fix plus re-minting |
| ~~A10~~ | **DONE (edc51f02).** `values.yaml` referenced a mint script that never existed. | `deploy/helm/elitea/values.yaml:2131` | Operators are pointed at a script that was never written |
| A4 | **PARTLY DONE (edc51f02): an opt-in objectInit hook exists, default off.** Artifact bucket must pre-exist. elitea-main probes the object store at boot and refuses to start. | `deploy/README.md` "What a Kubernetes install does NOT give you" | The migration Job fails and the release aborts, before anything can create it |
| A5 | **`CREATE DATABASE` (agent-state) and `CREATE EXTENSION vector`.** Already solved on BOTH stacks — `templates/dbInit/job.yaml` (enabled by default, weight -20) and `docker-compose.standalone-full.yml:287-299`. My "compose has no equivalent hook" was wrong | `values.yaml:116-124` | Residual work is a guard for `worker.enabled` + `dbInit.enabled=false`, size S |
| ~~A6~~ | **STRUCK.** pylon-indexer is deliberately absent from the chart — index ingest is served by the Go runtime plane through the agent worker. There is no template and no chart for it. `deploy/README.md:449-450` is stale text about the legacy compose file | `values.yaml:38-42` | Nothing to build |

**Not a gap, despite appearing in the seed scripts:** PAT rows
(`POST /api/v2/auth/token/` + the `/settings/tokens` page are both real —
`v2/auth/handler.go:76-79`), per-project roles and the project system PAT (both
are provisioning steps — `projectprovisioning/steps.go`), central RBAC
(seeded by migrations `0060`–`0091`), and the personal project (#609). The seed
scripts bypass working surfaces there for speed, not because the surface is
missing.

---

## B. Backend — built but not switched on

Every one of these is implemented, tested, and **off in a default Helm install**.
`deploy/helm/elitea/values.yaml:455-583` states the reason for each: the
capability needs production authentication, which the default chart does not
build, and `cmd/elitea-main` refuses to start with the flag on and auth off.

| # | Flag | What is dark | Gap? |
|---|---|---|---|
| B1 | `ELITEA_CONFIGURATIONS_ENABLED` | eight configuration + model-catalogue routes; the dependency agent dispatch and index ingest both compose through | **Yes** — this is what makes A3 manual |
| B2 | `ELITEA_CONFIGURATIONS_MUTATION_ENABLED` | three configuration **write** routes that *win* the path from the compatibility routes every install serves | **Yes, and it is a cutover** — they take a different request shape from the one `apps/elitea-web` sends today. Web-client work is part of turning this on |
| B3 | `ELITEA_PROJECT_INFO_ENABLED` | real project-info handler | **Now an honest 501 (62182c61)**, was — off, the prototype handler answers **200** with zero teammates and a null icon. The project screen renders that as truth |
| B4 | `ELITEA_INDEX_TYPES_ENABLED` | index/document-type read | **Now an honest 501 (62182c61)**, was a static six-loader list no data backed |
| B5 | `ELITEA_APPLICATION_SKILLS_ENABLED` | attached-skills read | **No** — the fallback answers the same rows in the same envelope |
| B6 | `runtime.enabled` | the whole runtime plane (agent execution, index dispatch/scheduling) | Prerequisite for A1 mattering at all |

**The real work here is not flipping flags.** It is making the default chart
build production authentication so the flags *can* be on, and closing the B2
request-shape cutover. Until then every install is either "auth off, capabilities
dark" or "hand-configured".

---

## C. Backend — implemented refusals and unported surfaces

Routes that exist and answer honestly that they cannot serve. `501` here is
deliberate (see `v2/analytics/handler.go:70-100` for why it is not `500`), so
these are *known* gaps, not bugs.

### C1. Genuinely missing, worth porting

| Surface | Route / file | State |
|---|---|---|
| **MCP `tools/call` execution** | `v2/mcp/server.go:217`, `registry.go:71,188` | Listing is real; running is not. Needs the agent runtime (agents) and the Python worker's toolkit dispatch (toolkits). Returns `isError: true` naming what is missing, never an empty success |
| **MCP SSE pair** (`GET /<pid>/sse` + `POST /<pid>/messages`) | `v2/mcp/handler.go:21` | Not ported. The modern streamable-HTTP transport is; this is the deprecated one |
| **Analytics by agent** | `repos/analytics.go:44-56` | Only the AGENT dimension is missing: a gateway request knows its model, not the agent that composed it, and nothing correlates the two. Tool is a weaker, different problem — nothing records a tool call outside a chat turn |
| ~~Billing dimensions~~ | — | **WRONG, struck.** Migration `0084_budget_usage_dimensions.sql` added `gateway.llm_usage_events` carrying project, user, provider, model, prompt/completion/total tokens, api_requests and cost_usd, with two writers and three readers. Four of the five dimensions ship today |
| **TTS voice list** | `v2/configurations/handler.go:1641,1657` | 501. The reference reads voices from the provider |
| ~~Realtime / audio streaming~~ | — | **WRONG, struck.** `realtime.go:448` answers 501 only when the dialer is nil; production wires it (`cmd/elitea-llm-gateway/main.go:404`), `/llm/v1/audio/speech` is registered (`internal/api/router.go:73`), and the `stream_format` refusal is a deliberate 400 on a unary route |
| **SCIM filter grammar** | `internal/scimdirectory/filter.go:122` | Grouping and `and`/`or`/`not` not implemented; some PATCH paths answer 501 (`scim/users.go:350`, `groups.go:283`) |
| **Conversation-DELETE leaves attachment bytes** | `repos/conversations.go:340-397` | Narrower than claimed: `delete_attachment` IS ported (`v2/conversations/handler.go:620-700`) and the `:1123` comment is stale. `ConversationsRepo.Delete` never calls the object store, and the retention sweeper eventually expires them — a timeliness gap, size S |
| ~~Agent markdown export~~ | — | **WRONG, struck.** `v2/eliteacore/handler.go:3514` branches on `format=md` into `writeMarkdownExport`; `export_markdown.go` is 25 KB with tests. The stale artefact is the frontend doc comment |
| **Config field suggestions** | `v2/admin/config_suggestions.go:64` | 501 whenever the deployment cannot enumerate the toolkit registry — i.e. always, on a Go-only stack |

### C2. Awaiting an architectural decision, not a port

| Surface | State |
|---|---|
| **Service descriptors / provider hub** | `v2/eliteacore/service_descriptors.go:145`. Successor is ADR-0012/P3. DeepWiki and Inventory already deploy separately and self-register — only the hub is missing |
| **Prebuilt MCP config machinery** | Main and Admin UI own manual catalogue editing. PostgreSQL changes apply to later catalogue reads and execution claims. Legacy YAML migration remains unported. |

---

## D. Admin UI

The admin SPA (`services/elitea-main/.../admin/`) is close to complete: Projects,
Users, Roles, Permission matrix, Secrets, Audit trail + heatmap, Schedules,
App requests, Identity providers, SCIM bindings, MCP servers, Platform models,
LLM proxy (providers/models/usage/logs/alerts/status), Gateway governance,
Features, Configuration.

What the Configuration page still withholds
(`v2/admin/config_schemas.go`; the server declares `unavailable_reason`, the SPA
only renders it):

| Section | Reason | Action |
|---|---|---|
| `observability` | pylon plugin config | **REMOVE the section, port one field.** `tracing_enabled` is env-owned per process (`libs/go/observability` reads `OTEL_*` in four binaries) so a DB row would be a second source of truth; `audit_trail_enabled` toggles a writer that does not exist (see A11); `analytics_enabled` is a genuine `platform_config` flag and belongs on the **Features** page beside `mcp_in_menu` — gate the `v2/analytics` routes on it too, or you ship a hidden button over an open endpoint |
| `runtime` | pylon plugin config | **REMOVE.** Ten fields, three owners, none of them this page: `ai_project_id` is read from env by elitea-main **and by the gateway, a separate process**; `ai_project_allowed_domains` is deployment-mounted YAML read once at startup; the four scheduling fields are `centry.schedule` rows the **Schedules & Tasks** page already edits; the rest are pylon-indexer's mounted YAML |
| `admin_panel` | pylon plugin config | **REMOVE.** Zero fields, and the same subject as `advanced`, which was removed 2026-08-24 for exactly this reason. A strictly worse `advanced`: it makes the promise and has nothing to describe |
| `service_descriptors` | provider hub is pylon-only | Blocked on C2 |
| `mcp_servers` | → `managed_surface: mcp_prebuilt_servers` | Done, no work |
| `llm_proxy` | → `managed_surface: llm_proxy` | Done, no work |
| `auth` | → `managed_surface: identity_providers` | Done, no work |
| `governance` | authored at `/admin/gateway/governance` | Done, no work |
| `agent_publishing.validation_rules`, `skill_publishing.validation_rules` | field-level; validation is deterministic here | Correctly withheld |

Missing admin surfaces (new work, not ports):

- **D1. Workload-session enrolment** — the UI for A1's *residual* gap (revocation,
  rotation, a second worker), not for install-time provisioning, which the chart
  hook already does. The registrar must be a **separate type** from
  `WorkloadSessionsRepository`, whose doc comment is a load-bearing statement that
  the verifier exposes no registration path.
  (Note: the admin SPA lives at `apps/elitea-web/src/pages/admin/`, not under
  `services/elitea-main/` as stated earlier in this document.)
- **D2. First-admin bootstrap** — a one-time claim flow, or chart-driven
  promotion, that works on the OIDC plane (A2).

---

## E. Product UI (elitea-web)

The old UI is a **moving target**: it gained 774 commits between the previous
submodule pin and `20b23c42`, and several of them are whole product areas that
elitea-web has no counterpart for. Parity is therefore not a fixed finish line,
and any plan that treats it as one will keep discovering new gaps at cutover.

The several hundred `NOT ported` comments inside `apps/elitea-web/src` are, on
inspection, deliberate micro-fidelity drops (tour targets, brand icon resolvers,
styling overrides) — not features. The gaps that matter are these.

### E0. Whole features added upstream since the old pin — nothing ported

| Feature | Upstream | elitea-web | Backend |
|---|---|---|---|
| **Agent Evaluation** — suites, datasets, cases, dimensions, runs, scorecards, human scores, import/promote, platform catalog | `widgets/evaluation/` (61 files, own `evaluationApi.js`) | **absent** | **absent** — 21 endpoints under `elitea_core/eval_*`; grep for `eval_suite|eval_dataset|eval_run|eval_dimension|eval_binding` across `services/` returns **0** |
| **Shared conversation** — public read-only chat at `/shared/chat/:token` | `pages/shared-conversation/` (4 files) | absent | unverified |
| **Skill Hub** — **mostly ported, and it is the Skills tab of the Catalog, not a separate item.** `PublicSkillsCatalog.tsx:1-21` documents itself as a deliberately reduced port, and the Go backend is registered (`router.go:2208`) | — | present | Residual: the `agents_with_skill` consumer (generated client exists, zero call sites) |
| **NPS survey** | `widgets/nps-survey/` (18 files, own API) | absent | absent |
| **Elitea Catalog** (`/elitea-catalog`) — **`agents-hub` is NOT deleted**: `features/agent-hub/` is intact (21 files) and `/agents-hub` still mounts a `LegacyCatalogRedirect`. Only the page *shell* is new, and it composes the existing Agents tab with the Skills tab | `EliteaCatalog.jsx:7-8`, `ProtectedRoutes.jsx:194` | **S, not L** — a two-tab shell over two bodies that both already exist, plus a redirect | n/a |
| **Compare versions**, **indexing report**, **edit-entity-with-AI** | `entities/compare-versions`, `entities/indexing-report`, `entities/edit-entity-with-ai` | absent | unverified |

Agent Evaluation is the largest single item in this document, in either surface,
and it is also live product work — the `feat:agent_eval` issues in
`elitea_issues` are open and being added to now.

### E0b. Routes added upstream, not present in elitea-web

`/toolkits/:tab/:toolkitId/test`, `/mcps/:tab/:mcpId/test`,
`/mcps/:tab/:mcpId/history`, `/toolkits/:tab/:toolkitId/index/:indexName/search`.
The toolkit index and run-history routes are **correctly** folded into tabs here
(`features/toolkits/indexes/`, `IndexesTab.tsx`, `run-history`), so verify each
of these is genuinely missing rather than re-homed before scheduling it.

`/settings/project-context` + `/settings/project-context/edit` are ported but at
a **different path** (`/settings/project-params`) — a deep-link and
documentation mismatch, not a missing feature.

### E1+. Feature-scale gaps in what was ported

| # | Gap | Evidence | Size |
|---|---|---|---|
| E0c | **STILL OPEN — and now load-bearing for canvas.** No socket server exists, and two features need one. `internal/api/socketio` was deleted with the prototype indexer transport (#126) and was never mounted; nothing serves canvas rooms or eval-run progress. `shared/api/socket/events.ts:474-596` still cites `socketio/server.go` line numbers as evidence — **stale references to a deleted file**, the same class as the 204 the pin refresh re-anchored | `cmd/elitea-main/main.go:1527-1537` | Blocks canvas presence AND the evaluation live-progress feed. Until it lands, two people editing one canvas silently last-write-wins — say so in the UI rather than shipping a lock that is not there |
| E1 | **Chat canvas editor is a shell, and is not mounted at all.** `ChatWithEditors.hooks.ts:222-246` supplies `onShowCanvasEditor` as a documented no-op and `canvasEditorRef` as permanently `null`, so filling the 18 TODOs alone ships nothing reachable. Slice 0 is composition; slice 1 is re-adding the imperative ref API that `shared/ui/CodeMirrorEditor` deliberately trimmed CodeMirror host, Mermaid output, markdown-table editor, undo/redo, quick-fix, presence socket and CSV import are all `TODO` placeholders | `features/chat-messages/ui/canvas/CanvasEditor.tsx` (18 TODOs), `Canvas.tsx` (4), `CanvasEditHeader.tsx` | L |
| E2 | **Playback message list** not rendered | `features/chat-messages/ui/playback/PlaybackChatBox.tsx:307` | M |
| E3 | **REOPENED, then addressed.** The UI was at parity but the BACKEND did not exist: `/social/author` dropped `max_context_tokens`, `preserve_recent_messages` and `enable_summarization` silently, and the runtime refused all context management. The Rust half landed (08f870e9); the Main-side contract is in flight. Original note follows — it was right that the placard matches the baseline, and wrong that this meant nothing was missing. The baseline's own `MemoryLongTermMemory.jsx:18-27` is *also* a "Coming soon" placard, and its import is **commented out** (`MemoryContextManagement.jsx:13`), so nothing renders it upstream. elitea-web already has the route and both real components. This row was wrong twice: first as "needs a backend", then as "the baseline ships it" | `MemoryFormContent.tsx`, `routes/_shell/settings/memory.tsx:19` | Optional one-liner: stop rendering the placard, to match the baseline |
| E4 | **Custom skill icons** = "coming soon" tooltip | `features/skills/ui/SkillForm.tsx:86` | S |
| E5 | **Per-word TTS highlight sync** dropped | `features/chat-messages/ui/chat-box/ApplicationAnswer.tsx:18` | S |
| E6 | **Default version is invisible after setting it.** NOT a 405 gap: the route, handler (`applications/handler.go:1202`) and hook all exist; the 405 applies only to a legacy request shape deliberately not ported. The real gap is that `Get` emits no `meta.default_version_id` and `getVersions` omits `is_default` | `applications/handler.go:122-160` | S (frontend-led) |
| E7 | **Phantom route.** The baseline declares `EditBucket` but has **zero call sites** and never mounts it; its real edit flow navigates to `/artifacts/create-bucket` with the bucket in state. Waive the route. The genuine gap is that elitea-web has **no bucket-edit affordance at all** | `routes.js:76` vs `ProtectedRoutes.jsx:285`, `Buckets.jsx:118-130` | S |
| E8 | **Support assistant**: attachments, screenshots and mermaid deliberately not ported | `v2/supportassistant/handler.go:51` | M, optional |

---

## F. Deliberate non-goals — do not schedule

Listed so they stop reappearing in gap reviews. Each is something the target
architecture drops on purpose, and each already answers honestly.

- Pylon runtime administration — plugin reload, remote plugin config, plugin
  listing (`v2/admin/handler.go:184`).
- Arbiter task nodes (`v2/admin/handler.go:289`), and the Arbiter/RabbitMQ
  transport generally (AGENTS.md forbids it; provisioning drops `rabbit_vhost`).
- Plugin version reporting / system info from Arbiter announcements
  (`v2/admin/handler.go:132`).
- InfluxDB provisioning (`projectprovisioning/steps.go`).
- The MCP `api` tool category — republishing REST endpoints as MCP tools. The
  per-endpoint opt-in list does not exist here, and inferring one would publish
  the admin surface to any PAT holder (`v2/mcp/handler.go:56`).
- The admin Configuration `advanced` section (removed 2026-08-24).
- `AUTH_DEV_MODE` — to be deleted, not completed (ADR-0017).

---

## Where the remaining work lives

Everything not closed on this branch is filed, each issue carrying its own
evidence, a backend / product-UI / admin-UI split, acceptance criteria and the
test bar below.

| Issue | What |
|---|---|
| [#616](https://github.com/EliteaAI/elitea-platform/issues/616) | MCP `tools/call` — the TOOLKIT half. A full vertical slice: proto capability, worker handler, and durable effect identity, because running a toolkit tool writes Jira tickets and pushes commits |
| [#617](https://github.com/EliteaAI/elitea-platform/issues/617) | Agent Evaluation — the run engine, results and scorecard. Three hard prerequisites, none of them UI |
| [#618](https://github.com/EliteaAI/elitea-platform/issues/618) | Analytics by TOOL. Blocked on a missing EVENT, not a missing column — sequence after #616 |
| [#619](https://github.com/EliteaAI/elitea-platform/issues/619) | Audit-event retention. The writer landed here deliberately without a sweeper |
| [#620](https://github.com/EliteaAI/elitea-platform/issues/620) | The Form plane still issues no actor PAT — the last install step needing seeded SQL |
| [#621](https://github.com/EliteaAI/elitea-platform/issues/621) | The spec and two comments describe refusals that no longer happen, plus a CI check for the whole class |
| [#622](https://github.com/EliteaAI/elitea-platform/issues/622) | Canvas presence over the EXISTING SSE plane — explicitly **not** a rebuilt socket server |
| [#623](https://github.com/EliteaAI/elitea-platform/issues/623) | SCIM compound filters. Recorded, not scheduled: the trigger is a named IdP actually sending one |
| [#624](https://github.com/EliteaAI/elitea-platform/issues/624) | **Decisions.** Three defaults that keep a stock install from configuring itself. Nothing should be built against these until they are answered |
| [#625](https://github.com/EliteaAI/elitea-platform/issues/625) | TTS highlight, support-assistant attachments, and the skill-hub attach dialog |

Two existing issues gained evidence rather than duplicates:
[#545](https://github.com/EliteaAI/elitea-platform/issues/545) (J28 is a seventh
journey of its class, with the diagnosis recorded) and
[#323](https://github.com/EliteaAI/elitea-platform/issues/323) (its premise has
landed; only a voices listing remains).

## The evidence bar every one of those carries

Stated once here because it is the thing this branch spent most of its effort
learning:

1. **Assert the ROW, not the absence of an error.** A handler that answers `200`
   and writes nothing has shipped here repeatedly.
2. **Integration tests SKIP silently** without `ELITEA_TEST_DATABASE_URL`, and a
   skipped package prints the same `ok` as a passing one.
3. **Prove red before green**, and say how.
4. **`go vet` is not the lint gate** — CI runs `golangci-lint` via
   `scripts/go/workspace-run.sh lint`.
5. **Run `check-gates-selftest`** on any web change: a `setup.ts` throw once
   emptied whole suites, and "0 test" reads as success.
6. **A `200` with a fallback body is worse than a `501`.**

## Port plan

Sequenced by what unblocks the most. Sizes are rough and per-surface.

### Wave 0 — stop measuring against a moving target  *(do first, it is cheap)*

| Step | Work | Size |
|---|---|---|
| 0.1 | **Refresh the `apps/elitea-ui` pin.** Attempted 2026-08-30; the tooling blockers are now fixed and committed, and what remains is evidence work that needs product decisions. **Done:** `gen-brand-tokens` no longer crashes on the new palettes, the route extractor no longer mis-reads nested settings routes or the renamed settings-index redirect. **Remaining, measured against `20b23c42`:** (a) two new asymmetric palette tokens (`background.folder.shadow`, `background.folder.borderGradient`) need `SYMMETRY_FILLS` entries — values already determined, see the commit message; (b) 44 new tokens and 18 changed values regenerate `default.pack.json`, so **visual baselines must be regenerated** through the `ci-web-e2e` `workflow_dispatch`, never locally; (c) **22 new ROUTE items** and **6 route renames** in the parity manifest; (d) **204 evidence references across 68 files** no longer resolve — 47 files moved, 2 are ambiguous, and **19 were deleted upstream**. (d) is the real cost: a blanket path remap would make the gate green while pointing evidence at the wrong lines, because some relocations are rewrites (`AIConfiguration.jsx` 82 → 221 lines), not moves. Each of the 19 deletions needs a decision — is the behaviour still a port requirement, or is the item retired with a waiver? | L |
| 0.2 | **Decide the cutover contract for upstream's in-flight work.** Agent Evaluation is being built in the old UI *now*. Either elitea-web ports it after the fact (permanent catch-up), or new product work targets the Go stack first. This is a programme decision, not an engineering one, and everything in Wave 5 depends on the answer. | — |

### Wave 1 — make a fresh deploy work without SQL  *(blocks everything)*

| Step | Work | Size |
|---|---|---|
| 1.1 | **Workload-session enrolment (A1).** Write path in elitea-main: `POST/DELETE /api/v2/admin/workload_sessions`, an issuance helper the worker's cert identity maps to, and a chart hook that enrols the shipped worker identity on install. Then D1 (admin page). | L |
| 1.2 | **First-admin bootstrap (A2).** Extend `identity.Provision`'s `InitialGlobalAdmins` consumption to the OIDC plane, or add a chart-driven promotion Job. | M |
| 1.3 | **Boot preconditions (A4/A5/A6).** Make the artifact bucket creatable by the migration/`db-init` Job instead of being a precondition; add a model-cache init container to the chart. | M |
| 1.4 | **Acceptance gate.** One CI job: fresh empty DB → `helm install` → login → create agent → chat turn completes, **with zero `psql`**. Without this gate the bucket refills. | M |

### Wave 2 — turn the dark capabilities on  *(unblocks A3, B3, B4)*

| Step | Work | Size |
|---|---|---|
| 2.1 | Make the default chart build production authentication, so B1–B4 can be `true` without a CrashLoopBackOff. | L |
| 2.2 | Flip `ELITEA_PROJECT_INFO_ENABLED` and `ELITEA_INDEX_TYPES_ENABLED`. Both are pure wins; the prototype fallbacks answer 200 with wrong data today. | S |
| 2.3 | **B2 cutover:** align `apps/elitea-web`'s configuration-write request shape with the mutation routes, then enable `ELITEA_CONFIGURATIONS_MUTATION_ENABLED`. This is what retires A3. | L |
| ~~2.4~~ | **STRUCK — already done.** ADR-0017 landed; the middleware has no bypass. What remains at `cmd/elitea-main/main.go:102-104` is a **removal tripwire** that refuses to start on `AUTH_DEV_MODE=true`, so a stale manifest cannot make an operator believe auth is off. Deleting it would make things *less* safe. Confirm it stays | — |

### Wave 3 — close the honest refusals

| Step | Work | Size |
|---|---|---|
| 3.1a | **MCP `tools/call`, agents only**, via the runtime plane. Ships alone behind the existing nil-check degradation, and is the half an MCP client actually wants. Needs a target discriminator on the internal tool descriptor first (`catalog.go:32-36` carries name/description/schema and nothing else), and a decision on whether an MCP run creates a conversation. | M |
| 3.2 | **Agent dimension only.** A `v2` signed-identity header carrying execution id (the canonical string is duplicated across two modules — bump the version or a rolling deploy fails every request), two nullable indexed columns, and a read-time join to `execution_jobs`. **Backfill is impossible**; follow the `usageDimensions.Available` precedent and omit the block rather than zero-fill it. | M |
| 3.3 | Conversation-DELETE attachment bytes (S) + two stale-comment deletions (XS) + surfacing `is_default` (S). | S |
| 3.1b | **MCP `tools/call`, toolkits.** A full vertical slice: new proto capability (narrowing two `reserved` ranges), a worker handler, and durable effect identity/idempotency/cancellation — running a toolkit tool writes Jira tickets and pushes commits, so the bounded sync bridge is explicitly not sufficient. Defer. | L |
| 3.4 | SCIM filter grammar and the 501 PATCH paths. Only if an enterprise SCIM client needs it — scope on demand. | M |
| 3.5 | TTS voices, realtime/audio. Scope on demand. | M |

### Wave 4 — admin UI completion

| Step | Work | Size |
|---|---|---|
| ~~4.1~~ | **DONE (029adb3b).** Removed all three (rationale per section above), and move `analytics_enabled` to the Features page. **No permission blast radius**: none of the three carries a `required_permission`, so `declaredPermissions()` is unchanged and the roles-catalogue test is untouched. The edit set is server + 2 Go tests + 4 web files, and the SPA needs no change beyond a doc comment and one fixture — it is fully data-driven off the payload | M |
| ~~4.2~~ | **STRUCK — already done.** `LoadPinnedCurrentToolkitSchemaSnapshot()` is called unconditionally at `cmd/elitea-main/main.go:1404` and **stops startup** on failure, so a running deployment always has the 52-entry registry. The 501 survives only as a typed-nil guard that cannot fire in a booted process. Worth one composition test asserting the registry is non-nil and enumerates >0 types | — |
| 4.3 | Service descriptors — blocked on ADR-0012/P3. | — |

### Wave 5 — product UI

| Step | Work | Size |
|---|---|---|
| 5.1 | **Canvas editor (E1) — slices 0 and 1 DONE** (composition + CodeMirror imperative ref API). Mermaid, table editor and quick-fix remain; presence is BLOCKED on E0c. | L |
| 5.2 | Playback list (E2) **DONE** (rendered AND mounted). Skill icons (E4) and bucket-edit (E7) remain. | M |
| 5.3 | Long-term memory (E3) — needs a backend; sequence after Wave 3. | L |
| 5.4 | TTS highlight (E5), support-assistant attachments (E8). Optional. | M |
| 5.5 | **Agent Evaluation (E0)** — 21 backend endpoints plus a 61-file UI area. Own workstream, not a task; sequence per 0.2. | XL |
| 5.6 | Shared conversation, Skill Hub, Elitea Catalog rename, compare-versions, indexing report, edit-with-AI (E0). | L |

---

## Two rules this repository keeps re-learning, and that this plan depends on

0. **A disclosed-gap comment is a claim about a moment in time, and nothing
   re-checks it.** Verifying Wave 3 against the code disproved **five** rows of
   this document's own section C1/E — every one of them transcribed from a
   `NOT PORTED` doc comment the code had since outgrown (billing dimensions,
   markdown export, realtime/audio, attachment cleanup, set-default-version).
   `v2/mcp/registry.go:71` and `repos/analytics.go:15-19` both say this about
   themselves in as many words. Worth a CI check: fail when a `NOT PORTED`
   comment sits in a file whose neighbouring symbol now exists.
1. **Absence reads as correctness.** A missing writer, an unenumerated registry,
   an empty table — all answer "fine" to every check that does not assert
   presence. Every Wave-1 step needs a test that fails when the thing is absent,
   not one that passes when nothing throws (A1 is exactly this shape).
2. **A 200 with a fallback body is worse than a 501.** B3 and B4 are dark
   capabilities whose fallbacks answer success with wrong data, which is why they
   went unnoticed. Prefer the refusal until the real handler is on.
