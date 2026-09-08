# Internal Elitea MCP source mapping

Status: the applications and skills categories, five Main-owned toolkit
builder operations, eight Main-owned configuration operations, three
Main-owned notification operations, and the complete three-operation project
context builder category, plus the three-operation project-secret category are
implemented. The three typed configuration operations require Main's current configuration composition.
Live per-instance toolkit discovery, model-backed draft generation, discovery, chat, analytics, and
artifacts remain gated by their owning Main capabilities.

This ledger keeps three MCP products separate:

1. The prebuilt remote MCP catalogue is platform configuration. An operator
   edits server templates in the Admin UI. Main materializes one immutable
   connection for a later worker claim.
2. Internal Elitea MCP categories let an Elitea chat or support assistant call
   selected Main business operations. Main owns these operations and their
   permissions. Rust consumes them through its existing configured HTTP MCP
   client.
3. External Elitea-as-MCP publishing exposes opted-in project capabilities to
   clients such as Codex, Claude, Cursor, and VS Code. It has separate opt-in,
   identity, transport, and execution rules.

Implementing one layer does not complete either of the other two.
The external surface has its own
[`external-elitea-mcp.md`](external-elitea-mcp.md) ledger.

Current-platform evidence was rechecked on 2026-09-04 against
`elitea_core` revision `6a036d777ca909fac377ceaec05719f0fa611b6d` and
`social` main revision `083df4b44ac251719ef68eac3080884471177969`.
The optional folder-access table and index contract is from social revision
`a1ca98e6248291cf40a5696dea12217c8ee24f62`. That revision is not yet on the
checked social main branch, so Main preserves the older no-override behavior
when the table is absent.

The typed configuration slice was rechecked on 2026-09-08 against
`configurations` revision `0f64774af6809b2b14b17f2c027fdd648e1e4556`.
Its sources are `api/v2/types.py` and `api/v2/models.py`.

## Applications category

The first internal category is available at
`/app/{projectID}/mcp/elitea_core/applications`. Its catalogue is explicit. Main
does not infer MCP tools from all OpenAPI operations because that could expose
delete, publish, execution, or administrative operations by accident.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `elitea_core/api/v2/applications.py::PromptLibAPI.get` | `models.applications.applications.list` | `get_elitea_core_applications` |
| `elitea_core/api/v2/applications.py::PromptLibAPI.post` | `models.applications.applications.create` | `post_elitea_core_applications` |
| `elitea_core/api/v2/application.py::PromptLibAPI.get` | `models.applications.application.details` | `get_elitea_core_application` |
| `elitea_core/api/v2/versions.py::PromptLibAPI.post` | `models.applications.versions.create` | `post_elitea_core_versions` |
| `elitea_core/api/v2/version.py::PromptLibAPI.get` | `models.applications.version.details` | `get_elitea_core_version` |
| `elitea_core/api/v2/version.py::PromptLibAPI.put` | `models.applications.version.update` | `put_elitea_core_version` |
| `elitea_core/api/v2/version_instruction_patch.py::PromptLibAPI.post` | `models.applications.version.update` | `post_elitea_core_version_instruction_patch` |
| `elitea_core/api/v2/application_relation.py::PromptLibAPI.patch` | `models.applications.application_relation.patch` | `patch_elitea_core_application_relation` |

The current source paths above are relative to
`pylon_main/plugins/elitea_core`. Each source operation has `mcp_tool=True` and
the `elitea_core/applications` tag. Main publishes exactly those eight
operations.

The current Go application list supports text search, classic-versus-pipeline
selection, limit, and offset. The internal schema publishes only those filters.
It does not advertise the current Python endpoint's author, status, tag, like,
ID, or sort filters because Main cannot honor them yet. Adding those filters is
a shared application-list parity slice; silently accepting them here would make
the MCP result incorrect.

## Skills category

