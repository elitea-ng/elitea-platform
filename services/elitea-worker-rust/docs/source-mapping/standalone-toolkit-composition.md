# Standalone toolkit activation and worker routing

Toolkit discovery and Test must run on the selected worker without activating
index ingestion. This change fixes Main composition and control verification.

## Source mapping

| Current business source under the umbrella workspace | Responsibility | Native owner |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py`, `PromptLibAPI.get` | Authorize a saved toolkit and request discovery independently of an index run. | Main `application/toolkitdiscovery` and `runtimecomposition/toolkit_discovery_runtime.go` |
| `projects/centry/pylon_indexer/plugins/indexer_worker/methods/indexer_toolkit_available_tools.py` | Dispatch SDK tool enumeration from the existing worker. | Rust `execution/toolkit_delivery_processor.rs`; Main `redisdispatch.ToolkitAvailableToolsProducer` |
| `projects/elitea-sdk/elitea_sdk/tools/__init__.py`, `get_toolkit_available_tools` | Enumerate instance tools and argument schemas. | Existing Rust toolkit registry and shared discovery command |
| `projects/centry/pylon_main/plugins/elitea_core/utils/mcp_service.py`, `__handle_call_tool_request` | Execute an exact saved tool through the shared runtime. | Existing Main toolkit-call admission and Rust direct runtime |

Inspected Core revision: `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
Inspected Indexer revision: `1f0fbcf5c3429c5ef08a3a3e7a4e81ee94b49460`.
Inspected SDK revision: `ecf49dfac73cd096da4c2297f3d91d13e526395a`.

## Confirmed gaps and changes

Main previously placed both standalone producers, their output handlers, and
their admission services under `IndexIngestDispatchEnabled`. Discovery config
also required that flag. The configured Rust worker consumes the agent command
stream, so enabling discovery on that deployment could not dispatch standalone
work without enabling an unrelated index runtime and still chose the wrong stream.
The production control capability map omitted both standalone contracts, and
the control command validator had no available-tools case.

`configuredToolkitRoute` now reads the existing worker capability selection.
Rust uses the configured agent stream, consumer group, capacity, and resource
class. Python retains its index-worker stream and class. A missing worker
selection retains the prior Python routing default. Discovery still requires
explicit activation and fails construction if its selected worker route is absent.
Config validation permits discovery with agent execution active and indexing off.

Main composes both standalone producers using that route and includes their
capability versions in the control verifier. The discovery validator checks the
command kind, root lineage, bounded toolkit type and settings reference, and
refuses a settings/runtime-context identity collision. The existing signed
envelope, claim, and immutable input checks still apply.

The Rust route reuses the agent graph's toolkit reader, settings resolver, and
claim-time materializer. The Python route reuses those of the existing index
graph. Toolkit admission constructors now take only those input interfaces;
they do not require a constructed index runtime. The output listener adds
standalone handlers independently of its optional index handler. Discovery's
bounded retirement and orphan cleanup loop joins the ordinary runtime lifecycle.
Index initializers, index terminal effects, scheduling, and the index publisher
remain conditional on explicit index activation.

## Verification and limits

Focused race tests pass for agent-only discovery config, Rust stream/class
selection, preservation of the Python route, and the absence of index capability
activation. Both actual toolkit producers prepare and append signed
reference-only commands to the agent stream in component tests. The control
verifier accepts discovery and rejects malformed kind, root lineage, missing
identities, and role collisions. Main API, runtime composition, and executable
packages compile.

These are configuration and component checks. They do not claim that the full
runtime has connected to production Redis, that a browser Test has completed,
or that a deployed worker has executed a real provider call. Root coordinates
those deployment and browser proof gates. No indexing flag is enabled to make
standalone toolkit verification pass.
