# Internal MCP integration checkpoint

## Scope

The September 11 checkpoint integrates Main's internal MCP handlers and shared product repositories.
Rust remains the tool caller. Main owns product authorization, database reads, and persisted changes.
Existing product tables retain their schema. This checkpoint adds no migration.

## Source mappings

- [Chat authority](internal-mcp-chat-authority.md) maps current Core chat operations, personas, folders, and participant settings.
- [Drafting](internal-mcp-drafting.md) maps skill and project-context draft generation through the shared model service.
- [Skill contracts](internal-mcp-skill-contracts.md) maps version selection, partial updates, authorship, and folder visibility.
- [Entity discovery](internal-mcp-entity-discovery.md) maps tag counts, search options, pagination, and visible entity filters.
- [External MCP completion](external-mcp-completion.md) maps dynamic schemas and terminal response classification.

Application instruction updates and application field updates remain separate operations.
Drafting returns proposed content without saving it.
Chat pipeline creation remains deferred. Existing pipeline execution remains in the acceptance scope.

## Implementation boundary

The router supplies shared draft, conversation, folder, toolkit, and configuration handlers to internal MCP.
The endpoint project is authoritative. Model-facing schemas omit redundant project input.
The fixed internal builder registry includes chat and discovery categories.
Runtime assembly consumes that registry in a separate pending integration slice.

Skill reads select the requested version and enforce folder visibility before returning instructions.
Skill updates preserve omitted metadata and reject versions owned by another skill.
Chat operations use the authenticated actor and preserve persona-derived defaults.
Search options and tags use the same service for REST and MCP.

External MCP identifies output-limit pauses separately from completed responses.
A wait timeout reports an unobserved final answer. It does not claim that the execution still runs.

## Verification

The isolated candidate passes 738 test events across seven API and discovery packages with PostgreSQL enabled.
The selected repository tests pass 46 test events. Neither run skips tests.
Database helpers create and remove isolated databases. They do not alter rehearsal application data.
Go vet passes for the seven packages and the Main command package.

These checks prove component and database contracts. They do not close the remaining deployed browser acceptance gates.
Runtime assembly, remaining Rust integration, cancellation, replacement, and full external MCP acceptance remain open.
Context-compaction runtime work remains reserved for point 4.