The skills category is available at
`/app/{projectID}/mcp/elitea_core/skills`. Main publishes six explicit
operations. It excludes delete, version creation, and LLM draft generation.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `elitea_core/api/v2/skills.py::PromptLibAPI.get` | `models.applications.skills.list` | `get_elitea_core_skills` |
| `elitea_core/api/v2/skills.py::PromptLibAPI.post` | `models.applications.skills.create` | `post_elitea_core_skills` |
| `elitea_core/api/v2/skill.py::PromptLibAPI.get` | `models.applications.skills.details` | `get_elitea_core_skill` |
| `elitea_core/api/v2/skill.py::PromptLibAPI.put` | `models.applications.skills.update` | `put_elitea_core_skill` |
| `elitea_core/api/v2/skill.py::PromptLibAPI.patch` | `models.applications.skills.update` | `patch_elitea_core_skill` |
| `elitea_core/api/v2/application_skills.py::PromptLibAPI.get` | `models.applications.applications.details` | `get_elitea_core_application_skills` |

Create accepts exactly one initial `base` version. Names follow the current
lowercase letter, digit, and internal-hyphen grammar. Names containing
`claude` or `anthropic` are refused.

Update accepts current nested version input. Main reads the current skill and
merges supplied fields before writing. This preserves omitted metadata and
base-version content. A foreign version ID cannot select another skill's
version.

Relation updates use an application version ID. Attach requires the exact
skill version ID. Detach addresses the existing relation without needing that
version ID. Existing repository guards retain the five-skill limit, ownership,
draft-state, duplicate, and embedded-version rules.

Main currently stores only tag names on this path. MCP results normalize those
names into `{name}` objects. The schema does not advertise tag IDs or metadata
that Main cannot preserve.

The Python list also supports tags, author, status, IDs, limit, and offset.
Main currently supports text, page, page size, and sorting. The narrower MCP
schema advertises only filters that Main honors.

## Toolkits category

The toolkit builder category is available at
`/app/{projectID}/mcp/elitea_core/toolkits`. Main currently publishes five of
the six operations marked `mcp_tool=True` by the current platform.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `elitea_core/api/v2/toolkits.py::PromptLibAPI.get` | `models.applications.toolkits.details` | `get_elitea_core_toolkits` |
| `elitea_core/api/v2/tools.py::PromptLibAPI.get` | `models.applications.tools.list` | `get_elitea_core_tools` |
| `elitea_core/api/v2/tools.py::PromptLibAPI.post` | `models.applications.tools.create` | `post_elitea_core_tools` |
| `elitea_core/api/v2/tool.py::PromptLibAPI.put` | `models.applications.tool.update` | `put_elitea_core_tool` |
| `elitea_core/api/v2/tool.py::PromptLibAPI.patch` | `models.applications.tool.patch` | `patch_elitea_core_tool` |

The executor reuses the same fully composed toolkit handler as the REST/UI
routes. Dynamic prebuilt-MCP schemas, settings definitions, secret sealing,
credential-reference validation, and guardrail policy therefore cannot drift
between the two entry points. Actor identity is derived from the authenticated
MCP principal. Project, author, owner, and object identities supplied inside
the model arguments cannot replace the URL and claim identities.

The Main instance list currently implements only pagination and name ordering.
The MCP schema consequently exposes only bounded `limit` and `offset`; it does
not pretend to support the current Python endpoint's query, sort, type, MCP,
application, author, artifact, or ID filters.

Toolkit-to-agent mutation preserves the meaningful distinction between an
absent `selected_tools` value and an explicitly empty array. The first attaches
without replacing a saved selection; the second records that the user selected
no tools. Published and embedded agent versions retain the REST handler's
mutation guard.

`get_elitea_core_toolkit_available_tools` is deliberately not published yet.
The current Python operation expands the exact saved toolkit configuration for
the current user and asks the SDK/provider for its live tool catalogue. Main's
present `AvailableTools` repository query instead reads toolkit attachments
from `entity_tool_mapping`; `DiscoverTools` reads stored rows by type. Neither
is live per-instance discovery, so exposing either through this name would
return plausible but incorrect data. The operation remains closed until Main
composes live discovery with claim-scoped settings expansion.
The separate [toolkit discovery ledger](toolkit-discovery.md) records the existing shared command and missing Rust dispatcher support.
Rust runtime tool enumeration and binding already work. They do not require this standalone discovery command.

