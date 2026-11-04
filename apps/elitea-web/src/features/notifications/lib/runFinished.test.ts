import { describe, expect, it } from 'vitest';

import { runFinishedTarget, runFinishedText } from './runFinished';

describe('runFinishedText', () => {
  it.each([
    ['completed', 'Run finished', 'A run is done.'],
    ['failed', 'Run failed', 'A run stopped with an error.'],
    ['waiting', 'Run needs you', 'A run waits for your answer.'],
    ['limit_reached', 'Run stopped at its limit', 'A run reached its chain limit.'],
  ])('%s → "%s"', (outcome, title, body) => {
    expect(runFinishedText(outcome)).toEqual({ title, body });
  });

  it('reads an unknown or missing outcome as a plain finish', () => {
    expect(runFinishedText('something_new').title).toBe('Run finished');
    expect(runFinishedText(undefined).title).toBe('Run finished');
  });
});

describe('runFinishedTarget', () => {
  const cloud = { origin: 'cloud', projectId: '9', conversationUuid: 'c-1' };

  it('opens the conversation of a cloud chain, in the meta project', () => {
    expect(runFinishedTarget(cloud, '1', false)).toEqual({ kind: 'conversation', projectId: '9', conversationUuid: 'c-1' });
    expect(runFinishedTarget(cloud, '1', true)).toEqual({ kind: 'conversation', projectId: '9', conversationUuid: 'c-1' });
  });

  it("falls back to the row's own project, and accepts a numeric meta project", () => {
    expect(runFinishedTarget({ conversationUuid: 'c-1' }, '4', false)).toEqual({ kind: 'conversation', projectId: '4', conversationUuid: 'c-1' });
    expect(runFinishedTarget({ conversationUuid: 'c-1', projectId: 5 as unknown as string }, '4', false)).toEqual({ kind: 'conversation', projectId: '5', conversationUuid: 'c-1' });
  });

  it('opens the thread of a desktop chain in the desktop app only', () => {
    const desktop = { origin: 'desktop', threadId: 't-7', conversationUuid: 'c-2', projectId: '3' };
    expect(runFinishedTarget(desktop, undefined, true)).toEqual({ kind: 'thread', threadId: 't-7' });
    // In a browser the conversation, when the chain has one, is the target.
    expect(runFinishedTarget(desktop, undefined, false)).toEqual({ kind: 'conversation', projectId: '3', conversationUuid: 'c-2' });
  });

  it('has no target for a desktop-only thread seen in a browser, or without ids', () => {
    expect(runFinishedTarget({ origin: 'desktop', threadId: 't-7', projectId: '3' }, '3', false)).toBeNull();
    expect(runFinishedTarget(undefined, '3', false)).toBeNull();
    expect(runFinishedTarget({ conversationUuid: '' }, '3', false)).toBeNull();
    expect(runFinishedTarget({ conversationUuid: 'c-1' }, undefined, false)).toBeNull();
  });
});
