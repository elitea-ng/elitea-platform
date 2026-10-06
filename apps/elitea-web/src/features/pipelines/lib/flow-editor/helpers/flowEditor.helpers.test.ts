import { describe, expect, it, vi } from 'vitest';

import {
  batchUpdateYamlNode,
  NEW_NODE_VERTICAL_GAP,
  NODE_CARD_WIDTH,
  calculatePositionForNewNode,
  canConnectToTarget,
  canCreateNodeType,
  convertNode,
  formatFStringValue,
  generateNodeIdByType,
  getAllowedNodeTypes,
  getInitialNodeId,
  getNodeTypeFlags,
  getTargetNodeTypeFlags,
  getToolName,
  measureNodes,
  removeYamlNodeVariablesMapping,
  updateNode,
  updateYamlNode,
  updateYamlNodeInputMappingVariable,
} from './flowEditor.helpers';
import { NodeHeightMap, PipelineNodeTypes } from '../constants/flowEditor.constants';

describe('measureNodes', () => {
  it('measures each node found in the DOM, leaves unfound nodes untouched', () => {
    const el = { getBoundingClientRect: () => ({ width: 200, height: 100 }) } as unknown as Element;
    const editorRef = { current: { querySelector: (sel: string) => (sel.includes('"A"') ? el : null) } as unknown as HTMLElement };
    const nodes: { id: string; position: { x: number; y: number }; measured?: { width: number; height: number } }[] = [
      { id: 'A', position: { x: 0, y: 0 } },
      { id: 'B', position: { x: 0, y: 0 } },
    ];
    const result = measureNodes(nodes, 2, editorRef);
    expect(result[0]?.measured).toEqual({ width: 100, height: 50 });
    expect(result[1]).toBe(nodes[1]);
  });
});

describe('convertNode', () => {
  it('strips measured/style when there is no layout version (legacy saved graph)', () => {
    const node = { id: 'A', position: { x: 0, y: 0 }, measured: { width: 1, height: 1 }, style: {} };
    expect(convertNode(node, undefined)).toEqual({ ...node, measured: undefined, style: undefined });
  });

  it('passes the node through unchanged when a layout version is present', () => {
    const node = { id: 'A', position: { x: 0, y: 0 } };
    expect(convertNode(node, '1.0')).toBe(node);
  });
});

describe('updateNode / updateYamlNode / batchUpdateYamlNode', () => {
  it('updateYamlNode sets a single field on the matched node, preserving its position', () => {
    const doc = { nodes: [{ id: 'A', tool: 'x' }, { id: 'B' }] };
    const set = vi.fn();
    updateYamlNode('A', 'tool', 'y', doc, set);
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', tool: 'y' }, { id: 'B' }] });
  });

  it('is a no-op when the node id is not found', () => {
    const doc = { nodes: [{ id: 'A' }] };
    const set = vi.fn();
    updateYamlNode('Z', 'tool', 'y', doc, set);
    expect(set).not.toHaveBeenCalled();
  });

  it('batchUpdateYamlNode merges by default, replaces entirely when replace=true', () => {
    const doc = { nodes: [{ id: 'A', tool: 'x', description: 'd' }] };
    const set = vi.fn();
    batchUpdateYamlNode('A', { tool: 'y' }, doc, set);
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', tool: 'y', description: 'd' }] });

    const set2 = vi.fn();
    batchUpdateYamlNode('A', { id: 'A', tool: 'z' }, doc, set2, true);
    expect(set2).toHaveBeenCalledWith({ nodes: [{ id: 'A', tool: 'z' }] });
  });

  it('omits verified explicit deletion markers and retains unrelated null/undefined values', () => {
    const doc = {nodes:[{id:'A',transition:'END',tool:'old',opaque:{value:null}}]};
    const set = vi.fn();
    batchUpdateYamlNode('A',{transition:undefined,tool:undefined},doc,set);
    const next = set.mock.calls[0]?.[0] as typeof doc;
    expect(Object.hasOwn(next.nodes[0]!, 'transition')).toBe(false);
    expect(Object.hasOwn(next.nodes[0]!, 'tool')).toBe(false);
    expect(next.nodes[0]?.opaque).toEqual({value:null});
    expect(doc.nodes[0]?.transition).toBe('END');
  });

  it('updateNode preserves node order via splice-in-place', () => {
    const doc = { nodes: [{ id: 'A' }, { id: 'B' }, { id: 'C' }] };
    const set = vi.fn();
    updateNode('B', doc, set, node => ({ ...node, tool: 'y' }));
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A' }, { id: 'B', tool: 'y' }, { id: 'C' }] });
  });
});

