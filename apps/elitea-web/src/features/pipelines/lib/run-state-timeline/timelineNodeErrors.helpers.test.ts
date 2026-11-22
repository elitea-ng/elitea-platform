import { describe, expect, it } from 'vitest';

import type { GroupedTimelineStep } from './timeline.types';
import {
  collectContinuedFailuresInRange,
  extractContinuedFailuresByStep,
  hasContinuedFailureInRange,
} from './timelineNodeErrors.helpers';

const step = (id: string, state?: Record<string, unknown>, langgraphNode?: string): GroupedTimelineStep => ({
  id,
  created_at: 1_700_000_000_000,
  ...(state ? { state } : {}),
  ...(langgraphNode ? { langgraph_node: langgraphNode } : {}),
});

describe('extractContinuedFailuresByStep', () => {
  it('attributes unique node_errors entries to the nearest prior step of that node and preserves structured fields', () => {
    const timeline = [
      step('shape', { node_errors: [] }),
      step('route', { node_errors: [] }),
      step('shape', {
        node_errors: [
          {
            node: 'shape',
            handled: 'continue',
            input_step: 3,
            error: { support_ref: 'ERR-101', code: 'UPSTREAM_503', message: 'Transient outage' },
          },
        ],
      }),
      step('route', {
        node_errors: [
          {
            node: 'shape',
            handled: 'continue',
            input_step: 3,
            error: { support_ref: 'ERR-101', code: 'UPSTREAM_503', message: 'Transient outage' },
          },
          {
            node: 'route',
            handled: 'error_output',
            input_step: 4,
            support_ref: 'ERR-202',
            message: 'Routed to error handle',
          },
        ],
      }),
    ];

    const byStep = extractContinuedFailuresByStep(timeline);
    expect([...byStep.keys()]).toEqual([2, 3]);
    expect(byStep.get(2)).toEqual([
      {
        node: 'shape',
        handled: 'continue',
        inputStep: 3,
        supportRef: 'ERR-101',
        code: 'UPSTREAM_503',
        message: 'Transient outage',
      },
    ]);
    expect(byStep.get(3)).toEqual([
      {
        node: 'route',
        handled: 'error_output',
        inputStep: 4,
        supportRef: 'ERR-202',
        message: 'Routed to error handle',
      },
    ]);
  });

  it('attributes an entry shown first on an interrupt marker back to its owning node', () => {
    const timeline = [
      step('shape'),
      step('interrupt', {
        node_errors: [{ node: 'shape', handled: 'continue', input_step: 1, support_ref: 'REF-1' }],
      }),
    ];

    const byStep = extractContinuedFailuresByStep(timeline);
    expect([...byStep.keys()]).toEqual([0]);
    expect(byStep.get(0)?.[0]?.supportRef).toBe('REF-1');
  });

  it('ignores entries from before a resumed timeline and ignores non-continued handling values', () => {
    const timeline = [
      step('review', {
        node_errors: [
          { node: 'earlier_node', handled: 'continue', input_step: 1 },
          { node: 'review', handled: 'stop', input_step: 2 },
        ],
      }),
    ];

    expect(extractContinuedFailuresByStep(timeline).size).toBe(0);
  });

  it('supports range checks and collection across collapsed groups', () => {
    const timeline = [
      step('head'),
      step('child', {
        node_errors: [{ node: 'child', handled: 'continue', support_ref: 'REF-9' }],
      }),
      step('tail'),
    ];
    const byStep = extractContinuedFailuresByStep(timeline);

    expect(hasContinuedFailureInRange(byStep, 0, 2)).toBe(true);
    expect(hasContinuedFailureInRange(byStep, 2, 2)).toBe(false);
    expect(collectContinuedFailuresInRange(byStep, 0, 2)).toHaveLength(1);
  });
});
