import { describe, expect, it } from 'vitest';

import { isRunFinished, stepStateValues } from './runStateValues';

/**
 * #6883: a pipeline whose last node is an agent. The agent's entry got no
 * state of its own (the run ended on `pipeline_finish`), and its last edge
 * wrote the final state onto the agent's tool entry before it.
 */
const terminalAgentTimeline = [
  { state: { answer: '', input: 'hi' } }, // start
  { state: { answer: 'draft', input: 'hi' } }, // agent's tool entry, last edge state
  { state: {} }, // the terminal agent entry
];

describe('stepStateValues (#6883)', () => {
  it('reads Before from the previous entry and After from the selected one for a middle step', () => {
    const timeline = [{ state: { n: 0 } }, { state: { n: 1 } }, { state: { n: 2 } }];
    expect(stepStateValues(timeline, 1, 'n', true)).toEqual({ before: 0, after: 1, isFinal: false });
  });

  it('shows an empty Before for the first step', () => {
    expect(stepStateValues([{ state: { n: 0 } }, { state: { n: 1 } }], 0, 'n', true).before).toBe('');
  });

  it('shows the final state for the terminal step of a finished run, even when that entry has no state', () => {
    expect(stepStateValues(terminalAgentTimeline, 2, 'answer', true)).toEqual({ before: 'draft', after: 'draft', isFinal: true });
  });

  it('prefers the terminal entry own state when it has one', () => {
    const timeline = [{ state: { answer: '' } }, { state: { answer: 'final' } }];
    expect(stepStateValues(timeline, 1, 'answer', true)).toEqual({ before: '', after: 'final', isFinal: true });
  });

  it('does not call the last step final while the run is still in progress', () => {
    expect(stepStateValues(terminalAgentTimeline, 2, 'answer', false)).toEqual({ before: 'draft', after: undefined, isFinal: false });
  });

  it('handles an empty timeline', () => {
    expect(stepStateValues([], 0, 'answer', true)).toEqual({ before: '', after: undefined, isFinal: false });
  });
});

describe('isRunFinished (#6883)', () => {
  it.each(['Completed', 'Error', 'Stopped'])('calls a %s run finished', (status) => {
    expect(isRunFinished(status)).toBe(true);
  });

  it.each(['In progress', 'Interrupt', 'Paused', '', undefined])('does not call a %s run finished', (status) => {
    // Interrupt is a human-in-the-loop pause: the run resumes and its state changes.
    expect(isRunFinished(status)).toBe(false);
  });

  it('keeps an interrupted run on its own last-step value instead of a scanned-back "final" one', () => {
    expect(stepStateValues(terminalAgentTimeline, 2, 'answer', isRunFinished('Interrupt'))).toEqual({
      before: 'draft',
      after: undefined,
      isFinal: false,
    });
  });
});
