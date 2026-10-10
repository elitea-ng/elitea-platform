# `configuration.validate.v1` on the Rust worker (2026-10-10)

Ports the Python worker's configuration-validation capability. Base: `main` at `98c5ed094`. The Python worker and the
pinned SDK (`b5113a1`) are unchanged.

## What the capability validates

**A schema verdict, not a connection test.** The Python handler (`handlers/validation.py`) binds the signed command
to the admitted catalog and then calls `model.model_validate(settings)` on the SDK's registered pydantic
configuration model, exactly once. It opens no connection. The SDK's `check_connection` hooks are a different
operation, reached through `toolkit.call_tool.v1` (the "test connection" button), not through this capability.

**Types.** Only the 32 SDK-registry types in `current_sdk_configuration_catalog_snapshot.json` (Main's
`UsesSDKValidation()` entries): `ado`, `aha`, `azure_search`, `bigquery`, `bitbucket`, `carrier`, `confluence`,
`delta_lake`, `figma`, `github`, `gitlab`, `google_places`, `jira`, `langfuse`, `openapi`, `pgvector`, `postman`,
`qtest`, `rally`, `report_portal`, `salesforce`, `service_now`, `sharepoint`, `slack`, `sonar`, `sql`, `testio`,
`testrail`, `xray`, `zephyr`, `zephyr_enterprise`, `zephyr_essential`. Credential configurations are 31 of them;
`pgvector` is the one `vectorstorage` entry. LLM, embedding and MCP configurations are not SDK registry entries:
Main normalizes them itself (the other 17 of its 49-type chain) and never sends them to a worker. The SDK's
`embedding.py` exists but is not registered.

**What "valid" means.** Whatever pydantic 2.12 accepts for that model in lax mode, from a JSON-decoded dict:
required keys present (a key typed `Optional[...]` with no default must still be present), types per field
(`str`/`SecretStr` strictly strings, `int` and `bool` with pydantic's lax coercions), `Literal` choices (`hosting`,
`auth_type`, `method`), `Optional[List[str]]` items, and two `model_validator(mode="before")` hooks that raise
`ValueError`: `github.validate_auth_sections` (a half-configured username/password or app id/private key pair) and
`openapi._validate_auth_consistency` (OAuth client-credential/delegated completeness, custom header). No model
forbids extra keys or bounds a number, so `UNKNOWN_FIELD` and `VALUE_OUT_OF_RANGE` are never produced.

## Request and result

- Command: `ConfigurationValidationCommandV1` (`validation.proto`), capability `configuration.validate.v1` version
  `1`, command type `CONFIGURATION_VALIDATE`, oneof field 32. It carries `configuration_revision_id`,
  `configuration_type`, catalog revision and digest, schema id/revision/digest and `settings_entry_id`. Settings are
  never on the bus: one input-bundle entry, role `configuration.settings`, `application/json`, at most 256 KiB, no
  runtime-context entry.
- Result: `ConfigurationValidationResultV1`, terminal frame `CONFIGURATION_VALIDATION_RESULT`, logical output id
  `configuration-validation:<configuration_revision_id>` (not execution-keyed; Main's output inbox expects exactly
  this), requested outcome always `SUCCEEDED`. It restates every identity and digest, `valid`, and `issues`:
  `{code, json_pointer, safe_message}` with `code` in `INVALID_CONFIGURATION | INVALID_VALUE | REQUIRED_FIELD |
  VALUE_NOT_ALLOWED` (Main also admits `UNKNOWN_FIELD`, `VALUE_OUT_OF_RANGE`) and the fixed message for that code.
  Issues are deduplicated by (code, pointer), capped at 64, sorted by (pointer, code). No rejected value, raw
  pydantic text or secret crosses.
- Refusals are runtime-error frames: bad identity or digest length -> `INVALID_INPUT`; catalog revision/digest or
  per-type schema mismatch -> `INCOMPATIBLE_VERSION`; type outside the table -> `UNSUPPORTED_CAPABILITY`; malformed
  JSON, duplicate member, non-object -> `INVALID_INPUT`; oversize input, string or nesting -> `RESOURCE_EXHAUSTED`.

