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
Deployment and browser acceptance for child variable mappings remain open.
Existing sandbox browser acceptance does not prove this new contract.