describe('updateYamlNodeInputMappingVariable', () => {
  it('sets a fixed mapping value verbatim for a string field', () => {
    const doc = { nodes: [{ id: 'A' }] };
    const set = vi.fn();
    updateYamlNodeInputMappingVariable('A', 'title', { type: 'fixed', value: 'hi' }, doc, set, 'string');
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', input_mapping: { title: { type: 'fixed', value: 'hi' } } }] });
  });

  it('parses a fixed numeric value from its string form', () => {
    const doc = { nodes: [{ id: 'A' }] };
    const set = vi.fn();
    updateYamlNodeInputMappingVariable('A', 'count', { type: 'fixed', value: '42' }, doc, set, 'integer');
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', input_mapping: { count: { type: 'fixed', value: 42 } } }] });
  });

  it('leaves the raw string when it fails to parse as JSON (invalid number input)', () => {
    const doc = { nodes: [{ id: 'A' }] };
    const set = vi.fn();
    updateYamlNodeInputMappingVariable('A', 'count', { type: 'fixed', value: 'not-a-number' }, doc, set, 'number');
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', input_mapping: { count: { type: 'fixed', value: 'not-a-number' } } }] });
  });

  it('coerces a fixed boolean value from a string', () => {
    const doc = { nodes: [{ id: 'A' }] };
    const set = vi.fn();
    updateYamlNodeInputMappingVariable('A', 'flag', { type: 'fixed', value: 'true' }, doc, set, 'boolean');
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', input_mapping: { flag: { type: 'fixed', value: true } } }] });
  });

  it('merges into any existing input_mapping rather than replacing it', () => {
    const doc = { nodes: [{ id: 'A', input_mapping: { other: { type: 'fixed', value: 1 } } }] };
    const set = vi.fn();
    updateYamlNodeInputMappingVariable('A', 'title', { type: 'variable', value: 'x' }, doc, set);
    expect(set).toHaveBeenCalledWith({
      nodes: [{ id: 'A', input_mapping: { other: { type: 'fixed', value: 1 }, title: { type: 'variable', value: 'x' } } }],
    });
  });
});

describe('removeYamlNodeVariablesMapping', () => {
  it('deletes only the given output key from variables_mapping', () => {
    const doc = { nodes: [{ id: 'A', variables_mapping: { out1: 'x', out2: 'y' } }] };
    const set = vi.fn();
    removeYamlNodeVariablesMapping('A', 'out1', doc, set);
    expect(set).toHaveBeenCalledWith({ nodes: [{ id: 'A', variables_mapping: { out2: 'y' } }] });
  });

  it('deep-clones so the original object is not mutated', () => {
    const original = { out1: 'x' };
    const doc = { nodes: [{ id: 'A', variables_mapping: original }] };
    removeYamlNodeVariablesMapping('A', 'out1', doc, vi.fn());
    expect(original).toEqual({ out1: 'x' });
  });
});

