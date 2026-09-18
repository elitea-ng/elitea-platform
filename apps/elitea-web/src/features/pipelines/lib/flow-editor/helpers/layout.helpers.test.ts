import { describe, expect, it } from 'vitest';

import type { FlowEdge, FlowNode } from '../reactFlowTypes';
import { doLayout } from './layout.helpers';

function node(id: string, type = 'agent'): FlowNode {
  return { id, type, position: { x: 0, y: 0 }, data: { label: id } };
}

describe('doLayout', () => {
  it('positions every input node and keeps its type/data', () => {
    const nodes = [node('a'), node('b')];
    const edges: FlowEdge[] = [{ id: 'e1', source: 'a', target: 'b' }];

    const result = doLayout({ nodes, edges });

    expect(result.nodes).toHaveLength(2);
    const ids = result.nodes.map(n => n.id).sort();
    expect(ids).toEqual(['a', 'b']);
    for (const laidOutNode of result.nodes) {
      expect(typeof laidOutNode.position.x).toBe('number');
      expect(typeof laidOutNode.position.y).toBe('number');
      expect(laidOutNode.type).toBe('agent');
    }
  });

  it('preserves every edge, including duplicate source/target pairs dagre would otherwise collapse', () => {
    const nodes = [node('a'), node('b')];
    const edges: FlowEdge[] = [
      { id: 'e1', source: 'a', target: 'b', sourceHandle: 'h1' },
      { id: 'e2', source: 'a', target: 'b', sourceHandle: 'h2' },
    ];

    const result = doLayout({ nodes, edges });

    expect(result.edges).toHaveLength(2);
    expect(result.edges.map(e => e.id).sort()).toEqual(['e1', 'e2']);
  });

  it('drops a node whose id was never in the input set (defensive filter on dagre output)', () => {
    const result = doLayout({ nodes: [node('a')], edges: [] });
    expect(result.nodes.map(n => n.id)).toEqual(['a']);
  });

  it('vertical orientation stacks nodes with rankdir TB (y increases down the chain)', () => {
    const nodes = [node('a'), node('b'), node('c')];
    const edges: FlowEdge[] = [
      { id: 'e1', source: 'a', target: 'b' },
      { id: 'e2', source: 'b', target: 'c' },
    ];
    const result = doLayout({ nodes, edges, orientation: 'vertical' });
    const byId = Object.fromEntries(result.nodes.map(n => [n.id, n]));
    expect(byId['a']?.position.y).toBeLessThan(byId['b']?.position.y ?? Infinity);
    expect(byId['b']?.position.y).toBeLessThan(byId['c']?.position.y ?? Infinity);
  });

  it('horizontal orientation stacks nodes left to right (x increases along the chain)', () => {
    const nodes = [node('a'), node('b')];
    const edges: FlowEdge[] = [{ id: 'e1', source: 'a', target: 'b' }];
    const result = doLayout({ nodes, edges, orientation: 'horizontal' });
    const byId = Object.fromEntries(result.nodes.map(n => [n.id, n]));
    expect(byId['a']?.position.x).toBeLessThan(byId['b']?.position.x ?? Infinity);
  });

  it('collapsed (expanded: false) nodes use the fixed 44px height', () => {
    const nodes = [node('a')];
    const result = doLayout({ nodes, edges: [], expanded: false });
    expect(result.nodes[0]?.measured?.height).toBe(44);
  });

  /* elitea_issues: #2520 — auto-layout must space nodes by each node's ACTUAL (measured) height, not a fixed gap that ignores content — a taller upstream node must push its downstream neighbour further down, not leave an oversized (or cramped) fixed-size gap regardless of content. */
  it('spaces a downstream node further away when its upstream neighbour is taller (real measured height, not a fixed gap)', () => {
    const shortUpstream: FlowNode = { ...node('a'), measured: { width: 460, height: 100 } };
    const tallUpstream: FlowNode = { ...node('a'), measured: { width: 460, height: 900 } };
    const downstream = node('b');
    const edges: FlowEdge[] = [{ id: 'e1', source: 'a', target: 'b' }];

    const shortResult = doLayout({ nodes: [shortUpstream, downstream], edges });
    const tallResult = doLayout({ nodes: [tallUpstream, downstream], edges });

    const shortGap = (shortResult.nodes.find(n => n.id === 'b')?.position.y ?? 0) - (shortResult.nodes.find(n => n.id === 'a')?.position.y ?? 0);
    const tallGap = (tallResult.nodes.find(n => n.id === 'b')?.position.y ?? 0) - (tallResult.nodes.find(n => n.id === 'a')?.position.y ?? 0);

    expect(tallGap).toBeGreaterThan(shortGap);
  });

  it('leaves `selected` undefined for an input node that never set it (baseline: `nodeData?.selected`, not `?? false`)', () => {
    const nodes = [node('a')];
    const result = doLayout({ nodes, edges: [] });
    expect(result.nodes[0]?.selected).toBeUndefined();
  });

  it('preserves an explicit `selected: true` from the input node', () => {
    const nodes: FlowNode[] = [{ ...node('a'), selected: true }];
    const result = doLayout({ nodes, edges: [] });
    expect(result.nodes[0]?.selected).toBe(true);
  });
});
