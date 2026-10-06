# Porting Guardrails

Unit plan. Closes the first of the not-yet-ported admin Configuration sections:
`guardrails`, whose five fields are declared in
`services/elitea-main/internal/api/v2/admin/config_schemas.go` and today answer
501 with `pylonPluginConfigUnavailable`.

## 1. What guardrails is

Five settings, all under the `toolkit_security.*` config path:

| key | type | what it governs |
|---|---|---|
| `blocked_toolkits` | `[]string` | toolkit TYPES disabled platform-wide — not registered, listed or creatable |
| `blocked_tools` | `map[string][]string` | individual tools blocked inside otherwise-allowed toolkits |
| `sensitive_tools` | `map[string][]string` | tools that must be authorised by the user before each call |
| `sensitive_action_company_name` | `string` | copy in the authorisation dialog |
| `sensitive_action_message_template` | `string` | copy in the authorisation dialog |

Reference implementations:

- authoring UI — `frontends/admin_ui/frontend/src/components/SchemaForm/GuardrailsSection.jsx`
- schema — `legacy/plugins/elitea_core/admin_schema.json`
- server-side blocklist — `legacy/plugins/elitea_core/utils/toolkit_security.py`
- runtime enforcement — `elitea-sdk/elitea_sdk/runtime/toolkits/security.py` and
  `elitea_sdk/runtime/middleware/sensitive_tool_guard.py`

## 2. What this platform already has

More than the section's `unavailable_reason` claims, which is why the section can
become live rather than stay withheld.

- **The store is real and generic.** `centry.platform_config`, written by
  `AdministrationPluginConfigValuesSave` and read by `internal/platformconfig`.
  `resources`, `mcp_configuration`, `agent_publishing` and `voice_features`
  already round-trip through it. Guardrails needs no new table and no migration.
- **The sensitive-action interrupt is already wired.** The HITL resume path
  (`approve` / `reject` / `edit` / `block_with_comment`) exists in
  `internal/application/agentexecution`, and `AgentExecutionInputV1` already
  carries `auto_approve_sensitive_actions` (field 30). What is missing is only
  the POLICY — which tools are sensitive.
- **The enum sources exist.** `PluginConfigSuggestions` answers 501 saying
  `toolkit_names`/`toolkit_tools` "have no source of truth in this service". That
  is stale: `internal/runtimecomposition/current_toolkit_schema_snapshot.json` is
  a digest-pinned projection of the SDK toolkit registry with per-type properties
  AND per-tool argument schemas. It is exactly the registry pylon read.
- **The Python worker is in this repo.** `services/elitea-worker-python`. So
  driving the SDK's sensitive-tool guard needs NO elitea-sdk change:
  `configure_sensitive_tools()` and `configure_blocklist()` are already public SDK
  functions with no non-test caller. The worker becomes that caller.

## 3. What is missing

Every consumer.

- Nothing filters blocked toolkits or tools from the type catalogue, the
  create/update paths, or the agent tool freeze.
- `platform_settings` does not publish `blocked_toolkits`, so elitea-web's
  `features/agents/lib/toolkitBlocklist.ts` and
  `features/toolkits/lib/helpers/toolkits.helpers.ts` take the blocklist as a
  parameter no call site supplies. Both files disclose this as a gap.
- The admin form has no widget for a `map[string][]string` field, so
  `blocked_tools` and `sensitive_tools` could not be edited even if the section
  were writable. `validateFieldValue`'s `object` case is likewise shallow — it
  checks `map[string]any` and nothing about the values.
- The sensitive-tool policy reaches the worker only through container env vars
  (`ELITEA_SENSITIVE_TOOLS`, set statically in
  `deploy/docker-compose.standalone-full.yml`; the retired `deploy/centry-hybrid/pov-compose.yml` set it too).

## 4. Design decisions

### D1 — One matching package, ported from the SDK, not from pylon

`internal/domain/guardrails` owns normalisation and every membership test.

The two reference implementations DISAGREE and the difference is load-bearing:

- pylon's `toolkit_security.py` compares with a plain `.lower()`, and supports no
  wildcard.
- the SDK's `security.py` uses `canonical_match_key` — lowercase then strip every
  non-alphanumeric character, so `CreateFile`, `create_file`, `create-file` and
  `Create File` all collapse to `createfile` — plus tool-name alias reduction
  (`github___list_branches` and `elitea_core:list_branches` both reduce to
  `list_branches`).

