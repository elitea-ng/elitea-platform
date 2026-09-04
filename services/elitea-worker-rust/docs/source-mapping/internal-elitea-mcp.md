# Internal Elitea MCP source mapping

Status: the applications and skills categories, five Main-owned toolkit
builder operations, and five Main-owned configuration operations are
implemented. Live per-instance toolkit discovery, typed model configuration
operations, the wider internal builder family, and external Elitea-as-MCP
publishing remain partial.

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
composes built-in pinned schemas, OpenAPI operation parsing, and remote MCP
discovery with claim-scoped settings expansion.

## Configurations category

The configurations category is available at
`/app/{projectID}/mcp/configurations`. Main currently publishes five of the
eight operations marked `mcp_tool=True` by the current platform.

| Current platform evidence | Permission | Main tool |
| --- | --- | --- |
| `configurations/api/v2/available.py::API.get` | see authorization note below | `get_configurations_available` |
| `configurations/api/v2/configurations.py::API.get` | `configurations.configurations.list` | `get_configurations_configurations` |
| `configurations/api/v2/configurations.py::API.post` | `configurations.configuration.create` | `post_configurations_configurations` |
| `configurations/api/v2/configuration.py::API.get` | `configurations.configuration.details` | `get_configurations_configuration` |
| `configurations/api/v2/configuration.py::API.put` | `configurations.configuration.update` | `put_configurations_configuration` |

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

`get_configurations_types`, `get_configurations_models`, and
`post_configurations_models` remain deliberately closed. Main's compatibility
types handler ignores the requested section, its compatibility model list does
not implement section/shared/default semantics, and its compatibility default
mutation is an explicit 503. Production REST uses separate typed services for
those paths. Internal MCP must reuse those same services before it can publish
the names; returning the compatibility answers would be plausible but wrong.

## Main ownership

Main owns listing, execution, authorization, project clamping, and mutation.
The fixed catalogues are in
`internal/api/v2/mcp/internal_{applications,skills,toolkits,configurations}_catalog.go`.
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

Main unit tests pin all four catalogues, wire schemas, permissions, project clamps,
runtime-independent dispatch, business-error shapes, and redacted
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

Configuration tests pin the five-operation catalogue, private wire fields,
permissions, project clamping, redacted failures, bounded repeated filters,
sort and pagination translation, title normalization, create/update field
allowlists, and refusal before handler invocation. Its isolated PostgreSQL
lifecycle proof creates a registry-backed configuration with the authenticated
actor, derives the correct section, partially updates it without erasing data,
and reads and lists the persisted row. A second PostgreSQL proof pins tracing
containment for ordinary project members, project admins, and the actor's own
personal project.

Prebuilt materialization tests prove that runtime placeholders cannot become
stored toolkit parameters, internal PATs are restricted to the configured Main
origin, and external origins receive no delegated platform credential. The
existing Rust configured-MCP tests remain the worker-side protocol proof.

## Remaining gates

Other internal categories must be mapped operation by operation. No generic
OpenAPI self-dispatch is allowed.

The sixth toolkit operation, live available-tool discovery, remains an explicit
Main parity gate. It must use the exact saved instance and current actor's
expanded configuration. The existing attachment/type SQL is not an acceptable
fallback.

Skill draft generation remains gated because Main does not yet own its model
operation. Multi-version skills, rich tag metadata, extended list filters, and
actor attribution also remain Main parity gates.

The three typed configuration operations and the Python list `ids` filter
remain explicit Main parity gates. They must reuse the production typed model
catalogue/default services rather than the reduced compatibility handlers.

The extended application-list filters and version-copy skill option remain
explicit parity gates. Main must own their durable data and validation before
the internal MCP schema can publish them.

External toolkit execution through Elitea-as-MCP remains separate from this
internal category. External agent execution exists behind the runtime gate;
external toolkit execution and agent or pipeline exposure controls still need
their own end-to-end proof.

OAuth and DCR callback ownership remains in Main and the UI. Rust consumes only
claim-scoped tokens and sanitized authorization metadata.
