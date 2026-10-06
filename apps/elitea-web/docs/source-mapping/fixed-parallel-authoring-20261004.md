# Fixed Parallel editor source mapping

The Worker source defines this schema. Fixed Parallel remains separate from Map and data shaping.
The production authoring gate remains false. The runtime integration gate remains false.
Stored YAML renders without deriving defaults or changing source text.

## Contract mapping

| Worker source | Editor source | Behavior |
| --- | --- | --- |
| `yaml.rs::RawParallelNodeDefinition` | `parallel.constants.ts`, `ParallelSettings.tsx` | Required branches, concurrency, wait, and exactly one output remain explicit. |
| `yaml.rs::RawParallelBranchDefinition` | `ParallelBranches.tsx`, `graphParallelAdmission.helpers.ts` | Branches use strict `{id, node}` mappings. Unknown fields remain available for refusal. |
| `yaml.rs::validate` | `graphParallelAdmission.helpers.ts` | Require 2–16 branches, unique result keys, and concurrency 1–8 within branch count. |
| `parallel_compiler.rs::validate_parallel_ownership` | `graphParallel.helpers.ts`, `graphParallelAdmission.helpers.ts` | Each branch owns one exact declared Agent. Parent entry, routes, pauses, and shared owners are refused. |
| `compiler.rs::validate_node_state` | `StateChannelSelect.tsx`, `graphParallelAdmission.helpers.ts` | The output names one declared user list channel. State order and descriptors remain unchanged. |
| `parallel.rs::collect_outcomes` | `ParallelSettings.tsx` | Results follow branch order. Entries contain `branch_id`, `node`, and `outputs`. No reducer field exists. |
| `parallel_application.rs::PARALLEL_AGENT_INPUTS_STATE_KEY` | `runtimeContract.constants.ts` | Private Parallel input and resume channels cannot become user output channels. |
| `compiler.rs::parse_pipeline_node` | `runtimeContract.constants.ts`, `useFlowEditorNodeTypes.tsx` | Stored Parallel nodes have a registered card and the compiler parse type. |
| `parallel_compiler.rs::FIXED_PARALLEL_INTEGRATION_READY` | `parallel.constants.ts`, `ParallelNode.tsx`, creation hook and catalog | Parsing does not enable authoring or execution. Both production gates remain false. |

The Agent card owns participant identity and input mappings. Parallel edits never recreate these mappings.
An explicit transition removal action changes only a uniquely owned Agent. Even `END` conflicts with owned execution.
A YAML null transition remains equivalent to an absent optional runtime transition.
Branch reordering changes list order and retains stable result keys and exact node identities.
Child renames use the existing reference helper. They change `branches[].node` and retain `branches[].id`.

## Scope and gates

This packet adds one card, settings, validation, defaults, and additive registration.
It preserves the frozen Map and shaping R2 controls and validation.
It preserves Code source controls, draft focus, YAML storage, state order, and rename integration.
It creates no global state descriptor, input broadcast, Map binding, or reducer.

The packet includes focused Node 24 tests, full UI type checking, and lint for all changed TypeScript files.
Expanded pipeline checks include unrelated baseline failures. The execution receipt identifies their exact results.
No Cargo check, build, live browser, container, credential, or database operation runs in this packet.
Source checks and component tests do not prove runtime recovery or deployment readiness.

## Source pins

The private runtime reference manifest records exact source hashes for these six files.
The implementation manifest records exact editor baselines and postimages.
The protected source manifest records every unchanged baseline source file.