## How Main uses the result

`CurrentSDKValidationExecutionValidator` is the only caller. It runs when the configuration write route is composed by
`ELITEA_CONFIGURATIONS_MUTATION_ENABLED=true`: for a create of an SDK-validated type it expands the settings, stages a
revision, submits a job on `ELITEA_RT_V1_VALIDATE` and polls (2 minute wait) the projected result. `valid=true` lets
the create proceed; `valid=false` becomes the field error `type` (HTTP invalid); a runtime failure or a timeout
becomes a dependency error. Main re-validates the whole result (`ValidationResult.Validate`): only the closed code
vocabulary with the canonical messages, sorted, unique, <= 64. The flag is off in every shipped profile
(deploy/README.md, "Why ELITEA_CONFIGURATIONS_MUTATION_ENABLED stays off"), so no shipped stack currently dispatches
a validation, and no shipped worker is bound to the VALIDATE stream.

## What was built

- `src/validation/`: a rule table generated from the pinned SDK (`configuration_rules.json`, 32 types, one row per
  field: kind, nullability, required, enum), the two hooks as code, pydantic's lax `int`/`bool` coercions
  (`lax.rs`), and duplicate-member rejection (`strict_json.rs`) to match `parse_settings_json`. The generator
  (`tests/fixtures/generate_configuration_validation_rules.py`) refuses to write unless the recomputed per-type schema
  digests and the catalog digest equal the values Main embeds.
- `ToolkitCommandKind::ConfigurationValidate`: verified command variant, wire scan (field 32, nested digests 4 and
  7), claim entry rules, result binding, restored-frame validation and the revision-keyed logical output id, on the
  direct-execution path the toolkit kinds already use. The processor binds first (no settings fetch for a command it
  cannot validate), fetches the one entry, evaluates, and publishes. It takes no `AuthorizeInvocation`: there is no
  effect to guard, and `BeginExecution` answers `StartedNow` again for an invocation still `PREPARING`, so a worker
  lost mid-validation is simply re-claimed (Python never calls Begin at all).
- Deployment: `deploy/docker-compose.standalone-rust-validate.yml` (opt-in; turns the mutation route on and adds a
  Rust worker bound to `ELITEA_RT_V1_VALIDATE` / `elitea-configuration-worker-v1`); Helm needs no change to bind a
  worker there (`natsStream`/`natsConsumer`), asserted by `render-worker.sh`.

## Proof

`src/execution/configuration_validation.rs` runs the worker's real binding, evaluation and frame construction over the
cross-language golden vectors the Python worker is held to (`testdata/proto/runtime/v1/configuration-validation/
{valid,invalid,unsupported}`) and requires the published frame to equal `expected-output.pb` and
`expected-cancelled-output.pb` byte for byte: verdict, issue order, identities, digests, settlement proposal and the
refusal text.

`src/validation/tests.rs` replays 1,343 recorded pydantic outcomes (`tests/fixtures/configuration_validation_cases.json`)
across all 32 types: every field with the probe set for its kind (null, bool, ints, floats, numeric text, enums,
lists, objects), required-key removal, unknown keys and the hook combinations. `shared_toolkit_tests.rs` round-trips
a signed command, claim, evaluated valid and invalid document and restored frame for every type.

## Known differences from the Python worker

- Traversal order when a document has two different limit violations at once (Rust visits keys sorted, Python in
  insertion order): which of `INVALID_INPUT`/`RESOURCE_EXHAUSTED` wins can differ.
- Nesting deeper than serde_json's 128-level parser limit is `INVALID_INPUT`, not `RESOURCE_EXHAUSTED`.
- A VALIDATE-bound Rust worker still opens the agent-state database (`bootstrap.rs`), like the agent worker.

## Not done

Live cross-process proof against a running Main (no stack was started for this change).
