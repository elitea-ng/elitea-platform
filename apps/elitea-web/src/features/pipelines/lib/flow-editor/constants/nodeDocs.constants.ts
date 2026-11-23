/**
 * Ported from `EliteaUI` (`origin/main` `46d85cda7`):
 * - `src/[fsd]/features/pipelines/flow-editor/lib/constants/nodeDocs.constants.js`
 * - `src/[fsd]/features/pipelines/flow-editor/lib/helpers/nodeDocs.helpers.js`
 */
import { DeprecatedTips } from './deprecated.constants';
import { PipelineNodeDisplayNames, PipelineNodeTypes, type PipelineNodeType } from './flowEditor.constants';

const DOCS_NODES_URL = 'https://docs.elitea.ai/how-tos/pipelines/nodes';

/** Safe `rel` attribute required whenever opening external documentation links in a new tab. */
export const NODE_DOCS_LINK_REL = 'noopener noreferrer';

export const NODE_DOCS_URLS: Readonly<Partial<Record<PipelineNodeType, string>>> = {
  [PipelineNodeTypes.LLM]: `${DOCS_NODES_URL}/interaction-nodes#llm-node`,
  [PipelineNodeTypes.Agent]: `${DOCS_NODES_URL}/interaction-nodes#agent-node`,
  [PipelineNodeTypes.Toolkit]: `${DOCS_NODES_URL}/execution-nodes#toolkit-node`,
  [PipelineNodeTypes.Mcp]: `${DOCS_NODES_URL}/execution-nodes#mcp-node`,
  [PipelineNodeTypes.Code]: `${DOCS_NODES_URL}/execution-nodes#code-node`,
  [PipelineNodeTypes.Router]: `${DOCS_NODES_URL}/control-flow-nodes#router-node`,
  [PipelineNodeTypes.Decision]: `${DOCS_NODES_URL}/control-flow-nodes#decision-node`,
  [PipelineNodeTypes.Hitl]: `${DOCS_NODES_URL}/control-flow-nodes#human-in-the-loop-node`,
  [PipelineNodeTypes.StateModifier]: `${DOCS_NODES_URL}/utility-nodes#state-modifier-node`,
  [PipelineNodeTypes.Printer]: `${DOCS_NODES_URL}/utility-nodes#printer-node`,
  // Custom is deprecated but has no migration guide
  [PipelineNodeTypes.Custom]: `${DOCS_NODES_URL}/execution-nodes#custom-node`,
};

export const MIGRATION_GUIDE_TOOLTIP = 'Open migration guide';

export interface NodeDocsLink {
  readonly url: string;
  readonly tooltip: string;
}

export function getNodeDocsLink(type: string): NodeDocsLink | null {
  const migrationGuideUrl = Object.hasOwn(DeprecatedTips, type) ? DeprecatedTips[type]?.linkUrl : undefined;
  if (migrationGuideUrl) {
    return { url: migrationGuideUrl, tooltip: MIGRATION_GUIDE_TOOLTIP };
  }

  const urls = NODE_DOCS_URLS as Readonly<Record<string, string | undefined>>;
  const url = Object.hasOwn(urls, type) ? urls[type] : undefined;
  if (!url) return null;

  const displayNames = PipelineNodeDisplayNames as Readonly<Record<string, string>>;
  const displayName = Object.hasOwn(displayNames, type) ? displayNames[type]! : type;
  return { url, tooltip: `Open ${displayName} node documentation` };
}
