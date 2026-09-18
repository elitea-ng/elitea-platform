import { describe, expect, it } from 'vitest';

import type { YamlPipelineDocument, YamlPipelineNode } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { renameFlowEdge, renameFlowNode, renameYamlDocument, renameYamlNode } from './NodeCardHeader.rename';

describe('renameYamlNode', () => {
  it('renames the node id when it matches', () => {
    const node: YamlPipelineNode = { id: 'Old' };
    expect(renameYamlNode(node, 'Old', 'New')).toMatchObject({ id: 'New' });
  });

  it('leaves the id unchanged for a different node', () => {
    const node: YamlPipelineNode = { id: 'Other' };
    expect(renameYamlNode(node, 'Old', 'New')).toMatchObject({ id: 'Other' });
  });

  /* elitea_issues: #1508 — renaming a Toolkit/Tool node must never touch its own `toolkit_name`/`tool` reference (the legacy bug made the toolkit disappear on rename); the rename pass only ever rewrites id/condition/decision/transition fields. */
  it('renaming a node id leaves its toolkit_name/tool fields untouched', () => {
    const node: YamlPipelineNode = { id: 'Old', toolkit_name: 'GitHub', tool: 'create_issue' };
    const result = renameYamlNode(node, 'Old', 'New');
    expect(result.id).toBe('New');
    expect(result.toolkit_name).toBe('GitHub');
    expect(result.tool).toBe('create_issue');
  });

  it('rewrites a legacy condition sub-object referencing the old name', () => {
    const node: YamlPipelineNode = {
      id: 'Cond1',
      type: 'condition',
      condition: {
        condition_definition: 'Old == 1',
        conditional_outputs: ['Old', 'Other'],
        default_output: 'Old',
      },
    };

    const result = renameYamlNode(node, 'Old', 'New');

    expect(result.condition).toEqual({
      condition_definition: 'New == 1',
      conditional_outputs: ['New', 'Other'],
      default_output: 'New',
    });
  });

  /* elitea_issues: #2706 — renaming a Router node must not spread its plain-string `condition` through the legacy condition-sub-object renamer (that spread is what produced the reported "[object Object]" / character-indexed-array YAML corruption). */
  it('does not treat a Router node\'s condition as a legacy condition rename target', () => {
    const node: YamlPipelineNode = {
      id: 'Router1',
      type: 'router',
      condition: { condition_definition: 'Old == 1' },
    };

    const result = renameYamlNode(node, 'Old', 'New');

    expect(result.condition).toEqual({ condition_definition: 'Old == 1' });
  });

  it('rewrites a legacy decision sub-object', () => {
    const node: YamlPipelineNode = {
      id: 'Dec1',
      decision: { nodes: ['Old', 'Other'], default_output: 'Old' },
    };

    const result = renameYamlNode(node, 'Old', 'New');

    expect(result.decision).toEqual({ nodes: ['New', 'Other'], default_output: 'New' });
  });

  it('rewrites a new-style Decision node\'s top-level nodes/default_output', () => {
    const node: YamlPipelineNode = {
      id: 'Dec2',
      type: 'decision',
      nodes: ['Old', 'Other'],
      default_output: 'Old',
    };

    const result = renameYamlNode(node, 'Old', 'New');

    expect(result.nodes).toEqual(['New', 'Other']);
    expect(result.default_output).toBe('New');
  });

  /* elitea_issues: #1516 — renaming a node must update every other node's `transition` reference to it in the same pass, so a connection to/from the renamed node can still be created (the legacy bug left the YAML pointing at a name that no longer existed). */
  it('rewrites a plain transition pointing at the old name', () => {
    const node: YamlPipelineNode = { id: 'Tool1', transition: 'Old' };
    expect(renameYamlNode(node, 'Old', 'New').transition).toBe('New');
  });

  it('leaves a transition pointing elsewhere untouched', () => {
    const node: YamlPipelineNode = { id: 'Tool1', transition: 'END' };
    expect(renameYamlNode(node, 'Old', 'New').transition).toBe('END');
  });

  it('is a no-op for a node with none of condition/decision/transition set', () => {
    const node: YamlPipelineNode = { id: 'Plain', tool: 'search' };
    expect(renameYamlNode(node, 'Old', 'New')).toEqual({ id: 'Plain', tool: 'search' });
  });
});