describe('getNodeTypeFlags / getTargetNodeTypeFlags / canConnectToTarget', () => {
  it('flags a legacy condition/decision source by id suffix, or by a live condition/decision field', () => {
    expect(getNodeTypeFlags('A~~~ConditionNode', undefined, undefined).isFromConditionNode).toBe(true);
    expect(getNodeTypeFlags('A', undefined, { condition: {} }).isFromConditionNode).toBe(true);
    expect(getNodeTypeFlags('A', 'routerNode_routes', undefined).isFromRouterHandle).toBe(true);
    expect(getNodeTypeFlags('A', 'hitlNode_approve', undefined).isFromHitlHandle).toBe(true);
  });

  it('getTargetNodeTypeFlags identifies End by id equality', () => {
    expect(getTargetNodeTypeFlags({ id: PipelineNodeTypes.End, position: { x: 0, y: 0 } }).isTargetEndNode).toBe(true);
    expect(getTargetNodeTypeFlags({ id: 'A', position: { x: 0, y: 0 } }).isTargetEndNode).toBe(false);
  });

  it('canConnectToTarget rejects special-to-special connections', () => {
    const source = getNodeTypeFlags('A~~~ConditionNode', undefined, undefined);
    const target = getTargetNodeTypeFlags({ id: 'B~~~DecisionNode', position: { x: 0, y: 0 } });
    expect(canConnectToTarget(source, { ...target, isTargetDecisionNode: true, isTargetConditionNode: false }, undefined)).toBe(
      false,
    );
  });

  it('canConnectToTarget allows a non-special source to connect to a special (condition-suffixed) target — only special-TO-special is rejected by this guard', () => {
    const source = getNodeTypeFlags('A', undefined, undefined);
    const target = getTargetNodeTypeFlags({ id: 'B~~~ConditionNode', position: { x: 0, y: 0 } });
    expect(canConnectToTarget(source, target, undefined)).toBe(true);
  });

  it('canConnectToTarget allows a special source to connect to a non-special target', () => {
    const source = getNodeTypeFlags('A~~~ConditionNode', undefined, undefined);
    const target = getTargetNodeTypeFlags({ id: 'B', position: { x: 0, y: 0 } });
    expect(canConnectToTarget(source, target, undefined)).toBe(true);
  });

  it('canConnectToTarget rejects connecting to End when the source already has a non-End transition', () => {
    const source = getNodeTypeFlags('A', undefined, undefined);
    const target = getTargetNodeTypeFlags({ id: PipelineNodeTypes.End, position: { x: 0, y: 0 } });
    expect(canConnectToTarget(source, target, { transition: 'B' })).toBe(false);
    expect(canConnectToTarget(source, target, { transition: PipelineNodeTypes.End })).toBe(true);
    expect(canConnectToTarget(source, target, undefined)).toBe(true);
  });
});

describe('canCreateNodeType', () => {
  it('forbids creating a Condition/Decision node from a special source node', () => {
    const source = getNodeTypeFlags('A~~~ConditionNode', undefined, undefined);
    expect(canCreateNodeType(PipelineNodeTypes.Condition, source)).toBe(false);
    expect(canCreateNodeType(PipelineNodeTypes.Tool, source)).toBe(true);
  });
});

describe('getAllowedNodeTypes', () => {
  it('excludes End/Ghost/Default/Function, alphabetically ordered by declared key', () => {
    const allowed = getAllowedNodeTypes();
    expect(allowed).not.toContain(PipelineNodeTypes.End);
    expect(allowed).not.toContain(PipelineNodeTypes.Ghost);
    expect(allowed).not.toContain(PipelineNodeTypes.Default);
    expect(allowed).not.toContain(PipelineNodeTypes.Function);
    expect(allowed).toContain(PipelineNodeTypes.Tool);
    expect(allowed).toContain(PipelineNodeTypes.Agent);
  });
});

describe('getToolName', () => {
  it('passes a string tool through unchanged', () => {
    expect(getToolName('my_tool')).toBe('my_tool');
  });

  it('prefers name, then description, then path for an object tool', () => {
    expect(getToolName({ name: 'N', description: 'D', path: 'P' })).toBe('N');
    expect(getToolName({ description: 'D', path: 'P' })).toBe('D');
    expect(getToolName({ path: 'P' })).toBe('P');
    expect(getToolName({})).toBe('');
  });

  it('falls through past an explicit empty-string name/description instead of returning a blank label', () => {
    expect(getToolName({ name: '', description: 'D', path: 'P' })).toBe('D');
    expect(getToolName({ name: '', description: '', path: 'P' })).toBe('P');
    expect(getToolName({ name: '', description: '', path: '' })).toBe('');
  });
});

