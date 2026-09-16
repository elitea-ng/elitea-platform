# Internal MCP configuration ID filtering

## Source and target mapping

| Current platform source | New platform owner | Contract |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/configurations/api/v2/configurations.py`, `get` | Main `internal/api/v2/configurations/read.go` and `handler.go` | Forward the optional comma-separated `ids` filter. |
| `projects/centry/pylon_main/plugins/configurations/utils.py`, `parse_ids_filter` | Main `internal/application/configurations/ids.go` | Bound IDs and treat empty input as no filter. |
| Same source, `get_configurations` project and shared queries | Main `internal/application/configurations/crud.go`, `internal/db/queries/configurations.sql`, `internal/infra/db/repos/configurations.go` | Apply IDs before count and pagination in each tenant query. |
| Current configuration endpoint MCP registration | Main `internal/api/v2/mcp/internal_configurations_catalog.go` and `internal_configurations_execute.go` | Expose and forward the same filter through internal MCP. |

All Main paths are relative to `services/elitea-main`.
Rust uses the Main-owned internal MCP surface. Rust does not own configuration persistence or duplicate its SQL filters.

## Implementation history

The September 9, 2026 follow-up adds the missing filter to both Main read paths and internal MCP.
The application service validates positive signed 32-bit IDs and removes duplicates in first occurrence order.
The maximum input count is 100 before duplicate removal. HTTP accepts one comma-separated parameter.
Empty text, an omitted filter, and an empty application slice mean no filter.
Repeated HTTP parameters, invalid tokens, nonpositive IDs, overflow, and oversized lists fail before database access.
The MCP schema accepts a bounded string and uses the same validation.

The current source silently drops malformed tokens and truncates oversized input. Main rejects these inputs to prevent accidental broad queries.
The source also raises page limits to the ID count. Main preserves existing explicit and default pagination limits.
ID filtering retains type, section, label search, sorting, shared visibility, and tenant boundaries.
Shared queries retain their existing omission of label search. Both shared counts and shared pages receive IDs.
SQL arguments use bound integer arrays. Generated query bindings come from `sqlc generate`.

## Verification

Application tests cover empty input, duplicate removal, positive bounds, malformed tokens, and the maximum count.
Application and repository tests check forwarding to project and shared counts and pages.
HTTP tests check parameter parsing, bound SQL placeholders, empty semantics, and preserved page limits.
MCP tests check forwarding and invalid argument rejection.
The PostgreSQL contract checks filtered counts, filtered pages, unknown IDs, pagination, and tenant identity.
The PostgreSQL contract requires the repository integration database environment. A skipped contract is not live database proof.

The application and configuration HTTP package suites pass after this change.
Focused internal MCP configuration tests pass.
The PostgreSQL configuration parity test passes against a private fixture database on September 9, 2026.
This verifies project and shared SQL filtering; it does not prove a deployed Rust-to-Main MCP session.