## Configurations category

The configurations category is available at
`/app/{projectID}/mcp/configurations`. Main publishes all eight current operations when its typed configuration services are composed.
Without those services, it publishes only the original five operations.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `configurations/api/v2/available.py::API.get` | see authorization note below | `get_configurations_available` |
| `configurations/api/v2/configurations.py::API.get` | `configurations.configurations.list` | `get_configurations_configurations` |
| `configurations/api/v2/configurations.py::API.post` | `configurations.configuration.create` | `post_configurations_configurations` |
| `configurations/api/v2/configuration.py::API.get` | `configurations.configuration.details` | `get_configurations_configuration` |
| `configurations/api/v2/configuration.py::API.put` | `configurations.configuration.update` | `put_configurations_configuration` |
| `configurations/api/v2/types.py::API.get` | `configurations.configurations.list` | `get_configurations_types` |
| `configurations/api/v2/models.py::API.get` | `configurations.configurations.list` | `get_configurations_models` |
| `configurations/api/v2/models.py::API.post` | `configurations.configuration.update` | `post_configurations_models` |

The current source paths above are relative to
`pylon_main/plugins/configurations`. The available catalogue REST endpoint is
authenticated but carries no project permission. Internal MCP is itself a
project-scoped execution surface and the catalogue exists to construct project
configuration writes, so Main deliberately strengthens that one tool to
`configurations.configurations.list`. It does not add a weaker execution path
through MCP for a caller who cannot list configurations in the project.

The executor reuses the exact fully composed configuration handler used by the
REST and UI routes. Dynamic configuration schemas, public-project shared rows,
schema-declared password sealing, stored-reference handling, provider
admission, and self-reference guards consequently stay on one implementation
path. The authenticated MCP actor becomes `author_id`; project, author, source,
status, section, type-on-update, and path identities supplied in tool arguments
cannot replace server-owned values.

The shared handler also preserves the current platform's tracing-credential
containment. Configuration types whose registry metadata includes the
`tracing` category (plus the legacy `langfuse` fallback) are visible and
mutable only to project `admin`, `super_admin`, or `system` roles, or to a
caller inside that user's `project_user_<userID>` personal project. Those are
independent signals: missing role-assignment data cannot revoke access to the
caller's own personal project. Catalogue, detail, create, and update paths fail
closed on an unavailable role lookup.
The inventory list remains unfiltered because agent runtime composition relies
on it, matching the current platform behavior.

List supports only the filters Main implements: repeated type and section,
label query, bounded project and shared pagination, shared inclusion, and the
Main sort whitelist. The current Python `ids` filter is not advertised because
the composed Main list handler cannot honor it. Create derives section from the
current registry and accepts the current OpenAPI create fields. Update is
partial and publishes only `elitea_title`, `label`, `data`, `meta`, and
`shared`; it cannot replace configuration type or section.

The three typed operations reuse `configurations/tool_handler.go`.
This adapter invokes the existing typed REST handlers with the same readers and vault writer.
It does not invoke Main's reduced compatibility handlers or add SQL.
`cmd/elitea-main/main.go` supplies the current configuration composition to REST and MCP through `internal/api/router.go`.

Types preserve the default `credentials` section and explicit empty-section selection.
Models preserve section normalization, shared-model selection, and default-model metadata.
Main supplies the public project identity, not the tool arguments.
Default-model updates write only to the endpoint project's vault.
`target_project_id` identifies the model's source project, not the vault to update.
The shared writer preserves omitted, empty, and null section behavior.
MCP bounds strings and requires a non-empty model name and positive source project ID.

MCP uses the same list and update permissions as Main's typed REST routes.
The legacy Python endpoints lack these explicit project checks.
Preserving that weaker authorization is not a compatibility requirement.

## Notifications category

The notifications category is available at
`/app/{projectID}/mcp/notifications`. Main publishes exactly the three current
operations marked `mcp_tool=True`; bulk mutation and deletion remain REST-only.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `notifications/api/v2/notifications.py::PromptLibAPI.get` | `models.notifications.notifications.list` | `get_notifications_notifications` |
| `notifications/api/v2/notification.py::PromptLibAPI.get` | `models.notifications.notification.details` | `get_notifications_notification` |
| `notifications/api/v2/notification.py::PromptLibAPI.put` | `models.notifications.notification.update` | `put_notifications_notification` |

