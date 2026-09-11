# Internal MCP application filters

## Source mapping

Core paths start at `projects/centry/pylon_main/plugins/elitea_core` in the umbrella workspace.
Main paths start at `services/elitea-main` in this repository.

| Current platform source | Main owner | Required behavior |
| --- | --- | --- |
| Core `api/v2/applications.py::PromptLibAPI.get` | `internal/api/v2/applications/list_filters.go` | Parse list filters for the endpoint project. Preserve exact limit and offset. |
| Core `utils/application_utils.py::list_applications_api` | `internal/domain/applications/list_filters.go` and `internal/infra/db/repos/applications.go::List` | Bound IDs, author, statuses, search, tags, dates, and pagination. Apply filters before count and pagination. |
| Core `list_applications_api` type predicates | Main application repository | Omitted type returns agents and pipelines. Classic excludes applications with any pipeline version. Pipeline includes them. |
| Core `list_applications_api` version filters | Main application repository | Match any version for author and status. Require every requested tag across versions. |
| Core `utils/like_utils.py` | Main application repository | Scope likes to the endpoint tenant and authenticated user. Match trend dates against stored like timestamps. |
| Core `folder_exclusion_clause` | `internal/application/foldervisibility` | Exclude no-access folders before totals and pagination. Reuse the shared predicate. |
| Core `utils/application_utils.py::list_applications` sorting | Main `applicationListSort` | Sort by a fixed supported column or likes expression. Add a stable application-ID tie-breaker. |
| Core application endpoint MCP metadata | `internal/api/v2/mcp/internal_applications_catalog.go` and `internal_applications_execute.go` | Publish only validated filters. Forward them to the shared Main handler with the resolved actor. |

## Behavior

The existing `get_elitea_core_applications` tool gains tags, IDs, author, status, likes, trend dates, and sorting filters.
It also supports `without_tags`.
All filters apply to whole applications. One application appears once, even when several versions match.
Existing tag-name and tag-ID inputs retain AND matching across versions.
Author and status filters independently match any version.

IDs accept at most 100 positive PostgreSQL integer keys.
HTTP and MCP page size is between 1 and 100. Offset is between 0 and 100000.
Internal repository reads retain a bounded 1000-row maximum for existing suggestion consumers.
An arbitrary offset remains exact. Main does not round it down to a page boundary.
Count and page queries use the same predicates inside one repeatable-read transaction.
A page past the final row retains the filtered total and returns an empty row array.

The endpoint supplies the project. Authentication supplies the actor for likes and folder access.
No-access folders exclude both agent and pipeline entries from an untyped list.
A missing actor or incomplete folder projection fails closed.
A deployment without the optional folder override projection retains authenticated project listing.

The list keeps the existing first-version author and type projection.
It keeps application metadata, including the configured default-version ID.
The existing detail and default-version resolution paths remain unchanged.
Filtering does not select a different version for those paths.

Main accepts fixed sort fields: `created_at`, `updated_at`, `name`, `id`, `author`, `authors`, and `likes`.
The direction is `asc` or `desc`.
Author sorting uses the stored author name, then email, then ID as fallback.
It does not fetch every matching application into memory to sort a page.

## Deliberate differences

The current platform can increase the limit to the number of requested IDs.
Main keeps the explicit page size and filtered total. Callers can request a larger bounded page.

The current platform disables likes queries outside Agent Studio.
Main applies an explicit likes or trend filter consistently in the selected tenant.
It does not silently remove a requested filter.
Malformed booleans, dates, IDs, sort values, and pagination return a safe validation error.

`folder_id` is not published as a filter. Folder clients use bounded `ids`.
Folder visibility still applies to every application query.
Project pin ordering and richer list-card metadata are outside this filter change.
No new mutation or application-drafting MCP operation is published.

## Implementation history

The change starts from Main revision `c56d19e9` and the preceding internal MCP drafting work.
Main already supports AND tag filtering, but internal MCP does not forward tags.
Main previously supports only text, type, and pagination through the MCP application list.
Its repository treats an omitted type as classic and rounds HTTP offsets through page conversion.

This change adds shared validation, fixed SQL predicates, and explicit MCP filter schemas.
It reuses `foldervisibility.Resolve` and `foldervisibility.ExclusionSQL` with the discovery and skill lists.
It changes count and page reads to one database snapshot.
It retains existing list status and tag projections.
Application suggestions now request `classic` explicitly and retain their existing 200-row scan bound.

## Verification

`TestApplicationListHTTPForwardsValidatedFiltersAndExactOffset` checks the HTTP parsing boundary.
The rejection table checks invalid IDs, bounds, booleans, dates, types, sort fields, and malformed query encoding.
`TestApplicationsRepoPostgres_ListExtendedFiltersAndFolderIsolation` uses an isolated PostgreSQL database.
It checks cross-version filters, mixed types, AND tags, IDs, exact offsets, likes, trend periods, and actor-specific folders.
Existing list tag, status, paging, and author tests also pass against PostgreSQL.

`TestInternalApplicationListFiltersReachSharedRepository` uses the in-process MCP endpoint with the real repository.
It verifies combined filters, filtered totals, empty pages, and handled validation failures.
The endpoint uses a test permission resolver. This is service-integration evidence, not deployed authentication proof.

Run the focused suites with `ELITEA_TEST_DATABASE_URL` set to an isolated test service:

```sh
go test ./services/elitea-main/internal/api/v2/applications ./services/elitea-main/internal/api/v2/mcp ./services/elitea-main/internal/infra/db/repos -run 'TestApplicationList|TestInternalApplicationListFilters|TestApplicationsRepoPostgres_List' -count=1
```

The fixture creates and drops its own temporary database.
No live project data is changed.
Deployed Rust chat consumption and production query-load measurements remain separate checks.
