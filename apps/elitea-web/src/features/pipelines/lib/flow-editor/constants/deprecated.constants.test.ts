import { describe, expect, it } from 'vitest';

import { PipelineNodeTypes } from '../constants/flowEditor.constants';
import { DeprecatedNodes, DeprecatedOrInvisibleNode, DeprecatedTips } from './deprecated.constants';

describe('DeprecatedTips', () => {
  it('has one entry per deprecated node type, each with text/linkText/linkUrl', () => {
    for (const type of DeprecatedNodes) {
      const tip = DeprecatedTips[type];
      expect(tip).toBeDefined();
      expect(tip?.text).toBe('This node is deprecated and will be removed in a future version. ');
      expect(tip?.linkText).toBe('View Migration Guide');
      expect(tip?.linkUrl).toMatch(/^\/docs\/how-tos\/pipelines\//);
    }
  });

  it('gives the Condition node its own condition-node-migration link', () => {
    expect(DeprecatedTips[PipelineNodeTypes.Condition]?.linkUrl).toBe(
      '/docs/how-tos/pipelines/condition-node-migration',
    );
  });

  it('gives Loop and LoopFromTool the same loop-node-migration link', () => {
    expect(DeprecatedTips[PipelineNodeTypes.Loop]?.linkUrl).toBe(
      DeprecatedTips[PipelineNodeTypes.LoopFromTool]?.linkUrl,
    );
  });

  it('has no entry for a node type that is not deprecated', () => {
    expect(DeprecatedTips[PipelineNodeTypes.LLM]).toBeUndefined();
  });
});

describe('DeprecatedNodes', () => {
  it('lists exactly the six deprecated pipeline node types', () => {
    expect(DeprecatedNodes).toEqual([
      PipelineNodeTypes.Function,
      PipelineNodeTypes.Condition,
      PipelineNodeTypes.Pipeline,
      PipelineNodeTypes.Loop,
      PipelineNodeTypes.LoopFromTool,
      PipelineNodeTypes.Tool,
    ]);
  });
});

describe('DeprecatedOrInvisibleNode', () => {
  it('is the deprecated types (by declared key name) plus End/Ghost/Default', () => {
    expect(DeprecatedOrInvisibleNode).toEqual(['Function', 'Condition', 'Pipeline', 'Loop', 'LoopFromTool', 'Tool', 'End', 'Ghost', 'Default']);
  });

  it('has 9 entries: 6 deprecated + End + Ghost + Default', () => {
    expect(DeprecatedOrInvisibleNode).toHaveLength(9);
  });
});