The current source paths above are relative to `pylon_main/plugins`. The list
contract is global user-scoped: the endpoint project authorizes the call but
does not filter notification rows. Main preserves that behavior, so a
notification whose payload names a different project can still appear in its
owner's list. The authenticated MCP principal is the only source of `user_id`.

Main deliberately closes an authorization hole in the current single-item
SQLAlchemy handlers. Their detail and mark-seen queries filter only by
notification ID. The existing Go notification store filters by both ID and
authenticated user for every read or write, and Internal MCP reuses that same
store. A caller therefore cannot read or mark another user's notification even
when numeric IDs are known.

The fixed queries are source-derived rather than a second handwritten MCP data
path. `internal/db/queries/notifications.sql` translates the current
SQLAlchemy filters into sqlc-generated operations: user scope, unseen and
event filters, escaped case-insensitive `meta.message` word matching, the
current sort fields, pagination, detail, and mark-seen. Count and row retrieval
are separate so `only_total` never loads rows. Mark-seen uses one idempotent
update-plus-fallback query: an unseen row changes once, while a row already
seen is returned without rewriting its timestamp.

The executor reuses the current REST handler's normalization and response DTO.
MCP adds strict bounds before the handler: maximum 1,000 rows, 32 search terms,
256 bytes per term, a bounded event type, and an explicit sort whitelist. It
does not forward unknown fields or publish bulk update, single delete, or bulk
delete.

## Project context builder category

The project-context builder category is available at
`/app/{projectID}/mcp/elitea_core/project_context`. Main publishes exactly the
three current operations marked `mcp_tool=True`.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `elitea_core/api/v2/project_context.py::PromptLibAPI.get` | `models.project_context.view` | `get_prompt_lib_project-context` |
| `elitea_core/api/v2/project_context.py::PromptLibAPI.put` | `models.project_context.edit` | `put_prompt_lib_project-context` |
| `elitea_core/api/v2/project_context.py::PromptLibAPI.delete` | `models.project_context.edit` | `delete_prompt_lib_project-context` |

Main reuses one `eliteacore.Handler` for REST/UI and Internal MCP. GET returns
the current five-field detail envelope; an absent row is
`{id:null, content:"", enabled:true, activation_description:null,
updated_at:null}`. PUT creates the deterministic `project_context_<projectID>`
configuration when absent. Content and enabled take their current replacement
defaults on every PUT. An omitted activation description preserves an existing
non-empty value, while explicit null or normalized blank removes it. Content
and activation descriptions are bounded to 2,500 and 300 Unicode characters.
DELETE returns 404 for an absent row and 204 after deletion.

The URL project is authoritative and the fixed schema does not expose row,
author, source, section, or status controls. Main stores the current RPC-create
identity: `Project Context`, `project_context`, `project_settings`, system
source, and valid status. Tenant schema identifiers remain validated dynamic
SQL because PostgreSQL cannot parameterize identifiers; all values are bound.

This category is the CRUD builder used by an Elitea agent to inspect and edit
project context. It is not the runtime progressive-disclosure reader that
decides when full context enters model instructions, and it does not publish
`generate_project_context_draft`. Draft generation is a separate model-backed
use case and remains closed until Main owns its model, service-prompt, failure,
and validation contract.

## Secrets category

The project-secret category is available at
`/app/{projectID}/mcp/secrets`. Main publishes exactly the three current
operations marked `mcp_tool=True`.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `secrets/api/v2/secrets.py::ProjectAPI.get` | `configuration.secrets.secret.list` | `get_secrets_secrets` |
| `secrets/api/v2/secrets.py::ProjectAPI.post` | `configuration.secrets.secret.create` | `post_secrets_secrets` |
| `secrets/api/v2/secret.py::ProjectAPI.put` | `configuration.secrets.secret.edit` | `put_secrets_secret` |

