import { describe, expect, it } from 'vitest';

import { BrandPack } from '@/shared/brand/schema';
import { DEFAULT_BRAND_PACK } from '@/shared/brand/tokens';

import { DeprecatedTips } from './deprecated.constants';
import { PipelineNodeTypes } from './flowEditor.constants';
import { MIGRATION_GUIDE_TOOLTIP, NODE_DOCS_LINK_REL, getNodeDocsLink } from './nodeDocs.constants';

const tenantPack = BrandPack.parse({
  ...DEFAULT_BRAND_PACK,
  id: 'contoso',
  product: {
    ...DEFAULT_BRAND_PACK.product,
    docsUrl: 'https://docs.contoso.example',
  },
});

describe('nodeDocs.constants', () => {
  it('exposes safe rel attribute for external documentation links', () => {
    expect(NODE_DOCS_LINK_REL).toBe('noopener noreferrer');
  });

  it.each([
    [PipelineNodeTypes.LLM, 'LLM', 'interaction-nodes#llm-node'],
    [PipelineNodeTypes.Agent, 'Agent', 'interaction-nodes#agent-node'],
    [PipelineNodeTypes.Toolkit, 'Toolkit', 'execution-nodes#toolkit-node'],
    [PipelineNodeTypes.Mcp, 'MCP', 'execution-nodes#mcp-node'],
    [PipelineNodeTypes.Code, 'Code', 'execution-nodes#code-node'],
    [PipelineNodeTypes.Router, 'Router', 'control-flow-nodes#router-node'],
    [PipelineNodeTypes.Decision, 'Decision', 'control-flow-nodes#decision-node'],
    [PipelineNodeTypes.Hitl, 'Human-in-the-loop', 'control-flow-nodes#human-in-the-loop-node'],
    [PipelineNodeTypes.StateModifier, 'State modifier', 'utility-nodes#state-modifier-node'],
    [PipelineNodeTypes.Printer, 'Printer', 'utility-nodes#printer-node'],
  ])('links %s to its documentation section under the default /docs origin', (type, name, anchor) => {
    expect(getNodeDocsLink(type)).toEqual({
      url: `/docs/how-tos/pipelines/nodes/${anchor}`,
      tooltip: `Open ${name} node documentation`,
    });
  });

  it('resolves tenant BrandPack product.docsUrl overrides dynamically', () => {
    expect(getNodeDocsLink(PipelineNodeTypes.LLM, tenantPack)).toEqual({
      url: 'https://docs.contoso.example/how-tos/pipelines/nodes/interaction-nodes#llm-node',
      tooltip: 'Open LLM node documentation',
    });
    expect(getNodeDocsLink(PipelineNodeTypes.Custom, tenantPack)).toEqual({
      url: 'https://docs.contoso.example/how-tos/pipelines/nodes/execution-nodes#custom-node',
      tooltip: 'Open Custom node documentation',
    });
  });

  it.each([
    PipelineNodeTypes.Function,
    PipelineNodeTypes.Tool,
    PipelineNodeTypes.Condition,
    PipelineNodeTypes.Pipeline,
    PipelineNodeTypes.Loop,
    PipelineNodeTypes.LoopFromTool,
  ])('links deprecated %s to its migration guide', (type) => {
    expect(getNodeDocsLink(type)).toEqual({
      url: DeprecatedTips[type]?.linkUrl,
      tooltip: MIGRATION_GUIDE_TOOLTIP,
    });
  });

  it('links Custom, which has no migration guide, to its documentation section', () => {
    expect(getNodeDocsLink(PipelineNodeTypes.Custom)).toEqual({
      url: '/docs/how-tos/pipelines/nodes/execution-nodes#custom-node',
      tooltip: 'Open Custom node documentation',
    });
  });

  it.each([
    PipelineNodeTypes.End,
    PipelineNodeTypes.Ghost,
    PipelineNodeTypes.Default,
    PipelineNodeTypes.Aggregate,
    PipelineNodeTypes.Map,
    PipelineNodeTypes.Parallel,
    PipelineNodeTypes.SplitOut,
    'unknown',
    '__proto__',
    'constructor',
  ])('returns null for %s', (type) => {
    expect(getNodeDocsLink(type)).toBeNull();
  });
});
