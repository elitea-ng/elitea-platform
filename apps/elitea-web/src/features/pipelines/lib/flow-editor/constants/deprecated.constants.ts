/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * constants/deprecated.constants.js` (53 lines, unit A2e). Pure data, no
 * behavioural changes from the baseline.
 *
 * `./index.ts` (unit A2c) explicitly anticipates this file landing here and
 * asks its owner to add `export * as DeprecatedConstants from
 * './deprecated.constants';` to the barrel -- done below in a follow-up
 * edit to that file (not a new file of its own, matching the baseline's own
 * `index.js` shape: `export * as DeprecatedConstants from
 * './deprecated.constants';`).
 *
 * `node.helpers.tsx` (unit A2c)'s `isDeprecatedNodeType` already carries its
 * own local minimal duplicate of the `DeprecatedNodes` array (documented in
 * that file's own header as "this array's owning sub-unit hasn't landed
 * yet") -- this file is the real, full port `DeprecatedTips`'s UI copy
 * needs; `node.helpers.tsx` is left as-is (not switched over to import from
 * here) since editing it is outside this sub-unit's owned-file fence.
 */
import { PipelineNodeTypes, PipelineNodeTypeNames } from '../constants/flowEditor.constants';

const DEPRECATED_TIP = 'This node is deprecated and will be removed in a future version. ';
const VIEW_MIGRATION_GUIDE_TEXT = 'View Migration Guide';

/** One `DeprecatedTips[type]` entry -- spreads directly onto `shared/ui/TextWithLink`'s props (`text`/`linkText`/`linkUrl`). */
export interface DeprecatedTip {
  readonly text: string;
  readonly linkText: string;
  readonly linkUrl: string;
}

/** `deprecated.constants.js:6-37` verbatim -- one migration-guide tooltip per deprecated node type. */
export const DeprecatedTips: Readonly<Partial<Record<string, DeprecatedTip>>> = {
  [PipelineNodeTypes.Condition]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/condition-node-migration',
  },
  [PipelineNodeTypes.Tool]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/tool-node-migration',
  },
  [PipelineNodeTypes.Function]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/function-node-migration',
  },
  [PipelineNodeTypes.Pipeline]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/pipeline-node-migration',
  },
  [PipelineNodeTypes.Loop]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/loop-node-migration',
  },
  [PipelineNodeTypes.LoopFromTool]: {
    text: DEPRECATED_TIP,
    linkText: VIEW_MIGRATION_GUIDE_TEXT,
    linkUrl: '/docs/how-tos/pipelines/loop-node-migration',
  },
};

/** `deprecated.constants.js:39-46` verbatim -- the node types that render the "Deprecated!" badge. */
export const DeprecatedNodes: readonly string[] = [
  PipelineNodeTypes.Function,
  PipelineNodeTypes.Condition,
  PipelineNodeTypes.Pipeline,
  PipelineNodeTypes.Loop,
  PipelineNodeTypes.LoopFromTool,
  PipelineNodeTypes.Tool,
];

/** `deprecated.constants.js:48-53` verbatim -- deprecated node types' declared key names, plus the always-invisible End/Ghost/Default types (layout helpers use this to skip auto-layout/visibility for all of them alike). */
export const DeprecatedOrInvisibleNode: readonly string[] = [
  ...DeprecatedNodes.map(nodeType => PipelineNodeTypeNames[nodeType] as string),
  PipelineNodeTypeNames[PipelineNodeTypes.End] as string,
  PipelineNodeTypeNames[PipelineNodeTypes.Ghost] as string,
  PipelineNodeTypeNames[PipelineNodeTypes.Default] as string,
];