**Corrected during implementation.** This plan first said the SDK supports a `*`
wildcard toolkit key, full stop. It does not: `find_sensitive_tool_match` falls
back to `_sensitive_tools['*']`, while `is_tool_blocked` only ever looks up the
exact canonical toolkit key. So the wildcard applies to `sensitive_tools` and NOT
to `blocked_tools`. The asymmetry is preserved rather than smoothed over —
inventing a blocked-tools wildcard would block in the catalogue what the worker
still executes, and `TestBlockedToolsHasNoWildcard` pins it.

The SDK's is both the stricter rule and the one that actually runs at tool-call
time, and the shipped compose files already rely on the `*` wildcard. Port the
SDK's. A Go side that matched more loosely than the worker would let a blocked
tool through the catalogue check and then be caught (or not) at execution — two
answers to one question.

Deliverable: a table-driven parity test whose cases are taken from
`elitea-sdk/tests/runtime/test_blocked_tools.py`, so the two implementations are
asserted equal rather than assumed equal.

### D2 — Enforce at the freeze, not only at the catalogue

Catalogue filtering is what an operator SEES; the agent tool freeze in
`internal/application/agentexecution/tools.go` is the only point a running agent
cannot route around (an agent version saved before a toolkit was blocked still
names it). Both are in scope, and the freeze is the load-bearing one.

### D3 — Instances stay visible

`Handler.List` (toolkit INSTANCES) is deliberately NOT filtered. An admin who
blocks `github` must still be able to see and delete the github toolkits that
already exist, and the credentials they hold. Hiding them would strand vault
entries no surface can reach. The instances are returned and marked blocked in
the client — which is why `platform_settings` must publish `blocked_toolkits`.

### D4 — Policy travels with the run, env stays as fallback

New `AgentExecutionInputV1` field 38, `bytes toolkit_guardrails` (tags 38–63 are
reserved today), carrying a bounded non-secret JSON policy snapshot — the same
precedent `next_input_suggestion` (field 37) set and documents.

The worker decodes it, and when present calls `configure_sensitive_tools()` and
`configure_blocklist()` before building the agent; `has_sensitive_tools_config()`
then returns true and the SDK's existing `_inject_sensitive_tool_guard` fires. When
the field is ABSENT the worker falls through to the env path exactly as today, so
the two shipped compose deployments keep working unchanged.

### D5 — Suggestions served from the pinned snapshot

`PluginConfigSuggestions` serves `toolkit_names` and `toolkit_tools` from the
pinned snapshot through an interface injected at the composition root — the seam
`ToolkitArgumentSchemaSource` already established, for the same reason
(`internal/runtimecomposition` imports the api packages, so the interface lives
in the consumer and the root injects). `projects` reads `centry.project`. The
stale comment is corrected in the same change.

### D6 — No v2.yaml edit

`platform_settings` declares `additionalProperties: true`, so publishing
`blocked_toolkits` is an addition the contract already permits — the same
argument the `mcp_in_menu_enabled` comment makes in `eliteacore/handler.go`. The
admin panel routes are not described in v2.yaml at all. This unit therefore does
not touch the spec and does not trip the six spec-edit gates.

The proto change does regenerate `libs/proto/gen/**` (Go and Python) via
`task proto`. The three conformance suites under `testdata/proto/runtime/v1`
(`configuration-validation`, `node-event`, `toolkit-available-tools`) carry no
agent-input vectors, so no vector work follows.

## 5. Work, in landing order

### PR-1 — elitea-main: the policy and its server-side enforcement

1. `internal/domain/guardrails` — canonical keys, `*` wildcard, alias reduction,
   `ToolkitBlocked`, `ToolBlocked`, `SensitivePolicy`; parity test vs the SDK cases.
2. `internal/platformconfig` — `SectionGuardrails`, the five key constants, and a
   `StringLists(key)` accessor for `map[string][]string`.
3. `internal/api/v2/admin/config_values.go` — deepen the `object` case: validate
   `additionalProperties`, bound key count and total bytes. An unbounded
   `blocked_tools` is read on every freeze.
