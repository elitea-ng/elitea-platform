# Plan: conversational agent generation (local and Elitea agents)

Status: draft. This document is a plan. The architecture decision is proposed
as [ADR-0037](https://github.com/elitea-ng/elitea-docs/pull/59) in
`elitea-docs` (pull request #59, status Proposed). Code for this plan lands
only after that ADR is approved.

## Fixed decisions

1. The local agent is an `AGENTS.md` file. The desktop runtime already reads
   it (`libs/rust/local-tools/src/project_instructions.rs`: root plus nested,
   case-insensitive name, 32 KiB total cap, symlinks and denied paths
   refused). No runtime change is needed for the local agent to be usable.
2. The chat surface hosts the feature in all three clients:
   - desktop: local project sessions and cloud sessions;
   - web: cloud chat;
   - mobile: cloud chat.
   The mobile client offers only the Elitea branch. It has no local
   workspace.
3. The Elitea branch uses a new streaming draft route. The existing
   one-shot route (`POST /elitea_core/generate_application_draft/...`)
   stays unchanged for older clients.
4. An Elitea agent becomes available through the existing public catalog
   (`listPublicApplications`). No new execution path is added.

## What already exists

- One-shot AI generation, served:
  `services/elitea-main/internal/api/v2/drafts/drafts.go`
  (`GenerateApplicationDraft`, issue #254 P1). One blocking LLM hop through
  the gateway (`v2predict.Completer`). Returns an `ApplicationDraft` with
  `suggested_toolkits/mcp/agents/pipelines/skills` (issue #881). Gated on
  `models.applications.applications.create`.
- Web UI already consumes it:
  `apps/elitea-web/src/features/agents/ui/generate-agent-modal/`
  (`GenerateAgentButton`, `GenerateAgentModal`, review form). Flag:
  `aiGeneration` in `apps/elitea-web/src/shared/config/backendCapabilities.ts`.
- Agent data model: `applications` rows plus `application_versions`
  (`agent_type` is `agent` or `pipeline`). Create and version routes are the
  reviewed `internal/api/v2/applications` handlers.
- Desktop local turn (D0, ADR-0029): resolves the agent version from the
  cloud, runs the local loop, loads `AGENTS.md` into the system
  instructions, and offers the local write tools (`write_file`,
  `edit_file`, `apply_patch`) inside a confined workspace, behind the
  approval bridge (`apps/elitea-desktop/src-tauri/src/d0/`).
- SSE in elitea-main is established: `internal/api/v2/executions/events.go`,
  the MCP resume streams, and the router excludes
  `text/event-stream` from compression.
- JSON schema validation is available in elitea-main:
  `github.com/santhosh-tekuri/jsonschema/v6` is already in
  `services/elitea-main/go.mod`.

## Design

One assistant, one question, two branches:

```
user: "create an agent for ... (in chat)"
assistant: "Local agent (an AGENTS.md in this project),
            or an Elitea agent (AI-generated, runs in the cloud)?"
   - local   -> write AGENTS.md with the existing local tools
   - Elitea  -> streaming draft route -> review form -> save -> publish
```

### Phase 0 — publish the agent schemas

Goal: one authoritative schema pair, served by the platform, used by the
server validator and by the client validators.

- Add two components to `services/elitea-main/api/openapi/v2.yaml`:
  - `AgentDefinition`: the intended cloud agent. `name`, `description`,
    `instructions`, `welcome_message`, `conversation_starters`, model
    settings, variables, tool references, skill references. This is the
    *intended* shape. It is not the database passthrough
    (`ApplicationVersionDetail`).
  - `LocalAgentDefinition`: `name`, `description`, `instructions`,
    `scope` (`root` or a folder path), and the caps the loader enforces
    (32 KiB total, no symlinked path, the nested-scope rule).
- Add one read route:
  `GET /api/v2/elitea_core/agent_schema/prompt_lib/{project_id}`.
  It answers the two schemas plus a `schema_version` string. No write, no
  tenant state. Auth and project permission run before the handler mounts,
  like every other product route.
- Check in a fixture for CI and offline validation:
  `services/elitea-main/api/openapi/agent-schemas.json`. A Go test asserts
  the served route and the fixture agree.
- Add a validator: `services/elitea-main/internal/api/v2/drafts/validate.go`.
  It checks a draft against `AgentDefinition` (jsonschema v6). Missing
  fields are a failure (422 machine code). Length caps truncate, as the
  one-shot route already does.

Checks: `task openapi`, `task contract:check`, `task test`.

### Phase 1 — the streaming draft route

Goal: a chat-grade generation: the assistant streams, the client shows the
text, then shows the review form.

- New route:
  `POST /api/v2/elitea_core/generate_application_draft_stream/prompt_lib/{project_id}`.
- Request: `user_description` (required), `conversation_history` (recent
  turns, bounded), `current_draft` (optional: the draft being improved).
- Response: `text/event-stream`. Frame types:
  - `draft.token`: a text delta from the model;
  - `draft.complete`: the full, validated `ApplicationDraft` (the same
    object the one-shot route returns, including the suggestion lists);
  - `draft.error`: a machine code plus a message.
- The hop: the same gateway path as `predict_llm`. Add a streaming
  completer to `internal/api/v2/predict` if one is not there. The route
  streams provider deltas as `draft.token`, then validates the final JSON
  before `draft.complete`. A failed validation answers with
  `draft.error` and a 422 status recorded on the stream end; a gateway
  failure answers 502; an empty `LLM_GATEWAY_URL` answers 503, the same
  codes as the one-shot route.
- Permission: `models.applications.applications.create`, the same gate as
  the one-shot route.
- Do not add this route to the command bus. It is a synchronous browser
  call, not a durable worker command.
- Save is unchanged: the review form submits through the reviewed
  `applications` create and version routes.

Checks: `task test`, `task vet`, `task lint`, `task build`. Fixtures:
exact SSE framing, headers, the 422/502/503 splits, auth (401/403/404 by
status), tenant isolation, a non-JSON model answer, a cut-off answer, a
draft missing a required field.

### Phase 2 — the local agent (zero Go, zero Rust)

Goal: the local branch works with what the desktop already ships.

- The local branch is a D0 conversation. The user asks for an agent. The
  assistant asks the branch question. On the local branch it writes
  `AGENTS.md` (root, or a nested `AGENTS.md` for a scoped agent) with the
  existing `write_file` or `edit_file` tool. The confined workspace and the
  approval bridge are the security boundary. No new IPC command. No new
  loader.
- The assistant follows `LocalAgentDefinition` (fetched in Phase 0 or
  bundled with the client build): the name, the scope rule, the 32 KiB cap.
  The loader re-enforces the caps on every read. The client-side check is a
  convenience. The loader check is the authority.
- The file is immediately usable: the next local turn reads it, and a cloud
  turn for the same workspace reads it too (D0 splices `AGENTS.md` into the
  turn). Committing the file makes it available to other machines.

Checks: none in Go or Rust. The desktop test suite gains conversation
fixtures only if the D0 prompt changes.

### Phase 3 — the chat experience in the three clients

Goal: one shared builder, three hosts.

- Shared core: add `libs/ts/client-core/agent-builder/`. It holds:
  - the draft state machine (tokens to a full draft, then review);
  - the `LocalAgentDefinition` validator;
  - the branch-question payload shape.
  The README of that package records what is shared and what is not.
- Desktop:
  - local project sessions: the branch question in the D0 turn. Local
    branch writes the file (Phase 2). Elitea branch calls the streaming
    route through the host fetch (the webview fetch already routes through
    the host's client; no new IPC).
  - cloud sessions: the Elitea branch only, same route.
  - the review form reuses the web component
    (`features/agents/ui/generate-agent-modal/GenerateAgentReviewForm.tsx`)
    in the desktop build mode.
- Web:
  - extend `GenerateAgentModal` to consume the streaming route. Keep the
    one-shot route as the fallback when the capability is off. Add the
    capability key next to `aiGeneration` in
    `shared/config/backendCapabilities.ts`.
  - add the route to `shared/api/endpoints.manifest.json` (the
    `check-endpoint-manifest` test fails on a missing entry).
  - after save, the agent lists in the existing agents-hub data flow.
    Publishing uses the existing publish routes. The public catalog lists
    it.
- Mobile:
  - regenerate the API client from `api/openapi/v2.yaml` (the
    `ci-mobile.yml` gate fails on a stale client). The mobile SSE helper
    (`src/api/sse/stream.ts`) reads the new stream.
  - the mobile builder offers only the Elitea branch.

Checks per client:
- web: `npm run typecheck`, `npm run lint`, `npm run test:unit`,
  `npm run build`.
- mobile: `npx expo lint`, `npx tsc --noEmit`, client regeneration.
- desktop: `cargo fmt --all -- --check`, `cargo clippy --locked
  --all-targets --all-features -- -D warnings`, `cargo test --locked
  --all-targets --all-features` (only when the Rust host changes; Phase 3
  as planned touches it only for conversation fixtures).

### Phase 4 — verification and delivery

- Go: read `task test`'s exit status directly. Read the `workspace-run`
  summary: a `FAIL` line names the module, a `NOT COVERED` line means
  `services/elitea-llm-gateway` is not tested here. Read the skip ledger.
- Contracts: `task proto`, `task contract:check`.
- Conformance: add fixtures for the two new routes under `conformance/`
  (exact statuses, headers, SSE frames, auth and tenant isolation).
- ADR: [ADR-0037, pull request #59](https://github.com/elitea-ng/elitea-docs/pull/59).
  Code lands only after it is approved.
- PR body states: the compatibility surface changed (two new routes, one
  OpenAPI change, mobile client regenerated), the security assumptions
  (permission gate, confined workspace, gateway egress), the failure
  modes exercised, and the checks run or skipped. Conventional Commits,
  ASD-STE100.

## Sequencing and size

| Phase | Surface | Size |
| --- | --- | --- |
| 0 | Go, OpenAPI | small: two schemas, one read route, one validator |
| 1 | Go, OpenAPI, clients | medium: the streaming completer is the main new code |
| 2 | none (prompt + docs only) | minimal |
| 3 | TS client-core, web, desktop, mobile | medium: shared state machine, three integrations |
| 4 | tests, fixtures, ADR | proportional to 0–3 |

Phase 0 lands first. Phase 1 lands behind it. Phases 2 and 3 land in
parallel. Phase 4 runs with each pull request.

## Open risks

- The gateway hop for the stream: the drafts package is blocking today.
  The streaming completer is new code in `internal/api/v2/predict`. Verify
  the gateway's `/llm/v1/chat/completions` stream dialect (data-only
  frames, `data: [DONE]` terminator) through the mTLS hop before the
  handler is written.
- Billing: each generation call is metered through the gateway against the
  project budget. A chatty builder can burn budget. The streaming route
  takes at most one completion per call; the review form does not
  regenerate on its own.
- The one-shot route stays. Two routes that produce the same draft shape
  must not drift: the shared validation (Phase 0) is the guard.
