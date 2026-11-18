# Adding a credential type or a toolkit type

This is the supported way to add a credential type or a toolkit type that the SDK gained after the
snapshots Main pins. It replaces the generator `scripts/contract/sync_toolkit_schema_snapshot.py`, which
was deleted with the Python worker (#1312). Decision D1 of epic #2254: new credential types are defined
natively in Go, toolkit schemas are owned by Rust, and a captured upstream fixture is each type's
contract test. No Python runs in CI, and the snapshots are **not** regenerated or re-pinned.

## Where the lists come from

| What the user sees | Served from | Pinned at |
|---|---|---|
| "Create toolkit" list, and each type's settings form | `GET /api/v2/toolkits/types`: `runtimecomposition/current_toolkit_catalogue_snapshot.json` + `internal/rustonly/rust_only_toolkit_tools.json` (`new_types`), gated by `current_rust_worker_toolkit_capability_snapshot.json` | SDK b5113a12, 2026-08-07 (frozen) |
| A toolkit's tool list and each tool's arguments | `current_toolkit_schema_snapshot.json` + the same `new_types` | same (frozen) |
| Credential types and their forms | `application/configurations/current_available_snapshot.json` + `application/configurations/native_types.go` | SDK a78d3654, 2026-07-22 (frozen) |

The pinned JSON files are data nobody edits any more. A type the SDK added later goes in the two native
places: `native_types.go` (credentials, Go) and `rust_only_toolkit_tools.json` `new_types` (toolkits,
written from Rust). The web, desktop and mobile clients only read the served lists, so a new type needs
no client code; the form is drawn from the served schema.

A type already in a pinned snapshot is refused if you define it natively too (one type, one definition).
Existing types are untouched, byte for byte.

## Step 0: capture the upstream fixture (once, by hand)

The fixture is the SDK's own schema with provenance. It is a checked-in JSON file, read by Go tests only.

1. Read the upstream source at a fixed commit:
   `gh api repos/EliteaAI/elitea-sdk/contents/<path>?ref=<commit> -q .content | base64 -d`
   (`elitea_sdk/configurations/<type>.py` for a credential, `elitea_sdk/tools/<type>/` for a toolkit).
   Never use the local `../elitea-sdk` clone: it is stale.
2. Rebuild the pydantic models with the same field definitions in a throwaway local script (never
   committed, never run in CI) and write `model_json_schema()` for each one. Record the pydantic version.
3. Write the fixture (`internal/sdkfixture` documents the format):

```json
{
  "fixture_version": "elitea.sdk-fixture.v1",
  "kind": "toolkit",
  "type": "tavily",
  "upstream": {
    "repo": "EliteaAI/elitea-sdk", "branch": "main",
    "commit": "<40 hex>", "paths": ["elitea_sdk/tools/tavily/__init__.py"],
    "committed_at": "2026-10-19", "captured_at": "2026-11-18",
    "captured_with": "pydantic 2.12.5 model_json_schema()"
  },
  "model_schema": { "...": "credential data schema, or the toolkit settings schema" },
  "tools": { "web_search": { "...": "argument schema" } }
}
```

A credential fixture has `kind: "configuration"`, no `tools`, and `model_schema` is the configuration
class's schema. Re-capture means a new commit in `upstream` and a reviewed diff of the file.

## Adding a credential type (Go)

1. Capture the fixture to `services/elitea-main/internal/application/configurations/testdata/sdk-fixtures/<type>.json`.
2. Add a `NativeConfigurationType` to `nativeConfigurationTypes` in
   `application/configurations/native_types.go`: id, section (`credentials`), title (the SDK class name),
   label, categories, and `Properties` and `Required` as the fixture has them. A secret field carries
   `secret: true`, `format: "password"`, `writeOnly: true`. `AuthSubsections` groups the fields in the form.
3. Connection check: add the probe to `toolkitCheckProbes` in
   `api/v2/configurations/toolkit_check.go` (it reads an identity endpoint, never content) and set
   `CheckConnection: true`. For a delegated sign-in type, answer through `check_connection_oauth.go`
   instead. Vendor traffic leaves through the existing egress guards.
4. Tests run themselves: `TestEveryNativeConfigurationTypeHasAFixtureAndOneDefinition` fails without a
   fixture, on a field, required or secret-flag difference, and when the type is also in a pinned
   snapshot; `TestNativeConfigurationTypesAreNotInTheSDKCatalogue` (runtimecomposition) covers the SDK catalogue.
   Add a probe test with fixtures: ok, 401, 403, 429, a hostile body.

`native_types_test.go` has a complete worked stub (`stub_example`) with its fixture.

## Adding a toolkit type (Rust-owned)

1. Capture the fixture to `services/elitea-main/internal/runtimecomposition/testdata/sdk-fixtures/<type>.json`
   (settings schema as `model_schema`, every tool's argument schema under `tools`).
2. Add the entry in the family that serves the type, under
   `libs/rust/agent-runtime/src/toolkits/families/<family>/`, next to its tools. Copy
   `families/web/tavily_catalogue.rs`: `metadata`, `settings`, `args_schemas` and, when a settings field
   references a saved credential, `properties`: `{"<field>_configuration": {"configuration_types": ["<credential type>"]}}`.
   Keep the SDK's tool order in `selected_tools`.
3. Register it in `rust_only_catalogue_document` (`families/mod.rs`) under `new_types`.
4. Regenerate the document Main embeds:
   `ELITEA_WRITE_RUST_ONLY_TOOLS=1 cargo test -p elitea-agent-runtime --features toolkit-office the_ui_catalogue_lists_every_tool_the_families_serve`
   (CI fails if the file differs from what Rust produces).
5. Regenerate the web fixtures and commit them:
   `ELITEA_WRITE_TOOLKIT_WEB_FIXTURES=1 go test ./internal/api/v2/toolkits/ -run TestTheWebPerTypeSettingsFixtureMatchesTheServedCatalogue`
   (`servedToolkitTypeSettings.json`), and add the type's entry to `servedToolkitTypeCatalogue.json`
   by hand: label, categories and, if the type is withheld, `hidden`, `unavailable` and the reason.
6. The type is served **hidden**, with the reason "this deployment's agent worker does not support the
   `<type>` toolkit", until the worker can build it. When the family lands, add the type to
   `supported_tool_types` in `current_rust_worker_toolkit_capability_snapshot.json`, update the
   capability tests, and flip the two fixture entries.
7. `TestNativeToolkitTypesMatchTheirCapturedUpstreamFixtures` compares the served settings fields,
   required fields and every tool's argument schema with the fixture.
8. If a tool sends, deletes or writes, register it with the sensitive-tool guard
   (`domain/toolcategory`).

The two snapshot entry-count tests count `53 pinned + the native types`, so adding a type does not edit them.

## Parity checklist

Record in the PR, per client: web (create-toolkit list, credential form, tool picker), desktop
(`apps/elitea-desktop`: runs the family locally, or shows an explicit "cloud only" notice), mobile
(`apps/elitea-mobile`: credential create and edit; sign-in redirect over `ai.elitea.app` where relevant).
Docs with every change: the type's page under `integrations/toolkits/`, `reference/toolkit-catalogue.mdx`,
`search-index.generated.json`, and a file in `docs/upgrading/` when operators must act.

## Worked example

The `tavily` toolkit type (SDK `elitea_sdk/tools/tavily`, commit 0c1ce249) was added through this path in
#2255: `families/web/tavily_catalogue.rs`, the `new_types.tavily` entry, `testdata/sdk-fixtures/tavily.json`.
It is served hidden until #2261 builds it; the credential it references, `tavily`, already exists.
