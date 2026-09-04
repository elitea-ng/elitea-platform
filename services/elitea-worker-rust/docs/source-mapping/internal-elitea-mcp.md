# Internal Elitea MCP source mapping

Status: the applications and skills categories are implemented. The wider
internal builder family and external Elitea-as-MCP publishing remain partial.

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

## Main ownership

Main owns listing, execution, authorization, project clamping, and mutation.
The fixed catalogues are in
`internal/api/v2/mcp/internal_{applications,skills}_catalog.go`. Their
in-process executors reuse Main business repositories. Application version
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

Main unit tests pin both catalogues, wire schemas, permissions, project clamps,
runtime-independent dispatch, business-error shapes, and redacted
infrastructure failures. Validation tests cover application and skill inputs.

PostgreSQL integration tests prove atomic backup and instruction patching,
stale-hash rollback, settings updates, trigger preservation, author changes,
variables, tags, and rejection of direct instruction edits. Shared application
integration tests prove that create paths persist pipeline settings and tags.

Skill executor tests prove bounded filtering, nested create input, current name
rules, partial-update merging, exact version ownership, and relation keys. A
PostgreSQL lifecycle test proves create, update, attach, list, and detach.

Prebuilt materialization tests prove that runtime placeholders cannot become
stored toolkit parameters, internal PATs are restricted to the configured Main
origin, and external origins receive no delegated platform credential. The
existing Rust configured-MCP tests remain the worker-side protocol proof.

## Remaining gates

Other internal categories must be mapped operation by operation. No generic
OpenAPI self-dispatch is allowed.

Skill draft generation remains gated because Main does not yet own its model
operation. Multi-version skills, rich tag metadata, extended list filters, and
actor attribution also remain Main parity gates.

The extended application-list filters and version-copy skill option remain
explicit parity gates. Main must own their durable data and validation before
the internal MCP schema can publish them.

External toolkit execution through Elitea-as-MCP remains separate from this
internal category. External agent execution exists behind the runtime gate;
external toolkit execution and agent or pipeline exposure controls still need
their own end-to-end proof.

OAuth and DCR callback ownership remains in Main and the UI. Rust consumes only
claim-scoped tokens and sanitized authorization metadata.