4. `internal/api/v2/admin/config_schemas.go` — drop `unavailable_reason` from
   `guardrailsSection()`. This is the change that makes the section writable, and
   it lands only alongside the consumers below.
5. `internal/api/v2/toolkits/handler.go` — filter in `toolkitTypeCatalogue()`
   (the choke point behind `ListTypeSchemas`), `ListTypes`, `AvailableTools`,
   `DiscoverTools`; refuse in `Create`, `Update`, `ForkToolkit` with a 403 that
   NAMES the blocked type. `List` untouched, per D3.
6. `internal/application/agentexecution/tools.go` — strip blocked toolkits and
   blocked tools from the frozen snapshot.
7. `internal/api/v2/eliteacore/handler.go` — `platform_settings` publishes
   `blocked_toolkits`.
8. `internal/api/v2/admin/handler.go` + composition root — real
   `PluginConfigSuggestions`.

### PR-2 — proto + worker: sensitive actions at runtime

9. `libs/proto/elitea/runtime/v1/agent.proto` field 38; `task proto`.
10. elitea-main resolves the policy at dispatch and stamps the field.
11. `services/elitea-worker-python` — decode in `protocol/agent.py`, call
    `configure_sensitive_tools()` / `configure_blocklist()` in
    `agents/sdk_adapter.py`, env fallback preserved.

### PR-3 — elitea-web: the authoring surface and the blocked badge

12. `pages/admin/api/adminConfigurationApi.ts` — declare `additionalProperties`,
    `enum_source`, `enum_source_keys`, `enum_source_values`.
13. `pages/admin/ConfigurationSectionForm.tsx` — a `toolMap` widget (toolkit
    select → tool multi-select) and `enum_source`-backed selects.
14. Feed `blocked_toolkits` from `platform_settings` into the two call sites that
    already take it as a parameter, closing both disclosed gaps.

## 6. Tests

- Go unit — normalisation parity table; catalogue filtering; create/update
  refusal; freeze stripping.
- Go postgres integration — admin GET/PUT round-trip on `guardrails` including
  object-map validation and the size bound; `platform_settings` publishes the list.
- Python worker — protocol decode; `configure_sensitive_tools` called with the
  decoded policy; env fallback when the field is absent.
- Web vitest — the map widget; the blocked badge on `ToolCard`.
- E2E — admin blocks a toolkit type, and it disappears from the create-toolkit
  type list.

## 7. Risks to state, not to bury

- **Runtime enforcement is only exercised where the runtime plane is on.** The
  freeze path runs behind `ELITEA_RUNTIME_ENABLED`. The catalogue and write-path
  enforcement are unconditional; the freeze and the worker policy are not. The
  section's copy must not claim otherwise.
- **Blocking a type does not revoke what exists.** Per D3, existing instances
  survive and stay visible. That is deliberate, and worth saying on the field
  description so an operator does not read "completely disabled platform-wide"
  as "existing toolkits stop working" — the frozen agent snapshot IS filtered,
  so they do stop working in agents while remaining visible to admins. Those two
  facts must both be in the copy.
- **The `*` wildcard is sensitive-tools only.** See the correction under D1. A
  deployment migrating a pylon `blocked_tools` map gets identical behaviour; the
  `{"*": ["delete_file"]}` shape the shipped compose files set works for
  `sensitive_tools`, which is the field they actually set.

## 8. PR-1 as landed

Delivered as planned, with three additions the work itself forced:

- `Values.String` alongside `Values.StringLists` in `internal/platformconfig`.
- The freeze's guardrail resolver is a REQUIRED constructor parameter rather than
  an option. A service built without one enforces nothing while looking exactly
  like a service whose operator configured nothing — the nil-dependency shape
  this repo has shipped before (#301/#314/#370).
- `PluginConfigSuggestions` treats an EMPTY registry as no registry. Zero toolkit
  types is not a state this platform can be in (the pinned snapshot declares 52),
  so an empty answer means a broken source — and that one rule also closes the
  typed-nil hole, where a non-nil interface holds a nil pointer whose methods
  answer empty rather than nil.

Two existing assertions in `config_values_postgres_integration_test.go` had to be
updated, and both were right to fire: the unavailable-section list, and
`TestSchemaDeclaresAvailabilityForEverySection`, which requires a section going
live to have its consumers written down next to it. Guardrails' four are now
recorded there.
