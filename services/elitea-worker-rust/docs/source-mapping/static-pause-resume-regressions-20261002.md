# Static pause resume correction

## Source mapping

| Source | Correction | Behavior |
| --- | --- | --- |
| `src/agents/graph/static_pause.rs` | Check the latest event markers before parsing a static pause. | Completed runs return `StaleDecision`. |
| `src/agents/graph/static_pause.rs` | Extract unchanged checkpoint checks into `validate_static_frontier`. | Exact root, descendant, version, and frontier checks remain required. |
| `src/agents/graph/static_pause_tests.rs` | Preserve the original selection in the completed-run fixture. | Both missing and stale selection remain refused. |
| `src/agents/graph/static_pause_tests.rs` | Add a malformed static marker fixture. | Claimed invalid static proof returns `CorruptSession`. |
| `src/agents/graph/state_modifier.rs` | Expose the existing recursive `template_value` converter to sibling modules. | JSON numeric primitives remain template numbers. |
| `src/agents/graph/router.rs` | Convert selected values and `json_loads` results before rendering. | Numeric loop conditions select the declared route. |
| `src/agents/graph/routing_tests.rs` | Test primitive, nested, array, float, and parsed JSON numbers. | Numeric values preserve declared route selection. |

## Failure evidence

The original loop stops after its first before and after pause.
The next invocation restores `count=1` and records `router_output=END`.
Its checkpoint has an empty frontier, step 2, and no cleared interrupt.
This result comes from private focused Worker fixture diagnostics.
The after-checkpoint adapter advances the saved frontier correctly.

The current dependency set enables arbitrary-precision JSON numbers.
Their serialized representation uses private maps.
The Router passes those maps directly to Minijinja.
A numeric comparison then selects the default route.
The StateModifier already converts these values recursively.
The correction reuses that converter without changing dependencies or versions.

The correction preserves frozen definitions and exact checkpoint checks.
It does not change public schemas, generated bindings, or state channels.
It preserves Router declared-target checks and rendering resource limits.
The private condition probe reproduces the failure with counts 0 through 3.
The same probe returns `tick`, `tick`, `tick`, and `END` after conversion.
This probe tests dependency behavior, not full Worker execution.

## Verification boundary

The assembled Worker passes 14 static pause tests and nine routing tests.
All tests use the locked offline dependency set, all features, one build job, and serial test threads.
The static loop regression verifies separate before and after pauses across repeated visits.
The numeric regression covers scalar, nested, array, floating-point, and parsed JSON values.
Strict Clippy remains a separate assembled check.
This note does not claim browser, database, or deployed runtime acceptance.
