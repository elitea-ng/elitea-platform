# Provider input ceiling

## Observed gap

A synthetic provider call rejects more than 272,000 input tokens on a route with a 400,000-token catalogue window.
A combined context window and an input-only ceiling are separate constraints.
Rust already supports optional `ModelContextLimitsV1.max_input_tokens`. Main does not populate it from model configuration.

## Source mapping

Current-platform `projects/centry/pylon_main/plugins/configurations/routing_models.py` resolves authorized model metadata.
Its model fields include `context_window` and `max_output_tokens`.
`configurations/utils.py` also projects these values from saved configuration data.
These files provide the catalogue ownership reference. The optional input ceiling extends the replatform contract.

Main `application/configurations/models.go` exposes the optional catalogue value.
Main `infra/db/repos/models.go` reads numeric or legacy string integers from existing configuration JSON.
Main `application/configurations/local_configurations_normalizer.go` preserves the field during model creation.
Main `application/configurations/model_input_limit_schema.go` extends the runtime model form without modifying the pinned legacy snapshot.
Main `application/agentexecution/model_context_limits.go` freezes only authorized catalogue limits into execution input.
Dedicated summary models use the same freezer and retain their own input ceiling.
The existing protobuf field requires no new wire format or database migration.
Rust `agents/context_budget.rs` takes the smaller of the usable combined budget and the provider input ceiling.

## Semantics

Full uses the model's combined context window. Balanced caps that combined window at 272,000 tokens.
The admitted response limit and safety margin reduce the combined budget.
An explicit provider input ceiling can reduce usable input further. Missing ceilings remain absent.
Do not infer this ceiling by subtracting the provider's maximum output from every context window.
A 1,000,000-token model can admit a smaller 200,000-input plus 32,000-output allocation within its combined window.
A model with a 200,000-token combined window cannot admit that same allocation.

## Verification state

Focused catalogue, schema, execution-snapshot, and application tests pass.
The OpenAPI Go and browser clients are regenerated with the existing repository tools.
Live model-form persistence and the resulting browser context gauge pass the checks below.
This change does not replace the current input estimate with provider-reported usage.

## Rehearsal verification update

The isolated Main image builds and deploys with its environment and six mounts preserved.
A fresh headed Playwright session displays the optional Maximum Input Tokens field in the model configuration form.
The field starts empty. Saved values and the execution gauge pass subsequent checks below.

The local Public catalogue contains `global.openai.gpt-5.6-luna`.
Its saved metadata declares a 272,000-token combined window and a 32,000-token maximum output.
The rehearsal catalogue now contains the same model route and limits.
It references the existing saved `ai_creds` configuration. No credential value is copied into this document.
The test setup does not change project default models or tier defaults.

A real Luna native structured-output correction request returns HTTP 200 with finish reason `stop`.
All returned evidence links match the allowed reference enum.
The provider reports 1,247 prompt tokens and 693 completion tokens for this request.
This probe verifies a small structured response, not large-context compaction or continuation quality.
Balanced, Full, repeated compaction, and recovery tests for Luna remain outstanding.

## Saved limit and execution acceptance

The configuration API saves the observed 272,000-token input ceiling on the existing rehearsal GPT model.
The catalogue retains its 400,000-token total window and 128,000-token maximum output.
Execution `2b75d8561430a880ec782c91f19fa9a5` uses the saved ceiling on the next request in chat 602.
Fresh headed Playwright readback shows 40,595 estimated input tokens within a 272,000-token usable input budget.
The settled answer preserves CEDAR-731 and teal after the preceding compaction.

The project picker exposes Public under its storage name, `promptlib_public`, in this session.
The editor route requires the numeric configuration ID. Earlier Public-label and UUID-link probes were test navigation errors.
Fresh headed Playwright saves 271,999 through the form, verifies reload, then restores and verifies 272,000.
The catalogue preserves the model total window and maximum output during both updates.
`ContextBudgetPanel.tsx` now shows Provider input limit when it reduces the combined-window allocation.
Seven panel tests and browser typechecking pass. The browser displays the additional row against the settled execution.
The deployed UI image is `sha256:d3ab52f7899f490023ea32a220ca93e2100c2da9eda567d2fcc847bb3ca7594c`.
