# Saved toolkit Test operation discovery

## Source mapping

Current Core `api/v2/toolkit_available_tools.py::PromptLibAPI.get` discovers callable operations by project and saved toolkit ID.
Current SDK `elitea_sdk/tools/openapi/__init__.py` obtains operation definitions from the configured OpenAPI instance.
These sources define the functional boundary. The new implementation reuses Main and Rust instance discovery.

Web `features/toolkits/ui/test-tools/TestToolSettings.tsx` now receives the owning project and saved toolkit ID.
`TestToolPane.tsx` passes both identifiers from the editor.
`TestTools.tsx` passes the saved toolkit ID for the transcript surface.
`entities/toolkit/api/toolkitToolsApi.ts` already selects instance discovery when that ID exists.
Rust's `toolkit.available_tools.v1` supplies the operation names and argument schemas through Main.

## Observed defect

The deployed delegated OpenAPI fixture has an empty `selected_tools` list.
Its Test picker falls back to type discovery and offers saved toolkit names as operation names.
An explicit selected operation hides this defect, which explains the successful earlier toolkit 31 checks.
The fix preserves explicit selection and static schemas, then uses saved-instance discovery for the remaining case.
Unsaved forms keep their existing type fallback.
No saved toolkit data or database schema changes are required.

## Verification

The three focused Test pane, settings, and transcript component suites pass with 36 tests.
The regression covers both explicit and empty OpenAPI operation selections.
It checks operation discovery, required arguments, and the actual Test request's operation and arguments.
TypeScript and focused lint checks pass.
Deployed authorization verification remains pending.

## Deployed browser evidence

UI image `sha256:f6bc3124d244627af34baabb9c4a2010cae613a314cf47cee3ec599fa2b18cb1` serves the repair.
Toolkit 27 retains its empty saved selection. Its picker now lists `echo_marker` and renders the required marker argument.
The Test request returns HTTP 409 with the exact toolkit authorization challenge.
Execution `da5d5d59f947c7336d12c6b315fb5a8b` identifies the initial challenge.
Skip displays the skipped result. The fixture's protected-call count remains 15.

Consent exposes a separate `oauth_resource_mismatch` failure during token exchange.
The OpenAPI flow omits the optional provider resource parameter, but reference storage currently requires that parameter.
This failure remains open. Picker and Skip evidence do not close the authorization retry gate.
