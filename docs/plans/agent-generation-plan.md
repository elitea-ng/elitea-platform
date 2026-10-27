# Plan: conversational agent creation (local and Elitea agents)

Status: implemented on `feat/agent-generation` (phases 0 to 3). The decision
is [ADR-0037](https://github.com/elitea-ng/elitea-docs/pull/59)
(`docs/internal/03-architecture/adrs/adr-0037-conversational-agent-creation.mdx`
in `elitea-docs`), merged. This file is the plan as built: the original
draft, corrected against the code and the owner rules. The corrections are
listed at the end.

## Fixed decisions

1. The local agent is an `AGENTS.md` file. The desktop runtime already reads
   it (`libs/rust/local-tools/src/project_instructions.rs`: root plus nested,
   case-insensitive name, 32 KiB total cap, symlinks and denied paths
   refused). A nested `AGENTS.md` applies when a turn references a path
   under its folder. No runtime change is needed.
2. Desktop is first. Its threads offer both branches when the turns run on
   this computer, and the Elitea branch only when the thread runs in the
   cloud. Web offers the Elitea branch. Mobile offers the Elitea branch: it
   has no folder.
3. The Elitea branch uses a new streaming draft route. The one-shot route
   (`POST /elitea_core/generate_application_draft/...`) stays for older
   clients, and stays the web fallback.
4. A saved Elitea agent is an ordinary project agent: it is in the project's
   agents list at once and runs from desktop, web and mobile. Publishing to
   the public catalog is optional and uses the existing publish routes. No
   new execution path is added.
5. Internal hops are gRPC over mTLS. The stream goes through the existing
   `predict.GatewayCompleter` → `llmproxy.Gateway.ChatCompletions` hop
   (server-streaming), never through an internal HTTP call.
6. The contract is the mock source. The schemas are components in
   `api/openapi/v2.yaml` with examples; there is no checked-in JSON fixture.

## Phase 0: publish the agent schemas

- `v2.yaml` components: `AgentDefinition` (the intended cloud agent: name,
  description, instructions, welcome message, up to four starters, model
  settings, variables, tool and skill references), `LocalAgentDefinition`
  (name, description, instructions, `scope` = `root` or a relative folder
  path), `AgentSchemaResponse`, and `DraftRefusedResponse` (the one-shot
  route's 422 now carries a `code`).
- Read route `GET /elitea_core/agent_schema/prompt_lib/{project_id}`
  (`getAgentSchema`): both schemas plus `schema_version` (the first 12 hex of
  their SHA-256) and an `ETag`. The schemas are plucked from the embedded
  spec, so the served schema and the contract cannot drift. Gate:
  `models.applications.applications.create` (the permission-map test only
  admits seeded, legacy-declared permission names).
- Validator: `internal/api/v2/drafts/schema.go` compiles the components with
  jsonschema v6. Both draft routes share `finishApplicationDraft`: parse,
  cut-off detection, validation, truncation. Machine codes:
  `agent_draft_invalid`, `agent_draft_unparseable`, `agent_draft_cut_off`.
- Client contract 1.28 (1.26 and 1.27 are taken by work in flight): lock
  regenerated, discovery document, `API_CONTRACT.md`,
  `docs/client-feature-parity.md`.

## Phase 1: the streaming draft route

- `POST /elitea_core/generate_application_draft_stream/prompt_lib/{project_id}`
  (`generateApplicationDraftStream`). Request: `user_description`,
  `conversation_history` (at most 20 user/assistant turns, each at most
  16384 characters), `current_draft` (the draft being revised).
- Response: `text/event-stream`, opened before the gateway answers, with a
  keepalive. Frames: `draft.token {text}`, then one `draft.complete` (the
  validated `ApplicationDraft`, as the one-shot route returns it) or one
  `draft.error {code, status, message}` (status 422, 500 or 502).
- Only a refusal before the stream opens is plain JSON: 400 (bad body), 403,
  503 (no gateway). Because the stream is already open when the gateway
  fails, a gateway failure is a `draft.error` with status 502, not an HTTP
  502.
- `predict.StreamingCompleter` (`stream.go`): `GatewayCompleter` asks the
  gateway with `stream: true` and reads the relayed OpenAI SSE (data-only
  frames, `data: [DONE]`, a mid-stream `{"error": ...}` frame without
  `[DONE]` is a cut-off). The default-model resolver streams too. A
  completer without streaming falls back to one delta.
- Billing: one completion per call, metered by the gateway as every stream
  is. The client never regenerates by itself.
- Not on the command bus: it is a synchronous user call.

## Phase 2: the local agent (zero Go, zero Rust)

- The D0 local turn has no per-turn instruction field, and a turn needs a
  saved agent version. So the local branch is an ordinary user message in
  the current thread: "write `AGENTS.md` at <scope> with these sections",
  composed by `localAgentPrompt` (`libs/ts/client-core/src/agent-builder`).
  The thread's agent writes the file with `write_file` or `edit_file`,
  behind the approval card. No new tool, no IPC command, no loader change.
- `localAgent.ts` holds the scope rule (relative, no `..`, no absolute
  path), the 32 KiB cap, and the `AGENTS.md` sections. The loader re-checks
  everything on every read; the client check is a convenience.
- The next turn in the folder reads the file. Committing it shares it.

## Phase 3: the three clients

- Shared (`@elitea/client-core/agent-builder`): the branch question
  (`initialBranch`), the local prompt, the draft-stream event vocabulary and
  `draftReducer` (idle, streaming, review, failed; a stream that ends
  without a result is the cut-off failure), the request body
  (`draftRequestBody`), and the create body (`agentCreateBody`: an
  `interface` agent with a `base` version, step limit 25, default model).
- Web: `GenerateAgentModal` streams when `aiDraftStreaming` is on, shows the
  text as it arrives, and its review step gains Refine (the next turn, with
  the conversation and the current draft). `shared/api/http.ts` gains a
  `stream` read so the POST stream uses the app's one fetch site. The
  endpoint manifest lists the route.
- Desktop (the web build in `--mode desktop`): the thread toolbar's menu
  ("Create Agent…") and the command palette ("Create agent…") open
  `CreateAgentDialog`. Local agent: the request, the folder and an optional
  name become the message sent into the thread. Elitea agent: the streamed
  builder; after save the agents list is refreshed and the new agent is
  picked for the thread. A thread in the cloud goes straight to the Elitea
  builder. The composer's slash commands are not changed (steering work
  touches the composer).
- Mobile: New chat offers "Create an agent" on a 1.28 workspace. The
  builder streams over the POST form of the stream transport (Bearer,
  `X-Client-Version`, no redirects), lets every field be edited, refines,
  saves through `createApplication`, and opens a chat with the new agent.
  The generated client lists `generateApplicationDraftStream` and
  `createApplication`.

## Verification

- Go: tests in `internal/api/v2/drafts` and `internal/api/v2/predict`
  (schema equals the spec, examples validate, SSE framing, the 400/503 vs
  frame split, cut-off, unparseable, missing field, history bounds, role
  filter), router permission and surface tables, the client-contract lock.
- Web: modal, stream, dialog and workspace tests; endpoint manifest,
  generated client, i18n, budgets and theme gates.
- Mobile: stream and screen tests, lint, typecheck, generated-client check.
- CI runs the full suites.

## Deferred

- The empty "no agents" state on desktop still opens the agent editor, not
  the builder.
- A native menu entry for Create agent (it needs a host menu id).
- Editing an existing agent conversationally on mobile.
- Tool and skill references in the generated agent: the draft's
  suggestions are shown on web, not attached automatically.

## Corrections to the original draft

1. The stream goes over the gRPC `ChatCompletions` hop, not the gateway's
   HTTP `/llm/v1/chat/completions` (owner rule: no internal HTTP).
2. No `agent-schemas.json` fixture and no "conformance fixtures": the
   schemas are `v2.yaml` components with examples, served from the embedded
   spec; Go tests and the spec-examples gate cover them.
3. A gateway failure on the stream is a `draft.error` frame with status 502,
   not an HTTP 502: the stream is open before the gateway answers.
4. The local branch is a user message, not a branch question inside the D0
   turn: D0 has no per-turn instruction field.
5. The mobile SSE helper is GET-only and resumable; the draft stream needs a
   POST without resume, so it has its own reader over the shared transport.
6. The web had no POST stream helper; `http.ts` gained a `stream` read.
7. A saved agent is usable at once from the project agents list; the public
   catalog is not the availability path.
8. The contract version is 1.28, and the contract-bump steps (lock,
   discovery, `API_CONTRACT.md`, feature parity, generated clients, the
   operation count) are part of phase 0 and 1, not a separate phase.
