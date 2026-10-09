import { describe, expect, it } from 'vitest';

import { goThroughNodesTree, parseNodes } from './parsePipelineTraversal.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';

describe('goThroughNodesTree', () => {
  it('is a no-op when the root node id is not found', () => {
    const nodes: never[] = [];
    goThroughNodesTree([], 'missing', nodes, [], [], []);
    expect(nodes).toEqual([]);
  });

  it('follows a straight-line transition chain to End, adding an End edge at the end', () => {
    const yamlNodes: YamlPipelineNode[] = [
      { id: 'A', type: 'tool', transition: 'B' },
      { id: 'B', type: 'tool' },
    ];
    const nodes: { id: string }[] = [];
    const edges: { source: string; target: string }[] = [];
    goThroughNodesTree(yamlNodes, 'A', nodes as never, edges as never, [], []);
    expect(nodes.map(n => n.id)).toEqual(['A', 'B']);
    expect(edges).toEqual([
      { id: 'xy-edge__A---B', source: 'A', target: 'B', type: 'custom', data: { label: undefined } },
      { id: 'xy-edge__B---EliteAPipelineEnd', source: 'B', target: 'END', type: 'custom' },
    ]);
  });

  it('dispatches to the router handler for a Router-type node', () => {
    const yamlNodes: YamlPipelineNode[] = [
      { id: 'R', type: 'router', routes: ['B'] },
      { id: 'B', type: 'tool' },
    ];
    const nodes: { id: string }[] = [];
    goThroughNodesTree(yamlNodes, 'R', nodes as never, [] as never, [], []);
    expect(nodes.map(n => n.id)).toEqual(['R', 'B']);
  });

  it('dispatches to the legacy condition handler when a node has a `condition` sub-object', () => {
    const yamlNodes: YamlPipelineNode[] = [
      { id: 'C', type: 'tool', condition: { conditional_outputs: ['B'] } },
      { id: 'B', type: 'tool' },
    ];
    const nodes: { id: string }[] = [];
    goThroughNodesTree(yamlNodes, 'C', nodes as never, [] as never, [], []);
    expect(nodes.map(n => n.id)).toEqual(['C', 'C~~~ConditionNode', 'B']);
  });
});

describe('parseNodes', () => {
  it('seeds only the End node when there is no YAML document', () => {
    const result = parseNodes(undefined);
    expect(result.nodes).toEqual([{ id: 'END', type: 'END', data: { label: 'End' }, position: { x: 60, y: 200 } }]);
    expect(result.edges).toEqual([]);
  });

  it('walks from entry_point, then sweeps any orphan nodes the entry point never reached', () => {
    const doc: YamlPipelineDocument = {
      entry_point: 'A',
      nodes: [{ id: 'A', type: 'tool', transition: 'END' }, { id: 'Orphan', type: 'tool', transition: 'END' }],
    };
    const result = parseNodes(doc);
    expect(result.nodes.map(n => n.id).sort()).toEqual(['A', 'END', 'Orphan'].sort());
  });

  it('retains the after-pause label on an END-transition edge when the document loads', () => {
    const document: YamlPipelineDocument = {
      entry_point: 'A',
      nodes: [{ id: 'A', type: 'llm', transition: 'END' }],
      interrupt_before: ['A'],
      interrupt_after: ['A'],
    };

    expect(parseNodes(document).edges).toEqual([
      { id: 'xy-edge__A---EliteAPipelineEnd', source: 'A', target: 'END', type: 'custom', data: { label: 'interrupt' } },
    ]);
  });

  it.each(['END', undefined])('retains after-pause labels on Router default route %s', (defaultOutput) => {
    const document: YamlPipelineDocument = {
      entry_point: 'R',
      nodes: [
        { id: 'R', type: 'router', routes: ['B'], ...(defaultOutput === undefined ? {} : { default_output: defaultOutput }) },
        { id: 'B', type: 'llm', transition: 'END' },
      ],
      interrupt_after: ['R'],
    };

    const edges = parseNodes(document).edges.filter((edge) => edge.source === 'R');
    expect(edges).toHaveLength(2);
    expect(edges.map((edge) => edge.data?.label)).toEqual(['interrupt', 'interrupt']);
  });

  it('retains after-pause labels on HITL terminal and stored-node routes', () => {
    const document: YamlPipelineDocument = {
      entry_point: 'H',
      nodes: [
        { id: 'H', type: 'hitl', routes: { approve: 'END', reject: 'B' } },
        { id: 'B', type: 'llm', transition: 'END' },
      ],
      interrupt_after: ['H'],
    };

    const edges = parseNodes(document).edges.filter((edge) => edge.source === 'H');
    expect(edges).toHaveLength(2);
    expect(edges.map((edge) => edge.data?.label)).toEqual(['interrupt', 'interrupt']);
  });

  it('preserves synthetic legacy branch edges and their existing pause labels', () => {
    const document: YamlPipelineDocument = {
      entry_point: 'A',
      nodes: [
        { id: 'A', type: 'tool', condition: { conditional_outputs: ['B'] } },
        { id: 'B', type: 'tool', transition: 'END' },
      ],
      interrupt_after: ['A'],
    };

    const { edges } = parseNodes(document);
    expect(edges.find((edge) => edge.source === 'A')?.data).toBeUndefined();
    expect(edges.find((edge) => edge.source === 'A~~~ConditionNode')?.data?.label).toBe('interrupt');
    expect(edges.find((edge) => edge.source === 'B')?.data).toBeUndefined();
  });

  it('normalizes non-array interrupt_before/after to empty arrays rather than throwing', () => {
    const doc = {
      entry_point: 'A',
      nodes: [{ id: 'A', type: 'tool', transition: 'END' }],
      interrupt_before: 'not-an-array',
      interrupt_after: null,
    } as unknown as YamlPipelineDocument;
    expect(() => parseNodes(doc)).not.toThrow();
  });

  it('filters out falsy entries in the nodes array before walking', () => {
    const doc = { entry_point: 'A', nodes: [{ id: 'A', type: 'tool', transition: 'END' }, null] } as unknown as YamlPipelineDocument;
    expect(() => parseNodes(doc)).not.toThrow();
  });
});