The current source paths above are relative to `pylon_main/plugins`. The
catalogue intentionally excludes plaintext `Get Secret`, deletion, hiding,
bulk replacement, and every administration-vault operation. List returns only
secret names, `{{secret.NAME}}` placeholders, and the existing `is_default`
flag. Create and update return the same safe metadata and never the supplied
value.

The executor reuses the exact `secrets.Handler` instance used by REST/UI and by
prebuilt-MCP credential materialization. Vault encryption, malformed-master-key
failure, absent-versus-unreadable handling, duplicate checks, and
collision-safe writes therefore remain one implementation path. The endpoint
project and authenticated actor are server-owned. Secret names are bounded to
128 ASCII letters, digits, or underscores before handler invocation, matching
the placeholder resolver and Main vault writer.

Update is value rotation, not rename. Although the current OpenAPI description
says “name and/or value”, current pylon overwrites the body `name` with the
path `secret` before validation. The fixed MCP schema consequently exposes the
existing `secret` name and replacement `value`, but no misleading rename
field. Unknown arguments never cross into the handler.

Main does not currently read pylon's `elitea_core.default_secret_keys` or
`ignore_default_secret_api` plugin configuration. Its existing REST response
therefore reports `is_default:false` and does not implement conditional
default-name suppression. The checked current deployment configuration defines
the default-key list but does not enable `ignore_default_secret_api`, so this
does not broaden its active list result. Importing those two settings into
Main remains an explicit REST-and-MCP parity gate; the MCP layer does not
invent a second configuration source.

## Closed internal categories

Main refuses an internal category until each published operation has a shared,
policy-complete implementation. It does not reinterpret a category as the
project's external agent or toolkit catalogue.

### Discovery

The current platform opts two operations into `elitea_core/discovery`.

| Current platform evidence | Permission | Current MCP name |
| --- | --- | --- |
| `elitea_core/api/v2/tags.py::PromptLibAPI.get` | `models.promptlib_shared.tags.list` | `get_elitea_core_tags` |
| `elitea_core/api/v2/search_options.py::PromptLibAPI.get` | `models.promptlib_shared.search` | `get_elitea_core_search_options` |

This category remains closed. Main's tag handler returns every stored tag row.
The current operation returns only tags used by applications, pipelines, or
skills. It also adds relation counts and applies entity visibility rules.

Main's Search Options handler returns only tag names and an empty collection
list. The current operation accepts selected application, pipeline, toolkit,
credential, and skill entities. It returns each selected entity's search
options and merges shared tag and collection results. Publishing either Main
answer under the current MCP name would return plausible but incorrect data.

### Chat

The current platform opts 13 operations into `elitea_core/chat`. They cover
conversation list and creation, participant read and removal, folder list,
folder creation and update, conversation read and update, participant
configuration, participant addition, message send, and durable continuation.

This category remains closed as one security boundary. Main's present REST
handlers do not yet preserve the current actor-visibility contract. The list
and folder queries can read all project conversations. Folder creation stores
owner `1` instead of the authenticated actor. Conversation creation drops
participants, privacy, source, metadata, instructions, and the required user
and dummy participants. Conversation update preserves only name and folder.

Message send and continuation also cross the durable runtime boundary. The
current MCP operations can wait for bounded results and return message groups.
Main currently admits execution and exposes a separate event stream. An
internal MCP executor must define bounded wait, cancellation, HITL,
authorization resume, and terminal result projection before it publishes
these operations.

Fix the shared REST and repository behavior first. Then reuse those exact
handlers or use cases from Internal MCP. Do not add an MCP-only ownership rule
or a second execution path.

### Analytics and artifacts

The analytics category remains closed until Main's persisted execution and
usage projections match the current aggregate and detail operations. Empty or
synthetic aggregates are not valid MCP results.

The artifacts category remains closed until artifact grants, object authority,
credential handling, and storage-provider behavior are complete. Internal MCP
must not bypass those gates through Main's compatibility routes.

## Main ownership