describe('renameYamlDocument', () => {
  it('renames entry_point when it points at the renamed node', () => {
    const doc: YamlPipelineDocument = { entry_point: 'Old', nodes: [{ id: 'Old' }] };
    expect(renameYamlDocument(doc, 'Old', 'New').entry_point).toBe('New');
  });

  it('leaves entry_point untouched when it points elsewhere', () => {
    const doc: YamlPipelineDocument = { entry_point: 'Other', nodes: [{ id: 'Old' }, { id: 'Other' }] };
    expect(renameYamlDocument(doc, 'Old', 'New').entry_point).toBe('Other');
  });

  it('renames every matching entry inside interrupt_before/interrupt_after', () => {
    const doc: YamlPipelineDocument = {
      nodes: [],
      interrupt_before: ['Old', 'Kept'],
      interrupt_after: ['Kept', 'Old'],
    };

    const result = renameYamlDocument(doc, 'Old', 'New');

    expect(result.interrupt_before).toEqual(['New', 'Kept']);
    expect(result.interrupt_after).toEqual(['Kept', 'New']);
  });

  it('leaves interrupt_before/interrupt_after undefined when the source had none', () => {
    const doc: YamlPipelineDocument = { nodes: [] };
    const result = renameYamlDocument(doc, 'Old', 'New');
    expect(result.interrupt_before).toBeUndefined();
    expect(result.interrupt_after).toBeUndefined();
  });

  it('maps every node through renameYamlNode', () => {
    const doc: YamlPipelineDocument = { nodes: [{ id: 'Old' }, { id: 'Other', transition: 'Old' }] };
    const result = renameYamlDocument(doc, 'Old', 'New');
    expect(result.nodes).toEqual([{ id: 'New' }, { id: 'Other', transition: 'New' }]);
  });

  it('defaults to an empty nodes array when the source has none', () => {
    const doc: YamlPipelineDocument = {};
    expect(renameYamlDocument(doc, 'Old', 'New').nodes).toEqual([]);
  });
});

describe('renameFlowNode', () => {
  const baseNode: FlowNode = {
    id: 'Old',
    type: 'defaultType',
    position: { x: 0, y: 0 },
    data: {},
  };

  it('renames the node id and data.label', () => {
    const result = renameFlowNode(baseNode, 'Old', 'New');
    expect(result.id).toBe('New');
    expect(result.data.label).toBe('New');
  });

  it('rewrites data.condition when present', () => {
    const node: FlowNode = {
      ...baseNode,
      data: { condition: { condition_definition: 'Old', conditional_outputs: ['Old'], default_output: 'Old' } },
    };

    const result = renameFlowNode(node, 'Old', 'New');

    expect(result.data.condition).toEqual({
      condition_definition: 'New',
      conditional_outputs: ['New'],
      default_output: 'New',
    });
  });

  it('rewrites data.decision when present (and condition is absent)', () => {
    const node: FlowNode = { ...baseNode, data: { decision: { nodes: ['Old'], default_output: 'Old' } } };
    const result = renameFlowNode(node, 'Old', 'New');
    expect(result.data.decision).toEqual({ nodes: ['New'], default_output: 'New' });
  });

  it('rewrites top-level nodes/default_output for a new-style Decision node', () => {
    const node: FlowNode = {
      ...baseNode,
      data: { type: 'decision', nodes: ['Old'], default_output: 'Old' },
    };

    const result = renameFlowNode(node, 'Old', 'New');

    expect(result.data['nodes']).toEqual(['New']);
    expect(result.data['default_output']).toBe('New');
  });

  it('leaves other data fields untouched', () => {
    const node: FlowNode = { ...baseNode, data: { isPerforming: true, status: 'running' } };
    const result = renameFlowNode(node, 'Old', 'New');
    expect(result.data['isPerforming']).toBe(true);
    expect(result.data['status']).toBe('running');
  });

  it('leaves a non-matching node\'s own id/label untouched, but still rewrites its data reference to the renamed node -- the fix for the cross-node rename desync', () => {
    const decisionNode: FlowNode = {
      id: 'Router 1',
      type: 'decision',
      position: { x: 0, y: 0 },
      data: { label: 'Router 1', type: 'decision', nodes: ['Old', 'End'], default_output: 'Old' },
    };

    const result = renameFlowNode(decisionNode, 'Old', 'New');

    expect(result.id).toBe('Router 1');
    expect(result.data.label).toBe('Router 1');
    expect(result.data['nodes']).toEqual(['New', 'End']);
    expect(result.data['default_output']).toBe('New');
  });
});

describe('renameFlowEdge', () => {
  const edge: FlowEdge = { id: 'e1', source: 'Old', target: 'End' };

  it('renames a matching source', () => {
    expect(renameFlowEdge(edge, 'Old', 'New')).toEqual({ id: 'e1', source: 'New', target: 'End' });
  });

  it('renames a matching target', () => {
    const targetEdge: FlowEdge = { id: 'e2', source: 'Start', target: 'Old' };
    expect(renameFlowEdge(targetEdge, 'Old', 'New')).toEqual({ id: 'e2', source: 'Start', target: 'New' });
  });

  it('leaves an unrelated edge untouched (same object identity)', () => {
    const unrelated: FlowEdge = { id: 'e3', source: 'A', target: 'B' };
    expect(renameFlowEdge(unrelated, 'Old', 'New')).toBe(unrelated);
  });
});