describe('calculatePositionForNewNode', () => {
  it('returns the start position immediately when the canvas is empty', () => {
    expect(calculatePositionForNewNode(60, 200, [], PipelineNodeTypes.LLM)).toEqual({ xPos: 60, yPos: 200 });
  });

  /**
   * The Add-node menu always computes the SAME start position (the viewport
   * centre), so a sequence of adds is exactly this loop. Node cards are
   * `NODE_CARD_WIDTH` wide and several hundred px tall (`NodeHeightMap`), so
   * the placement rule has to clear the rendered bounds, not just the
   * top-left point.
   */
  const addInSequence = (types: readonly string[]) => {
    const placed: { id: string; type: string; position: { x: number; y: number } }[] = [];
    for (const [index, type] of types.entries()) {
      const { xPos, yPos } = calculatePositionForNewNode(170, 100, placed, type);
      placed.push({ id: `${type}_${index}`, type, position: { x: xPos, y: yPos } });
    }
    return placed;
  };

  const boundsOf = (node: { type: string; position: { x: number; y: number } }) => ({
    left: node.position.x,
    right: node.position.x + NODE_CARD_WIDTH,
    top: node.position.y,
    bottom: node.position.y + (NodeHeightMap[node.type] ?? 500),
  });

  it('places LLM, Router and Printer added in sequence so their rendered cards never intersect', () => {
    const placed = addInSequence([PipelineNodeTypes.LLM, PipelineNodeTypes.Router, PipelineNodeTypes.Printer]);

    for (let i = 0; i < placed.length; i += 1) {
      for (let j = i + 1; j < placed.length; j += 1) {
        const a = boundsOf(placed[i]!);
        const b = boundsOf(placed[j]!);
        const intersects = a.left < b.right && a.right > b.left && a.top < b.bottom && a.bottom > b.top;
        expect(`${placed[i]!.id} vs ${placed[j]!.id}: ${intersects ? 'overlaps' : 'clear'}`).toBe(
          `${placed[i]!.id} vs ${placed[j]!.id}: clear`,
        );
      }
    }
  });

  it('stacks each new node below the lowest existing card, keeping the start x', () => {
    const placed = addInSequence([PipelineNodeTypes.LLM, PipelineNodeTypes.Router]);

    // LLM at the viewport centre; Router below its 460px-tall card + the gap.
    expect(placed[0]!.position).toEqual({ x: 170, y: 100 });
    expect(placed[1]!.position).toEqual({ x: 170, y: 100 + 460 + NEW_NODE_VERTICAL_GAP });
  });

  it('prefers a measured height over the NodeHeightMap estimate', () => {
    const measuredTall = [{ id: 'LLM_1', type: PipelineNodeTypes.LLM, position: { x: 170, y: 100 }, measured: { width: 460, height: 900 } }];
    expect(calculatePositionForNewNode(170, 100, measuredTall, PipelineNodeTypes.Printer)).toEqual({
      xPos: 170,
      yPos: 100 + 900 + NEW_NODE_VERTICAL_GAP,
    });
  });

  it('leaves the start position alone when existing cards are horizontally clear of it', () => {
    const faraway = [{ id: 'LLM_1', type: PipelineNodeTypes.LLM, position: { x: 2000, y: 100 } }];
    expect(calculatePositionForNewNode(170, 100, faraway, PipelineNodeTypes.LLM)).toEqual({ xPos: 170, yPos: 100 });
  });
});

describe('getInitialNodeId / generateNodeIdByType', () => {
  it('numbers normal node types sequentially, skipping used names (spaces and underscores ignored)', () => {
    expect(getInitialNodeId(PipelineNodeTypes.Tool, [{ id: 'Tool 1' }, { id: 'Tool2' }])).toBe('Tool_3');
    expect(getInitialNodeId(PipelineNodeTypes.Tool, [])).toBe('Tool_1');
  });

  it('generates a timestamped id for Condition nodes instead', () => {
    expect(getInitialNodeId(PipelineNodeTypes.Condition, [])).toMatch(/^Condition\d+~~~ConditionNode$/);
  });

  it('generateNodeIdByType seeds the node with its InitialNodeData shape', () => {
    const node = generateNodeIdByType(PipelineNodeTypes.Tool, []);
    expect(node).toMatchObject({ id: 'Tool_1', type: PipelineNodeTypes.Tool, tool: '', structured_output: false });
  });
});

describe('formatFStringValue', () => {
  it('passes strings and nullish values through unchanged', () => {
    expect(formatFStringValue('hi')).toBe('hi');
    expect(formatFStringValue(undefined)).toBeUndefined();
    expect(formatFStringValue(null)).toBeNull();
  });

  it('JSON-stringifies non-string values', () => {
    expect(formatFStringValue(42)).toBe('42');
    expect(formatFStringValue({ a: 1 })).toBe('{"a":1}');
  });
});