Main owns listing, execution, authorization, project clamping, and mutation.
The fixed catalogues are in
`internal/api/v2/mcp/internal_{applications,skills,toolkits,configurations,notifications,project_context,secrets}_catalog.go`.
Application and skill executors reuse Main business repositories. The toolkit
executor deliberately reuses the fully composed REST handler so every toolkit
mutation crosses the same policy and secret boundaries. Application version
updates use a dedicated PostgreSQL transaction.

The version read returns `instructions_sha256`. A settings update cannot change
instructions and creates a full backup before mutation. The backup copies the
version fields, variables, tags, tool mappings, skill mappings, and the legacy
application-tool rows when that relation exists.

The instruction-patch operation requires the hash from a fresh read. A fragment
must match exactly once unless `replace_all` is true. A stale hash, ambiguous
match, empty replacement, or no-op changes nothing. Backup creation and the
instruction update commit in one transaction.

Application detail resolves the configured default version first, then the
`base` version, then the newest existing version. A supplied version name is an
exact lookup. This matches the current platform instead of inheriting the Go
UI handler's first-row projection.

The project ID in the URL is authoritative. Every call resolves its exact
permission before dispatch. Infrastructure failures and invalid provider bodies
are redacted. Expected business failures remain MCP tool errors so the calling
model can respond without treating the MCP transport as broken.

## Runtime materialization

An operator can define this internal endpoint in the prebuilt MCP catalogue by
using the reserved `{project_id}` and `{personal_token}` placeholders. These
placeholders are runtime-owned and never appear as project-editable toolkit
fields.

Main injects the current project ID. It issues a current-user PAT only when the
materialized URL has Main's configured origin and an
`/app/{project}/mcp/...` path. It never sends that PAT to an external origin.
The signed worker input contains the final URL and header, not the source
template or a reusable parameter map.

Rust needs no internal-application implementation. Its standard configured MCP
path lists and calls the Main-owned tools after claim-time materialization. This
keeps application business rules in Main and removes the former
Core-to-Indexer-to-SDK execution split.

## Verification

Main unit tests pin all seven catalogues, wire schemas, permissions, project
clamps, runtime-independent dispatch, business-error shapes, and redacted
infrastructure failures. Validation tests cover application and skill inputs.

PostgreSQL integration tests prove atomic backup and instruction patching,
stale-hash rollback, settings updates, trigger preservation, author changes,
variables, tags, and rejection of direct instruction edits. Shared application
integration tests prove that create paths persist pipeline settings and tags.

Skill executor tests prove bounded filtering, nested create input, current name
rules, partial-update merging, exact version ownership, and relation keys. A
PostgreSQL lifecycle test proves create, update, attach, list, and detach.

Toolkit executor tests prove bounded pagination, authenticated actor ownership,
write-field allowlists, path-authoritative identities, input refusal before
mutation, and absent-versus-empty selected tool semantics. Its PostgreSQL
lifecycle test proves create, partial update, attach, explicit empty selection,
list, and detach. Router composition tests pin that REST and Internal MCP share
one policy-complete toolkit handler.

Configuration tests pin the five-operation base catalogue, private wire fields,
permissions, project clamping, redacted failures, bounded repeated filters,
sort and pagination translation, title normalization, create/update field
allowlists, and refusal before handler invocation. Its isolated PostgreSQL
lifecycle proof creates a registry-backed configuration with the authenticated
actor, derives the correct section, partially updates it without erasing data,
and reads and lists the persisted row. A second PostgreSQL proof pins tracing
containment for ordinary project members, project admins, and the actor's own
personal project.

`internal_configurations_typed_test.go` pins the composed eight-operation catalogue and refuses incomplete composition.
Tests exercise all three operations through MCP permission and project checks.
They preserve types, model defaults, shared selection, and endpoint-owned vault writes.
They also cover cancellation, safe failures, invalid input refusal, and section fuzzing.
The isolated PostgreSQL test composes real configuration repositories, model services, and the encrypted vault reader and writer.
It proves shared-model filtering, absent-vault refusal, persisted defaults, and endpoint-owned vault writes.
Project provisioning remains responsible for creating a missing vault.
These tests do not prove deployed chat-driven entity creation.

The 2026-09-08 checks pass:

