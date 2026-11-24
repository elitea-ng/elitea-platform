/**
 * Ported from `EliteaUI` (`origin/main` `46d85cda7`):
 * - `src/[fsd]/features/pipelines/flow-editor/lib/constants/nodeDocs.constants.js`
 * - `src/[fsd]/features/pipelines/flow-editor/lib/helpers/nodeDocs.helpers.js`
 */
import { docsLink } from '@/shared/brand/brandLinks';
import type { BrandPack } from '@/shared/brand/schema';

import { DeprecatedTips } from './deprecated.constants';
import { PipelineNodeDisplayNames, PipelineNodeTypes, type PipelineNodeType } from './flowEditor.constants';

const DOCS_NODES_PREFIX = 'how-tos/pipelines/nodes';

/** Safe `rel` attribute required whenever opening external documentation links in a new tab. */
export const NODE_DOCS_LINK_REL = 'noopener noreferrer';

const NODE_DOCS_PATHS: Readonly<Partial<Record<PipelineNodeType, string>>> = {
  [PipelineNodeTypes.LLM]: `${DOCS_NODES_PREFIX}/interaction-nodes#llm-node`,
  [PipelineNodeTypes.Agent]: `${DOCS_NODES_PREFIX}/interaction-nodes#agent-node`,
  [PipelineNodeTypes.Toolkit]: `${DOCS_NODES_PREFIX}/execution-nodes#toolkit-node`,
  [PipelineNodeTypes.Mcp]: `${DOCS_NODES_PREFIX}/execution-nodes#mcp-node`,
  [PipelineNodeTypes.Code]: `${DOCS_NODES_PREFIX}/execution-nodes#code-node`,
  [PipelineNodeTypes.Http]: `${DOCS_NODES_PREFIX}/execution-nodes#http-request-node`,
  [PipelineNodeTypes.Router]: `${DOCS_NODES_PREFIX}/control-flow-nodes#router-node`,
  [PipelineNodeTypes.Decision]: `${DOCS_NODES_PREFIX}/control-flow-nodes#decision-node`,
  [PipelineNodeTypes.Hitl]: `${DOCS_NODES_PREFIX}/control-flow-nodes#human-in-the-loop-node`,
  [PipelineNodeTypes.StateModifier]: `${DOCS_NODES_PREFIX}/utility-nodes#state-modifier-node`,
  [PipelineNodeTypes.Printer]: `${DOCS_NODES_PREFIX}/utility-nodes#printer-node`,
  // Custom is deprecated but has no migration guide
  [PipelineNodeTypes.Custom]: `${DOCS_NODES_PREFIX}/execution-nodes#custom-node`,
};

export const MIGRATION_GUIDE_TOOLTIP = 'Open migration guide';

export interface NodeDocsLink {
  readonly url: string;
  readonly tooltip: string;
}

export function getNodeDocsLink(type: string, pack?: BrandPack): NodeDocsLink | null {
  const migrationGuideUrl = Object.hasOwn(DeprecatedTips, type) ? DeprecatedTips[type]?.linkUrl : undefined;
  if (migrationGuideUrl) {
    return { url: migrationGuideUrl, tooltip: MIGRATION_GUIDE_TOOLTIP };
  }

  const paths = NODE_DOCS_PATHS as Readonly<Record<string, string | undefined>>;
  const pathSuffix = Object.hasOwn(paths, type) ? paths[type] : undefined;
  if (!pathSuffix) return null;

  const displayNames = PipelineNodeDisplayNames as Readonly<Record<string, string>>;
  const displayName = Object.hasOwn(displayNames, type) ? displayNames[type]! : type;
  return { url: docsLink(pathSuffix, pack), tooltip: `Open ${displayName} node documentation` };
}
