# Saved child pipeline variables

## Current-platform evidence

`projects/elitea-sdk/elitea_sdk/runtime/tools/application.py::_run` merges saved defaults with invocation variables.
It keeps each reconstructed child runnable local to its invocation.
This prevents one sibling invocation from replacing another sibling's variables.
The current implementation also returns child state for further result projection.

## Rust implementation

| Contract | Source |
| --- | --- |
| Parse task and additional fixed, variable, or fstring mappings | `src/agents/graph/application.rs::ApplicationNodeDefinition` |
| Expose declared child variable types | `src/agents/graph/compiler.rs::PipelineDefinition::declared_variable_types` |
| Bind the frozen child graph and its schema | `src/agents/pipeline.rs` |
| Validate targets and values before child execution | `src/agents/graph/application.rs::ApplicationNode` |
| Keep explicit input channels isolated | Native ADK `SubgraphNode` input mappings |
| Persist child values and defaults | Existing ADK child graph checkpoints |

Additional mappings preserve JSON types instead of converting every value to text.
Targets must exist in the saved child pipeline schema.
Invalid values fail before child execution and before downstream state publication.
Omitted mappings retain child defaults.
Explicit null fails validation for the currently supported non-nullable types.
This differs from the legacy rule that ignores null when a default exists.

Only declared parent user state and the task input can supply extra mappings.
Runtime control fields and message history cannot become child variable targets.
Internal mapping channels include the parent node identity.
They do not overwrite parent user variables.
The mapped payload retains the existing 240 KiB bound.
Task-only configuration digests remain unchanged.

This change does not support extra variables for ordinary Agent children yet.
Such configurations fail explicitly during binding.
Typed child output projection remains separate work.
No application database schema changes are required.

## Numeric template correction

The integration test exposes an existing serialization mismatch in `state_modifier.rs`.
With arbitrary-precision JSON enabled, numeric serialization exposes a private map to MiniJinja.
The template adapter now converts numeric primitives explicitly, including nested values and the `from_json` filter.
Numbers outside finite primitive range remain text rather than exposing the private serialization map.
MiniJinja retains its standard boolean rendering, including `True`.

## Verification

All 117 focused graph tests pass.
They check mapped values, retained defaults, type rejection, unknown targets, and reserved-field rejection.
It also checks nested HITL checkpoint identity and resume behavior.
The numeric regression checks integer arithmetic, nested decimal arithmetic, and parsed JSON arithmetic.

Formatting, diff checks, and Clippy with tests and warnings denied pass.
Deployment and successful browser acceptance for child variable mappings are recorded below.
Existing sandbox browser acceptance does not prove this new contract.

## Browser integration correction

The first browser attempt finds an obsolete UI admission rule that permits only the `task` mapping.
`graphAdmission.nodes.ts` now permits up to 64 mappings, requires `task`, and rejects reserved child targets.
`graphAdmission.nodeReads.ts` now validates every variable mapping against the parent state.
`runtimeContract.constants.ts` reserves the worker's internal child-variable channel prefix.
The runtime still validates the selected child's schema and types during binding and execution.
The UI does not claim schema validation for an unfetched child definition.

All 64 focused admission and node-default tests pass. Typechecking, focused lint, and the application build pass.
The worker image is deployed at `sha256:b66ac7dd17fdbc74c12dc312147c2650342a02c560a3bca497b1cf30bd8954f8`.
The matching rehearsal UI is `elitea-web:child-inputs-20260930`.

The rehearsal restart also exposes an incorrect Minikube example in the deployment guide.
Use the supported `kubelet.pod-max-pids` flag, not the configuration key as a flag.
The corrected node returns Ready and reports an effective `podPidsLimit` of 128.

## Deployed browser acceptance

Persistent chat 761 runs saved parent 136/version 143 with child 135/version 142.
It returns `3|for orders|2|True`: fixed integer, rendered label, mapped two-item list,
and the untouched child boolean default. Reload retains exactly one result.
The graph has no LLM node, so the selected model is not invoked by this check.

The ephemeral pipeline test rejects `count: wrong-type` and produces no child result.
Focused worker tests additionally prove rejection before a child checkpoint is created.
The deployed error message is still generic (`The runtime operation failed.`).
`application.rs` maps the typed validation failure into `node_failure`, losing its cause;
this diagnostic correction remains open and this negative browser check is not complete
acceptance of error quality. The valid parent fixture is restored and saved afterwards.

The association picker leaves the selected child visible after successful attachment.
Closing it reveals the attached child card; this is a separate UI polish issue, not a
failed relation write. The flow editor currently exposes only task mapping controls;
additional child mappings are authored through YAML.
