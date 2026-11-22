/**
 * Barrel matching the baseline's own `apps/elitea-ui/src/[fsd]/features/
 * pipelines/flow-editor/lib/constants/index.js`: a namespace object per
 * constants module, so every flow-editor helper can do
 * `import { FlowEditorConstants } from '../constants'` exactly like the
 * baseline's `import { FlowEditorConstants, ... } from '../constants'`.
 *
 * This is the ALREADY-LANDED contract several sibling pipelines/flow-editor
 * sub-units (A2d: `connectionOperations.helpers.ts`,
 * `deletionOperations.helpers.ts`, `layout.helpers.ts`) hard-import from
 * this exact path/shape (verified via `grep` against their landed source,
 * not assumed).
 *
 * `export * as X from './x'` (the baseline's own literal syntax, and this
 * file's first draft) is banned by this app's `no-export-all` oxlint rule
 * (R-L4: "index.ts is a curated public API, not a barrel"). `import * as ns`
 * + `export const X = ns` produces the identical runtime namespace-object
 * shape every consumer needs without using the banned `export *` syntax —
 * still exactly one curated named export per constants module.
 *
 * `DeprecatedConstants` (baseline: `deprecated.constants.js`) landed under
 * unit A2e — re-exported below alongside the other two, matching the
 * baseline's `index.js`.
 */
import * as FlowEditorConstantsNamespace from './flowEditor.constants';
import * as StateDrawerConstantsNamespace from './stateDrawer.constants';
import * as DeprecatedConstantsNamespace from './deprecated.constants';
import * as RuntimeContractConstantsNamespace from './runtimeContract.constants';
import * as NodeDefaultsConstantsNamespace from './nodeDefaults.constants';
import * as NodeDocsConstantsNamespace from './nodeDocs.constants';
import * as NodeFieldInfoConstantsNamespace from './nodeFieldInfo.constants';

export const FlowEditorConstants = FlowEditorConstantsNamespace;
export const StateDrawerConstants = StateDrawerConstantsNamespace;
export const DeprecatedConstants = DeprecatedConstantsNamespace;
/**
 * The Rust pipeline compiler's own admissibility rules (node-id grammar,
 * the `parse_pipeline_node` allow-list, the reserved `state:` keys), each
 * entry citing the runtime line it transcribes. Added because the editor
 * was minting documents the compiler refuses.
 */
export const RuntimeContractConstants = RuntimeContractConstantsNamespace;
/** `InitialNodeData` — the per-node-type seed payload, split out of `flowEditor.constants` for the §3.5 file-length budget. */
export const NodeDefaultsConstants = NodeDefaultsConstantsNamespace;
export const NodeDocsConstants = NodeDocsConstantsNamespace;
export const NodeFieldInfoConstants = NodeFieldInfoConstantsNamespace;
export { ValidationErrors } from './validation.constants';
