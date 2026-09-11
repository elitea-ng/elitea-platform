# Internal MCP entity discovery

## Scope and authority

Main owns tag counts, search option reads, and folder visibility.
Rust consumes the fixed MCP descriptors and does not query product tables.
The MCP category is `elitea_core/discovery`.

| Tool | Permission | Main owner |
| --- | --- | --- |
| `get_prompt_lib_tags` | `models.promptlib_shared.tags.list` | `internal/application/entitydiscovery.Service.Tags` |
| `get_prompt_lib_search_options` | `models.promptlib_shared.search` | `internal/application/entitydiscovery.Service.SearchOptions` |

Both operations use `mcp.Handler.callInternalTool` before database access.
The endpoint project remains authoritative.
A conflicting argument fails before permission resolution.
The resolved owning user controls folder visibility.
Missing identity fails, including deployments without folder overrides.

REST uses the same service through `TagsRepo.ListFiltered` and `eliteacore.Handler.SearchOptions`.
No Python RPC or REST bridge participates in these reads.

## Current source mapping

The Core source revision is `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
The Configurations source revision is `0f64774af6809b2b14b17f2c027fdd648e1e4556`.
Paths below are relative to each source plugin repository.

| Current source | Current behavior | Main implementation |
| --- | --- | --- |
| Core `api/v2/tags.py:9-30` | MCP opt-in and tag-list permission | `mcp/internal_entity_discovery.go` |
| Core `utils/tags.py:59-160` | Entity filters, distinct entity counts, and count ordering | `entitydiscovery/service.go`, `tagPage` |
| Core `utils/tags.py:192-232` | Any pipeline version classifies the entire application as a pipeline | `entitydiscovery/service.go`, `eligibility` |
| Core `utils/tags.py:234-263` | Skill author and text filters; skill counts | `entitydiscovery/service.go`, `eligibility` |
| Core `utils/tags.py:269-298` | Ordered coverage union excludes unattached tags | `entitydiscovery.Service.Tags` |
| Core `api/v2/search_options.py:12-35,60-129` | Seven singular sections and selected entity reads | `entitydiscovery.Service.SearchOptions` |
| Core `utils/searches.py:17-40,43-134` | Search, version filters, and actor folder exclusions | `entitydiscovery/service.go`, `searchPage` |
| Core `utils/searches.py:137-200` | Related tag options and prefixed pagination | `entitydiscovery.Service.SearchOptions` |
| Core `utils/utils.py:154-188` | All selected tags can occur across an entity's versions | `entitydiscovery/service.go`, `eligibility` |
| Core `rpc/application.py:600-647` | Application/pipeline options and readable toolkit names | `entitydiscovery/service.go`, `searchPage` |
| Core `rpc/skill.py:36-48` | Skill name and description search | `entitydiscovery/service.go`, `searchPage` |
| Core `models/pd/search.py:6-14` | Option rows contain numeric ID and name | `entitydiscovery/service.go`, SQL projection |
| Configurations `rpc/getters.py:99-124` | Credential label projection; rows omit total | `entitydiscovery.Service.SearchOptions` |

## Response and filter rules

Tag rows contain `id`, `name`, `data`, and one entity count.
Application and pipeline rows use `application_count`.
Skill rows use `skill_count`.
Counts use distinct entity IDs, not version IDs.
Coverage `all` merges application, pipeline, and skill pages in that order.
The first occurrence supplies each tag's count field.
Its total equals the merged page length, as in Core.
Single coverage totals count all matching tags before pagination.

Search results contain `collection`, `tag`, `application`, `pipeline`, `toolkit`, `credential`, and `skill`.
Unrequested sections contain empty rows and zero total.
The current collection section remains empty because Core has no collection reader here.
Requested credential results contain rows without total.
Credential projections exclude configuration data, metadata, status, and secrets.
Shared credential lookup uses only shared rows from the configured public project.

Application search uses names.
Skill search uses names and descriptions.
Tag discovery uses entity names and descriptions plus separate tag-name search.
Toolkit names follow the current display-name precedence.
Application status and author filters test any version independently.
Skill tag counts use author filtering and omit unsupported status and social filters, as Core does.

## Safety corrections and preserved behavior

Invalid coverage returns 400 instead of a successful empty result.
Database, scan, and projection failures return errors instead of empty collections.
Search tag options follow the selected entity kind and its folder restrictions.
This prevents cross-kind tag leakage from Core's unclassified tag subquery.

Pages contain at most 1,000 rows and offsets cannot exceed 100,000.
Zero or omitted unrestricted limits use the 1,000-row safety bound.
Toolkit and credential option pages retain the default limit of ten.
All-coverage tag output can contain three bounded pages before deduplication.
Search text contains at most 1,024 bytes.
Tag filters contain at most 100 unique positive IDs.
Status filters contain at most 32 values, each at most 64 bytes.
Sort fields and directions use fixed allowlists.
Each service call has a ten-second deadline and one read-only repeatable-read transaction.
The MCP result limit is one MiB.

`foldervisibility.Resolve` extracts the existing external MCP projection check.
An absent override table keeps the existing optional-overlay behavior.
An override table with missing folder or item tables fails closed.
`foldervisibility.ExclusionSQL` supplies the common actor exclusion predicate.
External MCP catalog reads retain the same resolver and do not gain another fallback.

## History and evidence

Core commit `8356032` adds folder-level permissions for applications, skills, and toolkits.
The former Main tag read suppresses database errors and omits relation counts and filters.
It also includes unattached tags in `all` coverage.
The former Main search read returns only tag strings and an empty collections array.
This change replaces those incomplete reads with one Main service.

`TestInternalDiscoveryPostgresCountsVisibilityFiltersAndEnvelopes` uses an isolated PostgreSQL database.
It verifies repeated versions, mixed pipeline types, counts, filters, page totals, and actor-specific exclusions.
It compares REST and authorized MCP results from the same service.
It checks shared credential scope, secret exclusion, cancellation, missing-schema errors, and incomplete folder projections.
The fixture also verifies inherited JSON tag data alongside Main JSONB data.
The four `TestTagsRepoPostgres_*` tests verify existing tag writes and the corrected discovery scope.
Unit tests verify descriptor names, permission ordering, project conflicts, and bounded input.

These checks provide component and PostgreSQL service-integration evidence.
They do not establish browser parity, production scale, or replacement behavior.