- Seventeen focused configuration tests, including three PostgreSQL integration tests, with zero skips.
- MCP, configuration, router, and Main command package tests without database credentials.
  Database-dependent cases skip in that broader run.
  Only the three focused database cases receive new PostgreSQL proof in this slice.
- Focused MCP and typed-handler race tests, plus bounded section fuzzing.
- `go vet` for the four changed package groups and `git diff --check`.

The database tests use isolated fixture databases on the rehearsal PostgreSQL service.
They remove only those fixture databases. They do not change deployed chats or credentials.

Notification tests pin the three-operation catalogue, omission of bulk and
delete operations, exact permissions, project clamping, authenticated actor
projection, bounded filter translation, current defaults, path-authoritative
notification identity, refusal before handler invocation, nil-executor
failure, and redacted infrastructure errors. An isolated PostgreSQL lifecycle
proof covers global per-user listing across project metadata, search and event
filters, `only_total`, cross-user detail refusal, persisted mark-seen, and an
idempotent second mark that does not change the row timestamp.

Project-context tests pin the three-operation catalogue, current names and
permissions, private discriminator omission, project clamping, authenticated
actor propagation, update-field allowlisting, null and size validation,
normalization, default response shape, and redacted failures. Its isolated
PostgreSQL lifecycle proves absent defaults, deterministic creation metadata,
activation preservation and removal, replacement defaults, read-after-write,
DELETE, repeated-delete 404, and post-delete defaults. OpenAPI route
conformance includes all three REST methods and the five-field response shape.

Secret tests pin the three-operation catalogue, omission of plaintext and
destructive operations, exact permissions, project clamping, authenticated
actor propagation, path-authoritative update identity, request-field
allowlisting, name/schema invariants, nil-executor behavior, and redacted
infrastructure failures. Its isolated PostgreSQL lifecycle proof covers empty
list, create, safe metadata response, list without values, rotation without
rename, direct vault verification, and encrypted-at-rest bytes containing
neither supplied plaintext value. Router composition tests pin one handler for
REST, Internal MCP, and prebuilt-MCP materialization.

Prebuilt materialization tests prove that runtime placeholders cannot become
stored toolkit parameters, internal PATs are restricted to the configured Main
origin, and external origins receive no delegated platform credential. The
existing Rust configured-MCP tests remain the worker-side protocol proof.

## Remaining gates

Other internal categories must be mapped operation by operation. No generic
OpenAPI self-dispatch is allowed.

The discovery category remains closed until Main implements relation-aware tag
listing and entity-specific Search Options. Protocol tests pin that refusal.

The chat category remains closed until shared Main REST handlers enforce actor
visibility and preserve conversation, folder, participant, and runtime
semantics. Do not publish a metadata-only approximation under current names.

The sixth toolkit operation, live available-tool discovery, remains an explicit
Main parity gate. It must use the exact saved instance and current actor's
expanded configuration. The existing attachment/type SQL is not an acceptable
fallback.

Skill draft generation remains gated because Main does not yet own its model
operation. Multi-version skills, rich tag metadata, extended list filters, and
actor attribution also remain Main parity gates.

Project-context AI draft generation and runtime progressive-disclosure loading
remain separate gates. Completing builder CRUD does not claim either behavior.

Default-secret metadata and conditional default-name suppression remain a
shared Main secrets parity gate. The current MCP category cannot fix them in
isolation because REST and MCP deliberately share the same vault handler.

The Python configuration-list `ids` filter remains a Main parity gate.
The three typed configuration operations now reuse the production services.
Deployed chat-driven use remains a verification gap in [the test register](../testing-gaps.md).

The extended application-list filters and version-copy skill option remain
explicit parity gates. Main must own their durable data and validation before
the internal MCP schema can publish them.

External Elitea-as-MCP stays separate from this internal category. Its
catalogue, direct read-only toolkit runtime, live proof, and remaining gates are
recorded in [`external-elitea-mcp.md`](external-elitea-mcp.md).

Main and the UI own OAuth and DCR callback behavior. Rust consumes only
claim-scoped tokens and safe authorization metadata.

The implemented proxy and its remaining live gates are mapped in
[`delegated-oauth-dcr.md`](delegated-oauth-dcr.md).
