# Configuration and toolset source mapping

The SDK snapshot registers 32 configuration families, 18 connection checks and
45 concrete standard toolkit classes with 619 fixed tool schemas. OpenAPI,
MCP, application and other runtime toolsets are dynamic. Rust will use immutable
configuration/toolset instances and one invocation kernel shared by direct
toolkit tests and ADK agent execution.

Target convention:

- public configuration descriptor/registry: `src/configurations/families/<family>.rs`
- claim-materialized runtime configuration: co-located as
  `src/toolkits/families/<family>/config.rs` when it exists only to construct
  that family's client
- toolset: `src/toolkits/families/<family>/{mod.rs,client.rs,tools.rs}`
- tests: co-located capability-gated unit contracts first, followed by
  `tests/configurations/<family>.rs` and
  `tests/toolkits/<family>_{catalog,materialization,invocation}.rs` when the
  public registry and cross-process composition land

Model-facing tool metadata is part of the functional contract for every
family. A complete port must give each tool a concise selection description
that distinguishes its operation and side effect, and give every argument its
meaning, required format, defaults, bounds and useful examples. Rust may
clarify or shorten weak source prose rather than copy it byte-for-byte, but it
must preserve the source operation and result meaning. Focused family tests
inspect names, descriptions and parameter schemas so selection quality cannot
silently regress or force avoidable model retries and token use.

## Frozen agent-tool snapshot boundary

The first Rust tool slice validates the immutable references admitted with an
agent request; it does not instantiate a toolkit. Main remains responsible for
freezing configuration settings into reference form. Rust borrows those JSON
documents from the owned request, applies explicit count/identifier bounds and
classifies configured, MCP and nested-application references without cloning or
formatting the settings.

The authoritative list depends on the current command kind:

- application execution reads `application.version_details.tools` and requires
  the top-level `tools` list to be empty;
- ad-hoc execution reads the top-level `tools` list and rejects a second nested
  nonempty list;
- the first reference with a non-null toolkit ID wins, matching the current SDK
  deduplication rule. Identity-only ad-hoc application references have a null
  toolkit ID and remain distinct.

The snapshot is bounded to 1,024 references, 1,024 selected/excluded tool names
per reference and 1,024 bytes per common identifier, in addition to the input
protocol's 256 KiB per-JSON-value, 64-level nesting and 64 KiB string limits.
Positive identifiers must fit PostgreSQL/Go signed 64-bit counters. These are
worker hardening bounds, not a claim that the UI supports that many active
tools.

| Source evidence | Business responsibility | Rust target | Status / deviation |
| --- | --- | --- | --- |
| Main `internal/application/agentexecution/tools.go::CurrentApplicationToolSnapshotService.FreezeCurrentApplicationVersion` and `internal/runtimecomposition/agent_runtime.go` | Freeze configured settings as references, preserve selected tools, assign the stable toolkit name and keep nested-application settings identity-only. A custom or Provider Hub participant with no actor-visible schema remains attached to the chat but is omitted from this execution snapshot with a structured warning; configuration and permission failures remain fatal | `src/toolkits/{snapshot,materialize}.rs` | Implemented validation and capability-isolation foundation. Settings remain sealed and borrowed. Rust omits only an unsupported frozen family with the same warning contract, so another supported participant remains runnable |
| Main `internal/application/agentexecution/start.go::currentApplicationInput` and `adhoc.go::currentAdhocInput` | Put application tools in frozen version details and ad-hoc tools at the top level | `FrozenToolSnapshot::from_request` | Implemented as one unambiguous source per request kind |
| Python worker `agents/sdk_adapter.py::EliteaSdkAgentAdapter::{execute_application,execute_adhoc}` | Carry the selected snapshot and claim-scoped MCP/HITL inputs into the SDK execution path | future authorized materializer | Snapshot only; no PAT, MCP token or toolkit client is created here |
| Legacy indexer worker `methods/indexer_agent.py::_indexer_agent_task_inner` and `methods/indexer_predict_agent.py::_indexer_predict_agent_task_inner` | Historical application/ad-hoc selection, MCP secret expansion and SDK invocation behavior | source evidence for materialization and compatibility tests | Intentional deviation: Rust will redeem credentials at the authorized use boundary instead of mutating copied dictionaries before SDK construction |
| SDK `runtime/toolkits/tools.py::get_tools` | Sanitize references, first-ID-wins deduplication, family dispatch, blocked-tool policy, MCP smart-auth and nested applications | `src/toolkits/{snapshot,materialize,policy}.rs` | Deduplication, classification, immutable blocklist and capability-isolated family materialization are implemented. Unsupported families emit a structured warning and do not hide supported toolsets; malformed supported families still fail closed. Remaining families stay capability-gated |

Evidence was refreshed against the platform/Python worker commit
`a87561d7a051e13716739764eb62299a253e10ba`, SDK commit
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, and legacy indexer-worker commit
`b6c4ce83d997acbbbeb58fe040317a9e9352236f` on 2026-08-18. Later slices must
refresh these pins because all three Python sources continue to evolve.

### Immutable blocklist generation

`src/toolkits/policy.rs::ToolAdmissionPolicy` implements the current SDK's
case-, prefix- and separator-insensitive blocklist membership without its
mutable module globals. One immutable policy generation is held by `Arc`, so an
in-flight invocation finishes under the exact policy that admitted it. The
production `serve` command requires a separate bounded regular-file snapshot
containing the current runtime `toolkit_security` dictionary; it does not read
ambient process environment or treat a missing dictionary as an empty policy.
The shared Python-compatible deployment-v1 document is unchanged. Container
orchestration still has to project that dictionary, and an atomic refresh owner
for later admin/runtime changes remains a production gate.

Whole-toolkit restrictions filter the frozen snapshot before any credential or
client can be materialized. Specific-tool restrictions are evaluated again on
the concrete ADK tool name, including `toolkit___name` and `toolkit:name`
aliases. They remain toolkit-scoped: the current blocked-tool contract does not
apply the sensitive-policy `*` wildcard globally. Separator-only entries are
ignored, matching SDK behavior. The policy is capped at 16,384 configured
identifiers and 1,024 bytes per identifier, and errors expose no configured
names.

Source ownership is split deliberately:

- Main `internal/api/v2/admin/config_schemas.go::guardrailsSection` describes
  the operator-facing `toolkit_security` shape;
- legacy indexer worker `module.py::Method._apply_toolkit_security` proves live
  reconfiguration and the current in-flight snapshot behavior;
- SDK `runtime/toolkits/security.py` and
  `tests/runtime/test_blocked_tools.py` define canonical name and alias matching;
- Rust owns the immutable policy generation and will apply the same value at
  materialization and invocation boundaries.

This is a static deny policy, not sensitive-action authorization. The SDK's
`read`/`write`/`search`/`index` grouping describes an operation; it does not
exempt a tool from deployment sensitivity. Trusted environment configuration
may mark any concrete tool, including a read used for testing, as sensitive.
Sensitive tools, MCP smart auth and direct HITL therefore retain a shared
durable invocation identity and `interrupt_id` state machine independent of
family metadata. An approval for identical arguments is never authority for a
later function call.

## Shared kernel mapping

| Python source | Responsibility | Rust target | Proof | Status / deviation |
| --- | --- | --- | --- | --- |
| SDK `configurations/__init__.py` | Configuration registry and schemas | `src/configurations/registry.rs`, family modules | Registry/catalog golden and schema differential tests | Planned |
| SDK `tools/__init__.py::{toolkit_config_schema,get_tools}` | Toolkit registry, selected tools, dispatch, blocked tools and metadata | `src/toolkits/registry.rs`, `src/toolkits/materialize.rs` | Complete family/tool inventory and invalid-selection tests | Planned |
| SDK `runtime/toolkits/tools.py::get_tools` | Runtime toolsets before standard/community dispatch | `src/toolkits/materialize.rs` | Runtime/family dispatch tests | Planned |
| SDK `tools/base/tool.py::BaseAction` and `tools/elitea_base.py::BaseToolApiWrapper.run` | Map selected tool name to bounded invocation | `src/toolkits/invocation.rs` | Native ADK metadata, top-level null normalization, bounds, cancellation and safe-error tests | Implemented shared kernel; family operations remain planned |
| SDK `runtime/toolkits/security.py` | Separator-insensitive blocked toolkit/tool policy | `src/toolkits/policy.rs`, `src/execution/production.rs` | Alias, scope, bound, strict mounted-snapshot and pre-materialization filter corpus | Startup generation is production-wired and immutable per invocation; container projection and atomic later-generation refresh remain planned |
| SDK `runtime/middleware/sensitive_tool_guard.py` | Sensitive-tool policy and confirmation admission | `src/agents/{sensitive_tools,direct_hitl}.rs` | Runtime dictionary binding, exact call-bound interrupt, strict decision/stale-session corpus, approved-effect rejection, one read-only native replay, structured reject/block-with-comment results and restart-before/after-result recovery | Partial capability-disabled path. Initial pause, exact read-only approval and same-call-ID blocked result are proven through ADK. A denied effect is never dispatched; an approved effect still requires durable intent/outcome ownership. Production request registration remains gated |

## Family inventory

`Check` means the Python configuration implements a live or authorization-aware
`check_connection`. Tool counts exclude dynamic OpenAPI/MCP/application tools.
Indexing tools are recorded as a later overlay in `indexing.md`.

