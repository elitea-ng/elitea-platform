import { describe, expect, it } from 'vitest';

import {
  collectBlockingNodeIssuesById,
  getNoEntryPointTestReason,
  getNoNodesTestReason,
  getNoPathToEndTestReason,
  getNodeNotConfiguredReason,
  getTestBlockReason,
  isEndReachable,
} from './pipelineReadiness.helpers';

describe('pipelineReadiness.helpers', () => {
  it('blocks testing when there are no nodes', () => {
    expect(getTestBlockReason({ nodes: [] })).toBe(getNoNodesTestReason());
    expect(getTestBlockReason(undefined)).toBe(getNoNodesTestReason());
  });

  it('blocks testing when entry_point is missing or does not match a node', () => {
    expect(getTestBlockReason({ nodes: [{ id: 'LLM_1', type: 'llm', transition: 'END' }] })).toBe(
      getNoEntryPointTestReason(),
    );
    expect(
      getTestBlockReason({
        entry_point: 'Missing',
        nodes: [{ id: 'LLM_1', type: 'llm', transition: 'END' }],
      }),
    ).toBe(getNoEntryPointTestReason());
  });

  it('blocks testing when no path connects entry_point to END', () => {
    expect(
      getTestBlockReason({
        entry_point: 'LLM_1',
        nodes: [
          { id: 'LLM_1', type: 'llm', transition: 'LLM_2' },
          { id: 'LLM_2', type: 'llm', transition: 'LLM_1' },
        ],
      }),
    ).toBe(getNoPathToEndTestReason());
  });

  it('allows testing when entry_point reaches END directly or via implicit decision default_output', () => {
    expect(
      getTestBlockReason({
        entry_point: 'Printer_1',
        nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }],
      }),
    ).toBe('');

    expect(
      getTestBlockReason({
        entry_point: 'Decision_1',
        nodes: [{ id: 'Decision_1', type: 'decision', decision: { nodes: ['Printer_1'] } }],
      }),
    ).toBe('');
  });

  it('reports the first unconfigured node issue when nodeIssuesById is populated', () => {
    expect(
      getTestBlockReason(
        {
          entry_point: 'LLM_1',
          nodes: [{ id: 'LLM_1', type: 'llm', transition: 'END' }],
        },
        { LLM_1: ['Prompt is required'] },
      ),
    ).toBe(getNodeNotConfiguredReason('LLM_1', 'Prompt is required'));
  });

  it('isEndReachable traverses multi-hop acyclic and cyclic graphs safely', () => {
    expect(
      isEndReachable(
        [
          { source: 'A', target: 'B' },
          { source: 'B', target: 'END' },
        ],
        'A',
      ),
    ).toBe(true);
    expect(
      isEndReachable(
        [
          { source: 'A', target: 'B' },
          { source: 'B', target: 'A' },
        ],
        'A',
      ),
    ).toBe(false);
  });

  it('normalizes numeric YAML node identifiers and ignores prototype keys safely', () => {
    expect(
      getTestBlockReason({
        entry_point: 1 as unknown as string,
        nodes: [{ id: 1 as unknown as string, type: 'printer', transition: 'END' }],
      }),
    ).toBe('');

    expect(
      getTestBlockReason(
        {
          entry_point: 'constructor',
          nodes: [
            { id: 'constructor', type: 'printer', transition: 'toString' },
            { id: 'toString', type: 'printer', transition: 'END' },
          ],
        },
        {},
      ),
    ).toBe('');
  });

  it('collectBlockingNodeIssuesById groups blocking admission issues by nodeId', () => {
    const issues = collectBlockingNodeIssuesById({
      entry_point: 'Tool_1',
      nodes: [{ id: 'Tool_1', type: 'toolkit', transition: 'END' }],
    });
    expect(Object.getPrototypeOf(issues)).toBeNull();
    expect(issues['Tool_1']?.length ?? 0).toBeGreaterThan(0);
    expect(getTestBlockReason({ entry_point: 'Tool_1', nodes: [{ id: 'Tool_1', type: 'toolkit', transition: 'END' }] }, issues)).toMatch(
      /^Configure node "Tool_1":/,
    );
  });
});
