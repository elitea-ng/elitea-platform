import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import { contextHistoryLabel } from '@/shared/lib/contextHistory';

import { areContextBlocksEqual, contextBlockForSave, readContextBlock } from './contextBlock';
import { contextBlockRefusal } from './errorMessage';

describe('readContextBlock', () => {
  it('reads no block as null', () => {
    expect(readContextBlock(undefined)).toBeNull();
    expect(readContextBlock(null)).toBeNull();
    expect(readContextBlock(['since_joined'])).toBeNull();
  });

  it.each(['full', 'since_joined', 'addressed_only', 'none'] as const)('round-trips the %s mode', (history) => {
    const block = readContextBlock({ history, include_side_talk: 'none' });
    expect(block).toEqual({ history, include_side_talk: 'none' });
    expect(contextBlockForSave(block)).toEqual({ history, include_side_talk: 'none' });
  });

  it('keeps activity_notes when it is stored', () => {
    expect(readContextBlock({ history: 'none', include_side_talk: 'party', activity_notes: true })).toEqual({
      history: 'none',
      include_side_talk: 'party',
      activity_notes: true,
    });
  });

  it('shows an unknown mode as the default', () => {
    expect(readContextBlock({ history: 'everything' })).toEqual({ history: 'full', include_side_talk: 'party' });
  });
});

describe('contextBlockForSave', () => {
  it('clears the key for the default block, so an untouched agent stores none', () => {
    expect(contextBlockForSave({ history: 'full', include_side_talk: 'party' })).toBeNull();
    expect(contextBlockForSave(null)).toBeNull();
  });

  it('keeps a block that differs from the default only in side talk', () => {
    expect(contextBlockForSave({ history: 'full', include_side_talk: 'none' })).toEqual({ history: 'full', include_side_talk: 'none' });
  });
});

describe('areContextBlocksEqual (issue 1894)', () => {
  it('compares field by field: key order and an absent activity_notes do not matter', () => {
    expect(areContextBlocksEqual({ history: 'none', include_side_talk: 'none' }, { include_side_talk: 'none', history: 'none', activity_notes: false })).toBe(true);
    expect(areContextBlocksEqual(null, null)).toBe(true);
  });

  it('sees every field, and null against a block', () => {
    const block = { history: 'none', include_side_talk: 'none' } as const;
    expect(areContextBlocksEqual(block, { ...block, history: 'full' })).toBe(false);
    expect(areContextBlocksEqual(block, { ...block, include_side_talk: 'party' })).toBe(false);
    expect(areContextBlocksEqual(block, { ...block, activity_notes: true })).toBe(false);
    expect(areContextBlocksEqual(block, null)).toBe(false);
  });
});

describe('contextHistoryLabel (one label set for the editor and the inspector, issue 1894)', () => {
  it('names each mode', () => {
    expect(contextHistoryLabel('full')).toBe('Everything it may see');
    expect(contextHistoryLabel('since_joined')).toBe('Since it joined');
    expect(contextHistoryLabel('addressed_only')).toBe('Only messages addressed to it');
    expect(contextHistoryLabel('none')).toBe('None (the current message only)');
  });

  it('shows a mode it does not know as itself', () => {
    expect(contextHistoryLabel('future_mode')).toBe('future_mode');
  });
});

describe('contextBlockRefusal', () => {
  function httpError(status: number, body: unknown): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/api/v2/elitea_core/version/x', body });
  }

  it('returns the server’s text for a 422 context_block_invalid', () => {
    const error = httpError(422, { code: 'context_block_invalid', error: 'meta.context.history must be one of full, since_joined, addressed_only, none' });
    expect(contextBlockRefusal(error)).toBe('meta.context.history must be one of full, since_joined, addressed_only, none');
  });

  it('ignores any other refusal', () => {
    expect(contextBlockRefusal(httpError(422, { code: 'soul_invalid', error: 'x' }))).toBeUndefined();
    expect(contextBlockRefusal(httpError(400, { code: 'context_block_invalid', error: 'x' }))).toBeUndefined();
    expect(contextBlockRefusal(new Error('boom'))).toBeUndefined();
    expect(contextBlockRefusal(undefined)).toBeUndefined();
  });
});