| Configuration family | Python configuration symbol | Python toolkit symbol(s) | Fixed tools | Check | Rust targets | Status / notable gate |
| --- | --- | --- | ---: | :---: | --- | --- |
| `artifact` | None; the bucket is an inline toolkit setting and the AUTHORITY is the live execution claim, not a credential | `runtime/tools/artifact::ArtifactWrapper` | 4 of 8 | No | `toolkits/families/artifact/{config,tools}.rs` | The one family whose authority is not in the frozen snapshot (#906): there is no third-party endpoint, only this platform's own storage, reached over main's private claim-bound content listener (`ContentServer.PostArtifact*`). `list_files`, `read_file`, `create_file` and `delete_file` carry the SDK's own names and argument names; the read enforces the SDK's 200,000-character agent-path cap and refuses with the same structured `content_too_large` object. Built only where the claim is in scope: the ordinary agent path and a root pipeline's direct and LLM nodes ([pipeline artifact toolkit nodes](pipeline-artifact-toolkit-nodes-20261009.md)). Saved child pipelines and nested applications still skip it; a direct node there is refused as `unsupported_capability`. `read_multiple_files`, `get_file_metadata`, `append_data`, `create_new_bucket` and the indexing pair remain gates |
| `github` | `configurations/github.py::GithubConfiguration` | `tools/github::EliteAGitHubToolkit` | 44 | Yes | `toolkits/families/github/{config,client,code_search,commits,projects,pull_requests,workflow_runs,tools}.rs`; common configured-tool materializer | Partial read profile: strict anonymous/PAT/basic/App probe parsing plus twenty identity, branch, file, repository-navigation, issue, pull-request, commit, server-side code-search, workflow-status and Project V2 reads; a mixed explicit SDK selection keeps these reads and omits unsupported operations with one bounded warning; the other 24 tools, workflow-log archives, App installation auth, sensitive effects and indexing remain gates |
| `ado_repos` | `configurations/ado.py::AdoConfiguration` | `tools/ado/repos::AzureDevOpsReposToolkit` | 21 | Yes | `configurations/families/ado.rs`; `toolkits/families/{ado,ado_repos}/` | Partial native family: all 15 repository operations; the 6 indexing tools stay on the Python worker |
| `ado_plans` | `configurations/ado.py::AdoConfiguration` | `tools/ado/test_plan::AzureDevOpsPlansToolkit` | 18 | Yes | `configurations/families/ado.rs`; `toolkits/families/{ado,ado_plans}/` | Partial native family: all 12 test-plan operations; the 6 indexing tools stay on the Python worker |
| `ado_boards` | `configurations/ado.py::AdoConfiguration` | `tools/ado/work_item::AzureDevOpsWorkItemsToolkit` | 19 | Yes | `configurations/families/ado.rs`; `toolkits/families/{ado,ado_boards}/` | Partial native family: 11 of 13 work-item operations; `get_image_by_url` (vision LLM), `attach_file_to_work_item` (artifact storage) and the 6 indexing tools stay on the Python worker |
| `ado_wiki` | `configurations/ado.py::AdoConfiguration` | `tools/ado/wiki::AzureDevOpsWikiToolkit` | 14 | Yes | `configurations/families/ado.rs`; `toolkits/families/{ado,ado_wiki}/` | Partial native family: all 8 wiki operations (image description in page content omitted); the 6 indexing tools stay on the Python worker |
| `gitlab` | `configurations/gitlab.py::GitlabConfiguration` | `tools/gitlab::EliteAGitlabToolkit` | 27 | Yes | `toolkits/families/gitlab/{config,client,tools}.rs`, shared `families/vcs_text.rs`; reuses `gitlab_org` transport, status/effect mapping, OLD/NEW editor and MR diff positions | Partial capability-disabled family (per-tool capability): all 21 non-index tools (10 reads, 9 writes, 2 deletes) keep SDK names, argument schemas, messages, protected-base-branch rules, the invocation-local active branch (moved by reads, as in the SDK) and first-page vs all-pages pagination. A selected index tool is omitted with a warning; the 6 index tools, non-UTF-8 document/image parsing, live check, exact-interrupt HITL and effect reconciliation remain gates |
| `gitlab_org` | shared `configurations/gitlab.py::GitlabConfiguration` | `tools/gitlab_org::EliteAGitlabSpaceToolkit` | 17 | Yes | `toolkits/families/gitlab_org/{config,client,edit,diff,tools}.rs` | Capability-disabled complete family: 8 reads, 8 writes and 1 delete; dynamic-project authority, live check, HITL and effect reconciliation remain gates |
| `qtest` | `configurations/qtest.py::QtestConfiguration` | `tools/qtest::QtestToolkit` | 25 | Yes | `configurations/families/qtest.rs`; `toolkits/families/qtest/` | Planned |
| `bitbucket` | `configurations/bitbucket.py::BitbucketConfiguration` | `tools/bitbucket::EliteABitbucketToolkit` | 22 | Yes | `toolkits/families/bitbucket/{config,client,tools}.rs`, shared `families/vcs_text.rs` and the `gitlab_org` OLD/NEW editor | Partial capability-disabled family (per-tool capability): all 16 non-index tools (8 reads, 7 writes, 1 delete) over Bitbucket Cloud REST 2.0 or Server REST 1.0 with a sensitive Basic credential, SDK names/schemas/messages and the invocation-local active branch. Several SDK paths that could never succeed are repaired rather than copied (Server PR commits/changes returned a generator repr, Server decline and inline comments raised `TypeError`, Server file listing indexed strings, Cloud PR reads leaked the client's `__dict__`, and an unset `cloud` never detected bitbucket.org). The 6 index tools, SDK 429 retry/backoff, live check, exact-interrupt HITL and effect reconciliation remain gates |
| `confluence` | `configurations/confluence.py::ConfluenceConfiguration` | `tools/confluence::ConfluenceToolkit` | 16 of 25 | Yes | `toolkits/families/confluence/{config,client,tools}.rs`; `toolkits/materialize.rs` | Materialized partial family (2026-10-09): the sixteen REST tools; image-description, attachment and file-upload tools and the six index tools are not served (`supported_tools.confluence`); see [Confluence REST family](#confluence-rest-family) |
| `jira` | `configurations/jira.py::JiraConfiguration` | `tools/jira::JiraToolkit` | 12 of 23 | Yes | `toolkits/families/jira/{config,client,tools}.rs`; `toolkits/materialize.rs` | Materialized partial family (2026-10-09): the twelve REST tools; image-description, attachment-content and artifact-upload tools and the six index tools are not served (`supported_tools.jira`); see [Jira REST family](#jira-rest-family) |
| `postman` | `configurations/postman.py::PostmanConfiguration` | `tools/postman::PostmanToolkit` | 31 | No | `toolkits/families/postman/` | Capability-disabled complete family: 8 reads, 19 writes, 3 deletes and 1 execute surface; management authority is fixed to the claimed Postman origin, while stored-request execution remains behind a separate sealed dynamic-egress authority |
| `elastic` | None; cluster origin and optional encoded API key are inline toolkit settings | `tools/elastic::ElasticToolkit` | 1 | No | `toolkits/families/elastic/{config,client,tools}.rs` | Capability-disabled complete read family: one bounded Query DSL search against a fixed verified-TLS cluster; approved DNS/IP egress, authorized materialization and live read/load proof remain gates |
| `keycloak` | None; authority and service-account credentials are inline toolkit settings | `tools/keycloak::KeycloakToolkit` | 1 | No | `toolkits/families/keycloak/{config,client,tools}.rs` | Capability-disabled complete family: one generic Admin REST execute surface retains reads, writes, deletes and actions inside one frozen HTTPS realm; exact-interrupt HITL, effect reconciliation, approved egress and live provider proof remain gates |
| `azure` | None; subscription and service-principal credentials are inline toolkit settings | `tools/cloud/azure::AzureToolkit` | 2 | No | `toolkits/families/azure/{config,client,tools}.rs` | Capability-disabled complete family: one generic ARM execute surface plus its resource-group health read inside the frozen public-cloud subscription; exact-interrupt HITL, effect reconciliation, approved egress and live Azure role proof remain gates |
| `gcp` | None; the service-account JSON is an inline sealed toolkit setting | `tools/cloud/gcp::GcpToolkit` | 1 | No | `toolkits/families/gcp/{config,client,tools}.rs` | Capability-disabled complete family: one generic scoped Google REST surface retains reads, writes, deletes and actions on approved `googleapis.com` origins; exact-interrupt HITL, effect reconciliation, DNS/IP egress and live service-account role proof remain gates |
| `kubernetes` | None; cluster origin and Bearer token are inline toolkit settings | `tools/cloud/k8s::KubernetesToolkit` | 2 | No | `toolkits/families/kubernetes/{config,client,tools}.rs` | Capability-disabled complete family: one generic Kubernetes REST execute surface plus its `/version` health read on an exact verified-TLS origin; exact-interrupt HITL, effect reconciliation, approved DNS/IP egress, CA policy and live RBAC proof remain gates |
| `service_now` | `configurations/service_now.py::ServiceNowConfiguration` | `tools/servicenow::ServiceNowToolkit` | 3 | No | `toolkits/families/service_now/{config,client,tools}.rs` | Capability-disabled complete family: one bounded incident read plus create and update effects over fixed-origin Table API; shared durable sensitive-tool approval, authorized materialization and cancellation-safe effect reconciliation remain gates |
| `testrail` | `configurations/testrail.py::TestRailConfiguration` | `tools/testrail::TestrailToolkit` | 23 | Yes | `toolkits/families/testrail/{config,client,format,schema,tools}.rs` (+ shared `families/python_repr.rs`) | Partial family (16 of 23): every case/suite/section/run/result read and the six case/section effects over the `testrail_api==1.13.4` wire contract; `add_file_to_case` needs raw artifact bytes the runtime cannot read, and the six index tools wait for Rust indexing. The capability snapshot lists the 16 served tools |
| `slack` | `configurations/slack.py::SlackConfiguration` | `tools/slack::SlackToolkit` | 7 | No | `toolkits/families/slack/{config,client,tools}.rs` | Capability-disabled complete family: seven bounded fixed-origin messaging, membership and workspace operations; authorized materialization, exact-interrupt HITL and cancellation-safe effect reconciliation remain gates |
| `azure_search` | `configurations/azure_search.py::AzureSearchConfiguration` | `tools/azure_ai/search::AzureSearchToolkit` | 2 | No | `toolkits/families/azure_search/{config,client,tools}.rs` | Capability-disabled complete read family: fixed configured index, two bounded reads, SDK 11.5.2 wire/result projection and no unbounded continuation; authorized materialization and live provider proof remain gates |
| `delta_lake` | `configurations/delta_lake.py::DeltaLakeConfiguration` | `tools/aws/delta_lake::DeltaLakeToolkit` | 3 | No | None | Blocked (`toolkit-port-b7-20261010.md`): needs Delta log replay, Parquet/Arrow over S3 and pandas `DataFrame.query` semantics; the Python image does not ship its dependencies either |
| `aws` | None; region and access-key credentials are inline toolkit settings | `tools/cloud/aws::AWSToolkit` | 1 | No | None | Blocked (`toolkit-port-b7-20261010.md`): the pinned SDK never binds it and cannot construct it; `execute_aws` needs botocore service models and SigV4, which the runtime does not have |
| `bigquery` | `configurations/bigquery.py::BigQueryConfiguration` | `tools/google/bigquery::BigQueryToolkit` | 11 | No | `toolkits/families/bigquery/{config,client,format,tools}.rs` | Partial capability-disabled family: 8 of 11 tools (reads, nearest-neighbour searches, job statistics, vector-index DDL and Delta Lake external-table creation); `similarity_search`, `similarity_search_with_score` and `execute` are not served |
| `xray` | `configurations/xray.py::XrayConfiguration` | `tools/xray::XrayToolkit` as `xray_cloud` | 12 | Yes | `toolkits/families/xray_cloud/{config,client,tools}.rs` | Partial family under the preserved `xray_cloud` runtime alias (6 of 12): all six GraphQL business tools over one client-credential token per invocation; `add_attachment_to_test_step` serves `filedata` and explains why `filepath` (raw artifact bytes) is unavailable; the six index tools wait for Rust indexing |
| `zephyr` | None; base URL and Basic-auth credentials are inline toolkit settings | `tools/zephyr::ZephyrToolkit` | 4 | No | `toolkits/families/zephyr/{config,client,tools}.rs` | Capability-disabled complete legacy family: one bounded step read plus all three sequential create effects; exact-interrupt HITL, durable partial-effect reconciliation, approved egress and live legacy-ZAPI proof remain gates |
| `zephyr_scale` | `configurations/zephyr.py::ZephyrConfiguration` plus optional `PgVectorConfiguration` | `tools/zephyr_scale::ZephyrScaleToolkit` | 26 | Yes | `toolkits/families/zephyr_scale/{config,client,folders,render,tools}.rs` over shared `toolkits/families/zephyr_rest/` | Partial: all 20 business operations against the fixed Cloud API with the token; the 6 inherited indexing tools are not served |
| `zephyr_squad` | None; credentials are inline toolkit settings | `tools/zephyr_squad::ZephyrSquadToolkit` | 15 | No | `toolkits/families/zephyr_squad/{config,client,tools}.rs` | Capability-disabled complete family: five bounded reads plus all eight writes and two deletes over fixed Squad Cloud JWT routes; authorized materialization, live credential proof, exact-interrupt HITL and cancellation-safe effect reconciliation remain gates |
| `zephyr_enterprise` | `configurations/zephyr_enterprise.py::ZephyrEnterpriseConfiguration` plus optional `PgVectorConfiguration` | `tools/zephyr_enterprise::ZephyrEnterpriseToolkit` | 11 | Yes | `toolkits/families/zephyr_enterprise/{config,client,tools}.rs` over shared `toolkits/families/zephyr_rest/` | Partial: all five business operations (three reads, two effects) over one bearer flex-REST client; the six inherited indexing tools are not served and the capability snapshot lists the family per tool |
| `zephyr_essential` | `configurations/zephyr_essential.py::ZephyrEssentialConfiguration` plus optional `PgVectorConfiguration` | `tools/zephyr_essential::ZephyrEssentialToolkit` | 51 | Yes | `toolkits/families/zephyr_essential/{config,client,tools}.rs` over shared `toolkits/families/zephyr_rest/` | Partial: 41 of the 45 business operations (24 reads, 17 effects); not served are the six indexing tools, the three automation-result uploads and the BDD ZIP download (see the family section) |
| `figma` | `configurations/figma.py::FigmaConfiguration` | `tools/figma::FigmaToolkit` | 17 | Yes | `toolkits/families/figma/{config,client,output,tokens,tools}.rs` | Partial capability-disabled family: eight REST reads/comment with the SDK's `extra_params` output reduction plus both design-token extractors; `analyze_file` (LLM/TOON) and the six indexing tools stay SDK-only; exact-interrupt HITL for the comment effect, approved egress and live proof remain gates |
| `rally` | `configurations/rally.py::RallyConfiguration` | `tools/rally::RallyToolkit` | 8 | No | `toolkits/families/rally/{config,client,tools}.rs` | Capability-disabled complete family: six bounded WSAPI reads plus create/update, with lazy per-invocation API-key/Basic authority; authorized materialization, exact-interrupt HITL, live WSAPI proof and cancellation-safe effect reconciliation remain gates |
| `sonar` | `configurations/sonar.py::SonarConfiguration` | `tools/code/sonar::SonarToolkit` | 1 | No | `toolkits/families/sonar/{config,client,tools}.rs` | Capability-disabled complete read family: one project-bound `/api/issues/search` request with bounded filters and raw JSON projection; authorized materialization and live Sonar TLS proof remain gates |
| `sql` | `configurations/sql.py::SqlConfiguration` | `tools/sql::SQLToolkit` | 2 | No | `toolkits/families/sql/` | Capability-disabled complete family: backend-specific PostgreSQL/MySQL execution plus bounded default-schema discovery; exact-interrupt HITL, effect reconciliation, TLS authority and driver preallocation controls remain gates |
| `google_places` | `configurations/google_places.py::GooglePlacesConfiguration` | `tools/google_places::GooglePlacesToolkit` | 2 | No | `toolkits/families/google_places/{config,client,tools}.rs` | Capability-disabled complete read family: supported Places API (New) projection for `places` and `find_near`; attribution/persisted-result policy, authorized materialization and live provider proof remain gates |
| `salesforce` | `configurations/salesforce.py::SalesforceConfiguration` | `tools/salesforce::SalesforceToolkit` | 6 | No | `toolkits/families/salesforce/{config,client,tools}.rs` | Capability-disabled complete family: six bounded CRM tools, including create/update and generic GET/POST/PATCH/DELETE; authorized materialization, exact-interrupt HITL and cancellation-safe effect reconciliation remain gates |
| `sharepoint` | `configurations/sharepoint.py::SharepointConfiguration` | `tools/sharepoint::SharepointToolkit` | 28 | Yes | `toolkits/families/sharepoint/{config,client,tools}.rs` | Partial capability-disabled delegated read family: 8 explicitly selected Graph operations cover lists, columns, metadata-only recursive file discovery and bounded raw OneNote XHTML with proactive missing-token guards and direct-node reactive 401 interrupts. Empty/all selection, ACS/app-only auth, remaining content/artifact/index/effect tools, rich OAuth discovery/DCR/refresh metadata, model-loop reactive 401 confirmation, approved egress and live-provider proof remain gates |
| `carrier` | `configurations/carrier.py::CarrierConfiguration` | `tools/carrier::EliteACarrierToolkit` | 18 | No | `toolkits/families/carrier/{config,client,tools}.rs` | Partial capability-disabled family: 15 ticket, backend and UI test/report operations with the SDK's confirmation steps; the archive tools `get_report_by_id`, `create_excel_report` and `create_ui_excel_report` stay SDK-only (archive/xlsx writers), and exact-interrupt HITL, effect reconciliation, approved egress and live proof remain gates |
| `report_portal` | `configurations/report_portal.py::ReportPortalConfiguration` | `tools/report_portal::ReportPortalToolkit` | 9 | Yes | `toolkits/families/report_portal/{config,client,tools}.rs` | Capability-disabled complete read family: nine bounded project/report reads, including explicit UTF-8 HTML and base64 PDF export projections; authorized materialization, egress policy and live provider proof remain gates |
| `testio` | `configurations/testio.py::TestIOConfiguration` | `tools/testio::TestIOToolkit` | 15 | Yes | `toolkits/families/testio/{config,client,tools}.rs` | Partial read family (13 of 15): every read over `Authorization: Token`; the two SDK writes stay unserved because their payloads do not map to the current provider create/confirmation operations (below), and the capability snapshot lists the 13 served tools so the catalogue marks the writes unavailable |
| `openapi` | `configurations/openapi.py::OpenApiConfiguration` | `tools/openapi::{EliteAOpenAPIToolkit,OpenApiAction}`, `tools/openapi/{api_wrapper,response_selection}.py` | Dynamic | Yes | `toolkits/families/openapi/{config,spec,client,response_selection,tools}.rs` | Partial capability-disabled family: bounded inline OpenAPI 3.x JSON/YAML parsing, selected dynamic operations, exact request schemas, fixed-origin JSON calls, static secret headers, anonymous/API-key/client-credentials/delegated OAuth and bounded schema-aware response search are implemented. A Private-project UI rehearsal proved selected `echo_marker` materialization, provider dispatch, same-call result, second model turn, persistence and retirement. Direct-node delegated 401 recovery has component proof in `delegated-auth-expiry.md`. Remote specifications, legacy auth objects, rich OAuth discovery/DCR, model-loop 401 re-authorization, non-JSON request bodies, binary/artifact routing and production egress remain gates |
| `langfuse` | `LangfuseConfiguration` | No standard toolkit | 0 | Yes | `configurations/families/langfuse.rs` | Planned; observability support configuration |
| `aha` | `configurations/aha.py::AhaConfiguration` | `tools/aha::AhaToolkit` | 32 of 33 | Yes | `toolkits/families/aha/`; `toolkits/materialize.rs` | Materialized partial family (2026-10-09): all 25 reads, 5 writes, 1 delete and the effectful combined execute surface pass the SDK conformance gate and are served; `attach_file` is implemented behind a claim-scoped verified temp-spool resolver but omitted from the served set (`supported_tools.aha`) until an artifact-read grant plane exists |
| `pgvector` | `PgVectorConfiguration` | No standalone toolkit | 0 | No | `configurations/families/pgvector.rs` | Planned; shared indexing/runtime dependency |

The OpenAPI slice follows the current SDK business boundary without copying its
LangChain wrappers. `elitea_sdk/configurations/openapi.py` maps to
`toolkits/families/openapi/config.rs`; `elitea_sdk/tools/openapi/api_wrapper.py`
maps to the bounded parser and fixed-origin client in `spec.rs` and `client.rs`;
`elitea_sdk/tools/openapi/response_selection.py` maps to the bounded selector in
`response_selection.rs`;
and `elitea_sdk/tools/openapi/{__init__,tool}.py` maps to native ADK dynamic
tools in `tools.rs`. `toolkits/materialize.rs` passes the invocation-scoped
delegated-token map into every configured family and merges the returned auth
catalog with MCP, while `agents/{ordinary,pipeline,application_tools}.rs`
retains that catalog at each owning `LlmAgent` or graph node. A missing token
therefore exposes the original operation, description and JSON schema through
a guarded tool; Authorize rematerializes that exact frozen API base URL and
replays the same provider call ID, while Skip executes no endpoint. Client
credentials reuse one zeroizing invocation-scoped token until the provider's
bounded `expires_in` window (with the SDK's one-minute refresh buffer); no token
is kept in a process-global registry.

Configured `headers` are claim-materialized secret values, not model arguments.
They apply to every operation with any primary authentication mode. Primary
authentication wins on a case-insensitive name collision. Host, length,
transfer and proxy-control headers remain rejected. Per-call headers stay
non-secret and cannot replace credential headers.

This is a dynamic toolset compiler, not a static Rust catalog. Each selected
specification operation becomes one native ADK tool whose operation ID,
summary/description, required inputs and JSON schema come from the frozen
document. Its bounded model description retains the SDK's toolkit name and base
URL suffix. The executor combines those operation rules with the toolkit's frozen
origin and authentication configuration. Query serialization preserves source
parameter order and operation-level overrides of shared path-item parameters;
it evaluates OpenAPI `style`, `explode` and `allowReserved` per parameter. Only
`allowReserved: true` enables the RFC 3986 reserved name/value safe sets. The
structural `&`/`=` delimiters stay encoded in names, and `+`/`#` stay encoded in
values so they cannot become a space or fragment boundary. Spaces are always
`%20`, and the raw Hyper/Rustls request URI avoids a second URL-normalization
pass. The differential test includes the SDK's Planisware/OData regression
shape but the implementation contains no OData-specific names or values.

Each operation schema now exposes only `response_search` and `response_limit`
as structured response controls. The client removes them before the HTTP call.
It uses successful response schemas, including local references, composed
schemas and keyed `additionalProperties` maps, to select the intended
collection. BM25 ranking returns untouched array objects or `key: object`
pairs inside the original envelope. The response includes compact selection
metadata and stays below 50,000 serialized characters. Omitting both controls
preserves the exact legacy UTF-8 result. `regexp` remains available alone and
cannot be combined with structured selection.

This is intentionally `partial`, not `ported`. Specifications must already be
sealed inline and are capped at 1 MiB; URL-based loading is rejected until a
claim-scoped egress grant exists. The initial client accepts JSON request bodies
and bounded UTF-8 output only. The SDK's discovery/resource-metadata and DCR
program, legacy nested auth shapes, model-loop 401 token recovery,
multipart/form/binary bodies, binary or artifact results and production network
admission remain explicit gaps. `src/toolkits/openapi_tests.rs` owns parser,
selection, dynamic schema, RFC query construction, auth precedence, guarded-tool,
exact-token rematerialization, secret-header precedence, array/map response
selection, block-policy and redacted-failure proof.

Direct-node OpenAPI 401 recovery now uses the shared delegated authorization signal.
The [active-run ledger](delegated-auth-expiry.md) records exact-node resume, Skip, and bounded repeated-rejection tests.
The worker does not exchange refresh tokens itself. Deployed active-run verification remains open.

The SharePoint slice is intentionally smaller than the current 28-tool SDK
catalog. `elitea_sdk/configurations/sharepoint.py` and the token/site-path logic
in `elitea_sdk/tools/sharepoint/__init__.py` map to
`toolkits/families/sharepoint/config.rs`; `api_wrapper.py` maps the selected
names, descriptions and input schemas to `tools.rs`; and delegated reads in
`graph_wrapper.py` plus `file_filters.py` map to the fixed Graph v1.0 client in
`client.rs`. The admitted operations are `read_list`, `get_lists`,
`get_list_columns`, `get_files_list`, `onenote_get_notebooks`,
`onenote_get_sections`, `onenote_get_pages` and
`onenote_get_page_content`. File discovery returns metadata only and preserves
site-prefix, document-library, recursive traversal, extension-filter and
provider-next-link behavior without downloading content.

This subset rejects an empty selection because the SDK interprets empty as the
complete catalog. A mixed saved selection exposes only supported reads that the
user selected. It reports the count of omitted operations without exposing
their arguments or configuration. A selection with no supported read still
fails closed. `rest_wrapper.py` and `authorization_helper.py` remain the
separate ACS/app-only authority gap. `read_document`, sharing-link parsing,
OneNote image or attachment interpretation, artifact upload, indexing, and all
effects remain closed until the artifact boundary and effect receipts exist. A
real Graph 401 is converted to the common delegated-auth signal and direct
Toolkit nodes can checkpoint it. A model-owned loop still requires a
runtime-discovered confirmation adapter when a previously accepted token
expires. Proactive missing-token model calls already use the native
original-call confirmation.
`src/toolkits/sharepoint_tests.rs` owns configuration/token precedence,
guarded schema, fixed-origin/sensitive-header, pagination, library-path,
metadata-only traversal, reactive-401 and materializer-catalog proof. This
mapping was rechecked against SDK commit
`c181fc0fb56e9db017cb301841644680684b4b54` on 2026-08-24.

`testio` is ported as a partial read family rather than as a nominally
complete one. Its connection check and Test IO's current customer API
authenticate with `Authorization: Token`, while all fifteen runtime methods
send `Bearer`; Rust sends `Token`. The public `get_exploratory_test` schema
omits the product ID its implementation requires, and product-scoped list
calls can construct a literal `/products/None/` path: Rust adds an optional
`product_id` to `get_exploratory_test` (an extra optional property keeps every
SDK-shaped call valid) and refuses either exploratory-test read without one
instead of requesting a `None` path. `list_products` applies the documented
product-ID filter itself because the SDK's nested `filter` dict is encoded by
`requests` as `filter=product_ids` and never filters. The SDK's two writes are
not served: current exploratory-test creation requires a product-scoped
nested request with a test environment and a feature-or-template choice,
while the SDK exposes unrelated legacy device/date/goal fields, and its
`confirm_bug_fix` payload maps neither to the current confirmation-information
request nor to the separate bug-state transitions. Serving them needs a
coordinated platform/SDK contract revision or provider-backed migration
fixtures, not guessed provider effects. `testio_tests.rs` owns the wire,
result-member, selection and SDK-conformance proof.

Additional standard toolkits without a registered same-named configuration are
tracked separately: AWS (1), Azure Resource Manager (2), GCP (1), Kubernetes
(2), Keycloak (1), Elastic (1), PPTX (2), and Yagmail (1). Yagmail, Elastic,
Keycloak, Azure Resource Manager, GCP and Kubernetes are now implemented
completely behind capability gates from inline claim-materialized settings. LocalGit
(13) is blocked in the cloud worker, not merely deferred; see
[LocalGit is not a worker family](#localgit-is-not-a-worker-family). The
remaining families still require an explicit authority source and admission
policy before live porting.

### LocalGit is not a worker family

SDK `tools/localgit/{__init__,local_git,tool}.py` at the pinned revision
`b5113a1` exposes 13 tools (`create_file`, `read_file`, `update_file`,
`delete_file`, `checkout_branch`, `checkout_commit`, `get_diff`,
`update_file_content_by_lines`, `list_files`, `get_files_in_folder`,
`commit_changes`, `read_multiple_files`, `grep_file`) over a checkout at
`os.path.join(base_path, repo_path)`, optionally cloned from `repo_url` and
hard-reset to `commit_sha`. The toolkit is `hidden` in the catalogue and its
snapshot schema declares no configuration properties. Rust deliberately
registers no family for it:

- **No path authority exists in the worker.** `base_path`, `repo_path` and
  `repo_url` are user-authored toolkit settings, so the root itself is chosen
  by the caller. Every SDK operation joins the model's `file_path` with
  `os.path.normpath` and no containment check, so `../` and absolute paths
  escape even that root. Confining to a user-chosen directory on a shared,
  multi-tenant worker host is no confinement; the worker has no per-claim
  workspace, and inventing one (a deployment root plus per-execution
  directories, cleanup and quotas) is a platform decision, not a port.
- **Clone is unmanaged egress.** `repo_url` makes the worker clone an arbitrary
  remote over git transports, outside the per-family HTTPS allowlists and
  bounds every configured family uses. `gix` exists only in `libs/rust/repo-ingest`
  for the indexing pipeline's own fetches.
- **The pinned SDK cannot serve a call either.** `LocalGit.validate_toolkit`
  is a `mode='before'` classmethod that opens the repository into a local
  variable and never stores it; every tool then dereferences the undeclared
  `self.repo` and `LocalGitAction._run` returns `Error: Traceback …
  AttributeError`. The SDK's own unit test injects `repo` by hand ("set
  externally in production", which nothing does). Tool names are additionally
  prefixed with `repo_path + "_"`, which no saved agent selection matches.

The meaningful equivalent already exists where a confined workspace exists:
the desktop host's `libs/rust/local-tools` (ADR-0029) provides
openat/`O_NOFOLLOW`-confined file, grep, shell and git tools rooted at the
folder the person opened. A frozen `localgit` reference reaching the cloud
worker keeps today's behavior: it is skipped as an unsupported family with
`agent_toolkit_skipped`, and the capability snapshot does not list it.

The SDK also defines `EmbeddingConfiguration`, but it is not one of the 32
registered configuration families. Rust will model it as a referenced model
contract rather than silently registering a 33rd family.

### GitHub reference-family foundation

GitHub is the first concrete family and fixes the ownership convention without
claiming the full 44-tool SDK catalog. Main still freezes the selected toolkit
and configuration identity in
`internal/application/agentexecution/tools.go`. During claim-scoped input
materialization,
`internal/infra/storage/configurations_materializer.go::materializeAgentExecution`
resolves the frozen configuration settings and unsecrets the nested owned
configuration into the bounded input document. The authorized Rust assembler
parses that sealed materialized shape only after
`AUTHORIZED_NOW`; it will not perform another configuration lookup or persist a
second plaintext copy.

`src/toolkits/families/github/config.rs` validates the current nested
`github_configuration` shape, GitHub Enterprise API base, repository, branch
and selected-tool bounds. It preserves the SDK authentication precedence:
access token, username/password, GitHub App, then anonymous. Secret strings are
non-`Clone`, non-`Debug` and zeroized on ordinary drop. The decoded GitHub App
key/JWT buffers are also zeroized; `ring` does not guarantee erasure of its
internal RSA key schedule, so complete key-schedule erasure remains a worker
process-isolation and termination property rather than an overclaimed local
guarantee.

`src/toolkits/families/github/client.rs` creates one bounded `reqwest::Client`
per materialized toolkit invocation. Its pool is reused by that toolkit's
calls, never stored in a process-global credential registry. HTTPS-only
requests are origin-bound to the frozen API base, redirects are disabled,
timeouts and response bodies are capped, and diagnostics retain no URL,
repository, credential or upstream body. GitHub primary/secondary rate-limit
`403` responses remain retryable and distinct from ordinary authorization
failures. The probe matches the current SDK behavior: anonymous is a
validation-only success, token/basic call `/user`, and App JWT calls `/app`.
App-backed tool execution is still rejected because an installation-token
exchange has not been implemented.

`src/toolkits/families/github/tools.rs` exposes twenty selected ordinary reads.
It uses ADK-Rust 2.0.0's native `Tool` and `BasicToolset` boundary.
The common configured-tool materializer now selects this family for live work.
A mixed explicit SDK selection retains supported reads and omits other tools.
The worker emits one bounded warning with counts for this partial profile.
An empty selection still means all 44 SDK tools, so Rust rejects it.
Issue and pull-request schemas omit an artificial 64-bit maximum. Bedrock
rejects that maximum before model execution. Runtime parsing still requires a
positive unsigned integer.
The current tests prove auth precedence and malformed pairs,
Enterprise/repository normalization and bounds, origin/header binding, PKCS8
App JWT shape, bounded response projection, rate-limit classification, native
ADK selection/policy/argument behavior, exact line slicing/large-file guidance,
cumulative batch admission and bounded regex search.

The first file group follows this source-to-Rust chain:

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main `internal/application/agentexecution/tools.go` and `internal/infra/storage/configurations_materializer.go::materializeAgentExecution` | Freeze the selected toolkit and resolve the exact repository, branch and owned configuration into the admitted request | `config.rs` consumes only that sealed materialized shape; it performs no configuration lookup and creates no second credential authority |
| Python worker `agents/sdk_adapter.py::EliteaSdkAgentAdapter::{execute_application,execute_adhoc}` | Both agent kinds enter the same pinned SDK application/toolkit runtime | The authorized Rust assembler selects the GitHub family for both agent kinds. It exposes only implemented selected reads and keeps unsupported effects unavailable |
| SDK `tools/github/github_client.py::{_read_file,read_file,read_multiple_files}` | GitHub Contents API, active-branch default, optional repository for a single read, 1-indexed inclusive line slicing, structured 200,000-character guidance, and cumulative batch skipping without fetching later files | `client.rs::GitHubApi::read_text_file` plus `tools.rs` preserve those results. Paths, UTF-8/base64 content, file bytes, batch count and output are bounded. Rust intentionally performs one asynchronous transport attempt; the lifecycle owns retry rather than copying the SDK's blocking `time.sleep` retry loop |
| SDK `tools/elitea_base.py::BaseToolApiWrapper::search_file` and `tools/utils/text_operations.py::{apply_line_slice,search_in_content}` | `grep_file` is case-insensitive, supports regex or literal input, reports one match per line with before/after context, and uses Python-compatible line boundaries | `tools.rs` uses the non-backtracking Rust `regex` engine with explicit pattern/program/context/match/output limits. Invalid regex is a stable invalid-input error before network use instead of the SDK's warning plus ambiguous no-match result |
| SDK `tools/utils/file_metadata.py::{guard_text_read,capped_read_multiple_files}` and `runtime/langchain/constants.py::DEFAULT_MAX_OUTPUT_CHARS` | Plain content below the cap; schema `1.0` `content_too_large` guidance above it; line-range honesty for an unchunkable single line; one cumulative 200,000-character batch budget | Rust emits the same machine discriminator, line instructions and skip notice. Per-file batch failures are data-free so upstream exception bodies, private repositories and credentials cannot enter model context or logs |
| SDK `tools/github/github_client.py::{_get_files,list_files_in_main_branch,list_files_in_bot_branch,get_files_from_directory}` | Resolve the configured base or active ref, fetch one recursive Git tree, retain only blobs, and optionally filter one directory prefix | `client.rs::GitHubApi::list_repository_files` preserves the base/active scopes and recursive path results. It bounds the tree body, entry/path/file counts and serialized output. GitHub's `truncated=true` is a resource error instead of the SDK's warning plus a silently incomplete list |
| SDK `tools/github/github_client.py::{get_issues,get_issue,validate_search_query,search_issues}` plus `schemas.py::{NoInput,GetIssue,SearchIssues}` | `get_issues` is exposed as configured-repository/open-only despite a wider Python signature; detail and search permit an optional repository; search prepends `repo:`, distinguishes issues from pull requests and returns the current empty-result message | `client.rs::GitHubApi::{list_open_issues,get_issue,search_issues}` and `tools.rs` preserve those callable schemas, projected field names and Python `datetime.isoformat()` timestamp form. Rust keeps one bounded page of at most 100 results, caps response/body/metadata/serialized output, rejects the SDK's dangerous-query patterns before transport and omits unknown upstream fields and error bodies |
| SDK `tools/github/github_client.py::{list_open_pull_requests,get_pull_request,list_pull_request_diffs}` plus `schemas.py::{ListPullRequestsInput,GetPR}` | Configured-repository open-listing; optional repository for one PR; PR metadata, issue comments, commit messages and changed-file patch fragments | `client.rs::GitHubApi::{list_open_pull_requests,get_pull_request,list_pull_request_files}`, `pull_requests.rs` and `tools.rs` preserve names, callable scopes and success field meanings. Rust deliberately replaces double-stringified comments/commits and token-budget-dependent missing fields with complete bounded typed arrays, preserves null bodies/users, normalizes timestamps like Python, caps detail items at ten, caps changed files at 300 and verifies the PR's declared `changed_files` before bounded local pagination. GitHub patch fragments remain fragments and null remains null; no tool claims full file contents |
| SDK `tools/github/github_client.py::{get_commits,get_commit_changes,get_commits_diff}` plus `schemas.py::{GetCommits,GetCommitChanges,GetCommitsDiff}` | List commits with optional repository/ref/path/time/author filters; inspect one commit's totals and changed-file fragments; compare two general commit, branch or tag refs | `client.rs::GitHubApi::{list_commits,get_commit_changes,compare_commits}`, `commits.rs` and `tools.rs` preserve the public names, default count, success field names, renamed-file metadata and Python timestamp form. Rust canonicalizes date-only and offset timestamps to UTC before transport, safely projects missing authors as `Unknown`/null, caps list and compare commits at 100, commit files at 300, compare files below GitHub's ambiguous 300-file ceiling, patch fragments at 64 KiB and total output at 200,000 characters. It pages one commit until complete and rejects mismatched SHAs or overflow instead of silently returning a partial change set. Empty identical/behind comparisons derive the head only from the same compare snapshot (`base_commit`/`merge_base_commit`), so a mutable ref cannot be substituted by a later fallback request |
| SDK `tools/github/github_client.py::search_code` plus `schemas.py::SearchCode` and PyGithub 2.3.0 `Github.search_code` | Blank-query rejection; configured-repository auto-scope unless an explicit `repo:`, `org:` or `user:` scope exists; optional indexed sort/order and caller-visible page metadata; file/repository/text-match success fields | `client.rs::GitHubApi::search_code`, `code_search.rs` and `tools.rs` preserve the public schema and explicit-scope behavior. Rust sends `page`/`per_page` in one direct `/search/code` request instead of walking and discarding lazy PyGithub pages, reports GitHub's actual `incomplete_results`, omits unknown provider fields, and never performs a per-result content request merely to discover absent text matches. Query, first-1,000 search window, page size, response, item/fragment/match collections and 200,000-character output are bounded; enums and 422 responses become stable invalid-input failures, and query/repository/upstream bodies are not logged or rendered |
| SDK `tools/github/github_client.py::get_workflow_status` plus `schemas.py::GetWorkflowStatus` and PyGithub 2.3.0 `Repository.get_workflow_run`/`WorkflowRun.jobs` | Decimal string run ID, optional repository override, run identity/status/ref/timestamp metadata and job identity/status/timestamps | `client.rs::GitHubApi::get_workflow_status`, `workflow_runs.rs` and `tools.rs` preserve the callable schema and success fields through one run request plus one explicit jobs request. Rust validates the run ID before transport, verifies every returned job belongs to that run, preserves provider-nullable status/name/URL fields, caps each response and serialized output, normalizes timestamps, omits steps/unknown provider fields and returns at most 100 jobs together with `jobs_total_count` and `jobs_truncated`; this makes the deliberate bound visible instead of materializing the SDK's unbounded lazy iterator. Workflow log ZIP retrieval is a separate gate because archive, entry, path and decompressed-byte limits must be enforced together |
| SDK `tools/github/graphql_client_wrapper.py::{list_project_issues,_list_project_issues_internal,_process_project_fields,_process_project_items}`, `tool_prompts.py::GraphQLTemplates` and `schemas.py::ListProjectIssues` | Select a repository Project V2 by number and return its identity, fields, single-select options, bounded item content, labels, assignees and text/date/single-select values | `client.rs::GitHubApi::list_project_issues`, `projects.rs` and `tools.rs` preserve the callable schema and projected success fields. Rust deliberately replaces the SDK's structure request plus mutable pagination loop with one fixed GraphQL query for at most 100 items, requests the option color that the Python projector exposes but its query omits, adds `items_total_count`/`items_truncated`, caps every nested collection, response and serialized output, and rejects partial GraphQL data whenever `errors` is nonempty. The endpoint is derived from the admitted REST base as GitHub documents: `/graphql` for a root API base and `/api/graphql` for an Enterprise `/api/v3` base; the pinned SDK's unconditional `requester.base_url + "/graphql"` is not copied. `search_project_issues` remains gated because the pinned GraphQL query declares but never uses `search_query`; Rust will not advertise an unfiltered list as search |

The GitHub REST projection accepts only a `type=file`, exact base64-declared
size and valid UTF-8 payload. Directories, symlinks, submodules, malformed
encoding, binary content and responses above the one-MiB decoded ceiling fail
closed. Repository and file arguments are encoded as URL path segments after
rejecting absolute paths, empty segments and `.`/`..`; they cannot replace the
admitted API origin. Repository navigation first resolves the configured branch
(falling back to the commit endpoint for a SHA, tag or other ref like the SDK), then accepts only a complete
bounded recursive Git tree. Main/active whole-tree tools take no arguments, as
their current SDK schemas require; directory listing is active-branch scoped.
The GraphQL path shares the same invocation-scoped pool, immutable credentials,
origin and timeout policy, but uses a fixed POST body and a separately derived
GitHub-documented endpoint. Provider error messages and partial data are never
returned to the model or retained in diagnostics.

Connection checking remains a Main API responsibility. Main owns caller and
project authorization, configuration/revision selection, request bounds,
audit, and the public single/batch response contract. Its current
`internal/api/v2/configurations/handler.go::{CheckConnection,BatchCheckConnections}`
handlers return unconditional success, whereas Python worker
`agents/sdk_adapter.py::EliteaSdkConfigurationAdapter.check_connection`, legacy
indexer worker `methods/indexer_check_connection.py`, and SDK
`configurations/github.py::GithubConfiguration.check_connection` perform the
real family probe. The target Main composition dispatches a typed bounded
validation operation to the same Rust GitHub client rather than adding a second
Go GitHub implementation. If a family later remains entirely inside a
Main-owned connector with the same credential and egress boundary, Main may
keep its probe there; the invariant is one family client per execution plane,
not one prescribed language.

The partial read profile now enters live agent materialization. The standalone
deployment proves the complete branch-read loop. Main freezes the configured
GitHub toolkit, Rust exposes 20 selected reads beside another toolset, the model
calls `list_branches_in_repo`, the configured GitHub request succeeds, and the
terminal result remains visible after chat reload. Production readiness still
needs platform egress, private-DNS and Enterprise-CA policy. GitHub App tool
execution needs an installation-token exchange. The remaining 24 SDK
operations include six indexing-only operations. Sensitive effects need direct
HITL fencing before registration.

### Google Places complete read family

Google Places is the first complete fixed-tool family after the GitHub
reference kernel. The current source baseline is SDK
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, Python worker's pinned SDK
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, and legacy indexer worker
`62656d2b3bd51ded513693a1bd9b2f3a303ce09c`. Main freezes `results_count`,
the selected tools and the owned Google Places configuration through
`internal/application/agentexecution/tools.go`, then
`internal/infra/storage/configurations_materializer.go` resolves the nested
`api_key` into the claim-scoped input. Application and ad-hoc execution both
carry that same snapshot through
`elitea_worker/agents/sdk_adapter.py::EliteaSdkAgentAdapter`; Rust therefore
uses one family and does not fork the business lifecycle by agent kind.

The SDK source is
`elitea_sdk/tools/google_places/{__init__,api_wrapper}.py` with
`googlemaps==4.10.0`. It exposes exactly two read tools: `places(query)` and
`find_near(current_location_query,target,radius=3000)`. Empty selection means
both. The Python implementation uses legacy Text Search, per-place Details,
Geocoding and Nearby Search endpoints. Its `places` list comprehension calls
Details twice for every successful result, so one page can cause 41 requests;
its validator also stores the client in class-shared state, allowing one
wrapper's key to replace another's. Those are implementation defects, not
business contracts, and are not reproduced.

`src/toolkits/families/google_places/config.rs` accepts only the
claim-materialized nested credential, zeroizes the invocation-scoped key, and
keeps secret-bearing values non-`Clone` and non-`Debug`. Null or zero
`results_count` means the provider first-page maximum of 20, positive larger
values clamp to 20 like the current first-page behavior, and negative or
malformed values fail before client creation. `tools.rs` exposes both native
ADK-Rust read-only tools through the shared materialization policy. It makes
`target` truthfully required: the SDK schema calls it nullable, but its null
normalization removes the positional argument and the implementation then
fails before a provider call. Radius null still becomes 3,000 meters and the
admitted range is Google's documented 1 through 50,000 meters.

`client.rs` intentionally projects the same user-visible operation over the
supported Places API (New), rather than copying a legacy wire API that Google
has frozen. `places` performs one
`POST https://places.googleapis.com/v1/places:searchText` with a fixed field
mask and sensitive API-key header, returns the current numbered name/address/
place-ID/phone/website text, and preserves the exact no-result message.
`find_near` performs one bounded Geocoding request and one location-biased Text
Search New request because Nearby Search New does not accept the SDK's free
text target. It returns bounded typed JSON instead of Python's unstable list
`repr`. Both paths use one invocation-scoped pooled HTTPS-only client, disable
redirects, cap request/response/result sizes, perform one asynchronous attempt,
and retain no query, location, key, URL or provider body in errors. Main and the
authorized lifecycle remain the retry and deadline owners.

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main `internal/application/agentexecution/tools.go` and `internal/infra/storage/configurations_materializer.go` | Freeze the selected family and claim-materialize its owned API key for application and ad-hoc execution | `config.rs` parses only that sealed shape; no environment fallback, lookup, global client or second credential authority |
| Python worker `agents/sdk_adapter.py::EliteaSdkAgentAdapter::{execute_application,execute_adhoc}` | Both agent kinds delegate the same selected toolkit snapshot to the SDK | Future authorized Rust assembly selects this same native family for both kinds; current capability registration stays empty |
| SDK `tools/google_places/api_wrapper.py::{places,fetch_place_details,format_place_details}` | Text query, first-page result count, numbered visible fields and no-result message | `client.rs::GooglePlacesApi::places` uses one Places New request with a fixed field mask; it removes duplicate detail fan-out and class-shared credentials while preserving the callable result |
| SDK `tools/google_places/api_wrapper.py::find_near` | Geocode a starting location, search a target with a radius bias, return the first bounded provider page | `client.rs::GooglePlacesApi::find_near` uses Geocoding plus location-biased Text Search New, validates target/radius before transport and returns stable typed JSON; neither the legacy nor new API promises strict containment |
| SDK `tools/google_places/__init__.py::GooglePlacesToolkit` and `tools/base/tool.py::BaseAction` | Empty/subset selected-tool semantics, top-level null normalization and read grouping | `tools.rs` plus the shared invocation/policy kernel expose exactly `places` and `find_near`, default null radius to 3,000, and reject the misleading missing/null target before provider use |

The current configuration catalog declares no Google Places connection check,
so Rust does not invent one. Main retains the public check-connection/auth/
revision/audit boundary. Production registration additionally waits for an
authorized toolkit assembler, restricted billing-key component proof, and a
product/legal decision on Google Maps attribution and persistence/caching of
Places content in tool results and checkpoints. A passing capability-disabled
unit suite is not that policy approval. The tools are safe to call concurrently,
but the future authorized assembler must still apply the invocation-wide
bounded ADK tool-concurrency policy; reqwest's idle-pool setting is not an
execution-concurrency limit.

### Sonar complete read family

Sonar is a complete one-tool read family, not a generic REST proxy. Evidence was
refreshed on 2026-08-18 against SDK
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, Python worker's pinned SDK
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, legacy indexer worker
`62656d2b3bd51ded513693a1bd9b2f3a303ce09c`, and the Rust-branch Main/Python
source at `557c3c7fd23eed5f0b8d2d1679f7d3367e1a6981`. The only SDK change between
the worker pin and current catalog is the `read` group annotation; the
configuration, tool schema, request, output and failure behavior are otherwise
unchanged.

Main
`internal/application/agentexecution/tools.go::CurrentApplicationToolSnapshotService.FreezeCurrentApplicationVersion`
freezes the selected tool, Sonar project and nested configuration.
`internal/infra/storage/configurations_materializer.go::{materializeAgentExecution,materializeCurrentAgentTools}`
redeems the token into the claimed input, and Python worker
`agents/sdk_adapter.py::EliteaSdkAgentAdapter::{execute_application,execute_adhoc}`
carries the same immutable snapshot for both agent kinds. Rust therefore has
one invocation-scoped family owner; the model never chooses the origin,
credential or project.

The current SDK sources are
`elitea_sdk/configurations/sonar.py::SonarConfiguration`,
`elitea_sdk/tools/code/sonar/__init__.py::SonarToolkit` and
`elitea_sdk/tools/code/sonar/api_wrapper.py::SonarApiWrapper`. Empty selection
means the sole `get_sonar_data(relative_url,params=null)` read tool. The wrapper
parses a JSON query object, overwrites `componentKeys` with the configured
project, performs one Basic-token GET and returns decoded provider JSON. The
callable annotation says `str`, but the actual successful value is an object.
That object result is the business contract.

The Python implementation also stores its `requests.Session` in class state,
so constructing a second wrapper replaces the first wrapper's credential. It
has no timeout, follows redirects, accepts path and query escape in
`relative_url`, accepts arbitrary valid JSON until a later `TypeError`, and
places a Python traceback in the invalid-JSON `ToolException`. Those are
implementation defects, not behavior to preserve.

`src/toolkits/families/sonar/config.rs` validates one HTTPS configured origin
with an optional trusted context path, rejects userinfo/query/fragment/encoded
path ambiguity, and owns a bounded zeroized non-`Clone`, non-`Debug` token.
`client.rs` appends the fixed `/api/issues/search` endpoint, disables redirects,
uses a sensitive Basic `token:` header and one invocation-scoped connection
pool, and performs one asynchronous request. The public argument remains for
schema compatibility but accepts only exact `/api/issues/search`.

The query parser accepts a bounded JSON object and a versioned allowlist of
public issue-search filters. It rejects nested values, unknown parameters and
every alternate project/component scope; caller `componentKeys` is discarded
and the claim-materialized project is injected exactly once. `p` and `ps` are
positive integers no greater than 100 with a 10,000-item window, missing `ps`
becomes 100, and array/scalar/query/body/decoded-issue/serialized-output limits
are explicit. The bounded successful JSON object is returned without inventing
or silently truncating provider fields. Errors retain only a stable code and
retryability; token, origin, project, filters, provider text and body never
enter diagnostics.

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main tool freezer and configuration materializer | Freeze exact selected tool/project/revision and redeem the owned token only inside a claimed execution | `config.rs` consumes only the materialized nested shape; no environment fallback, lookup, global client or second credential authority |
| Python worker `EliteaSdkAgentAdapter::{execute_application,execute_adhoc}` | Both kinds use the same frozen standard-toolkit dispatch | Future authorized Rust assembly selects this same family for both; capability registration remains empty |
| SDK `SonarApiWrapper::parse_payload_params` | Missing/null/empty params mean no caller filters; valid JSON supplies issue filters | `client.rs` accepts an object only, bounds its complete encoded form and emits stable `InvalidInput`/`ResourceExhausted` errors without traceback |
| SDK `SonarApiWrapper::get_sonar_data` | One issue-search request, configured project overrides caller scope, decoded JSON returned unchanged | `SonarApi::get_sonar_data` keeps one GET and raw bounded object result while fixing endpoint/origin escape, redirect, timeout and class-shared credential defects |
| SDK `SonarToolkit` and shared `BaseAction` | Empty/subset selection, nullable `params`, read grouping and toolkit-name description | `tools.rs` plus the shared invocation/policy kernel expose exactly `get_sonar_data`, truthfully constrain its schema and reapply the immutable blocklist |

Neither current `SonarConfiguration` nor `SonarToolkit` implements a
connection check. Legacy
`methods/indexer_check_connection.py::indexer_configuration_check_connection`
returns `Check connection is not implemented yet for sonar`, and the static
catalog declares `check_connection_supported=false`. Rust therefore does not
invent a public check. Main retains public authorization, revision, audit and
connection-status ownership; its future route may delegate a real bounded probe
to this same family client if the product contract adds one.

Production registration remains disabled until the authorized toolkit
materializer composes this owner into both agent kinds and a credentialed
component test proves deployment CA/private-DNS policy, Basic-token
compatibility, exact project isolation, redirects disabled, and Sonar's
documented `503` behavior while issue indexing is active. The future assembler
also owns the invocation-wide tool-concurrency ceiling; the HTTP idle-pool
setting is not an execution limit.

### Azure Search complete read family

Azure Search is a complete two-tool read family over one configured index. The
evidence baseline is current SDK
`9bba9da409771803f28c0ee21f5d0b9a8f456219` (`0.9.19`), Python worker's
pinned SDK `b5113a129329b85d23c2d5c2bf55f18e307414ec` (`0.9.8`), legacy
indexer worker `62656d2b3bd51ded513693a1bd9b2f3a303ce09c`, and
`azure-search-documents==11.5.2` with `azure-core==1.30.2`. The current SDK
only adds `read` group annotations to this family; its public schemas, request
construction and result projection otherwise match the worker pin. The lean
standalone Python worker image does not install `azure-search-documents`, so
the source contract is evidence, not proof that the current Python deployment
can execute this family.

Main freezes `index_name`, selected tools and the owned
`azure_search_configuration` through
`internal/application/agentexecution/tools.go`, then
`internal/infra/storage/configurations_materializer.go` resolves the endpoint
and API key into the claim-scoped input. Python worker
`agents/sdk_adapter.py::EliteaSdkAgentAdapter` carries the same snapshot for
application and ad-hoc execution. The model can choose search text, a bounded
result count, ordering, selected fields or a document key; it cannot replace
the endpoint, index, API key or API version.

SDK `elitea_sdk/tools/azure_ai/search/api_wrapper.py::AzureSearchApiWrapper`
registers exactly `text_search` and `get_document`; vector and hybrid helpers
are present but commented out of `get_available_tools`. Empty selection means
both in source order. `api_version`, `api_base`, `openai_api_key` and
`model_name` do not affect either registered read. The wrapper creates
`SearchClient` without an API version, so its exact wire uses stable
`2024-07-01`:

- `text_search` posts to
  `/indexes('{index}')/docs/search.post.search?api-version=2024-07-01` with
  `search`, optional `top`, comma-joined `orderby` and comma-joined `select`;
- `get_document` gets
  `/indexes('{index}')/docs('{key}')?api-version=2024-07-01` with optional
  `$select`;
- both send a sensitive `api-key` and
  `Accept: application/json;odata.metadata=none`.

The Python default `limit=-1` omits `top`, and `list(SearchItemPaged)` then
posts every provider continuation body to the same route without a total page,
item, byte or time limit. Empty field/order arrays also become empty strings due
to an unreachable normalization condition. Rust deliberately maps null or
`-1` to `top=100`, admits only `1..=100`, makes one bounded request and omits
empty arrays. It never follows `@odata.nextLink` or
`@search.nextPageParameters`. This is a visible resource-safety correction,
not claimed byte-for-byte behavior.

Search output remains a list of provider document objects. As in SDK
`_paging.py::convert_search_result`, every item contains
`@search.score`, `@search.reranker_score`, `@search.highlights` and
`@search.captions`, inserting null when absent; the underscore spelling of
`reranker_score` intentionally preserves Python's projection rather than the
REST `@search.rerankerScore` name. Document lookup returns the bounded provider
object unchanged.

`src/toolkits/families/azure_search/config.rs` parses only the materialized
HTTPS origin, provider-valid index name and zeroized non-`Clone`, non-`Debug`
key. `client.rs` owns one invocation-scoped pool, disables redirects and
automatic retries, fixes the API version and original origin/index, encodes
document keys, caps query lists/request/body/result/output, and retains no
endpoint, key, query or provider body in diagnostics. `tools.rs` publishes the
two native ADK tools in SDK order and preserves their `read` classification.
That classification is independent from environment-sensitive policy: the
shared direct-tool HITL adapter may still require an exact `interrupt_id`
approval for either read.

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main tool freezer and configuration materializer | Freeze the exact index/tool selection and redeem the owned endpoint/key only inside one claimed execution | `config.rs` accepts the nested materialized shape and creates no environment fallback, global client or second credential authority |
| Python worker `EliteaSdkAgentAdapter::{execute_application,execute_adhoc}` | Both agent kinds delegate the same standard-toolkit snapshot | Future authorized assembly selects this same family for both kinds; capability registration remains empty |
| SDK `AzureSearchApiWrapper::{validate_fields,text_search}` plus Azure Search SDK 11.5.2 | One configured-index text search, optional order/projection and Python result metadata | `client.rs::AzureSearchApi::text_search` retains the wire/result meaning while bounding `-1`, order clauses, selected fields, response bytes and total items and refusing continuation fanout |
| SDK `AzureSearchApiWrapper::get_document` | Retrieve one key from the configured index with optional selected fields | `client.rs::AzureSearchApi::get_document` preserves the encoded key route and bounded provider dictionary |
| SDK `AzureSearchToolkit` and shared `BaseAction` | Empty/subset selection, top-level null normalization, descriptions and `read` grouping | `tools.rs` plus the shared invocation/policy kernel expose exactly the two registered tools and apply the immutable deployment policy |

Connection checking is contradictory upstream and is not reproduced. The
configuration class has no check, the static catalog truthfully says
`check_connection_supported=false`, and the legacy configuration path returns
unsupported. The toolkit schema separately attaches a broken method that reads
nonexistent top-level fields and probes Azure OpenAI deployments rather than
Azure Search. Current Go Main's generic route also returns unconditional
success and is not provider evidence. Rust exposes no check in this slice.

Production registration remains disabled until the authorized tool materializer
composes the family into both agent kinds and a credentialed component test
proves real TLS/private-endpoint policy, exact index isolation, bounds, error
redaction and configuration-driven sensitivity. A future truthful Search probe
may reuse this family client, but must not revive the Azure OpenAI check.

### ServiceNow complete incident family

The source baseline is current SDK
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, worker-pinned SDK
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, Python worker/Main
`1282fc7d2111a46d3e2e7ec15ecdc013e8b3c93e` and exact transport dependency
`pysnc==1.1.10`. The current-versus-pinned SDK diff only adds the
`read`/`write`/`write` group annotations; configuration, public argument
schemas, operations and results are otherwise unchanged.

Main freezes `{type, toolkit_name, settings}` through
`internal/application/agentexecution/tools.go` and claim-time
`internal/infra/storage/configurations_materializer.go` resolves the nested
`servicenow_configuration`. Python worker
`agents/sdk_adapter.py::EliteaSdkAgentAdapter` passes that same frozen snapshot
to both application and ad-hoc SDK entry points. Rust therefore accepts only
the claimed nested configuration and has no environment or secondary lookup
fallback.

SDK `tools/servicenow/api_wrapper.py::ServiceNowAPIWrapper` registers all three
incident operations in this exact order:

1. `get_incidents` (`read`) accepts an optional filter object;
2. `create_incident` (`write`) accepts an optional bounded JSON field object,
   including numeric or nested provider values and an intentional empty
   default incident;
3. `update_incident` (`write`) accepts a required `sys_id` and a JSON-object
   string of fields to update.

Empty selection means all three; a nonempty subset preserves source order.
Successful tools return the SDK-compatible JSON *string* containing an array
of raw-value incident records. With `sysparm_display_value=all`, `pysnc`
unwraps each `{value, display_value, link}` field to the non-null raw value and
falls back to `display_value` when the raw value is null; Rust preserves that
projection and discards the provider envelope.

`pysnc` uses Basic authentication and fixed Table API routes under
`/api/now/table/incident`. Every request carries
`sysparm_display_value=all`, `sysparm_exclude_reference_link=true` and
`sysparm_suppress_pagination_header=true`. List additionally sends the encoded
query, response fields, limit and offset zero. Create is one POST expecting
201. Update deliberately retains the source's GET-then-PATCH behavior so a
missing incident fails before mutation; PATCH expects 200. Neither effect is
retried by the family.

Rust fixes bounded defects without removing functionality:

- one materialized toolkit owns one non-`Clone`, non-`Debug`, zeroizing
  credential and HTTP pool; this removes the SDK validator's class-global
  endpoint/credential/field overwrite;
- HTTPS origin or a validated short instance is fixed before invocation;
  redirects, userinfo, query/fragment and argument-selected destinations are
  rejected;
- missing credentials fail during materialization instead of later client
  construction;
- null response fields use the SDK's ten-field defaults, and an empty string
  also uses those defaults rather than silently requesting every field;
- `number_of_entries` defaults to 100, is bounded to `1..=100`, controls only
  the first-page limit and is no longer accidentally emitted as a table
  column filter;
- filter keys are limited to the documented incident fields; encoded-query
  metacharacters, oversized values, invalid 32-character `sys_id`s, malformed
  update JSON and unbounded field sets fail before network use;
- request, streamed response, projected output, argument depth/node/string
  counts and deadlines are bounded; provider bodies, credentials, URLs,
  filters and update data never appear in diagnostics;
- reads expose transient retry hints but the client performs one attempt;
  create/PATCH transport and status ambiguity is never advertised retryable.
- `pysnc` silently drops an update field when it was absent from the configured
  GET projection. Rust deliberately sends every validated field requested by
  the caller, preventing a successful-looking update from losing data.

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main freezer/materializer and Python shared SDK adapter | Claim-scoped nested configuration, selected tools and one application/ad-hoc family contract | `config.rs` parses the exact materialized authority and creates no process-global or environment authority |
| SDK `ServiceNowAPIWrapper::get_incidents` and `pysnc::GlideRecord.query` | Documented equality/description filters, default fields, one ordered first page and raw-value JSON-array string | `client.rs::ServiceNowApi::get_incidents` preserves the result meaning while fixing the accidental control filter and bounding query, fields, results and bytes |
| SDK `ServiceNowAPIWrapper::create_incident` | Create one incident, omit null fields, permit an empty body and return the created raw-value record | `client.rs::ServiceNowApi::create_incident` performs one non-retried POST with bounded body/result and stable ambiguity-safe errors |
| SDK `ServiceNowAPIWrapper::update_incident` | Read the exact incident first, omit null updates, patch once and return the updated raw-value record | `client.rs::ServiceNowApi::update_incident` validates the `sys_id` and bounded JSON object, then performs the same GET/PATCH fanout without write retry; unlike `pysnc`, it does not silently discard requested fields missing from the GET projection |
| SDK `ServiceNowToolkit` and shared `BaseAction` | Exact catalog order, empty/subset selection, top-level null normalization and operation groups | `tools.rs` plus the shared invocation/policy kernel expose all three tools; effects are not omitted |

Neither the configuration nor toolkit defines `check_connection`; current
catalogs truthfully report it unsupported. Rust does not invent a probe. A
future check can reuse the same origin-bound client only after Main defines a
truthful provider contract.

Production registration remains disabled, not functionally reduced. The read,
create and update implementations are all compiled and tested. Authorized
materialization, a credentialed ServiceNow component test and the shared
durable exact-`interrupt_id` guardrail remain activation gates. Before either
write is enabled, an owned cancellation-safe effect boundary must also retain
effect identity through provider completion and reconcile an ambiguous
POST/PATCH rather than settling cancellation as if no effect occurred. Group
metadata does not decide sensitivity: policy may require approval for the read,
either write, all three or none.

### Salesforce complete CRM family

The source baseline is current SDK
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, worker-pinned SDK
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, Python worker/Main
`1282fc7d2111a46d3e2e7ec15ecdc013e8b3c93e` and the worker's
`requests==2.34.0` transport contract. The current-versus-pinned SDK behavior
diff only adds `write`/`read`/`execute` group annotations. Both SDK versions
define the same configuration, callable arguments, lazy OAuth flow and result
shapes; neither contains focused Salesforce family tests.

Main freezes `{type, toolkit_name, settings}` in
`internal/application/agentexecution/tools.go` and claim-time
`internal/infra/storage/configurations_materializer.go` resolves the nested
`salesforce_configuration`. Python worker
`agents/sdk_adapter.py::EliteaSdkAgentAdapter::{execute_application,execute_adhoc}`
passes the same frozen tool snapshot into both SDK entry points. Rust accepts
only this materialized nested authority. It has no environment credential or
secondary configuration lookup.

SDK `tools/salesforce/api_wrapper.py::SalesforceApiWrapper` exposes the full
family in this order:

1. `create_case` (`write`) creates one Case from subject, description, origin
   and status;
2. `create_lead` (`write`) creates one Lead from last name, company, email and
   phone;
3. `search_salesforce` (`read`) runs one complete SOQL query; the required
   `object_type` remains a compatibility label and does not alter the query;
4. `update_case` (`write`) changes status and optionally a nonempty
   Description;
5. `update_lead` (`write`) changes nonempty Email and/or Phone;
6. `execute_generic_rq` (`execute`) calls a version-relative Salesforce REST
   resource using GET, POST, PATCH or DELETE.

Empty selection means all six and a subset retains this source order. The Rust
catalog does not hide effects. Each model-facing description distinguishes the
business operation, read/effect behavior, retry ambiguity and preferred
dedicated tool. Every argument schema states its required format, null/default
meaning, bound and useful example where one affects selection. This is a
deliberate clarity improvement over the SDK's one-line action labels: tool
selection quality and avoidable retry tokens are part of the functional
contract, while the operations and result meaning remain unchanged.

The client lazily POSTs the client-credentials form to
`/services/oauth2/token`, then binds all resource requests to the configured
HTTPS origin and `/services/data/{api_version}` root. Dedicated creates POST
the source field names, search encodes one `q` pair and returns the first page,
and updates PATCH a validated 15- or 18-character Salesforce record ID.
Generic GET maps its JSON-object string to scalar query pairs; generic
POST/PATCH/DELETE use it as a JSON body. A successful JSON response is returned
as the SDK-compatible object/array, while 204 update/generic success returns
the same `{success,message}` shape.

Rust preserves functionality while closing source defects and unbounded
behavior:

- one materialized toolkit owns one non-`Clone`, non-`Debug`, zeroizing client
  credential, lazy token and bounded HTTP pool;
- only an exact HTTPS origin and `v<digits>.<digits>` API version are admitted;
  redirects, userinfo, path/query/fragment authority and argument-selected
  origins are rejected;
- request, response, output, token, SOQL, generic path/query and recursive JSON
  sizes are bounded; diagnostics contain no token, credential, URL, query,
  payload or provider body;
- the client disables reqwest's implicit protocol retries. It refreshes and
  replays once only after an explicit provider 401, which proves rejection;
  transport loss, timeout, rate limit or 5xx after an effect dispatch is an
  `UnknownOutcome`, never automatic retry authority;
- search intentionally returns one page instead of following unbounded
  `nextRecordsUrl` fanout;
- Case Description keeps the SDK rule that omitted/empty means unchanged and
  cannot clear the field; Lead update rejects the SDK's no-op empty PATCH;
- generic method is the documented uppercase enum, GET uses query parameters,
  and a version-root-relative path cannot supply a scheme, authority, query,
  fragment, percent escape or traversal component.

| Current business source | Preserved behavior | Rust owner / deliberate hardening |
| --- | --- | --- |
| Main freezer/materializer and Python shared SDK adapter | Claim-scoped nested configuration, selected tools, stable toolkit name and one application/ad-hoc family contract | `config.rs` parses that exact authority and creates no global credential or lookup fallback |
| SDK `SalesforceApiWrapper::{authenticate,_headers,create_case,create_lead}` | Lazy client-credentials token plus dedicated Case/Lead POST field maps and raw JSON results | `client.rs::SalesforceApi::{create_case,create_lead}` preserves the calls and results through one bounded origin-bound client; effect ambiguity is typed and nonretryable |
| SDK `SalesforceApiWrapper::search_salesforce` | Required compatibility label, complete caller SOQL and first raw query page | `client.rs::SalesforceApi::search_salesforce` preserves the label/query contract, encodes one `q` pair and bounds the single returned page |
| SDK `SalesforceApiWrapper::{update_case,update_lead}` | PATCH one exact record, omit empty optional fields and return the 204 success object | Rust validates Salesforce IDs, retains the Case no-clear rule and rejects the source's empty Lead no-op before network use |
| SDK `SalesforceApiWrapper::execute_generic_rq` | Version-relative GET/POST/PATCH/DELETE escape hatch and JSON-string params | Rust retains all four methods while fixing lowercase/method ambiguity, GET query semantics and route confinement; DELETE is not omitted |
| SDK `SalesforceToolkit`, model classes and shared `BaseAction` | Exact source order, empty/subset selection, public arguments and operation groups | `tools.rs` plus the shared invocation/policy kernel expose all six native ADK tools with selection-oriented descriptions and immutable block filtering |

Neither Salesforce configuration nor toolkit defines `check_connection`; the
current catalogs report it unsupported and Rust does not invent a probe.
Client construction is offline and OAuth occurs only at real invocation. A
future explicit check can reuse the same bounded client after Main defines its
authorization and audit contract.

Production registration remains disabled, not functionally reduced. The read,
create, update and generic delete-capable implementations are compiled and
tested. Activation requires the authorized application/ad-hoc materializer, a
credentialed Salesforce component proof, and the shared durable
exact-`interrupt_id` guardrail. Operation group is not authorization: trusted
configuration may mark search, one write, the generic tool, all six or none as
sensitive. After effect dispatch, the owned invocation must retain effect
identity through provider completion or explicit unknown-outcome recovery;
dropping an ADK tool future must not settle cancellation as proof that a
create, PATCH or DELETE did not happen.

### Slack complete messaging family

Slack is a complete seven-tool family over the fixed Slack Web API origin. The
source baseline is SDK `9bba9da409771803f28c0ee21f5d0b9a8f456219` and the
Python worker's pinned SDK source
`b5113a129329b85d23c2d5c2bf55f18e307414ec`; their Slack behavior is identical
apart from the current source's `read`/`write` group annotations. The worker's
two SDK patches are MCP-only. The source declares `slack_sdk==3.35.0`, although
the standalone Python worker's selective dependency lock does not install that
extra; therefore deployment availability is not used as parity evidence.

Main freezes `type`, `toolkit_name`, selected tools and the nested Slack
configuration, then claim-scoped materialization resolves its token for both
application and ad-hoc execution. Rust accepts authority only from that nested
claimed map. One non-`Clone`, non-`Debug`, zeroizing token owner creates an
invocation-scoped pool against exactly `https://slack.com/api/`; arguments
cannot select another host, redirects and reqwest retries are disabled, the
Bearer header is sensitive, and request, response, projection and deadlines
are bounded.

The complete catalog remains in SDK order:

1. `send_message` (`write`) posts a top-level message or thread reply and
   returns `success`, `channel_id`, `ts` and the supplied `thread_ts`.
2. `read_messages` (`read`) returns the newest first-page messages projected to
   `ts`, `user`, `message`, `app_name` and optional `thread_ts`.
3. `create_slack_channel` (`write`) creates one public or private channel.
4. `list_channel_users` (`read`) projects first-page member IDs and names.
5. `list_workspace_users` (`read`) projects first-page IDs, names, bot flags,
   email and team fields.
6. `invite_to_conversation` (`write`) changes channel membership and retains
   Slack's bounded successful response object.
7. `list_workspace_conversations` (`read`) projects the first public-channel
   page; the source description incorrectly promises groups and DMs even
   though it does not set Slack's `types` argument.

Tool descriptions and every parameter schema are treated as executable model
contracts. They distinguish configured fallback from explicit conversation
IDs, include representative IDs and timestamps, explain result shapes and
first-page behavior, state the history limit (`1..=15`, default 10), the
channel-name grammar and invitee bound, and warn that visible-content and
membership effects cannot be blindly retried after an unknown outcome. The
send contract also states that the source-compatible Slack defaults retain
`mrkdwn` parsing and link unfurling rather than calling the content plain text. These
clarifications intentionally replace misleading source prose, including the
source channel-name parameter that describes a destination ID.

| Python/SDK source | Observable responsibility | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `SlackConfiguration`, `SlackToolkit::get_tools` and Main/Python worker materialization | Nested token/default channel, empty/subset selection, source order, toolkit identity and groups | `config.rs` and `tools.rs` retain the exact seven operations, fail unknown selections, keep credentials out of model metadata and apply immutable deployment policy |
| SDK `SlackApiWrapper::send_message` and `slack_sdk::WebClient.chat_postMessage` | Configured target fallback, optional thread reply and posted-message identity | `client.rs::SlackApi::send_message` performs one non-retried bounded effect and preserves the successful projection; transport or response ambiguity is explicit `UnknownOutcome` |
| SDK `SlackApiWrapper::read_messages` | Latest first-page history plus the exact browser-useful message projection | Rust performs one history request instead of the source's redundant preceding `auth.test`, caps broadly deployable history at 15 and does not fetch continuations |
| SDK `SlackApiWrapper::create_slack_channel` | Public/private channel creation and returned channel ID | Rust validates the documented lowercase 80-character name, sends one effect attempt and returns the same success meaning |
| SDK `SlackApiWrapper::list_channel_users` | First membership page followed by one `users.info` lookup per ID | Rust caps the member page and runs at most eight lookups concurrently, drains them without detached tasks and restores original membership order; source fanout is unbounded and sequential |
| SDK `SlackApiWrapper::{list_workspace_users,list_workspace_conversations}` | First provider pages and fixed field projections | Rust makes the page bounds explicit, preserves nullable fields and truthfully describes public-channel-only behavior |
| SDK `SlackApiWrapper::invite_to_conversation` | Comma-joined user invitation and raw successful response | Rust validates one through 100 unique IDs, preserves caller order, bounds the result and never retries the effect after ambiguity |

Slack's configuration model has no connection check and the current catalog
truthfully records `connection_check_supported=false`. The toolkit schema's
dynamically attached check is not evidence: it reads a nonexistent top-level
token, returns a dictionary where the shared decorator expects an HTTP
response, and can fail before a Slack request. Rust does not reproduce or
advertise it. A future check must be an explicit Main-owned audited `auth.test`
operation using this same bounded client.

Production registration remains disabled, not functionally reduced. Sending,
channel creation and invitations are implemented effects. Their activation
requires exact durable approval keyed by `interrupt_id` plus the canonical
target and arguments, and an owned post-dispatch outcome/reconciliation
boundary. Reads are not automatically safe: trusted configuration may also
mark message, membership or user-email access sensitive. Operation group never
grants execution authority. Live Slack scope/rate-limit proof and both
application/ad-hoc claim-materialization component tests remain gates.

### Rally complete WSAPI family

Rally is a complete eight-tool family over its fixed WSAPI v2.0 root. The
source baseline is SDK `9bba9da409771803f28c0ee21f5d0b9a8f456219`, the
Python worker's pinned SDK source is
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, and the two worker patches are
MCP-only. Current and pinned Rally behavior differs only by current
`read`/`write` group metadata. `pyral==1.6.0` is the provider implementation
evidence; the standalone Python worker's selective dependency lock does not
install it, so package presence is not treated as deployed-runtime proof.

Main freezes `type`, stable `toolkit_name`, selected operations, optional
workspace/project and the nested Rally configuration. Claim-scoped
materialization resolves either the API key or username/password only for an
accepted application or ad-hoc command. Rust accepts authority only from that
nested map. API-key authentication takes source-compatible precedence over
stale Basic fields. A bare saved hostname and a pathless HTTPS origin normalize
to the same exact authority; HTTP, userinfo, alternate paths, query and fragment
input fail before a request.

The SDK constructs `pyral.Rally` inside a Pydantic validator and assigns it to
a class attribute. Construction performs user, subscription, workspace,
project and schema requests, and a second wrapper can replace the first
wrapper's endpoint, credential and context. Rust deliberately performs none of
that during parsing, catalog enumeration or assembly. One non-`Clone`,
non-`Debug`, zeroizing invocation owner creates a lazy bounded client with no
redirect or implicit retry. Workspace/project references are resolved lazily
inside that owner and never enter model-visible descriptions.

The complete source-order catalog is:

1. `get_types` (`read`) lists visible WSAPI entity API names.
2. `get_entities` (`read`) performs one bounded entity/query read. Story,
   UserStory and User Story map to HierarchicalRequirement, while a safe
   `PortfolioItem/<Subtype>` route remains supported.
3. `get_project` (`read`) selects projects by exact name or the configured
   context.
4. `get_workspace` (`read`) selects workspaces by exact name or configured
   context.
5. `get_user` (`read`) optionally selects exact UserName values.
6. `get_context` (`read`) joins the project, workspace and user reads into one
   structured result; the independent requests use structured concurrency.
7. `create_artifact` (`write`) sends the source-compatible WSAPI `PUT` to the
   dynamic entity's `/create` route.
8. `update_artifact` (`write`) resolves FormattedID when necessary and sends
   one WSAPI `POST` to the exact ObjectID route.

There is no public delete operation in `RallyApiWrapper.get_available_tools`;
the fact that `pyral` internally exposes delete is not authority to add a ninth
tool. Conversely, both public writes are implemented and are not omitted while
HITL is under construction.

Tool and parameter descriptions are tested model contracts. They explain when
to list entity types, the Rally query expression and examples, `fetch`
semantics, exact-name/default behavior, first-page result shapes, the `1..=100`
entity cap, canonical field names, identifier requirements and the
non-retryable ambiguity of create/update effects. This intentionally replaces
source descriptions such as “Get user stories” for an arbitrary entity query
and the incorrect user-only description on `get_context`.

| Python/SDK source | Observable responsibility | Rust owner / deliberate improvement |
| --- | --- | --- |
| `RallyConfiguration`, `RallyToolkit::get_toolkit` and Main/Python materialization | Nested credential, workspace/project, empty/subset selection, toolkit identity and source grouping | `config.rs` and `tools.rs` retain the exact eight operations with claim-scoped authority, stable order and immutable deployment filtering |
| `RallyApiWrapper::{get_types,get_entities}` and `pyral.query_builder` | Dynamic type names, aliases, caller query, fetch mode and result limiting | `client.rs` structurally encodes one WSAPI request and sets provider `pagesize` to the approved result cap instead of downloading the SDK's 1,000-record page and slicing afterward |
| `RallyApiWrapper::{get_project,get_workspace,get_user,get_context}` | Context discovery and exact-name reads | Rust repairs the SDK's `Name = "None"` fallback and ToolException-in-string composite, returns bounded structured JSON and keeps the three context reads owned |
| `RallyApiWrapper::{create_entity,update_entity}` and `pyral.Rally::{obtainSecurityToken,put,post}` | API-key or same-session Basic security-token effects and returned artifact identity | Rust retains `PUT /create` and `POST /{ObjectID}`, validates object input and identifiers, performs no automatic effect retry, and does not issue the SDK's failure-prone post-commit presentation GET |
| SDK eager schema validation/case normalization and `Rally._greased` | Friendly field-name normalization plus relationship-list shorthand using a downloaded workspace schema | Rust requires bounded canonical WSAPI ElementName keys, preserves `entity/123` list-to-`_ref` transformation and relies on the provider's effect response; only friendly case/custom-field normalization is intentionally stricter, removing eager schema fanout |

Successful effects retain the provider's available FormattedID or ObjectID in
the result. A transport/timeout failure after dispatch, a server failure after
dispatch, or an unprojectable successful effect response is a nonretryable
`UnknownOutcome`; neither cancellation nor an ADK timeout proves that Rally did
not commit. Reads can be marked sensitive independently of their `read` group,
and both writes require the shared exact-`interrupt_id` approval plus an owned
effect identity/reconciliation boundary before activation.

Neither the Rally configuration nor toolkit defines `check_connection`, and
the catalogs report it unsupported. Rust does not replace the SDK's accidental
constructor-time discovery with a hidden probe. A future connection check must
be an explicit audited contract using the same bounded client.

Production registration remains disabled pending authorized application/ad-hoc
materialization, both API-key and Basic live WSAPI proofs, the shared durable
HITL wrapper, and cancellation-safe effect reconciliation. The implementation
is complete rather than read-only: both create and update remain compiled and
tested behind those gates.

### Legacy Zephyr complete ZAPI step family

The independently registered legacy `zephyr` toolkit is a complete four-tool
family and is not an alias for `zephyr_scale`, `zephyr_squad`,
`zephyr_enterprise`, or `zephyr_essential`. Current SDK `9bba9da` and
worker-pinned SDK `b5113a1` expose the same four operations, schemas, order and
wire behavior; current adds only one `read` and three `write` group annotations.
Its credentials are inline `base_url`, `username`, and sealed `password`
settings in Main's frozen toolkit schema. The separately registered
`ZephyrConfiguration` is consumed by `zephyr_scale` and does not describe this
legacy Basic-auth client. Neither legacy toolkit source defines a connection
check or an indexing operation.

The complete source order is `get_test_case_steps`,
`add_new_test_case_step`, `add_test_case`, and `add_test_cases`. All four use
the fixed configured prefix followed by
`/teststep/{issue_id}?projectId={project_id}`. The read sends one GET and
projects `stepBeanCollection` to the source-compatible order_id, step, data,
and result message. Every write sends the same POST body with exact step, data,
and result fields. The two batch tools fully validate their JSON-string input
before network access, then preserve case and step order while using the same
single-step operation. No create, update-like append, or batch effect is
omitted.

Rust replaces the SDK's class-global client with one invocation-owned client
bound to an exact HTTPS origin and optional frozen path prefix. It keeps the
password zeroizing, uses Basic authentication only on that origin, verifies
TLS, rejects redirects and proxy inheritance, performs no automatic retry, and
bounds the request, two-MiB provider response, and 512-KiB model result. IDs
must be positive. Single-case batches allow 1 through 50 steps; multi-case
batches allow 1 through 20 cases and at most 100 total steps. The encoded JSON
string is limited to 64 KiB, matching the shared tool-admission boundary rather
than advertising an unreachable larger payload.

The source logs complete step bodies, returns provider text directly, and can
leave sequential batches partially applied while reporting only the failing
call. Rust emits no body, credential, origin, or identifier in diagnostics.
Definite read errors remain typed and redacted; a transport loss, timeout, 408,
429, 5xx, or unprojectable response after effect dispatch is nonretryable
unknown outcome. Once any step in a batch is confirmed, every later failure is
also an unknown partial outcome. Compact structured batch receipts report only
created case/step counts, avoiding the source's repetition of all model input.

Tool and parameter descriptions are tested model-selection contracts. They
state the exact read result, Jira issue/project ID roles, JSON object/array
shape and examples, empty data/result semantics, batch and byte limits,
sequential execution, duplicate risk, partial effects, and required
reconciliation. Read/write groups remain metadata; any tool can independently
be configured sensitive.

| Python/SDK source | Observable responsibility | Rust owner / deliberate improvement |
| --- | --- | --- |
| `tools/zephyr/__init__.py::{get_tools,ZephyrToolkit,toolkit_config_schema}` plus Main's frozen toolkit schema/materializer | Inline origin and Basic credentials, empty/subset selection, source order and groups | `config.rs` and `tools.rs` keep the exact four operations, reject unknown persisted selections, expose bounded selection-oriented schemas, and never use the incompatible Zephyr Scale configuration |
| `tools/zephyr/api_wrapper.py::ZephyrV1ApiWrapper` | Step projection, one-step create and sequential batch orchestration | `tools.rs` prevalidates complete batches, preserves ordering, emits compact receipts, and classifies partial effects for reconciliation |
| `tools/zephyr/Zephyr.py::Zephyr` | Basic-auth GET/POST ZAPI transport | `client.rs` binds one exact verified-TLS authority, disables ambient proxy/redirect/retry behavior, and bounds wire, projection and stable errors |

Production registration remains disabled until application/ad-hoc
claim-materialization fixtures and a harmless live legacy-ZAPI read prove the
saved endpoint contract, approved DNS/IP egress is enforced, the shared direct
sensitive-tool wrapper authorizes the exact invocation by durable
`interrupt_id`, and an effect owner durably records and reconciles every
single-step or partial-batch outcome across cancellation and restart.

### Zephyr Squad complete Jira test-management family

Zephyr Squad is a complete fifteen-tool family over the fixed
`https://prod-api.zephyr4jiracloud.com/connect` endpoint. The implementation
is based on SDK revision `9bba9da409771803f28c0ee21f5d0b9a8f456219` and
worker-pinned SDK revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec`; the two revisions have the
same methods, schemas and wire behavior, while the current revision adds the
five `read`, eight `write` and two `delete` group annotations. The worker's
two pinned SDK patches are MCP-only. The SDK has no focused Zephyr Squad
tests, making the Rust route/JWT/body fixtures primary compatibility proof.

Unlike the other Zephyr variants, `zephyr_squad` has no separately registered
configuration model and no `check_connection`. Its public toolkit settings
contain required `account_id`, secret `access_key`, secret `secret_key`, and
`selected_tools` defaulting to empty/all. Main's current toolkit snapshot and
generic freezer preserve this inline shape; the claim-scoped configuration
materializer is the only component allowed to redeem its sealed secret
values. `config.rs` accepts that materialized shape directly and never reads
credentials from the environment. The non-cloneable authority and client also
remove the SDK validator's class-global `_client` credential crossover.

The exact public catalog is preserved in source order:

1. `get_test_step` (`read`) reads one issue/project/step tuple.
2. `update_test_step` (`write`) replaces a step from a JSON object.
3. `delete_test_step` (`delete`) removes one step.
4. `create_new_test_step` (`write`) appends a step.
5. `get_all_test_steps` (`read`) uses the source's v2 test-step route.
6. `get_all_test_step_statuses` (`read`) reads the status catalog.
7. `get_bdd_content` (`read`) reads Gherkin content.
8. `update_bdd_content` (`write`) replaces Gherkin content.
9. `delete_bdd_content` (`delete`) sends the source-compatible `[]` body.
10. `create_new_cycle` (`write`) creates a cycle.
11. `create_folder` (`write`) creates a uniquely named cycle folder.
12. `add_test_to_cycle` (`write`) adds or copies tests by method.
13. `add_test_to_folder` (`write`) adds issue keys to a folder.
14. `create_execution` (`write`) creates a test execution.
15. `get_execution` (`read`) reads one execution and status.

`client.rs` reproduces the SDK's route spelling and query order because they
are part of the JWT QSH. Each request carries `Authorization: JWT` with
HS256 claims `sub`, `iss`, `iat`, `exp=iat+300`, and the SHA-256 query-string
hash, plus sensitive `zapiAccessKey`. Rust samples the clock once rather than
the SDK's two calls around a possible second boundary. It uses the existing
`ring` and `base64` dependencies, disables redirects and automatic retries,
and enforces fixed request, response and output limits.

The top-level `json` arguments remain strings for YAML/SDK compatibility but
are decoded before dispatch, must be bounded JSON objects rather than arrays
or scalars, and enforce the documented required/conditional fields. Opaque
path IDs are bounded single URL-safe segments and numeric issue/project IDs
must be positive. Model-facing descriptions state each tool's purpose,
required fields, formats, examples, result type and mutation/duplicate risk.
Provider JSON is returned as canonical JSON instead of Python's non-JSON
`str(dict)` representation; text remains bounded. Provider bodies, routes,
credentials and payloads never enter errors.

| Current business source | Preserved behavior | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `tools/zephyr_squad/__init__.py::{get_tools,toolkit_config_schema,get_toolkit}` and Main toolkit snapshot/freezer/materializer | Inline claim-materialized credentials, empty/subset selection, source order, toolkit identity and groups | `config.rs` and `tools.rs` retain all fifteen operations, reject unknown persisted selections and apply immutable deployment blocking without exposing secrets |
| SDK `ZephyrSquadApiWrapper::{validate_toolkit,get_available_tools}` | One concrete client and exact public schemas/descriptions | Invocation-scoped authority replaces the class-global client; tested selection-oriented descriptions and bounded schemas improve model choice without changing tool names |
| SDK `ZephyrSquadCloud` fifteen methods | Exact v1/v2 methods, paths, query names/order and BDD bodies | `client.rs::ZephyrSquadApi` preserves every route and method, validates path segments and sends one attempt only |
| SDK `_generate_jwt_token` | Five-minute HS256 JWT and QSH over method plus exact API path | Deterministic injected-clock signer has a fixed golden vector and marks both authorization headers sensitive |
| SDK `_do_request` | JSON or text success and provider failures | Bounded canonical JSON/text projection and stable redacted error taxonomy; post-dispatch transport, 408/429/5xx or decoding ambiguity for effects is nonretryable `UnknownOutcome` |

PyJWT is an undeclared direct SDK dependency currently obtained transitively
through the Python image dependency graph; the worker lock has no
family-specific import assertion. The Rust implementation removes that
fragility. Production registration remains disabled pending authorized
application/ad-hoc materialization, a credentialed harmless status-catalog
read against the currently supported SmartBear endpoint, the shared durable
exact-`interrupt_id` HITL wrapper, and cancellation-safe effect
identity/reconciliation. Any read may independently be configured sensitive;
catalog effect groups never authorize execution.

### Zephyr Enterprise business family

Zephyr Enterprise serves the five business operations of the worker-pinned
SDK revision `b5113a129329b85d23c2d5c2bf55f18e307414ec` (none of the five
worker patches touch Zephyr). The six inherited indexing tools
(`index_data`, `list_indexes`, `remove_index`, `search_index`,
`stepback_search_index`, `stepback_summary_index`) are not served, because
indexing does not exist in this runtime; `supported_tools.zephyr_enterprise`
in the Rust capability snapshot lists the five served names, so the
catalogue marks the others unavailable and a direct call to one is refused.
A persisted selection keeps the served names it contains and logs the omitted
ones; a selection of only indexing tools skips the toolkit.

Main freezes `zephyr_configuration` (`base_url`, redeemed `token`).
`config.rs` requires an HTTPS base (a path prefix such as `/zephyr` is kept)
and the token: the SDK schema marks it optional, but its client refuses to
start without it. The shared `zephyr_rest` client sends a sensitive
`Authorization: Bearer` header, disables redirects and automatic retries,
bounds request, response and output sizes, and appends every identifier as
one percent-encoded path segment, refusing dot segments and control
characters, so no argument can leave the `flex/services/rest/latest` prefix
of the configured instance.

| SDK operation | Route | Rust behavior |
| --- | --- | --- |
| `get_test_case` (read) | `GET testcase/{testcase_id}` | Provider object (an empty body is `""`) |
| `search_zql` (read) | `POST advancesearch/zql` with the decoded `zql_json` | Provider result; a failure is a read failure, never an unknown effect |
| `create_testcase` (effect) | `POST testcase/` (trailing slash kept) with the decoded `create_testcase_json` | Provider object; post-dispatch ambiguity is `UnknownOutcome` |
| `add_steps` (effect) | `GET testcase/{tree}`, then `GET testcase/versions?testcaseid=` (last entry), then `GET testcase/{version}/teststep`, then one `POST testcase/{version}/teststep/detail/{tree}` per step | Same body (`tcId`, `maxId`, `step{step,data,result,orderId}`, `tctId`, chained `id`), order continuing from the highest existing `orderId`; the SDK's `Step added: ...` lines joined by `;`; empty or null `steps` returns the SDK's `Steps cannot be empty.`; a failure after the first confirmed append is `UnknownOutcome` |
| `get_testcases_by_zql` (read) | `GET testcase?zqlquery=` | The SDK's `Test case ID: {id}, Test case: {testcase}` lines, or its `No test cases found for the provided ZQL query.` sentence |

Every description keeps the SDK's `Toolkit: {name}` prefix line and its
`Zephyr Enterprise instance: {base_url}` suffix line, cut at 1000
characters. Provider objects inside text are canonical JSON rather than
Python's `str(dict)`. Where the SDK returns `ToolException("Unable to ...:
{e}")` as the tool text for an HTTP failure, Rust returns the shared stable,
redacted error taxonomy instead (no provider bodies, routes or tokens). The
configuration model's `check_connection` probe is not part of the toolkit and
is not ported here. Proof: `toolkits/zephyr_enterprise_tests.rs`
(configuration, catalogue/selection/policy, exact routes and bodies, SDK
output text, add-steps version resolution and partial-effect handling,
redaction, SDK schema gate) and `toolkits/zephyr_rest_tests.rs` (shared
transport). Live provider proof, exact-interrupt HITL and durable effect
reconciliation remain activation gates, as for the other families.

### Zephyr Essential business family

Zephyr Essential is the Zephyr Scale Cloud v2 REST API behind its own
configuration type. The family serves 41 of the 45 business operations of the
worker-pinned SDK revision `b5113a129329b85d23c2d5c2bf55f18e307414ec`, over
the shared `zephyr_rest` bearer client described for Zephyr Enterprise. Main
freezes `zephyr_essential_configuration` (`base_url`, redeemed `token`); a
missing or empty `base_url` selects the SDK wrapper's default
`https://prod-api.zephyr4jiracloud.com/v2`, and any base must be HTTPS.

The operations are one table in `tools.rs` (name, SDK model title, verb, path
segments, arguments, query mapping), so the SDK's `ZephyrEssentialAPI` routes
are reviewable in one place: test cases (list, create, get, update, links,
issue/web links, versions, test script, test steps), test cycles (list,
create, get, update, links, issue/web links), test executions (list, create,
get, update, test steps, update test steps, sync script, links, issue link),
projects, folders (list, create, get, find by name), link deletion, the four
issue-link reverse lookups and `healthcheck`. Optional filters become query
parameters only when given (as `requests` drops `None`), in the SDK's order.
GET tools are read-only; POST/PUT/DELETE are effects whose post-dispatch
ambiguity is `UnknownOutcome`. Results are the provider JSON, its text, or
`""` for an empty body, as the SDK's `_do_request` returns them;
`list_test_cases` and `get_test_case_test_steps` return the page's `values`
array as the SDK does.

SDK special cases kept:

- the three issue-link tools refuse a body without `issueId` before any
  request, returning the SDK's exact guidance text (including the `issueKey`
  lookup advice) as the tool result;
- `create_folder` resolves `parentName` to `parentId` when no `parentId` is
  given and returns the SDK's `Parent folder with name '...' not found.`
  when the lookup fails; `parentName` stays in the body, as in the SDK;
- `find_folder_by_name` matches names ignoring case and returns the folder
  object or `null`.

Deliberate differences: `find_folder_by_name` (and the parent lookup) walks
the `/folders` pages (`maxResults=100`, `isLast`, at most 50 pages) where the
SDK reads only the first page at the server's default size and so misses
later folders; the parent lookup is scoped to the payload's `projectKey` and
`folderType` when present, where the SDK searched every project. HTTP
failures use the stable redacted taxonomy instead of the SDK's
`Unexpected status code ...` text with the provider body.

Not served, and absent from `supported_tools.zephyr_essential`:

- the six indexing tools (no indexing in this runtime);
- `create_custom_executions`, `create_cucumber_executions` and
  `create_junit_executions`: the SDK passes its `files` argument, a path
  string, to `requests` as `files=`, which raises before any request (a string
  is not a 2-tuple list), so these tools cannot succeed in the SDK; serving
  them needs a defined file source (for example an artifact) and multipart
  upload, which is a new contract rather than a port;
- `retrieve_bdd_test_cases`: the endpoint answers `application/zip`; the SDK
  returns the archive decoded as text, which is not usable, and this runtime
  has no ZIP reader to return the feature files instead.

Proof: `toolkits/zephyr_essential_tests.rs` (configuration and default base,
catalogue groups and unserved selection, the exact method/route/query/body of
every table operation, `values` projection, issue-link guidance, folder
lookup paging, parent resolution and not-found text, text/empty replies and
argument refusal, SDK schema gate). Live provider proof, exact-interrupt HITL
and durable effect reconciliation remain activation gates.

### Zephyr Scale business family

Zephyr Scale serves all twenty business operations of the worker-pinned SDK
revision `b5113a129329b85d23c2d5c2bf55f18e307414ec`, which drive
`zephyr-python-api==0.1.0` (its Cloud wrapper, `CloudApiWrapper`, and
`ZephyrSession`). The six inherited indexing tools are not served (no
indexing in this runtime); `supported_tools.zephyr_scale` lists the twenty.

Configuration. Main freezes the shared `zephyr` configuration under
`zephyr_configuration`. The SDK validator accepts a token, username/password
or cookies, but then always builds `ZephyrScale(token=values['token'])`: the
library's Cloud wrapper with its fixed `https://api.zephyrscale.smartbear.com/v2/`
origin and bearer authentication, ignoring `base_url`. Rust does the same,
explicitly: the origin is the fixed Cloud API, the token is required, and the
other fields are accepted and ignored. Requests go through the shared
`zephyr_rest` client (sensitive bearer header, no redirects or automatic
retries, bounded bodies, percent-encoded path segments).

Pagination. The library's `get_paginated` reads every page from the given
`startAt`, following each `next` link's query until `isLast`; `maxResults`
is only the page size. `client.rs::paginated` keeps that, takes only the
query of `next` (so a provider link can never move the token to another
origin), stops when a page has no `values`, no usable `next` or an unchanged
query (the library would loop), and is bounded (200 pages, 10,000 items).

Outputs follow the SDK's strings: `Extracted tests: ...`, `Extracted test
steps: ...`, `Test case with name ... was created: ...`, `Steps for test
case ... were added/updated: ...`, `Extracted folders: ...`, `Versions for
test case ...`, `Found N test cases matching ...: [json]` and the rest.
`render.rs` reproduces Python's `str()`/`repr()` of the decoded objects
(`'single quotes'`, `True`, `None`, Python's quote choice and escapes) and
`json.dumps(indent=2)` with `ensure_ascii` for the search result; object
members are written in sorted key order (the canonical-ordering rule of
`crate::canonical`), the only difference from the SDK's text. `folders.rs`
reproduces the folder-tree helpers (`_build_folder_hierarchy`, name and
path lookup, `_collect_subfolders`) and `_parse_tests`.

Behavior preserved: the search's client-side filters (search term over
`str(tc.values())`, any-of labels, custom-field rules for scalar and list
values, step search over `inline` description/test data/expected result with
optional inclusion), sort, `limit_results`, field projection including
`customFields.<name>`, the criteria message; folder reads that fail are
skipped with a warning as in the SDK; `update_test_steps` reads all steps,
validates every update (its exact `index`/`inline` messages) before the
single `OVERWRITE` write.

Deliberate differences, each a defect in the pinned SDK:

- `get_test_steps` and `get_links` read `kwargs['return_list']` /
  `kwargs['return_only_links']`, which a tool call never passes, so both
  always returned a `KeyError` text after fetching; Rust returns the
  intended `Extracted test steps: ...` / `Links for test case ...` text.
- `create_test_case` with `steps` reads `response['test_case_key']`, which
  the Cloud API never returns, so the case was created but the steps were not
  and the tool reported failure; Rust appends the steps to the response's
  `key`.
- `update_test_case` forwarded the raw `additional_fields` JSON string as a
  body member named `additional_fields`; Rust merges the decoded fields, as
  the argument documents.
- `create_test_cases` validates every case before the first create (the SDK
  raised mid-batch on a malformed case after creating the earlier ones), and
  treats a missing `additional_fields`/`steps` as empty instead of raising;
  the result is the SDK's list of per-case lines, an API failure for one case
  reported as its `Unable to create test case ...` line.
- `_collect_subfolders` returned `list(set(...))`; Rust keeps first-seen
  order so the folder chosen as `folder ID` and the request order are
  deterministic. A cyclic folder tree terminates instead of recursing.
- Sorting mixed value types (a `TypeError` in the SDK) uses a total order.
- `archived` is accepted and, as in the SDK, never sent.
- HTTP failures use the stable redacted taxonomy instead of the library's
  `Error {status}. Response: {body}` text.

Proof: `toolkits/zephyr_scale_tests.rs` (token-only configuration, Python
renderings, catalogue and effect groups, `next`-link pagination and parsed
rows, the read sentences and routes, create-with-steps on the created key,
batch validation and per-case lines, step updates and their refusals, effect
bodies, folder path/name/recursive walks with a skipped folder, search
filters/sort/limit/projection/message, folder-scoped step search and the
custom-field refusal text, SDK schema gate). Live provider proof,
exact-interrupt HITL and durable effect reconciliation remain activation
gates.

### ReportPortal complete read family

ReportPortal is a complete nine-tool read family over one configured project.
The implementation follows SDK revision
`9bba9da409771803f28c0ee21f5d0b9a8f456219` and worker-pinned revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec`; their ReportPortal schemas,
routes and projections are identical, while the current SDK adds the `read`
group metadata. The worker's two pinned SDK patches do not touch this family.
The Python SDK has no focused ReportPortal tests, so the Rust exact-route,
content and isolation corpus is the executable compatibility proof.

Main freezes the required nested `report_portal_configuration` containing
`endpoint`, `project` and secret `api_key`, then claim-materializes that secret
for both application and ad-hoc execution. `config.rs` accepts only that owned
shape, normalizes one approved HTTPS origin, and creates no environment or
process-global credential fallback. This intentionally removes the SDK
validator's class-level `_client`, which can otherwise substitute credentials
between toolkit instances. The API key is zeroized, non-cloneable and absent
from model metadata, serialized arguments and diagnostics.

The public source-order catalog is preserved, with every operation marked
`read`:

1. `get_extended_launch_data_as_raw` returns one HTML or PDF launch export.
2. `get_extended_launch_data` returns bounded readable text from one HTML
   launch export.
3. `get_launch_details` returns one launch object.
4. `get_all_launches` returns one zero-based launch page.
5. `find_test_item_by_id` returns one test-item object.
6. `get_test_items_for_launch` returns one zero-based item page.
7. `get_logs_for_test_items` returns one zero-based log page.
8. `get_user_information` returns one user object.
9. `get_dashboard_data` returns one dashboard object.

Each tool description states the selection purpose, identifier source,
format/default/bounds, one representative example and the exact result shape.
It does not repeat the configured endpoint or project as the SDK currently
does. Empty selection still means all nine tools in catalog order; a persisted
unknown selection fails closed instead of silently producing an incomplete
toolset.

The raw-export operation keeps the source's `html|pdf` choice but makes its
otherwise unrepresentable Python `bytes` result explicit. HTML is returned as
bounded UTF-8. Small PDF exports have a bounded base64 conformance fallback in
a structured object containing the format, content type, encoding, byte length
and content. PDF input is capped at 383 KiB so base64 plus the envelope remains
within the 512 KiB tool-result limit; this is a transport safeguard, not the
intended user experience for report downloads. Production activation requires
large PDF exports to stream into durable artifact/object storage and return an
authorized reference rather than expanding the document inline.
`get_extended_launch_data` always asks for HTML, matching the source's actual
request, and uses a deterministic bounded HTML-to-text projection. It
rejects malformed UTF-8 and PDF/content-type substitution rather than passing
PDF bytes to PyMuPDF with `filetype="html"`, the unreachable/broken source
branch. JSON operations accept only a bounded JSON object.

The SDK passes `page.page=1` by default even though the provider index is
zero-based, thereby skipping the first page. Rust intentionally defaults to
zero and validates a finite nonnegative page bound. All identifiers and the
project are encoded as path or query components, redirects and automatic
retries are disabled, one invocation performs exactly one request, and body,
output and deadlines are bounded. Provider text, response bodies, endpoint,
project and credentials never appear in typed errors.

| Current business source | Preserved behavior | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `configurations/report_portal.py::ReportPortalConfiguration` and `check_connection` | Project-scoped Bearer configuration plus an authenticated project probe | `config.rs` validates claim-owned authority; the future public check route must use the same bounded family transport for the exact project read rather than duplicating provider logic |
| SDK `tools/report_portal/__init__.py::{get_tools,toolkit_config_schema,get_toolkit}` and Main freezer/materializer | Nested configuration, empty/subset selection, source order, toolkit identity and read grouping | `tools.rs` preserves all nine tools and tested model-facing schemas while preventing endpoint/project disclosure |
| SDK `ReportPortalApiWrapper` nine methods | One request per selected launch/item/log/user/dashboard/export operation | `client.rs::ReportPortalApi` preserves the route/result meaning and exposes bounded typed HTML, PDF and JSON projections |
| SDK `RPClient` | Bearer header and fixed API route families | One invocation-scoped pooled client uses the accepted HTTPS origin, percent-encoded authority components, no redirects/retries and finite transport limits |
| Python raw/readable export handling | Raw HTML/PDF and management-readable launch content | Explicit UTF-8/base64 envelope and deterministic HTML text replace bytes leaking through a `str` annotation and the broken PDF-as-HTML branch |

For future general PDF analysis, the efficient default is text-first: extract
bounded text page by page, then render only scanned or layout-dependent pages
for a multimodal model. Rendering every page to an image is reserved for cases
where text extraction cannot preserve the information needed by the task.

The registered configuration check remains an activation contract; this
capability-disabled family does not publish a check route yet. Its future route
must use the same bounded client for the exact authenticated project read
rather than duplicating provider behavior in Main. Production family
registration remains disabled pending that check composition, authorized
application/ad-hoc materialization, approved egress, current
configuration-catalog projection, durable large-export artifact streaming and
a credentialed live provider test. There
is no mutation/effect-reconciliation gate because every operation is a read.
Reports, logs and user data may still be independently configured as sensitive;
read grouping never bypasses the shared exact `interrupt_id` policy.

### GitLab Org complete repository family

`gitlab_org` is a separate seventeen-tool catalog from the standard GitLab
toolkit. The implementation follows current SDK revision
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, worker-pinned revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec` and
`python-gitlab==4.5.0`. The current and pinned implementations have identical
schemas, routes and result behavior; current adds the eight `read`, eight
`write` and one `delete` group annotations. There is no GitLab Org indexing,
artifact or LLM path.

Main freezes the shared nested `gitlab_configuration` reference and keeps the
private token sealed. Claim-time configuration materialization accepts only an
application or ad-hoc execution request, unseals that already-frozen revision,
and passes the resulting tool settings through the Python worker SDK adapter.
Rust accepts only that claim-owned HTTPS root origin and zeroizing token. It
does not authenticate or resolve repositories while constructing the toolset,
removing the SDK's eager network dependency and shared mutable client state.

The public source-order catalog is preserved:

1. `create_branch` creates a branch from the invocation's current active ref.
2. `set_active_branch` changes only invocation-local branch state and performs
   no provider request.
3. `list_branches_in_repo` returns a bounded wildcard-filtered branch list.
4. `get_issues` returns the first bounded page of open issues.
5. `get_issue` returns one issue and its first ten comments.
6. `create_pull_request` creates a merge request into the configured base
   branch.
7. `comment_on_issue` posts the packed `<iid>\n\n<comment>` input.
8. `create_file` creates one text file after an exact not-found preflight.
9. `read_file` reads UTF-8 text or a validated 1-indexed inclusive line range.
10. `update_file` applies deterministic OLD/NEW marker pairs and commits the
    complete bounded result.
11. `delete_file` deletes one path on the selected branch.
12. `get_pr_changes` returns one bounded merge-request diff with displayed row
    indices.
13. `create_pr_change_comment` binds a comment to one displayed diff row and
    the merge request's exact base/head/start revisions.
14. `list_files` returns bounded blob paths from one repository tree.
15. `list_folders` returns bounded tree paths from the same tree contract.
16. `append_file` reads and updates the same resolved repository and branch.
17. `get_commits` returns a bounded commit projection with optional ref, path,
    author and timezone-bearing RFC3339 filters.

Every tool and parameter description states the full `group/project` repository
format, configured/default repository behavior, configured base branch versus
invocation-local active branch, result bound and remote-effect risk. It also
documents the branch limit, first-page issue/comment behavior, 200,000-character
and 512 KiB serialized file-read guidance, 1 MiB read-source ceiling, 256 KiB
writable-file/edit ceiling, dedicated-line OLD/NEW grammar, zero-based displayed
diff-row identity, bounded tree traversal and RFC3339 requirements. Model
metadata never exposes the origin, token or configured allowlist.

Nonempty configured repositories form a strict ordered allowlist; omission
selects the first entry. The source-compatible empty configuration permits a
repository argument visible to the token, but production activation of that
mode requires a Main-issued claim-scoped organization-wide repository grant.
The complete toolset serializes calls because active-branch mutation and calls
that consume it must not race. Selected tools still preserve catalog order;
empty selection means all and persisted unknown names fail closed.

Rust intentionally replaces unbounded `get_all`/`all=True` iteration with
finite page and item budgets, validates positive IDs and ordered line ranges,
bounds provider bodies, decoded files, edits, diffs, requests and results, and
performs no hidden retry. Only an exact file-not-found response permits create;
append cannot read one project and write another. Unified-diff parsing accepts
standard counted and count-less hunks. Transport ambiguity or unusable success
data after an effect is an explicit nonretryable unknown outcome, never an
ordinary retryable dependency error.

Thirteen focused tests cover the exact catalog and descriptions, configured and
dynamic project authority, encoded routes and request bodies for every remote
effect, exact completion statuses, nullable results, pagination and commit
filters, UTF-8/read/write bounds, Python-compatible wildcard classes, OLD/NEW
matching, diff positions, secret redaction and fail-closed policy admission.

| Current business source | Preserved behavior | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `configurations/gitlab.py::GitlabConfiguration::check_connection` | Root GitLab origin, private-token authentication and `/api/v4/user` probe | `config.rs` owns the claim-scoped authority; the future public check delegates the same bounded client instead of duplicating provider logic |
| SDK `tools/gitlab_org/__init__.py::{get_tools,toolkit_config_schema,get_toolkit}` | Nested configuration, repositories/base branch, source order, selection and toolkit identity | `config.rs` and `tools.rs` preserve the separate seventeen-tool contract without construction I/O |
| SDK `GitLabWorkspaceAPIWrapper` and `python-gitlab==4.5.0` | Branch, issue, merge-request, file, tree and commit routes plus PRIVATE-TOKEN auth | `client.rs` uses one bounded invocation client, exact encoded project/path/query components, no redirects/retries and typed data-free failures |
| SDK `BaseCodeToolApiWrapper.edit_file` and `utils.text_operations` | Dedicated-line OLD/NEW pairs, exact then tolerant unique matching and sequential edits | `edit.rs` preserves deterministic edit semantics with a finite linear work budget and provider-byte/request caps |
| SDK `gitlab.utils::{get_diff_w_position,get_position}` | Displayed diff row numbers and exact GitLab discussion position | `diff.rs` accepts standard unified hunks, binds exact diff refs and caps every provider string and projected row |
| Main toolkit freezer/materializer and Python `EliteaSdkAgentAdapter` | Same frozen tool contract for application and ad-hoc execution | Rust tests consume the materialized nested shape; no environment/global credential fallback exists |

Production family registration remains disabled pending delegated connection-
check composition, application/ad-hoc materialization proof, live GitLab scope
and pagination fixtures, explicit organization-wide authority for dynamic mode,
and the shared exact-`interrupt_id` approval plus cancellation-safe effect
receipt/reconciliation owner. Groups are presentation metadata: any read may be
configured sensitive, and no write/delete is authorized merely by its group.

### SQL complete database family

`sql` is the current SDK's complete two-tool database family. The behavioral
baseline is current SDK revision
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, worker-pinned revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, Main's frozen configuration and
toolkit catalogs, and the shared application/ad-hoc SDK adapter. Current adds
only `execute` and `read` group annotations plus error-decorator metadata; the
connection, query and reflection behavior is otherwise unchanged.

Main freezes `dialect`, `database_name`, selected tools and the nested
`sql_configuration` before dispatch. The password remains sealed until the
claimed application or ad-hoc request is materialized. Rust accepts the
registered integer port directly, repairing the SDK mismatch where the public
configuration declares an integer but the wrapper expects a string. It builds
typed SQLx options instead of interpolating credentials into a URL, uses the
configured host/port/database as fixed authority, performs no construction
I/O, and never exposes the host, database, SQL text or any part of the password
through model metadata or diagnostics. SQL has no public connection check;
the SDK's lazy `SELECT 1` is first-use behavior, not configuration metadata.
Rust preserves the existing MySQL session SQL mode rather than replacing the
tenant or server policy. Before user SQL is dispatched it reads the bounded
session mode and fails closed when the exact `NO_BACKSLASH_ESCAPES` mode is
present, because that mode would invalidate the lexical admission contract.
The focused corpus proves both preservation of ordinary modes and this
pre-dispatch rejection.

Both public operations are retained:

1. `execute_sql` executes exactly one bounded PostgreSQL or MySQL statement.
   It returns a bounded array of row objects for a row-producing statement and
   a data-free execution receipt for a non-row statement. Transaction-control
   and multiple statements are rejected because the worker owns commit and
   ambiguity handling. SQL is unrestricted at the product boundary, so this
   action is always an effect even when its current text appears read-only.
2. `list_tables_and_columns` returns base tables and ordered columns from the
   configured database's default schema. Rust replaces the SDK's unbounded
   table-by-table reflection fanout with one deterministic bounded
   `information_schema` query per invocation.

PostgreSQL and MySQL use separate row projectors rather than SQLx `Any`. This
preserves common booleans, signed and MySQL unsigned integers, finite floats,
exact decimal strings, dates/times/timestamps, UUIDs, JSON, binary values and
supported PostgreSQL arrays. Unsupported provider/domain/composite types fail
with a stable type-only diagnostic instead of being coerced or leaking a row.
Rows, columns, catalog entries, query bytes, statement time and serialized
output are finite. No database operation is retried. Once the statement has
been dispatched, a timeout, cancellation, disconnect, decode/bound failure or
uncertain commit is a nonretryable unknown outcome; the SQL text is never
echoed in a success or error.

Two SQLx boundaries remain explicit production gates. Its PostgreSQL options
have no environment-free public constructor, so deployment `PG*` TLS material
can still influence the connector even after ordinary claim fields are
overridden. Both PostgreSQL and MySQL drivers also allocate one complete wire
row/packet before the family can enforce post-decode row and result limits.
Production activation therefore needs an owned TLS/trust policy plus a
sanitized connector/upstream option constructor, and a driver/server response
allocation bound or equivalent trusted database control. Credentialed
PostgreSQL and MySQL protocol/TLS fixtures must prove these constraints.

| Current business source | Preserved behavior | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `configurations/sql.py::SqlConfiguration` | Nested host, optional integer port, username/password configuration | `config.rs` validates claim-owned fixed authority, repairs integer-port materialization and zeroizes the password |
| SDK `tools/sql/__init__.py::SQLToolkit` | PostgreSQL/MySQL selection, database name, source order, empty/subset selection and toolkit identity | `config.rs` and `tools.rs` preserve the two-tool catalog without origin-bearing descriptions or construction I/O |
| SDK `SQLApiWrapper.client` and SQLAlchemy engine setup | Lazy provider connection and first-use failure | `client.rs` uses backend-specific typed SQLx options and bounded one-attempt connection ownership; no credential DSN is created |
| SDK `SQLApiWrapper.execute_sql` | Arbitrary committed SQL and row/non-row results | `lexer.rs`, `project.rs` and `client.rs` retain the complete effect while adding one-statement admission, JSON-safe type projection, finite results and unknown-outcome semantics |
| SDK `SQLApiWrapper.list_tables_and_columns` | Default-schema base table and column metadata | `client.rs` replaces unbounded reflection fanout with one ordered bounded provider-specific information-schema query |
| Main freezer/materializer and Python `EliteaSdkAgentAdapter` | Same frozen nested settings for application and ad-hoc execution | Focused Rust fixtures consume the claim-materialized nested settings without host/user/password fallback; end-to-end application/ad-hoc envelopes and SQLx's ambient PostgreSQL TLS-material seam remain activation gates |

Production family registration remains disabled. `execute_sql` requires the
shared durable exact-`interrupt_id` approval and cancellation-safe effect
receipt/reconciliation owner. Either operation may independently be configured
sensitive; `read`/`execute` groups are model/catalog metadata and never grant
database authority.

### Aha complete product-management family

`aha` is a complete thirty-three-tool product-management family. The behavioral
baseline is current SDK revision
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, worker-pinned revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec`, Main's frozen toolkit and
configuration catalogs, and the shared Python application/ad-hoc SDK adapter.
The current and pinned implementations have identical schemas, routes, result
semantics and focused Aha tests; current adds only the 25 `read`, six `write`,
one `delete` and one `execute` group annotations. `manage_record` can create,
update or delete and is therefore effectful regardless of its presentation
group.

Main freezes the nested `aha_configuration`, its owning configuration project
and the selected source catalog before dispatch. The token remains sealed until
claim-time materialization for an application or ad-hoc execution. Rust accepts
only that claim-owned HTTPS origin and zeroizing Bearer token, constructs no
client during schema/catalog inspection and never exposes the private origin or
token in tool descriptions. The future public connection check will use the
same bounded client for exact `GET /api/v1/me` validation rather than duplicating
the SDK's redirect-following, body-leaking probe.

The source-order catalog is retained in four parts:

1. Direct REST reads: `get_feature`, `get_requirement`, `get_release`,
   `get_initiative`, `get_epic`, `get_idea` and `get_product`.
2. Bounded REST collections and search: `list_products`, `list_features`,
   `list_requirements`, `list_releases`, `list_initiatives`, `list_epics`,
   `list_ideas` and `search`. Release scope wins over product scope where the
   SDK defines both.
3. Fixed GraphQL and uniform read dispatchers: `get_page`,
   `search_documents`, `get_feature_gql`, `get_requirement_gql`,
   `find_project`, `search_records` and `read_records`. The page dispatcher
   deliberately honors the requested projection and format instead of dropping
   them as the SDK currently does.
4. Comments, records, metadata and attachments: `add_comment`,
   `list_comments`, legacy `manage_record`, `create_record`, `update_record`,
   `delete_record`, `create_record_link`, `copy_record`, `fields_metadata`,
   `field_options_metadata` and `attach_file`.

The REST root is `{origin}/api/v1` and the fixed GraphQL endpoint is
`{origin}/api/v2/graphql`. Reads preserve Aha reference and parent-scope rules,
top-level field allowlisting, JSON/CSV/Markdown selection and the SDK's explicit
empty-result messages. The native formatter is deterministic and bounded; it
does not depend on pandas or the worker image's optional `tabulate` package.
List pagination starts at page one, validates positive monotonic metadata,
requires the endpoint's expected collection key and stops at the advertised
record cap plus a finite page ceiling. It never follows redirects or an
upstream URL.

Record creation is release-scoped for features and epics, feature-scoped for
requirements, and product-scoped for ideas, releases, initiatives and pages.
Release and initiative updates/deletes also require the product parent. The
legacy `manage_record` parent alias is preserved, but model guidance prefers
the operation-specific tools and runtime authorization must bind its exact
action. Record-link creation retains all seven link codes and resolves Aha
reference numbers through bounded REST, GraphQL or collection reads before one
effect request. Copy remains release-only. Every effect is one attempt; an
unexpected success status/shape, timeout, cancellation, rate limit or provider
failure after dispatch is a nonretryable unknown outcome that requires
reconciliation.

`attach_file` preserves the existing Elitea artifact contract rather than
accepting a local path. Its public `/{bucket}/{filename}` reference is parsed and
authorized by a claim-scoped artifact resolver. The resolver binds the immutable
artifact version, exact byte length and SHA-256 digest, downloads into a private
bounded temp spool, verifies all three before Aha dispatch, and then streams the
verified file once as reqwest multipart field `attachment[data]`. This avoids a
same-sized in-memory copy without allowing same-length content substitution. A `to_do`
uploads to `/tasks/{id}/attachments`; other supported records are read once to
obtain `description.id` and then upload to `/notes/{note_id}/attachments`.
Aha's provider limit is strictly below 300,000,000 bytes and uploads must
complete within 40 seconds; the family enforces both the decimal length ceiling
and the invocation deadline. Production activation additionally requires the
real artifact client, project authority and a shared temp-disk/concurrency
budget. No artifact path, filename header injection, token, origin or provider
body reaches diagnostics.

Tool and parameter descriptions are part of the executable model contract.
They identify REST versus Markdown/GraphQL reads, exact parent/reference
formats, scope precedence, page and result bounds, output formats, link codes,
the operation-specific alternative to `manage_record`, artifact path syntax,
and whether an action can create duplicates or has an unknown outcome. Group
metadata does not grant execution authority: any of the 25 reads may be
configured sensitive, while all eight effectful tools require the shared exact
`interrupt_id` decision and cancellation-safe effect receipt/reconciliation
owner.

| Current business source | Preserved behavior | Rust owner / deliberate improvement |
| --- | --- | --- |
| SDK `configurations/aha.py::AhaConfiguration` | Nested base URL/token schema and authenticated `/api/v1/me` check | `config.rs` owns claim-scoped HTTPS authority; future check composition reuses the bounded client |
| SDK `tools/aha/__init__.py::{get_tools,toolkit_config_schema,get_toolkit}` | Thirty-three source-ordered tools, empty/subset selection and toolkit identity | `config.rs` and `tools.rs` preserve the complete catalog without construction I/O or origin-bearing model text |
| SDK `AhaApiWrapper::_rest_request`, `_paginate` and `_collect` | Bearer REST reads/effects and record-capped pagination | `client.rs` fixes origin, expected collection keys, page/body/output budgets, exact effect statuses and safe errors |
| SDK four fixed GraphQL queries | Page/document search plus Markdown feature/requirement reads | `client.rs` retains fixed query documents/variables and rejects partial/error/oversized responses safely |
| SDK `_project_record` and `_format_output` | Top-level field projection and JSON/CSV/Markdown | `format.rs` supplies deterministic native formatting and explicit size bounds |
| SDK record/comment/link/copy methods | Parent-scoped mutations, link resolution and result projection | `client.rs` and `tools.rs` retain all effects while making ambiguity and action-specific authority explicit |
| SDK `get_file_bytes_from_artifact` and `attach_file` | Elitea artifact retrieval followed by Aha multipart upload | `artifact.rs` binds immutable version/length/SHA-256, verifies a private bounded temp spool before dispatch and then streams the exact multipart attachment effect |
| Main toolkit freezer/materializer and Python `EliteaSdkAgentAdapter` | Same frozen tool contract for application and ad-hoc execution | Rust fixtures consume the materialized nested shape; no environment/global credential fallback exists |
| SDK Aha unit and credential-gated end-to-end suites | Route, schema, formatter and provider evidence | `aha_tests.rs` adds fourteen focused route/schema/model-metadata, adversarial bound, unknown-outcome, secret-isolation and artifact-authority tests |

**Materialization (2026-10-09).** `materialize.rs` now dispatches `aha`
(`materialize_a_to_k`) and the Rust capability snapshot lists it with
`supported_tools.aha` naming 32 tools. `attach_file` is the one tool left out:
the sealed resolver needs an artifact-read GRANT (immutable version, byte length
and SHA-256 bound before dispatch), and no host provides one — the claim-bound
platform reader (`PlatformWriter::read_artifact`) returns capped decoded text,
not verified bytes. `build_aha_toolset` therefore takes an optional resolver;
production passes none, the tool is not built, an explicit selection of it is
dropped with one `agent_toolkit_tools_skipped` warning, and a selection of only
`attach_file` skips the toolkit as unsupported. A defensive
`AhaArtifactResolver::unavailable()` refuses every path as unauthorized if a call
ever reached it. Empty selection serves the 32 tools. The SDK conformance gate
(`every_tool_keeps_the_sdk_contract`) now checks the production profile, so the
served set and `supported_tools.aha` are asserted equal; no schema drift and no
exemption was needed.

Production registration remains disabled pending authorized application/ad-hoc
materialization, fixed-origin egress and live `/api/v1/me` proof, shared
per-tool sensitive policy, durable exact-interrupt continuation, effect receipt
and reconciliation, and the real claim-scoped artifact resolver. Large tool
results must retain one event owner and remain below the output-frame boundary;
the Python worker's known duplicated 51,979-byte Aha result is a regression
fixture rather than behavior to reproduce.

### Jira REST family

`jira` serves twelve of the SDK type's twenty-three tools. The baseline is the
worker-pinned SDK revision `b5113a129329b85d23c2d5c2bf55f18e307414ec`
(`elitea-sdk` 0.9.8; none of the five pinned patch commits touch Jira) and the
`atlassian-python-api==4.0.7` client it drives. Tool names and argument schemas
are the SDK's and pass the conformance gate with no exemption.

Configuration. Main freezes the nested `jira_configuration` (`base_url`,
`hosting`, `username`, `api_key`, `token`) and the toolkit settings (`cloud`,
`api_version`, `limit`, `labels`, `additional_fields`, `custom_headers`,
`verify_ssl`, `selected_tools`). Rust resolves cloud and the REST version
exactly as `_hosting_to_cloud`/`_resolve_api_version` do: toolkit `cloud`
wins, else hosting, else an `*.atlassian.net` host (host match, not
substring); an explicit `2`/`3` wins, else cloud or an Atlassian host means v3.
A `token` containing `JSESSIONID` is sent as cookies, any other token as
Bearer, otherwise `username`+`api_key` as Basic. Deliberate differences: the
origin must be HTTPS (plain HTTP and `verify_ssl: false` skip the toolkit as
unsupported rather than disabling TLS checks); a custom header may not replace
`Authorization`/`Cookie`, platform identity or framing headers; `limit` is
clamped to 1000; blank list items are dropped.

The SDK's `JiraClient` validates the credential with `GET /rest/api/{v}/myself`
when it is built, because Jira answers an unauthenticated search with 200 and
no issues. Rust probes once per toolset before the first operation and returns
the SDK's own sentences (`Authentication failed: Invalid username or API
key.`, `... Access forbidden.`, the v2/v3 Hosting hint on 404).

| SDK tool | Route and behaviour kept | Rust differences |
| --- | --- | --- |
| `search_using_jql` | v3 `GET search/jql` with `nextPageToken`/`isLast`, v2 `GET search` with `startAt`; `maxResults=min(100, limit)`, `fields=*all`; `_parse_issues` projection plus additional fields; `No Jira issues found` / `Found N Jira issues:` | Total capped at 1000 and 50 pages (`limit: 0` means 1000); issue URLs are `{base}/browse/{key}` (the SDK concatenated without a slash) |
| `create_issue` | `POST issue?updateHistory=false` with `fields`/`update`, the SDK's validation sentences, default labels after | A label failure after creation is appended as a warning instead of reporting the created issue as an error |
| `update_issue`, `modify_labels` | `PUT /rest/api/2/issue/{key}` (the SDK client hardcodes v2), label add/remove operations, default labels after update | Default labels are applied only after a successful update |
| `list_comments`, `add_comments` | `GET`/`POST issue/{key}/comment`; v3 comments are one ADF paragraph | — |
| `list_projects` | Cloud pages `project/search`; Server reads `project` | `nextPage` is followed only on this toolkit's own REST root, at most 20 pages |
| `set_issue_status` | `GET` transitions, case-insensitive target-status match, `POST` transition, default labels after | The SDK sent the `update` block as `fields` too; Rust sends `fields` and `update` as given, and names the available statuses when none matches |
| `get_specific_field_info`, `get_remote_links` | `GET issue/{key}?fields=`, falling back to the non-null field list; `GET issue/{key}/remotelink` | — |
| `link_issues` | `POST issueLink` with the link comment (ADF on v3) | — |
| `execute_generic_rq` | Any path on the instance; `params` (outermost `{...}`) as query for GET, JSON body otherwise; search responses projected as `process_search_response`; `HTTP: {method} {url} -> {status} {reason} {body}` | The path must stay a plain path under the configured base (no scheme, `//`, query, fragment or dot segments); methods GET/HEAD/OPTIONS/POST/PUT/PATCH/DELETE |

Results are text, as the SDK's are; where the SDK printed a Python `repr`, Rust
prints compact JSON. A Jira 4xx is returned as the SDK-prefixed text with
Jira's `errorMessages`/`errors` (bounded to 2 KiB); 401, 429, 5xx and transport
failures are typed `AdkError`s, and any failure after an effect was sent is a
nonretryable unknown outcome. Reads above 512 KiB are refused; effect
confirmations are truncated instead.

Not served, each listed in `supported_tools.jira` by omission and dropped with
one `agent_toolkit_tools_skipped` warning when selected:
`get_field_with_image_descriptions` and `get_comments_with_image_descriptions`
(their default path is a vision-model call per image; toolkit families are
given no model handle), `get_attachments_content` (the SDK's
`process_content_by_type` document/OCR/LLM parser; `libs/rust/doc-extract`
exists but is not linked into the runtime and has no image path),
`add_file_to_issue_description` and `update_comment_with_file` (they upload
artifact bytes; the host's artifact reader returns capped text, not verified
bytes), and the six index tools. A selection with nothing served left skips
the toolkit as unsupported; empty selection serves the twelve.

### Confluence REST family

`confluence` serves sixteen of the SDK type's twenty-five tools against the
same worker-pinned SDK revision and `atlassian-python-api==4.0.7` as Jira. Tool
names and argument schemas pass the conformance gate with no exemption.

Configuration and URLs. The nested `confluence_configuration` carries
`base_url`, `hosting`, `username`, `api_key` and `token`; toolkit settings add
`space`, `api_version` (`Auto`/`1`/`2`), `limit` (per-request page size,
default 5, at most 100), `max_pages` (default 10, at most 50), `labels`,
`custom_headers`, `verify_ssl` and `cloud`. Cloud and the version resolve as
`_hosting_to_cloud`/`_resolve_confluence_api_version` do. As the SDK does, a
Cloud base URL loses a trailing `/wiki` and page links get `/wiki` exactly once
(`_build_page_url`), while the REST root is the `atlassian` client's: the base
plus `/wiki` when the URL names `atlassian.net`/`jira.com` without it.
Credentials, HTTPS-only, `verify_ssl` and custom-header rules are Jira's.
`number_of_retries`/`min_retry_seconds`/`max_retry_seconds` are accepted and
ignored: Rust families make one attempt and report a typed retryable failure.

| SDK tool | Route and behaviour kept | Rust differences |
| --- | --- | --- |
| `create_page`, `create_pages` | Duplicate-title check, parent defaulting to the space home page, v1 `POST rest/api/content` (fixed-width metadata, ancestors) or, on v2, space-id lookup and `POST api/v2/pages` reshaped like v1; page label and default labels; the SDK's result sentence | The duplicate check uses the target space (the SDK always checked the toolkit space); a missing space is a sentence, not a provider 400; a label failure is a warning on the created page; batches stop at 50 pages |
| `delete_page` | By id, or by title in the toolkit space; `DELETE rest/api/content/{id}` | — |
| `update_page_by_id`, `update_page_by_title`, `update_pages`, `update_labels` | Title-collision check, `history` version + 1, `PUT rest/api/content/{id}?status=current`, the unchanged-content short-circuit, label replacement, default labels, the SDK's result sentence with version and diff link | Without `new_body` the page keeps its STORAGE body; the SDK resent the rendered `view` HTML as storage, which flattens macros, so `update_labels` and title-only updates no longer rewrite content |
| `get_page_tree` | `child/page` 100 at a time, pre-order, `{id: [title, parent_id]}` | At most 1000 pages and depth 32 |
| `get_pages_with_label`, `list_pages_with_label` | CQL `type=page AND label="..."` paging with de-duplication up to `max_pages` | The label is CQL-escaped; the list variant uses the search result titles instead of reading every page body |
| `read_page_by_id` | `expand=body.{format},version`; Markdown via `markdownify` semantics, ADF returned verbatim; `skip_images` strips base64 images; the SDK's not-found sentence | Markdown is produced by `html-to-markdown-rs` (ATX headings, no preprocessing), not byte-identical to `markdownify` |
| `search_pages`, `search_by_title`, `site_search` | The SDK's CQL with the space unquoted and re-quoted, `ceil(max_pages/limit)` result pages, each match read and rendered; site search's ten previews joined by `---` | `search_pages` escapes the query like the other two; an unreadable page is skipped instead of ending the batch |
| `get_page_id_by_title` | `rest/api/content?type=...&spaceKey=...&title=...` | — |
| `execute_generic_confluence` | Path under the client root, params as query for GET and JSON body otherwise, every status returned as `HTTP: {method}{url} -> {status}{reason}{body}`, Markdown for `/rest/api/content/{id}` GETs | The path must stay a plain path under the root; an effect's 5xx is an unknown outcome instead of text |

Results are text; structured values are compact JSON where the SDK printed a
Python `repr`. Confluence 4xx answers are `Confluence API error: HTTP ...`
text with the provider's `message`/`errors`; 401, 429, 5xx and transport
failures are typed, and any failure after an effect was sent is a nonretryable
unknown outcome.

Not served: `get_page_with_image_descriptions` (a vision-model call per
image), `get_page_attachments` (the SDK's attachment parser and LLM analysis),
`add_file_to_page` (artifact bytes), and the six index tools — the same gaps,
for the same reasons, as Jira's.

### Postman complete collection-management family

Postman is a complete thirty-one-tool family over one configured workspace and
default collection. The current SDK at
`9bba9da409771803f28c0ee21f5d0b9a8f456219`, the worker-pinned SDK at
`b5113a129329b85d23c2d5c2bf55f18e307414ec` plus its two MCP-only patches,
and Main's catalog revision `a78d3654f99d8ff89ca7233f20a66d676e564f79`
have the same Postman schemas and behavior; current adds only eight `read`, one
`execute`, nineteen `write` and three `delete` group annotations. Those groups
describe effects for the model and catalog. They neither grant execution nor
decide whether a deployment marks a particular read or effect sensitive.

Main freezes the schema-declared nested `postman_configuration`, selected tool
names, default `collection_id` and `environment_config`. Claim-time
materialization redeems the nested API key before the Python worker passes the
same application or ad-hoc settings to the SDK. The Rust configuration parser
accepts that materialized shape, requires a nonempty API key despite the
source's nullable descriptor, and retains downstream environment values only
as zeroizing canonical JSON. Toolkit construction performs no provider I/O.
The configuration catalog truthfully says connection checking is unsupported;
the source toolkit's nominal checker addresses nonexistent flattened fields
and is not reproduced.

The catalog retains all source operations in order: eight collection, folder,
request, script, search and deterministic analysis reads; twenty-two Postman
management effects; and `execute_request`. Management calls are confined to
the configured HTTPS Postman origin, percent-encode all identifiers, disable
redirects and implicit retries, and apply finite request, response, JSON,
traversal and output bounds. Reads return native bounded projections. Stored
auth, variable values, headers and bodies are redacted unless a tool explicitly
requests a bounded script. Path resolution uses exact case-insensitive segments
and rejects sibling ambiguity instead of silently selecting or overwriting the
first substring match.

The SDK's `PostmanAnalyzer` remains business functionality rather than model
decoration. Rust retains its collection, folder and request security,
performance, documentation, test-coverage, naming, hardcoded-data, issue,
score, recommendation and optional improvement rules. Traversal is iterative
and bounded, and projected URLs remove user information and secret query
values. Search still evaluates the raw collection fields before redacting the
returned matches, so hardening does not change which requests match.

Collection and subtree mutations preserve the source's whole-collection or
individual folder/request routes, null semantics and result meaning. Rust
rejects the source's crashing null request-body case before dispatch, treats
folder `auth: null` as unchanged while collection/request null clears auth, and
never retries an effect. A transport, unexpected-status, confirmation or
post-success projection failure after dispatch is a nonretryable unknown
outcome. The in-process mutation lock is only a local ordering aid; production
activation still requires a durable per-collection fingerprint/fence plus
effect receipt and reconciliation so concurrent worker invocations cannot
overwrite a newer collection.

`execute_request` is intentionally a separate authority boundary. Reading a
saved request from Postman does not authorize sending its variable-expanded
URL. The production client therefore has no constructor or injection point for
the sealed downstream-egress authority. A future owner must bind the exact
invocation, approved origin and DNS result; reject private, loopback,
link-local and rebinding targets; disable or reauthorize redirects; filter
dangerous headers; bound variable expansion, request and response data; retain
duplicate query/form pairs; redact credentials and payloads; and bind any
remote effect to the durable interrupt and effect receipt. Request-level auth
is the only saved-request auth fallback, matching the SDK rather than widening
credential use to collection or folder auth. Raw JSON with line comments keeps
the SDK's JSON execution behavior through a bounded string-aware cleaner, so a
`//` inside a quoted URL is not corrupted. Postman pre-request and test scripts
remain stored data and are not executed by this tool.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `configurations/postman.py::PostmanConfiguration` and Main's frozen configuration/toolkit catalogs | Nested Postman origin, workspace and API-key authority plus claim-time materialization | `config.rs` validates the claim-owned HTTPS authority and stores the dynamic profile as zeroizing data |
| `tools/postman/__init__.py::PostmanToolkit` and `api_wrapper.py` argument models/catalog | Exact 31-tool order, selected-tool behavior, defaults, schemas and operation groups | `tools.rs` exposes selection-oriented descriptions and executable bounded schemas without leaking origin or credentials |
| `PostmanApiWrapper::_make_request`, collection readers and management effects | Fixed-origin management wire, result projection, full-collection and individual-resource mutations | `client.rs` owns one bounded no-redirect/no-retry client, typed safe errors and effect-aware outcomes |
| `postman_analysis.py::PostmanAnalyzer` | Request/folder/collection scoring, findings, recommendations, improvements and path helpers | `analysis.rs` preserves deterministic rules with bounded iterative traversal and redacted URL projection |
| `PostmanApiWrapper::execute_request` and `_apply_authentication` | Stored-request variable/auth/body resolution followed by arbitrary downstream HTTP | the private dynamic-execution boundary has no production authority constructor until claim-bound egress is composed |
| Main toolkit freezer/materializer and Python `EliteaSdkAgentAdapter` | Same nested tool settings for application and ad-hoc requests | focused Rust fixtures cover the materialized shape; live Main-to-worker proof remains an activation gate |

Production registration remains disabled pending authorized application/ad-hoc
materialization, live Postman management proof, durable exact-`interrupt_id`
sensitive-tool continuation, per-collection effect ownership and reconciliation,
and the separately sealed dynamic-egress authority. Every read may independently
be sensitive because collections can contain credentials, scripts and payloads.

### Yagmail complete SMTP family

Yagmail is a complete one-tool family over inline toolkit settings rather than a
separately registered configuration. Current SDK `9bba9da` and worker-pinned SDK
`b5113a1` expose the same `send_gmail_message` operation and SMTP behavior;
current adds only its `write` group metadata. Main's frozen toolkit schema marks
only the top-level password as secret, preserves the frozen host, username and
selection, and redeems the password only for the accepted application or ad-hoc
claim. No connection check or indexing capability exists.

The source lazily sends through implicit TLS on configured host port 465 and
normalizes a username without `@` to a Gmail address. It also stores its client
on the wrapper class, uses an unverified Python TLS context, retries a disconnected
send up to three times, has no deadlines or bounds, and can interpret a message
that names a local file as an attachment. The Rust client instead owns one
non-debuggable, zeroizing credential set per materialized toolkit; accepts only a
bounded DNS host (defaulting absent or null to `smtp.gmail.com`); verifies native
trust roots and the hostname; supports CRAM-MD5, PLAIN and LOGIN authentication;
and makes exactly one bounded SMTP transaction. It treats the message as literal
UTF-8, creates escaped text/plain and text/html alternatives, folds MIME headers,
dot-stuffs DATA, and returns the source-compatible empty object when every
recipient is accepted. A partial recipient refusal becomes a bounded receipt of
address and SMTP code without provider diagnostic text. Timeout, disconnect or
invalid acceptance after DATA begins is a nonretryable unknown outcome.

The Python generated argument schema accidentally requires `cc` even though the
method defaults it to `None` and shared dispatch removes explicit null values.
Rust deliberately repairs that mismatch: `cc` is optional-null with default null,
so omission and null both mean no copy recipients. Rust also rejects an unknown
persisted selected-tool name instead of silently materializing an empty family.
The public schema and tested descriptions make character and UTF-8 byte limits,
mailbox format, literal-body semantics, one-attempt behavior and duplicate risk
visible to the model. The operation remains non-read-only and
non-concurrency-safe; the `write` group is metadata, not authorization.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/yagmail/__init__.py::{EliteAYagmailToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline host, username, sealed password and selected-tool materialization | `config.rs` validates the exact claim-owned host/login, normalizes the safe host/username defaults and keeps the password zeroizing |
| `yagmail_wrapper.py::{SendEmail,YagmailWrapper}` | One public send schema, runtime default/null handling and SDK class-global wrapper | `tools.rs` exposes the complete catalog, truthful optional `cc`, selection-oriented descriptions and independent policy metadata |
| pinned `yagmail==0.15.293` sender, address and MIME helpers | TLS SMTP authentication, envelope recipients, DATA and result behavior | `client.rs` owns verified implicit TLS, bounded SMTP/MIME parsing, one-attempt effect semantics and redacted typed outcomes |
| Main application/ad-hoc freezer and claim materializer | Password remains sealed until accepted execution | focused Rust fixtures cover inline materialization and secret-safe construction; live Main-to-worker proof remains an activation gate |

Production registration remains disabled until the shared direct-sensitive-tool
wrapper authorizes the exact invocation by durable `interrupt_id`, a durable
pre-send intent binds the exact arguments and stable Message-ID, effect receipts
and unknown outcomes can be reconciled across restart/cancellation, SMTP egress is
restricted to the frozen host on port 465 with DNS/IP policy, and a live server
with a trusted certificate proves the complete claim-materialization path.

### Keycloak complete Admin REST family

Keycloak is a complete one-tool family over inline toolkit settings rather than
a separately registered configuration. Current SDK `9bba9da` and worker-pinned
SDK `b5113a1` expose the same `execute` operation and request behavior; current
adds only its `execute` group metadata. Main's toolkit schema marks
`client_secret` as secret and freezes the HTTPS base URL, realm, client ID and
selection for claim-time materialization. Neither the configuration nor toolkit
advertises a connection check, and the family has no indexing behavior.

The source obtains a client-credentials token for every invocation, then sends
one arbitrary Admin REST method to `/admin/realms/{realm}{relative_url}` and
returns the raw response text. Its shared `requests.Session` also carries Basic
authentication, which can overwrite the intended Bearer header on the Admin API
request. It replaces every single quote in `params`, admits arbitrary paths,
follows redirects, has no finite response or request deadline, and returns raw
provider failures. Rust intentionally repairs those security and correctness
defects: one invocation-scoped non-debuggable client owns a fixed normalized
HTTPS origin/context path, percent-encoded realm and zeroizing secret. The token
request uses the source form credentials without session-global Basic auth; the
Admin call uses only the returned Bearer token. Redirects and protocol retries
are disabled and the token is not cached or refreshed.

The complete public method surface remains available, including custom bounded
HTTP method tokens and DELETE. A relative URL must begin with one slash and stay
inside the configured realm; schemes, authorities, fragments, traversal,
decoded separators, backslashes and controls are rejected. Query parameters
remain part of `relative_url`, while `params` is a strict JSON object string sent
as the request body for every method. An omitted, null or empty `params` value
sends `{}`, preserving the runtime default without accepting single-quoted
pseudo-JSON. Every 2xx response returns bounded UTF-8 text, including an empty
string for 204. GET, HEAD and OPTIONS are read-class requests; all other methods
are effects. No request is automatically retried, and timeout, transport,
transient-status or post-accept projection failure after effect dispatch becomes
a nonretryable unknown outcome.

The tool and parameter descriptions are a tested selection contract. They state
the fixed configured realm, relative path/query format and example, strict JSON
body semantics, accepted method scope, 512 KiB serialized-result ceiling,
confidentiality and mutation/delete/action risk, independent approval,
one-attempt behavior and reconciliation requirement. The tool is always non-read-only and
non-concurrency-safe because its arguments select the effect; the `execute`
group remains metadata and never supplies authorization.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/keycloak/__init__.py::{KeycloakToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline base URL, realm, client ID, sealed client secret and selected-tool materialization | `config.rs` validates one claim-owned HTTPS realm authority, retains an optional deployment context path and keeps the exact secret zeroizing |
| `tools/keycloak/api_wrapper.py::Execute` and `KeycloakAPIWrapper` | One public generic method/path/body schema, per-call client-credentials token and Admin REST dispatch | `tools.rs` exposes the complete catalog and selection-oriented bounded schema; `client.rs` owns the exact two-request wire with proper Bearer separation |
| Main application/ad-hoc freezer and claim materializer | Secret remains sealed until accepted execution | focused Rust fixtures cover inline materialization and secret-safe construction; live application/ad-hoc provider proof remains an activation gate |

Production registration remains disabled until the shared direct-sensitive-tool
wrapper authorizes the exact invocation by durable `interrupt_id`; a durable
effect intent/receipt owner binds method, path and canonical body and reconciles
unknown outcomes across cancellation/restart; egress constrains the frozen
Keycloak origin and DNS/IP resolution; and live Keycloak service-account roles
prove both application and ad-hoc claim materialization. Read requests may also
be independently sensitive because Admin API responses contain identity data.

### Azure complete Resource Manager family

Azure is a complete two-tool family over inline toolkit settings rather than a
separately registered configuration. Current SDK `9bba9da` and worker-pinned
SDK `b5113a1` expose the same source-ordered `execute` and
`azure_integration_healthcheck` operations; current adds only `execute` and
`read` group metadata. Main's frozen toolkit catalog marks only `client_secret`
as secret and otherwise freezes the subscription, tenant, client and selected
tool names for claim-time materialization. No public configuration connection
check or indexing behavior exists; the health check is itself the second model
tool.

The intended source flow creates a client-secret credential, obtains a token
for `https://management.azure.com/.default`, and dispatches an arbitrary HTTP
method to a caller-provided Azure Resource Manager URL. In both pinned SDKs the
operation is unusable because it calls missing `json_query_load` and
`bad_domain` helpers before its exception boundary. If those calls were
available, the source would still accept arbitrary request kwargs and origins,
follow redirects, expose raw provider text, and provide no finite body or
deadline bounds. Rust repairs that broken boundary without dropping either
tool or any bounded RFC HTTP method token.

One invocation-owned non-debuggable client retains the exact claim-materialized
subscription, tenant, client ID and zeroizing secret. It performs the official
client-credentials form exchange at the fixed public Microsoft Entra endpoint,
then sends one Bearer-authenticated request to the exact
`https://management.azure.com` origin. The URL must remain below the configured
`/subscriptions/{subscription_id}` scope; userinfo, alternate ports or origins,
fragments, traversal, decoded separators and redirects are rejected. This is an
intentional security narrowing of the source's unscoped absolute URL, and the
inline schema has no sovereign-cloud authority with which to authorize another
login or ARM origin.

`optional_args` remains compatible with object or JSON-object-string input, but
its executable schema admits only bounded `headers`, `params`, `json`, `data`
and `files`. Authorization, Host, length and hop-by-hop headers cannot be
overridden. Query values are scalar or scalar arrays. JSON, raw text and form
bodies are bounded; multipart values are inline text or
`[filename,text,content_type?,headers?]`, with at most sixteen files and 240 KiB
of file content. No value is interpreted as a local path or artifact reference.
The complete materialized argument remains below the shared 256 KiB boundary.

Every successful 2xx response returns one bounded UTF-8 body string, including
an empty string for no content. GET, HEAD and OPTIONS are read-class requests;
all other methods are effects. No token or ARM request is automatically
retried. Timeout, transport, transient status, redirect, oversized response or
post-accept projection failure after effect dispatch becomes a nonretryable
unknown outcome. The read-only health tool makes the documented resource-group
list request with API version `2021-04-01` and preserves the source tuple as a
JSON array: `[true,""]` or `[false,<stable redacted reason>]`.

The tool and parameter descriptions are tested model-selection contracts. They
state the fixed subscription authority and absolute URL example, required API
version, method/effect split, exact option shapes, inline-only multipart rule,
512 KiB result ceiling, confidential read risk, independent approval, one-shot
transport and reconciliation requirement. The generic tool is always
non-read-only and non-concurrency-safe because its arguments select the effect;
group metadata grants no authority.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/cloud/azure/__init__.py::{AzureToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline subscription, tenant, client ID, sealed client secret and selected-tool materialization | `config.rs` validates one public-cloud subscription authority, keeps the secret zeroizing and performs no construction I/O |
| `tools/cloud/azure/api_wrapper.py::{AzureApiWrapper.execute,azure_integration_healthcheck}` | Generic ARM dispatch and resource-group health read | `tools.rs` exposes both source operations with selection-oriented schemas; `client.rs` owns the bounded two-request OAuth/ARM wire and stable result/error projection |
| Microsoft Entra client-credentials and ARM resource-group REST contracts | Token form, Bearer use and exact health route | deterministic request fixtures cover both origins, request shapes, status classes and effect ambiguity; live credential proof remains gated |
| Main application/ad-hoc freezer and claim materializer | Client secret remains sealed until accepted execution | focused Rust fixtures cover inline materialization and tenant isolation; full application/ad-hoc live provider proof remains an activation gate |

Production registration remains disabled until the shared direct-sensitive-tool
wrapper authorizes the exact invocation by durable `interrupt_id`; a durable
effect intent/receipt owner binds method, URL, headers and canonical body and
reconciles unknown outcomes across cancellation or restart; external egress
enforces the approved public-cloud Entra and ARM DNS/IP destinations; and live
Azure service-principal roles prove both application and ad-hoc materialization.
Health and generic read requests may also be independently sensitive because
ARM responses contain tenant resource metadata.

### Elasticsearch and compatible OpenSearch complete read family

Elasticsearch is a complete one-tool read family over inline toolkit settings,
and the same direct REST implementation supports compatible OpenSearch search
clusters without an Elasticsearch client library or version handshake.
Current SDK `9bba9da` and worker-pinned SDK `b5113a1` expose the same
`search_elastic_index(index, query)` operation and generated argument schema;
current adds only `read` group metadata. Main's frozen toolkit catalog marks
the optional top-level `api_key` as secret and uses `url` as the toolkit naming
field. There is no registered configuration object, public connection check or
indexing overlay in this family.

The pinned Python worker does not declare the `elasticsearch` package, so the
wrapper's guarded import can make toolkit construction fail before a tool is
materialized. Even when installed, the public schema supplies one optional
secret string while the wrapper model requires a two-string tuple, stores its
client on the wrapper class, and disables certificate verification. Query and
response sizes, deadlines and result windows are unbounded. Rust treats these
as source defects rather than dropping the family or copying unsafe behavior.

One invocation-owned, non-debuggable client requires an exact claim-materialized
HTTPS cluster origin. Root-path custom ports remain supported, while userinfo,
configured path prefixes, queries, fragments, malformed percent syntax and
plaintext HTTP are rejected. The optional public API-key string is interpreted
as Elasticsearch's encoded API-key value and sent only as
`Authorization: ApiKey <encoded-value>`, matching the provider's documented
header contract. Anonymous search remains available when the key is null or
omitted, as in the SDK schema. Native root and hostname verification are on;
ambient proxies, redirects and automatic retries are off.

The sole operation issues exactly one `POST /{index}/_search` with a JSON Query
DSL object. The target may be one index, data stream or alias expression, a
comma-separated set, or a bounded `*`/`?` wildcard. Path separators, traversal,
date-math syntax, remote-cluster colons and expressions beginning with reserved
prefix characters are rejected. The query must be a JSON-object string below
64 KiB of UTF-8 input with bounded depth, nodes and strings. `size` keeps the provider default
of ten when absent and is capped at one hundred; `from` is capped at ten
thousand. The tool performs no scroll or continuation requests. Broad wildcard,
script, runtime-field and aggregation cost remains subject to the cluster's own
query controls and is called out to the model.

OpenSearch originated from Elasticsearch 7.10.2 and retains this core Search
API path, JSON request form and response structure. The two products have since
diverged, so the model-facing query description requires clauses supported by
the configured cluster rather than promising cross-product support for every
new query feature. Anonymous OpenSearch clusters and current OpenSearch API
keys use the existing wire contract. Older/self-managed deployments that only
offer Basic authentication and Amazon OpenSearch domains protected by AWS
Signature Version 4 are not supported by the SDK's URL-plus-optional-key schema;
those remain explicit configuration/authentication gaps rather than silently
reinterpreting the saved `api_key`.

One successful response must have a JSON media type, including Elasticsearch
and OpenSearch `application/*+json` vendor forms, and a top-level object. The provider
body is capped at 2 MiB before decode and the serialized model result at 512
KiB. The native response object remains intact, including hits, aggregations,
timing and shard metadata. Authentication, authorization, not-found,
rate-limit, timeout, unavailable, malformed and resource-limit failures are
typed, redacted and never include the origin, key, index, query or provider
body.

The tool and parameter descriptions are tested model-selection contracts. They
state index/alias/data-stream selection, wildcard and comma examples, exact
Query DSL form, result-window limits, first-response-only behavior, result
shape and ceiling, confidential-data risk, independent approval and expensive
query cues. The tool is read-only and concurrency-safe at the invocation
boundary; `read` metadata still grants no authorization and deployment policy
may independently require exact-invocation approval.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/elastic/__init__.py::{ElasticToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline cluster URL, optional sealed API-key string, selection and source group | `config.rs` validates one claim-owned HTTPS authority, normalizes the encoded key contract and performs no construction I/O |
| `tools/elastic/api_wrapper.py::{ELITEAElasticApiWrapper.search_elastic_index,SearchElasticIndexModel}` | One index plus Query DSL string search and native response | `tools.rs` exposes the complete bounded model contract; `client.rs` owns the exact Search API wire and stable projection/error taxonomy |
| Elasticsearch and OpenSearch Search APIs plus compatible API-key authentication | `POST /{index}/_search`, JSON body and optional encoded API-key header | deterministic fixtures cover exact route, header/body, native/vendor JSON media types, response shapes and status classes; Basic auth and Amazon SigV4 remain gated |
| Main application/ad-hoc freezer and claim materializer | API key remains sealed until accepted execution | focused Rust fixtures cover inline parsing, redaction and tenant isolation; full application/ad-hoc live-provider proof remains gated |

Production registration remains disabled until Main projection and claim-time
materialization are composed for this hidden family, external egress constrains
the frozen cluster DNS/IP destination, and a live cluster proves encoded-key or
anonymous authentication, index privileges and acceptable query-load policy.
Searches may independently be sensitive because indexed documents can contain
private operational or customer data.

### GCP complete scoped REST family

GCP is a complete one-tool family over inline toolkit settings rather than a
separately registered configuration. Current SDK `9bba9da` and worker-pinned
SDK `b5113a1` expose the same `execute_request` operation and schema; current
adds only `execute` group metadata. Main's frozen toolkit catalog marks the
`api_key` string as secret. Despite that legacy name, its value is the complete
Google service-account JSON document. There is no public connection check or
indexing behavior.

The Python wrapper eagerly parses and refreshes Google credentials while the
toolkit is being constructed, accepts caller-selected scopes, arbitrary URLs
and arbitrary `requests` keyword arguments, follows redirects and returns raw
provider failures. Its execution path also passes a Pydantic `SecretStr` where
Google Auth expects the decoded service-account object. Rust intentionally
repairs that unusable and over-broad boundary without dropping the generic
operation or any bounded RFC HTTP method token.

One invocation-owned, non-debuggable client parses only a claim-materialized
`type=service_account` document whose token URI is exactly
`https://oauth2.googleapis.com/token`. It retains the PKCS#8 RSA private key in
a zeroizing buffer, requires at least a 2048-bit modulus, signs one one-hour
RS256 JWT bearer assertion and exchanges it with one form POST. The claims bind
the service-account email, one to thirty-two exact unique
`https://www.googleapis.com/auth/...` scopes, the fixed token audience, and one
captured issue time. Construction performs no network access and no ambient
credential or environment fallback exists.

The API URL must use verified HTTPS at `googleapis.com` or one of its
subdomains. Userinfo, custom ports, fragments, repeated path separators,
traversal, decoded separators, malformed or double percent escapes, redirects
and alternate origins are rejected. `optional_args` accepts only bounded
`params`, `headers`, `json`, and `data`: query values are scalars or repeated
scalar arrays; headers cannot replace Authorization, Host, length, proxy or
hop-by-hop transport fields; `json` is serialized as JSON; and `data` is a
literal UTF-8 body or form object. Filesystem paths, artifacts, client objects,
certificate overrides, proxy controls, redirect controls and retry controls are
not part of the public contract. The complete argument stays below the shared
256 KiB boundary, the request body below 240 KiB and serialized JSON output
below 512 KiB.

The client performs exactly one OAuth exchange and one API request, with native
root verification, no proxy inheritance, no redirect and no automatic retry.
Successful JSON is returned as its native value; an empty 2xx preserves the
source success string. GET, HEAD and OPTIONS are reads; all other methods are
effects. Once an effect request is dispatched, timeout, transport loss,
redirect, 408, 429, 5xx, oversized response or post-accept projection failure
becomes a nonretryable unknown outcome. Read failures use stable data-free
authentication, authorization, not-found, rate-limit, timeout, unavailable,
invalid-response and resource-limit errors.

The tool and parameter descriptions are tested model-selection contracts. They
state the exact scope and Google-origin forms, option shapes, method-to-effect
split, 512 KiB result ceiling, confidential-read risk, one-attempt behavior,
202 Accepted semantics and reconciliation requirement. The generic tool is
always non-read-only and non-concurrency-safe because its arguments determine
the effect; its `execute` group remains metadata and grants no authority.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/cloud/gcp/__init__.py::{GcpToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline sealed service-account JSON, selected-tool materialization and source group | `config.rs` validates the official service-account fields, retains the RSA key in zeroizing memory and performs no construction I/O |
| `tools/cloud/gcp/api_wrapper.py::{GcpApiWrapper.execute_request,ExecuteRequest}` | One generic method/scope/URL/options schema and authenticated request | `tools.rs` exposes the complete bounded model contract; `client.rs` owns JWT signing, OAuth exchange, Google authority and stable result/error projection |
| Google service-account OAuth JWT bearer contract | RS256 assertion, fixed token form and scoped Bearer use | deterministic fixtures decode header/claims/form and prove one token plus one API request |
| Main application/ad-hoc freezer and claim materializer | Service-account JSON remains sealed until accepted execution | focused Rust fixtures cover inline parsing and redaction; full application/ad-hoc live-provider proof remains an activation gate |

Production registration remains disabled until the shared direct-sensitive-tool
wrapper authorizes the exact invocation by durable `interrupt_id`; a durable
effect intent/receipt owner binds method, scopes, URL, headers and canonical
body and reconciles unknown outcomes across cancellation or restart; external
egress enforces the approved OAuth and Google API DNS/IP destinations; and live
service-account roles prove both application and ad-hoc claim materialization.
Reads may independently be marked sensitive because Google API responses can
contain tenant data, IAM policy and infrastructure metadata.

### Kubernetes complete REST family

Kubernetes is a complete two-tool family over inline toolkit settings rather
than a separately registered configuration. Current SDK `9bba9da` and
worker-pinned SDK `b5113a1` expose the same source-ordered
`execute_kubernetes` and `kubernetes_integration_healthcheck` operations;
current adds only `execute` and `read` group metadata. Main's frozen toolkit
catalog marks only `token` as secret. There is no public configuration check,
indexing behavior, or generated provider-specific operation: the model-facing
health tool is the second operation.

The source either constructs a client for the supplied URL and token with TLS
certificate verification disabled, or silently loads ambient kubeconfig when
either value is missing. Its validator stores the client on the wrapper class,
so a later toolkit can overwrite an earlier toolkit's origin and credential.
The generic method accepts arbitrary HTTP methods, paths, headers, and JSON,
but the advertised object forms for `body` and `headers` are passed to
`json.loads` and fail. Responses are unbounded and nonempty error bodies are
returned as if they were ordinary results; health reports success for any
JSON-decodable response regardless of HTTP status.

Rust preserves both public operations and every bounded RFC HTTP method token,
while intentionally removing those unsafe behaviors. One invocation-owned,
non-debuggable client requires the claim-materialized exact HTTPS cluster
origin and zeroizing Bearer token; it never reads kubeconfig, proxy variables,
or another ambient credential source. Userinfo, non-root configured paths,
query/fragment authority, cross-origin redirects, malformed escapes, repeated
path separators, decoded traversal and encoded separators are rejected. An
explicit custom port such as `6443` remains supported. The client verifies the
certificate and hostname against native trust roots and performs no automatic
retry.

`body` and `headers` truthfully accept an object or strict JSON-object string.
Bodies are bounded JSON objects; the source's array-excluding schema remains
intact. Headers are a bounded string map and may select Kubernetes patch media
types, but cannot replace Authorization, Host, length, proxy, or hop-by-hop
transport headers. The complete argument stays below the shared 256 KiB limit,
one request body below 240 KiB, and one serialized UTF-8 result below 512 KiB.
The sub-URL starts with one slash and may carry a bounded query because the
public schema exposes no separate query parameter.

GET, HEAD, and OPTIONS are classified as reads; all other methods are effects.
Every 2xx response, including 202 Accepted, preserves the source's UTF-8 body
string. Read failures use stable redacted typed errors. Once an effect request
is dispatched, transport loss, timeout, redirect, 408, 429, 5xx, oversized or
unprojectable success becomes a nonretryable unknown outcome. The health tool
uses exactly `GET /version` and returns `[true,""]` only for a successful JSON
response, otherwise `[false,<stable redacted reason>]`.

The model-facing descriptions are tested selection contracts. They state the
configured-cluster boundary, exact path/query form, method-to-effect split,
body/header shapes, patch media-type use, result ceiling, confidential-read
risk, 202 semantics, one-attempt behavior and reconciliation requirement. The
generic operation is always non-read-only and non-concurrency-safe because its
arguments determine the effect; group metadata grants no authority.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `tools/cloud/k8s/__init__.py::{KubernetesToolkit,get_tools,toolkit_config_schema}` and Main's frozen toolkit catalog/materializer | Inline cluster URL, sealed token, selection and source groups | `config.rs` requires one exact claim-owned HTTPS authority, keeps the token zeroizing and performs no construction I/O |
| `tools/cloud/k8s/api_wrapper.py::{KubernetesApiWrapper.execute_kubernetes,kubernetes_integration_healthcheck}` | Generic REST dispatch and `/version` health read | `tools.rs` exposes both operations with selection-oriented schemas; `client.rs` owns bounded verified-TLS wire and stable result/error projection |
| Main application/ad-hoc freezer and claim materializer | Token remains sealed until accepted execution | focused Rust fixtures cover inline materialization and tenant isolation; full application/ad-hoc live-cluster proof remains an activation gate |

Production registration remains disabled until the shared direct-sensitive-tool
wrapper authorizes the exact invocation by durable `interrupt_id`; a durable
effect intent/receipt owner binds method, path, headers and canonical body and
reconciles unknown outcomes across cancellation or restart; external egress
enforces the approved cluster DNS/IP destination; and either a native-root
certificate or a future claim-owned CA contract plus live Kubernetes RBAC prove
both application and ad-hoc materialization. Reads may independently be marked
sensitive because API responses can expose secrets, workload configuration and
cluster topology.

### BigQuery partial warehouse family

BigQuery is a configuration-backed family: Main seals
`bigquery_configuration.api_key`, which holds the complete service-account
JSON, and freezes the optional `project`, `location`, `dataset` and `table`
defaults beside it. Worker-pinned SDK `b5113a1` exposes eleven tools. The SDK
toolkit binds only explicitly selected tools (an empty selection binds none),
prefixes `Toolkit:` and then `Project:` to each description and cuts it to 1000
characters. Rust keeps all three rules.

The pinned SDK cannot run any of its tools. `BigQueryAction` is built without
`mode`, so every call dispatches the empty tool name and returns an
`Error: ... Unknown tool name` traceback; the vector tools call a
`_row_to_document` helper that does not exist; the vector SQL calls
`VECTOR_DISTANCE`, which BigQuery does not have; and `execute` requires a client
that only a previous call would have created. Rust therefore ports what each
wrapper method was written to do rather than the failure, and records each
repair here.

| Python source | Observable responsibility | Rust target |
| --- | --- | --- |
| `configurations/bigquery.py` and `tools/google/bigquery/__init__.py::{BigQueryToolkit,get_tools}` | Nested sealed service account, table defaults, explicit-only selection and description prefix | `config.rs` parses `bigquery_configuration` through the `gcp` family's non-debuggable signing configuration; `tools.rs` selects in SDK order and omits unserved names with a warning |
| `BigQueryApiWrapper.bigquery_client` (`google.cloud.bigquery.Client`) | Service-account OAuth, job project (configured, else the key's `project_id`) and location | `client.rs` signs with `gcp::client::service_account_token_request` and the `bigquery` scope, then runs jobs.query, jobs.getQueryResults polling and paging over verified HTTPS with no redirect or retry |
| `get_documents`, `_create_filters`, `_get_table_id` | `SELECT *` over the configured table with `doc_id IN UNNEST(@ids)` and a dict or WHERE-text filter | `tools.rs::where_clause` reproduces the bare-number/quoted-text rule; values become escaped GoogleSQL literals, keys must be field paths and `;` is refused |
| `similarity_search_by_vector[_with_score]`, `similarity_search_by_vectors`, `batch_search` | Nearest rows by a `FLOAT64` array parameter, `ORDER BY score ASC LIMIT k`; `batch_search` quotes every dict value and refuses text queries without an embedding model | `EUCLIDEAN_DISTANCE(embedding, @query_embedding)`, the distance the SDK's own vector index declares; the `{**d, ...}` branches of `similarity_search_by_vectors` are kept as written |
| `create_vector_index`, `create_delta_lake_table` | `CREATE VECTOR INDEX IF NOT EXISTS ... IVF/EUCLIDEAN`; tables.insert of a `DELTA_LAKE` external table with `exists_ok=True` | DDL and tables.insert are effects: a post-dispatch timeout, 408, 429, 5xx or unreadable answer is a non-retryable unknown outcome; a 409 reads the existing table back |
| `job_stats` | `get_job(job_id)._properties["statistics"]` | jobs.get in the configured location, projected to `statistics` |
| `process_output` (`json.dumps(rows, default=str)`) | Result text | `format.rs` emits CPython's default `json.dumps`: schema column order, `", "`/`": "` separators, `ensure_ascii`, `repr` floats and the `str()` forms of TIMESTAMP, DATETIME and TIME values the Python client builds |

Not served: `similarity_search` and `similarity_search_with_score` embed text
through `self.embedding`, which no toolkit path sets, and `execute` reflects
over arbitrary Python client methods, which has no REST equivalent. They are
absent from `supported_tools.bigquery`, so the catalogue marks them
unavailable. A result over 1000 rows is refused with a narrow-the-filter error
rather than returned unbounded. Production registration remains gated on the
same live service-account, exact-interrupt HITL, effect-reconciliation and
egress proofs as the `gcp` family.

### Figma partial design-file family

Ported from the worker-pinned SDK revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec` (no pinned patch touches Figma)
and the `FigmaPy==2018.1.0` client it wraps. Configuration is
`figma_configuration.token` (sent as a sensitive `X-Figma-Token`) plus the
toolkit-level `global_limit` and `global_regexp` defaults; a missing token or
a `global_regexp` that does not compile refuses the toolkit, as the SDK's
validator does. All calls go to the fixed `https://api.figma.com/v1/` origin
through the family's bounded transport (no redirects or automatic retries,
16 MiB response cap); keys and ids are single percent-encoded path segments.

| SDK tool | Route | Result |
| --- | --- | --- |
| `get_file_nodes` | `GET files/{key}/nodes?ids=` | Raw JSON through `process_output` |
| `get_file` | `GET files/{key}` | FigmaPy `File` attributes: `name`, `last_modified`, `thumbnail_url`, `document`, `components`, `schema_version`, `styles` |
| `get_file_versions` | `GET files/{key}/versions` | `versions`, `pagination` |
| `get_file_comments` | `GET files/{key}/comments` | `comments` rebuilt from FigmaPy `Comment`'s nine attributes |
| `post_file_comment` | `POST files/{key}/comments` | `{"message", "client_meta"?}` body (the SDK's override of FigmaPy's broken payload); one effect |
| `get_file_images` | `GET images/{key}?ids=` | `err`, `images` |
| `get_team_projects` | `GET teams/{id}/projects` | `projects` |
| `get_project_files` | `GET projects/{id}/files` | `files` |
| `extract_design_tokens` | `GET files/{key}/nodes?ids=&depth=` | Colours, strokes, typography, effects, summary and raw entries |
| `extract_design_tokens_batch` | the same, up to five entries at once | `full`, `compact`, `summary` or `style_guide` |

`output.rs` reproduces `process_output`: the result is serialized as Python's
`json.dumps` writes it (`", "`/`": "` separators, `ensure_ascii`), because
`limit` counts characters of that text and `regexp` is matched against it.
`regexp` uses `fancy-regex`, so lookaround (which the SDK's own documented
example uses) behaves as in Python `re`, under a bounded backtracking budget.
Over the limit, the RAW result is reduced by `fields_retain`/`fields_remove`
between `depth_start` and `depth_end`, prefixed with the SDK's note and cut at
`limit` characters. An empty result answers the SDK's "Response result is
empty" text, and an unusable control answers `Error in '<tool>': ...`, as the
SDK's catch-all does.

`tokens.rs` reproduces the extraction and the four dedup passes, including
Python's `round` (ties to even; an integer stays an integer) and value-equal
dedup keys (`1` and `1.0` collide, as in a Python dict).

Deliberate differences:

- `simplified_dict` turns everything below depth 3 into Python `str()`
  reprs (so a file's pages and a version's user become repr strings). Rust
  returns those values as JSON; key order follows the worker's `serde_json`.
- FigmaPy appends `geometry`, `version`, `scale` and `format` to the query
  as bare values (`?paths`), so Figma never received them. Rust sends them
  as named parameters; a `key=value` argument (the SDK's own internal
  `depth=1`) is kept as that pair.
- A non-2xx Figma answer is a redacted error instead of the SDK's
  `Error in '...': Figma API error <status>: <body>` text, and an ambiguous
  `post_file_comment` is an unknown-outcome error.
- Token results are JSON values (the SDK returns `json.dumps` text); a batch
  holds at most 100 entries.

Not ported: `analyze_file` renders frames to images and asks the toolkit's
LLM to explain screens and flows, over the 2,900-line TOON serializer; a
Rust toolkit is materialized from settings alone and holds no model
authority, and a model-free variant would answer something the SDK never
does. The six indexing tools wait for the shared indexing overlay. The
capability snapshot lists the ten served tools.

### Carrier partial performance-testing family

Ported from the worker-pinned SDK revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec` (the five pinned patches do not
touch Carrier). The SDK has no focused Carrier tests, so the fixtures in
`libs/rust/agent-runtime/src/toolkits/carrier_tests.rs` are the primary
compatibility proof.

Configuration is the SDK's: `carrier_configuration.{url,organization,private_token}`
plus the toolkit-level `project_id`, which the SDK refuses when empty. The URL
must be HTTPS without credentials, query or fragment; a path prefix is kept.
`project_id` becomes a path segment, so only a bounded URL-safe token (or a
non-negative integer frozen by an older form) is admitted. The token is a
sensitive header and never reaches an error.

Every request goes through the family's bounded transport (redirects and
automatic retries disabled, fixed timeouts, 4 MiB response cap). Each model id
is ONE percent-encoded path segment, so `report_id="55/../x"` cannot reach a
second route. The SDK's two header sets are kept: the session calls carry
`Authorization: Bearer` and `X-Organization`; `add_tag_to_report` and
`create_backend_test` use the bare lower-case `bearer` and no organization.
`create_backend_test` and `create_ui_test` post the definition as the
form field `data`, as `requests.post(data=...)` does.

| SDK tool | Route(s) | Rust behaviour |
| --- | --- | --- |
| `get_ticket_list` | `GET issues/issues/{pid}?board_id=&limit=100` | Tag (lower-cased) and status filters, newline-joined titles |
| `create_ticket` | `GET engagements/engagements/{pid}`, `POST issues/issues/{pid}` | Date validation before any request, engagement name to hash id, `None` fields omitted, `item` required |
| `get_reports` | `GET backend_performance/reports/{pid}` | Name/tag filters and the SDK's trimmed fields |
| `add_tag_to_report` | `POST backend_performance/tags/{pid}/{id}` | Success is the answer text containing `Tags was updated`, whatever the status |
| `get_tests`, `get_test_by_id` | `GET backend_performance/tests/{pid}` | Trimmed list; `{}` when the id is absent |
| `run_test_by_id` | tests list, `GET shared/locations/default/{pid}`, `POST backend_performance/test/{pid}/{id}` | The SDK's four confirmation answers (missing test, parameters, location, cloud settings) and one run |
| `create_backend_test` | `GET integrations/integrations/{pid}?name=reporter_email`, `POST backend_performance/tests/{pid}` | The SDK's prompts for each missing field and runner/email shaping |
| `get_ui_reports` | `GET ui_performance/reports/{pid}` | Name and naive/aware ISO date filtering; a naive-versus-aware comparison answers Python's own error text |
| `get_ui_report_by_id` | reports list, `GET ui_performance/results/{pid}/{uid}?sort=loop&order=asc` | Unique sorted `.html` links under the SDK's fixed `platform.getcarrier.io` prefix; a link failure is an empty list, as in the SDK |
| `get_ui_tests` | `GET ui_performance/tests/{pid}` | Trimmed source (repository credentials are never projected), optional config and schedules |
| `run_ui_test` | tests list, `GET ui_performance/test/{pid}/{id}`, `GET shared/locations/{pid}`, `POST ui_performance/test/{pid}/{id}` | Default-configuration prompt, location validation and public/project/cloud region resolution |
| `update_ui_test_schedule` | tests list, `GET`/`PUT ui_performance/test/{pid}/{id}` | Cron shape check and the SDK's PUT body with existing schedules kept |
| `create_ui_test` | `POST ui_performance/tests/{pid}` | The SDK's success and 400/other-status markdown |
| `cancel_ui_test` | `GET ui_performance/reports/{pid}`, `PUT ui_performance/report_status/{pid}/{id}` | Command parsing, cancelable listing and final-state refusal |

Deliberate differences, each a repair of a source defect:

- The SDK's `get_tools` reads `selected_tools` from the tool dictionary rather
  than its settings, so the Python toolkit always serves all eighteen tools.
  Rust honours `settings.selected_tools`; an empty list serves all fifteen,
  an explicit list keeps the served names and omits the three SDK-only names
  with one `agent_toolkit_tools_skipped` warning, and an unknown name refuses
  the toolkit.
- `run_test_by_id` compares `str(test["id"])` with the INTEGER `test_id`, so
  an id lookup never matches in the SDK. Rust compares the id as text.
- Results that the SDK `json.dumps` are returned as JSON values; dictionaries
  the SDK returns as prompts stay JSON objects.
- A post-dispatch transport failure, 408, 429 or 5xx on an effect is an
  unknown-outcome error (never retried), where the SDK printed a failure
  report for a test or cancellation that may have applied.
- The SDK raises `KeyError` when a test parameter lacks `default`/`type`;
  Rust projects `null`.

Not ported: `get_report_by_id` downloads every `reports_test_results_<build>`
zip from the report bucket, unzips and merges JMeter/Gatling logs, re-zips the
merged errors and uploads that archive back to Carrier; `create_excel_report`
and `create_ui_excel_report` additionally parse the logs and write `.xlsx`
workbooks. Those need zip read/write and an xlsx writer that neither the
worker's nor the desktop host's Cargo lock carries (the libs workspace has
`zip` only transitively through `calamine`). The capability snapshot lists
the fifteen served tools, so elitea-main marks the three unavailable.

### Toolkit types that cannot be ported as configured families yet

Each type below was surveyed at the worker-pinned SDK revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec` (plus its pinned patches) and is
NOT dispatched by `materialize.rs`, so the Rust worker keeps skipping it with
`agent_toolkit_skipped` and the capability snapshot does not list it. None is
a missing HTTP client: every one needs an authority a configured family is
never given. `materialize.rs` builds a family from frozen settings alone (plus
the claim-lent `ArtifactToolAuthority` for `artifact`); no family receives a
model (`host::ModelTransport` is bound once, for the agent itself) or the
code sandbox (`host::CodeSandbox` reaches only pipeline code nodes). Porting a
schema without that authority would ship tools that cannot do what their SDK
twins do, so nothing is faked.

#### `pptx` (blocked)

`pptx` is a configured toolkit (`bucket_name` plus the agent's `llm`, which
the SDK injects into the settings) with two tools, both LLM-driven:

- `fill_template(file_name, output_file_name, content_description,
  pdf_file_name?, batch_size?)` downloads the template, collects every
  placeholder text frame, asks the model for structured content in batches
  of `batch_size` (with each slide's PDF page rendered by PyMuPDF as an image
  when `pdf_file_name` is given), rewrites the runs keeping their formatting,
  and uploads the result.
- `translate_presentation(file_name, output_file_name, target_language)`
  unzips the deck, asks the model to translate every text run and SmartArt
  diagram node in batches, re-zips and uploads it.

Blocked on: (1) a model authority lent to a configured family, which no
family has; (2) OOXML read/write: the worker lock carries no `zip` or
`quick-xml` (the libs workspace has `zip` only through `calamine`), so
adding them grows the worker and desktop dependency graphs; (3) PDF page
rendering for `pdf_file_name`, for which nothing in the Rust tree is a
PyMuPDF equivalent. The artifact half (`ArtifactToolAuthority` read/write)
already exists. A port is a host change first: lend a bound model (for
example a `ToolModelAuthority` beside `ArtifactToolAuthority` through
`materialize_configured_toolsets_with_artifact_authority`), then the family.

#### `imagegen` (blocked)

`imagegen` (#864, pinned SDK patch `afff2349`) has `generate_image(prompt,
size?, n?)` and `edit_image(image, prompt, mask?)`. Both POST to the
gateway's OpenAI-compatible images API (`/llm/v1/images/generations` as
JSON, `/llm/v1/images/edits` as multipart) with the TOOLKIT's own
`image_generation_model`, decode `b64_json` (or fetch `url`) results, and
save each image as `{name_prefix}{generate|edit}-{i}.png` in the toolkit's
`bucket`, answering `{"artifacts": [{"filepath", "filename"}]}`.

Blocked on the model half only:

1. `libs/rust/llm-wire/src/route.rs` declares the chat, embeddings and
   messages routes; there is no images route or response type.
2. The worker reaches the gateway only through `transport::model_facade`,
   which consumes one claim-scoped credential into ONE bound chat model
   (`ModelGatewayClient`, mTLS, origin-bound). Nothing exposes a second,
   non-chat call on that channel, and `host::ModelTransport::bind` returns
   an ADK `Llm`, not an images client.
3. `materialize.rs` lends families only the artifact authority; an image
   family needs an image-generation authority lent the same way (and Main
   must freeze `image_generation_model` into the snapshot the claim serves).

The artifact half is already there: `ArtifactToolAuthority` reads the
`edit_image` source and mask and writes the results under the live claim.
elitea-main already models the gap: `nativeWorkerGatedToolkitTypes` hides
`imagegen` on a Rust-worker deployment, and the `image_generation` internal
tool reports `rustAvailable: false`.

#### `data_analysis` (blocked)

`data_analysis` is an internal tool (the chat "Data Analysis" toggle), not a
configured family: its `bucket_name` is the conversation attachment bucket
injected at run time, and the Rust worker already recognizes and skips it
with `agent_internal_tool_skipped` (`agents/internal_tools.rs`). Its one
tool, `pandas_analyze_data(query, filename)`, loads the file into a pandas
DataFrame (CSV, Excel via calamine, Parquet, JSON, XML, HDF5, Feather,
pickle), asks the agent's LLM to write pandas code for the query (retrying up
to five times with the traceback), runs that code in the SDK's restricted
in-process executor, and saves any matplotlib chart as `chart_<uuid>.png` in
the bucket, answering the text result or the chart links.

Blocked on three authorities at once: (1) a model for code generation, which
no tool is lent; (2) an isolated Python runtime with pandas and matplotlib:
the Rust worker's only one is the Pyodide code-runner behind
`host::CodeSandbox`, reached today only by pipeline code nodes, so lending it
to a tool (with the file bytes as job input and the chart as job output) is
new host plumbing; (3) the attachment bucket as an artifact authority, which
exists (`ArtifactToolAuthority`) but is lent only to configured families.
The row in "Special runtime toolsets" above keeps the long-term direction.

### Azure DevOps Boards work item family

`ado_boards` is the first of the four Azure DevOps families. The behaviour
source is the worker-pinned SDK revision
`b5113a129329b85d23c2d5c2bf55f18e307414ec`
(`tools/ado/work_item/{__init__,ado_wrapper}.py`, `configurations/ado.py`);
none of the worker's five SDK patches touches `tools/ado`. The SDK talks to
Azure DevOps through `azure-devops` `WorkItemTrackingClient` v7.1, and Rust
calls the same REST routes and route versions directly.

The four ADO types share one authority, kept in `toolkits/families/ado/`:
`config.rs` reads `ado_configuration.{organization_url,token}`, `project`,
`limit` (default 5), `selected_tools` and the per-type optional settings;
`client.rs` owns one HTTPS-only, redirect-free, retry-free, size-bounded
transport and sends the PAT exactly as the SDK's
`BasicAuthentication('', token)` does (`Authorization: Basic base64(":" +
PAT)`, marked sensitive); `work_items.rs` holds the work item calls that
`ado_plans` reuses; `format.rs` reproduces msrest `Model.as_dict()` key
naming and Python `repr()` text; `toolset.rs` builds the ADK tools. An
Azure DevOps Server collection URL is accepted as the SDK accepts it; plain
HTTP is refused because the PAT travels as Basic credentials.

Served tools, in SDK order, with their routes:

| Tool | Route (api-version) | Result |
| --- | --- | --- |
| `search_work_items` | `POST {project}/_apis/wit/wiql?$top=` (7.1-preview.2), then `GET wit/workitems/{id}?fields=` per hit (7.1-preview.3) | `[{id, url, <field>: value or "N/A"}]`, `"No work items found."`; `limit` 0/absent uses the toolkit limit, `-1` omits `$top`; `System.Id`/`System.WorkItemType` are dropped from `fields` as in `_parse_work_items` |
| `create_work_item` | `POST {project}/_apis/wit/workitems/${type}` JSON Patch | `{id, message}`; parse failures and HTTP 400 rule errors return the SDK texts (`Issues during attempt to parse work_item_json: ...`, `Error creating work item: <provider message>`) |
| `update_work_item` | `PATCH {project}/_apis/wit/workitems/{id}` JSON Patch | `Work item (<id>) was updated.` |
| `delete_work_item` | `DELETE {project}/_apis/wit/workitems/{id}` | `Work item <id> was successfully deleted.` |
| `get_work_item` | `GET {project}/_apis/wit/workitems/{id}?fields=&asOf=&$expand=` | `{id, url, fields...}` plus msrest `relations` for `$expand` Relations/All |
| `link_work_items` | `GET _apis/wit/workitemrelationtypes` once per toolset, `PATCH _apis/wit/workitems/{source}` (no project, as the SDK) | SDK link sentence, or the SDK's "Link type is incorrect ..." text with the `{name: referenceName}` map |
| `get_relation_types` | `GET _apis/wit/workitemrelationtypes` (7.1-preview.2), cached | `{name: referenceName}` |
| `get_comments` | `GET {project}/_apis/wit/workItems/{id}/comments` (7.1-preview.4): first page `$top=<limit>`, then continuation pages of 3 | msrest-shaped comment list truncated to `limit_total` |
| `link_work_items_to_wiki_page` / `unlink_work_items_from_wiki_page` | `GET _apis/projects/{project}`, `GET wiki/wikis/{id}`, `GET wiki/wikis/{id}/pages?path=`, then one `PATCH` per work item | `vstfs:///Wiki/WikiPage/<quote(project/wiki/path)>` artifact links; the SDK's success/no-link/failure sentences with `json.dumps` failure maps |
| `get_work_item_type_fields` | `GET {project}/_apis/wit/workitemtypes/{type}` (7.1-preview.2), cached per type | The SDK report byte for byte, including `Type: Unknown` (the v7.1 field model has no `type`) |

Gaps and deliberate differences:

- `get_image_by_url` describes an attachment with the toolkit's vision LLM,
  and `attach_file_to_work_item` reads artifact storage. A configured toolkit
  holds neither authority in this runtime, so both are unserved and listed
  out of `supported_tools.ado_boards`; a selection naming only unserved tools
  skips the toolkit.
- `get_work_item(parse_attachments=true)` and `get_comments(process_images=true)`
  return the work item or comments without image descriptions or parsed
  attachment content, which is what the SDK returns when no LLM is
  configured.
- The SDK caches a failed field-definition read as empty until
  `force_refresh`; Rust does not cache a failure.
- A WIQL result above 200 work items returns a model-visible "narrow the
  query" text instead of 200+ sequential reads.
- Provider error bodies stay out of results except HTTP 400/422 messages on
  work item create/update, which the SDK shows the model and which a caller
  needs to supply missing required fields. Effects that fail after dispatch
  (transport, 408, 429, 5xx) are `UnknownOutcome` and never retried.

Proof: `toolkits/ado_boards_tests.rs` (configuration, WIQL/field reads,
JSON Patch create, rule-error text, unknown outcome, relations, comment
pagination, wiki artifact links, field report golden, selection/policy,
argument validation, SDK conformance).

### Azure DevOps Test Plans family

`ado_plans` serves all twelve non-index tools of
`tools/ado/test_plan/test_plan_wrapper.py` at the same pinned SDK revision,
over the shared `families/ado` connection. The SDK's `TestPlanClient` comes
from `connection.clients` (v7.0); test case work items go through the work
item wrapper it builds internally, which Rust reuses as
`families/ado/work_items.rs`.

| Tool | Route (api-version 7.0 unless noted) | Result |
| --- | --- | --- |
| `create_test_plan` | `POST {project}/_apis/testplan/plans` | `Test plan <id> created successfully.` |
| `delete_test_plan` | `DELETE .../testplan/plans/{planId}` | `Test plan <id> deleted successfully.` |
| `get_test_plan` | `GET .../testplan/plans/{planId}` or `.../plans` (first page) | msrest `as_dict()` object or list; `plan_id` 0/absent lists |
| `create_test_suite` | `POST .../testplan/Plans/{planId}/suites` | `Test suite <id> created successfully.` |
| `delete_test_suite` | `DELETE .../testplan/Plans/{planId}/Suites/{suiteId}` | `Test suite <id> deleted successfully.` |
| `get_test_suite` | `GET .../Plans/{planId}/Suites/{suiteId}` or `.../suites` | `as_dict()` object or list |
| `add_test_case` | `POST .../Plans/{planId}/Suites/{suiteId}/TestCase` | `as_dict()` test case list |
| `create_test_case` | `POST {project}/_apis/wit/workitems/$Test Case` (7.1-preview.3), then `add_test_case` | the added test cases |
| `create_test_cases` | `create_test_case` per entry | list of per-entry results |
| `get_test_case` / `get_test_cases` | `GET .../TestCase/{id}` or `.../TestCase`, then `GET wit/workitems/{id}` (`$expand=Relations` without `fields`) | `as_dict()` test case plus `work_item_full_details`; the SDK's "No test cases found ..." text |
| `get_all_test_case_fields_for_project` | `GET wit/workitemtypes/Test Case` | the work item field report for `Test Case` |

The `*_create_params` and `suite_test_case_create_update_parameters`
arguments stay JSON strings with the SDK's Python attribute names
(`area_path`, `parent_suite`, `work_item`). `model_body` admits exactly the
msrest model's attributes (an unknown name returns the constructor's
`TypeError` text, as the SDK does), renames them to REST camelCase and
coerces the `int` attributes (`work_item.id` "23" becomes 23) as msrest's
serializer does. Test steps are rebuilt into `Microsoft.VSTS.TCM.Steps`
byte for byte as `ElementTree.tostring` writes them (`steps.rs`), from the
JSON array or the `<Steps><Step>` XML; the XML reader is bounded and refuses
DTDs and undefined entities. A `TF401320` or validation failure on create
returns the provider rule text with the SDK's pointer to
`get_all_test_case_fields_for_project`.

Differences: `create_test_cases` validates every entry before creating the
first work item (the SDK raises `KeyError` midway, after creating earlier
entries), defaults a missing `test_steps_format` to `json`, and takes at
most 50 entries. `get_test_cases` reads at most 200 test cases with their
work items and otherwise returns a model-visible "read them by id" text.
A failed field-definition read is not cached. As in the SDK, a failed work
item read leaves a test case without `work_item_full_details`.

Proof: `toolkits/ado_plans_tests.rs` (ElementTree step golden output, XML
reader defaults/entities/refusals, create params naming and errors, add/list
`as_dict` shapes, create-test-case flow and rule-error hint, test case
details, unknown outcome, argument shapes, SDK conformance).

### Azure DevOps Wiki family

`ado_wiki` serves all eight non-index tools of `tools/ado/wiki/ado_wrapper.py`
at the pinned SDK revision, over the shared `families/ado` connection and the
`WikiClient`/`CoreClient` v7.0 routes the wrapper uses (`connection.clients`).
The live settings path is the common `tools/ado/__init__.py::get_tools`, which
adds the optional `default_wiki_identifier`; every tool falls back to it, and
the tool descriptions end with `Default wiki: <name>` as the SDK's do.

| Tool | Route (api-version 7.0) | Result |
| --- | --- | --- |
| `get_wiki` | `GET {project}/_apis/wiki/wikis/{wiki}` | `_format_wiki_response`: id, name, type, url, project_id, repository_id, mapped_path, optional remote_url and versions |
| `get_wiki_page` | `GET .../wikis/{wiki}/pages/{id}` or `.../pages?path=` with `recursionLevel` and `includeContent` | `{eTag (ETag header), page: {id, path, git_item_path, remote_url, url, order, is_parent_page, is_non_conformant, sub_pages, content?}}`; 404 returns the SDK's "Page ... not found in wiki ..." text |
| `get_wiki_page_by_path` / `get_wiki_page_by_id` | `GET .../pages?path=` or `.../pages/{id}` with `includeContent=true` | the page content |
| `delete_page_by_path` / `delete_page_by_id` | `DELETE .../pages?path=` or `.../pages/{id}` | the SDK's "has been deleted" sentences |
| `modify_wiki_page` | `GET .../wikis` (create via `GET _apis/projects` + `POST .../wikis` when missing), `GET .../pages?path=` for the eTag, `PUT .../pages?path=&versionDescriptor.*` with `If-Match` | `{eTag, id, page: url}`, or the expanded page with `expanded` |
| `rename_wiki_page` | `POST .../wikis/{wiki}/pagemoves?comment=&versionDescriptor.*` | `{eTag, page_move: as_dict(WikiPageMove)}` |

Page writes and moves are retried once without the version descriptor when
the provider answers "The version '{0}' either is invalid or does not exist."
as the SDK does; a missing page (404) on the eTag read makes the write a
create.

Gaps and differences: the SDK describes images in page content with the
toolkit LLM (`process_images`, `image_description_prompt`) and, for
`/.attachments/` images, reads them through an internal repos wrapper. No
LLM is lent to a toolkit here, so content is returned with its image
references unchanged, which is the SDK's own result when no description is
produced. `rename_wiki_page` returns the msrest-shaped move instead of the
SDK's `str()` of a model object. `modify_wiki_page` treats a wiki addressed by
id as existing; the SDK compares names only and would try to create a wiki
named after the id.

Proof: `toolkits/ado_wiki_tests.rs` (wiki format and default identifier,
expanded page and 404 text, raw page content and description suffix, eTag
`If-Match` write with the version retry, wiki and page creation, page move
and delete sentences, argument validation, SDK conformance).

### Azure DevOps Repos family

`ado_repos` serves all fifteen non-index tools of
`tools/ado/repos/repos_wrapper.py` at the pinned SDK revision over the
`GitClient` v7.0 routes, all under
`{project}/_apis/git/repositories/{repository_id}` except the project-level
`git/pullrequests/{id}` read. Settings add the required `repository_id` and
`base_branch`/`active_branch` (`main` when absent or empty). The toolset owns
the wrapper's mutable active branch: `set_active_branch`, `list_files`,
`read_file`, `create_branch`, `create_file` and `update_file` move it exactly
where the SDK assigns `self.active_branch`.

| Tool | Route | Result |
| --- | --- | --- |
| `list_branches_in_repo` / `set_active_branch` | `GET .../stats/branches` | the SDK's "Found N branches" and "Switched to branch" / "does not exist" texts |
| `list_files` | `GET .../items?scopePath=&recursionLevel=Full&includeContentMetadata=true&versionDescriptor.*` | blob paths |
| `read_file` | `GET .../items?path=&versionDescriptor.*` with `Accept: text/plain` | text, 1-indexed `offset`/`limit` slices (`apply_line_slice`), and the `guard_text_read` `content_too_large` object relabelled to `offset`/`limit` above 200000 characters |
| `create_file` / `update_file` / `delete_file` | `GET .../stats/branches?name=` for the head, `POST .../pushes` with an `add`/`edit`/`delete` change | `Created/Updated/Deleted file <path>`; create and update refuse the base branch with the SDK's "protected" text; create probes `GET .../items` first |
| `create_branch` | `GET .../stats/branches?name=` twice, `POST .../refs` from the active branch head | the SDK's created/exists/spaces texts |
| `list_open_pull_requests` / `get_pull_request` | `GET .../pullrequests?searchCriteria.status=active` or `GET git/pullrequests/{id}`, then threads and commits per PR | the SDK's Python `str()` list text (open PRs) or the JSON list |
| `list_pull_request_files` | iterations, last iteration's changes, `GET .../items` at the target and source commits | `json.dumps` text of `[{path, diff}]` with `difflib.unified_diff` output (`diff.rs`) |
| `get_work_items` | `GET .../pullRequests/{id}/workitems` | the first ten ids |
| `comment_on_pull_request` | `POST .../pullRequests/{id}/threads` | the SDK's thread texts; inline comments carry `CommentThreadContext` positions |
| `create_pull_request` | `POST .../pullrequests` | `Successfully created PR with ID <id>`; a 400/409 provider refusal returns the SDK's "Unable to create pull request" text |
| `get_commits` | `GET .../commits?searchCriteria.*` with `str(datetime.fromisoformat(...))` dates | `[{sha, author, createdAt, message, url}]` |

`update_file` uses the GitLab Org family's OLD/NEW marker engine
(`gitlab_org::edit::apply_update`, now visible to sibling families) and the
SDK's texts for unsupported files, missing markers and no-op edits. The diff
reproduces `SequenceMatcher` exactly, including the autojunk rule for lines
that make up more than 1% of a 200+ line file, and is checked against
CPython goldens.

Differences: the SDK validates the repository and both branches over the
network at construction; here materialization stays network-free. The SDK
caches file content per toolkit; Rust always reads the provider. `get_commits`
reports each commit's own date where the SDK repeats the first commit's.
`create_branch` reports a ref update the provider answers with
`success: false`; the SDK reports success. Inline comments are all validated
before the first thread is created. Bounds: `list_files` returns a
"list a subdirectory" text above 20000 files or one result,
`list_open_pull_requests` reads at most 100 PRs, `list_pull_request_files`
diffs at most 100 changes and files up to 1 MiB, and the SDK's
`ToolException` texts that would carry provider error bodies are stable
Azure DevOps error codes.

Proof: `toolkits/ado_repos_tests.rs` (configuration, CPython difflib
goldens, `fromisoformat`/line-slice parity, read slicing and guard, create
and update pushes, branch flows, open-PR Python text, PR diff JSON text,
inline and query comments, commit filters, unknown outcome, argument
validation, SDK conformance).

## Special runtime toolsets

| Python source | Behavior | Rust target | Status / deviation |
| --- | --- | --- | --- |
| SDK `runtime/tools/ask_user.py` plus `runtime/toolkits/tools.py` internal-tool selection | Runtime-built clarification tool with 1-4 normalized questions and a structured UI answer | `src/agents/internal_tools.rs`, direct/nested agent session replay and pipeline LLM graph replay | Implemented capability-disabled through native ADK confirmation. Object/string answers replace the original call result; they are not new user turns. Main application/ad-hoc projection and `answer` admission are included. Nested-parallel saved-child calls have distinct hierarchical cards, atomic complete-set admission, frozen child-scope validation, and exact-answer replay coverage |
| SDK `runtime/toolkits/mcp.py` | Remote MCP discovery and invocation | `src/toolkits/mcp.rs`, `src/agents/{ordinary,pipeline}.rs` | Partial capability-disabled HTTP implementation with exact selected-tool discovery/invocation, bounded RMCP protected-resource and authorization-server discovery, exact delegated authorization resume, and native direct/pipeline composition. Fixed and declared-parameter catalogue-backed HTTP definitions are supported through Main claim-time resolution. External Elitea-as-MCP can execute opted-in selected read-only toolkit operations through the separately gated durable direct-tool path. Stdio, descriptor sync, remaining OAuth transport variants, effectful external calls and external agent/pipeline controls remain gated |
| SDK `runtime/toolkits/mcp_config.py` | Saved HTTP/stdio MCP definitions | Main prebuilt catalogue, dynamic toolkit schema and claim materializer; Rust MCP module; external MCP runner client | Partial capability-disabled for fixed and declared-parameter HTTP definitions with project-vault secret sealing. Main also injects runtime-only project identity and a current-user PAT for trusted same-origin internal MCP endpoints. The Main-owned application, skill, toolkit, configuration, notification, project-context, and secret operations are mapped in `internal-elitea-mcp.md`. Discovery, chat, analytics, artifacts, and live per-instance toolkit discovery remain closed there. Stdio is intentionally externalized. The SDK snapshot declares no static tools for `mcp_config` (the server's tools are discovered), so there is no schema gate; the Rust worker capability lists `mcp_config` because `snapshot.rs` classifies it as MCP and the direct MCP branch serves it, which `TestRustCapabilitySnapshotMatchesTheRustSource` derives from the source |
| SDK `runtime/toolkits/application.py` | Nested applications | `src/agents/application_tools.rs`, `src/agents/graph/application.rs` | Partial capability-disabled direct-agent and saved-pipeline nesting with exact version, hierarchy, cycle/tier bounds and durable nested HITL; private immutable Main child resolution, child variables and further recursive pipeline nodes remain gated |
| SDK `runtime/toolkits/artifact.py` | 16 artifact tools and indexing coupling | `src/toolkits/artifact.rs` | Planned; artifact service boundary required |
| SDK `tools/memory` and `runtime/toolkits/vectorstore.py` | Four memory and four vectorstore tools | `src/toolkits/{memory,vectorstore}.rs` | Memory blocked pending the gate 7b ownership decision; vectorstore blocked on the native indexing gate (both in `toolkit-port-b7-20261010.md`) |
| SDK `runtime/tools/sandbox.py` | Two Pyodide variants | External sandbox client | Intentional deviation: library abstraction is not a sandbox. Blocked as a toolkit (`toolkit-port-b7-20261010.md`): the SDK binds no tool for a configured `sandbox` type, and the Rust code runner has no Main grant path or lent authority for model-initiated jobs |
| SDK `runtime/tools/data_analysis.py` | Generated data analysis over artifacts and pandas-shaped tabular operations | External sandbox/artifact boundary with Polars as the likely native data-frame engine | Blocked (surveyed 2026-10-10, see "Toolkit types that cannot be ported as configured families yet"): needs a tool-lent model, the `CodeSandbox` lent to a tool and the attachment-bucket artifact authority together; neither pandas byte-for-byte behavior nor in-process arbitrary Python execution is implied |
| SDK `community/inventory` | Dynamic retrieval/ingestion registry | `src/toolkits/inventory.rs` | Planned; current 14-schema versus 9-default drift must be resolved |

## Safe implementation ownership

The shared schema, bounded HTTP, credential, policy, invocation-event,
cancellation and ADK `Toolset` kernel is now stable enough for independent
REST families; Google Places, Sonar, Azure Search and ReportPortal are complete
reads, while ServiceNow, Salesforce, Slack, Rally, GitLab Org and Zephyr Squad
prove the same boundary can retain bounded create/update/delete effects without
activating them ahead of durable approval.
These follow the partial GitHub reference family. Parallel family work still
must not share mutable files or weaken the capability gate.
Non-overlapping batches are:

1. GitHub completion as the broad reference family, including its gated effects.
2. Independent simple REST families using the Google Places/Sonar ownership pattern.
3. Standard GitLab and Bitbucket with separate owners (both now partial: all
   non-index tools); LocalGit is blocked in the cloud worker (see above).
   GitLab Org is already complete behind its gate.
4. All four ADO toolsets under one owner.
5. Jira and Confluence after one shared Atlassian normalizer.
6. qTest, TestRail, Xray and the indexing-backed Zephyr variants as coherent
   test-management batches; legacy Zephyr and Zephyr Squad are already complete
   behind capability gates.
7. OpenAPI before Postman.
8. Cloud/data toolsets only after explicit allowlists replace unrestricted
   generic execution.
9. SharePoint, Figma and PPTX as content-heavy independent batches.
10. Special runtime toolsets as architecture work, not ordinary family ports.
11. Shared indexing overlay last.

## Shared executable boundary

`src/toolkits/invocation.rs` deliberately wraps ADK-Rust 2.0.0's native
`Tool` and returns its native `BasicToolset`; it is not a replacement toolkit
framework. The wrapper freezes bounded model-visible action metadata, applies
the immutable deployment blocklist to each concrete action, validates bounded
object arguments, preserves the SDK's removal of top-level null optionals, and
redacts delegated failures while retaining stable categories and retry hints.
It owns no background task and performs no retry. Externally visible effects
still require a family-specific effect identity or idempotency boundary because
dropping a future cannot prove a remote effect did not happen. Expected,
model-actionable business failures remain bounded structured tool results;
`AdkError` is reserved for redacted infrastructure and control failures.

The SDK toolkit families are still product functionality and will be ported,
not replaced by configuration descriptors. Main retains the public
`check_connection` routes, project authorization, configuration revision and
audit/result contract. The target route delegates its actual bounded probe to
the same family client used for execution instead of duplicating every vendor
client in Go; the GitHub section above is the first concrete example.

Each Rust family owns configuration parsing needed by its client, claim-scoped
materialization, the actual probe operation when delegated, and its concrete
ADK tools. GitHub provides the broad reference and Google Places the first
complete fixed read family before independent families are split into separate
work. Sensitive-tool confirmation,
MCP smart authorization, nested applications, code execution and indexing
overlays remain separate capabilities because their durable authority and
isolation rules differ from an ordinary function call.
