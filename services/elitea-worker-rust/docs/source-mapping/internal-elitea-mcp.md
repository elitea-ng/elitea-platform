# Internal Elitea MCP source mapping

Status: the applications category is implemented; the wider internal builder
family and external Elitea-as-MCP publishing remain partial.

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

## Main ownership

Main owns listing, execution, authorization, project clamping, and mutation.
The fixed catalogue is in
`internal/api/v2/mcp/internal_applications_catalog.go`. The in-process executor
reuses existing application handlers when their contracts match. It uses a
dedicated PostgreSQL transaction for internal version updates.

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

Main unit tests pin the exact eight tool names, wire schemas, permissions,
project clamp, runtime-independent dispatch, business-error shape, and redacted
infrastructure failures. Validation tests cover current agent types, variables,
tags, and conversation starters.

PostgreSQL integration tests prove atomic backup and instruction patching,
stale-hash rollback, settings updates, trigger preservation, author changes,
variables, tags, and rejection of direct instruction edits. Shared application
integration tests prove that create paths persist pipeline settings and tags.

Prebuilt materialization tests prove that runtime placeholders cannot become
stored toolkit parameters, internal PATs are restricted to the configured Main
origin, and external origins receive no delegated platform credential. The
existing Rust configured-MCP tests remain the worker-side protocol proof.

## Remaining gates

Other internal categories must be mapped operation by operation. No generic
OpenAPI self-dispatch is allowed.

The extended application-list filters and version-copy skill option remain
explicit parity gates. Main must own their durable data and validation before
the internal MCP schema can publish them.

External toolkit execution through Elitea-as-MCP remains separate from this
internal category. External agent execution exists behind the runtime gate;
external toolkit execution and agent or pipeline exposure controls still need
their own end-to-end proof.

OAuth and DCR callback ownership remains in Main and the UI. Rust consumes only
claim-scoped tokens and sanitized authorization metadata.
